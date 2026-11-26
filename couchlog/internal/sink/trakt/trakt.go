// Package trakt sends your history to Trakt (trakt.tv) by external ID.
package trakt

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/sink"
)

// Window is how far a play in your Trakt history may be from an entry's
// watched time and still be that viewing (Trakt keeps the time you send,
// but plays may come from other apps: trakt-scrobbler, Plex).
const Window = 12 * time.Hour

// Trakt is the destination.
type Trakt struct {
	ClientID string
	// Token returns a valid access token (Couchlog's device login, or for
	// now trakt-scrobbler's token, which it renews itself).
	Token   func(context.Context) (string, error)
	BaseURL string // "" = https://api.trakt.tv
	sink.Client
}

// Name implements sink.Sink.
func (t *Trakt) Name() string { return "trakt" }

func (t *Trakt) base() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return "https://api.trakt.tv"
}

func (t *Trakt) headers(ctx context.Context, auth bool) (map[string]string, error) {
	h := map[string]string{"trakt-api-key": t.ClientID, "trakt-api-version": "2"}
	if auth {
		tok, err := t.Token(ctx)
		if err != nil {
			return nil, err
		}
		h["Authorization"] = "Bearer " + tok
	}
	return h, nil
}

type ids struct {
	Trakt int    `json:"trakt,omitempty"`
	IMDb  string `json:"imdb,omitempty"`
	TMDB  int    `json:"tmdb,omitempty"`
	TVDB  int    `json:"tvdb,omitempty"`
}

func idsOf(it model.Item) ids {
	x := ids{IMDb: it.IMDb, TMDB: it.TMDB}
	if it.Kind == model.KindEpisode {
		x.TVDB = it.TVDB // a movie's TVDB id is not what Trakt matches on
	}
	return x
}

func (a ids) same(b ids) bool {
	return a.IMDb != "" && a.IMDb == b.IMDb || a.TMDB != 0 && a.TMDB == b.TMDB || a.TVDB != 0 && a.TVDB == b.TVDB
}

type play struct {
	ID        int64     `json:"id"`
	WatchedAt time.Time `json:"watched_at"`
	Movie     *struct {
		IDs ids `json:"ids"`
	} `json:"movie"`
	Show *struct {
		IDs ids `json:"ids"`
	} `json:"show"`
	Episode *struct {
		Season int `json:"season"`
		Number int `json:"number"`
	} `json:"episode"`
}

// history lists your plays of one kind ("movies", "episodes") in a range.
func (t *Trakt) history(ctx context.Context, kind string, from, to time.Time) ([]play, error) {
	h, err := t.headers(ctx, true)
	if err != nil {
		return nil, err
	}
	var out []play
	for page := 1; page <= 30; page++ {
		q := url.Values{"start_at": {from.UTC().Format(time.RFC3339)}, "end_at": {to.UTC().Format(time.RFC3339)},
			"page": {fmt.Sprint(page)}, "limit": {"100"}}
		var batch []play
		if err := t.Do(ctx, "GET", t.base()+"/sync/history/"+kind+"?"+q.Encode(), h, nil, &batch, false); err != nil {
			return nil, fmt.Errorf("trakt: reading your history: %w", err)
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// match is the play of e in plays (closest in time within Window), or nil.
func match(e sink.Entry, plays []play) *play {
	want := idsOf(e.Item)
	at := time.UnixMilli(e.WatchedAt)
	var best *play
	var bestGap time.Duration
	for i, p := range plays {
		var got ids
		if e.Item.Kind == model.KindMovie {
			if p.Movie == nil {
				continue
			}
			got = p.Movie.IDs
		} else {
			if p.Show == nil || p.Episode == nil || p.Episode.Season != e.Item.Season || p.Episode.Number != e.Item.Episode {
				continue
			}
			got = p.Show.IDs
		}
		gap := p.WatchedAt.Sub(at)
		if gap < 0 {
			gap = -gap
		}
		if gap <= Window && want.same(got) && (best == nil || gap < bestGap) {
			best, bestGap = &plays[i], gap
		}
	}
	return best
}

func kindOf(it model.Item) string {
	if it.Kind == model.KindMovie {
		return "movies"
	}
	return "episodes"
}

// plays reads your history around the entries, per kind.
func (t *Trakt) plays(ctx context.Context, entries []sink.Entry) (map[string][]play, error) {
	out := map[string][]play{}
	ranges := map[string][2]time.Time{}
	for _, e := range entries {
		k, at := kindOf(e.Item), time.UnixMilli(e.WatchedAt)
		r, ok := ranges[k]
		if !ok || at.Add(-Window).Before(r[0]) {
			r[0] = at.Add(-Window)
		}
		if !ok || at.Add(Window).After(r[1]) {
			r[1] = at.Add(Window)
		}
		ranges[k] = r
	}
	for k, r := range ranges {
		p, err := t.history(ctx, k, r[0], r[1])
		if err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, nil
}

// exists reports whether Trakt has the title (by any ID).
func (t *Trakt) exists(ctx context.Context, it model.Item) (bool, error) {
	typ := "movie"
	if it.Kind == model.KindEpisode {
		typ = "show"
	}
	h, _ := t.headers(ctx, false)
	x := idsOf(it)
	tried := false
	for _, q := range []struct{ src, id string }{{"imdb", x.IMDb}, {"tmdb", itoa(x.TMDB)}, {"tvdb", itoa(x.TVDB)}} {
		if q.id == "" {
			continue
		}
		tried = true
		var res []map[string]any
		if err := t.Do(ctx, "GET", t.base()+"/search/"+q.src+"/"+url.PathEscape(q.id)+"?type="+typ, h, nil, &res, false); err != nil {
			return false, err
		}
		// An IMDb id shared by a movie and a show answers with the movie
		// even for type=show: only a hit of the right type counts.
		for _, r := range res {
			if r["type"] == typ && r[typ] != nil {
				return true, nil
			}
		}
	}
	if !tried {
		return false, fmt.Errorf("trakt: no external IDs")
	}
	return false, nil
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

// Check implements sink.Sink: already in your history (Added), on Trakt
// but not in your history (Ready), or not on Trakt yet (NotFound).
func (t *Trakt) Check(ctx context.Context, entries []sink.Entry) ([]sink.Result, error) {
	plays, err := t.plays(ctx, entries)
	if err != nil {
		return nil, err
	}
	out := make([]sink.Result, 0, len(entries))
	for _, e := range entries {
		r := sink.Result{ItemID: e.Item.ID}
		if match(e, plays[kindOf(e.Item)]) != nil {
			r.Status, r.Error = model.Added, "Already in your history"
		} else if ok, err := t.exists(ctx, e.Item); err != nil {
			r.Status, r.Error = model.Failed, err.Error()
		} else if ok {
			r.Status = model.Ready
		} else {
			r.Status, r.Error = model.NotFound, "Not on Trakt yet"
		}
		out = append(out, r)
	}
	return out, nil
}

// bodyFor is the /sync/history body for one play.
func bodyFor(e sink.Entry) map[string]any {
	at := time.UnixMilli(e.WatchedAt).UTC().Format("2006-01-02T15:04:05.000Z")
	if e.Item.Kind == model.KindMovie {
		return map[string]any{"movies": []any{map[string]any{"watched_at": at, "ids": idsOf(e.Item)}}}
	}
	return map[string]any{"shows": []any{map[string]any{
		"ids": idsOf(e.Item),
		"seasons": []any{map[string]any{"number": e.Item.Season,
			"episodes": []any{map[string]any{"number": e.Item.Episode, "watched_at": at}}}},
	}}}
}

// Push implements sink.Sink: one play per entry, at its watched time.
func (t *Trakt) Push(ctx context.Context, entries []sink.Entry) ([]sink.Result, error) {
	h, err := t.headers(ctx, true)
	if err != nil {
		return nil, err
	}
	out := make([]sink.Result, 0, len(entries))
	for _, e := range entries {
		r := sink.Result{ItemID: e.Item.ID}
		x := idsOf(e.Item)
		if x.IMDb == "" && x.TMDB == 0 && x.TVDB == 0 {
			r.Status, r.Error = model.NotFound, "No external IDs"
			out = append(out, r)
			continue
		}
		var res struct {
			Added    map[string]int `json:"added"`
			NotFound map[string]any `json:"not_found"`
		}
		if err := t.Do(ctx, "POST", t.base()+"/sync/history", h, bodyFor(e), &res, true); err != nil {
			r.Status, r.Error = model.Failed, "Trakt: "+err.Error()
		} else if res.Added["movies"]+res.Added["episodes"] >= 1 {
			r.Status = model.Added
		} else if notEmpty(res.NotFound) {
			r.Status, r.Error = model.NotFound, "Not on Trakt yet"
		} else {
			r.Status, r.Error = model.Failed, "Trakt added nothing"
		}
		out = append(out, r)
	}
	return out, nil
}

func notEmpty(m map[string]any) bool {
	for _, v := range m {
		if l, ok := v.([]any); ok && len(l) > 0 {
			return true
		}
	}
	return false
}

// Remove implements sink.Sink: takes out only the play at the entry's
// watched time (by its history id), never other viewings of the title.
// A play that is not there anymore counts as removed.
func (t *Trakt) Remove(ctx context.Context, entries []sink.Entry) ([]sink.Result, error) {
	plays, err := t.plays(ctx, entries)
	if err != nil {
		return nil, err
	}
	h, err := t.headers(ctx, true)
	if err != nil {
		return nil, err
	}
	out := make([]sink.Result, 0, len(entries))
	for _, e := range entries {
		r := sink.Result{ItemID: e.Item.ID, Status: model.Removed}
		p := match(e, plays[kindOf(e.Item)])
		if p == nil {
			r.Error = "Not in your Trakt history"
			out = append(out, r)
			continue
		}
		var res struct {
			Deleted map[string]int `json:"deleted"`
		}
		if err := t.Do(ctx, "POST", t.base()+"/sync/history/remove", h, map[string]any{"ids": []int64{p.ID}}, &res, true); err != nil {
			r.Status, r.Error = model.Failed, "Trakt: "+err.Error()
		}
		out = append(out, r)
	}
	return out, nil
}

var _ sink.Sink = (*Trakt)(nil)
