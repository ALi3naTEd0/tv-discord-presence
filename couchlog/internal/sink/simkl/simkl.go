// Package simkl sends your history to Simkl (simkl.com) by external ID.
package simkl

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/sink"
)

// Simkl is the destination. Simkl keeps whether you watched something, not
// each viewing, so Check looks at your lists and Remove unmarks the title
// or episode.
type Simkl struct {
	ClientID string
	Token    func(context.Context) (string, error)
	BaseURL  string // "" = https://api.simkl.com
	sink.Client

	mu     sync.Mutex
	lists  *lists
	listAt time.Time
}

// Name implements sink.Sink.
func (s *Simkl) Name() string { return "simkl" }

func (s *Simkl) base() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return "https://api.simkl.com"
}

func (s *Simkl) url(path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("client_id", s.ClientID)
	q.Set("app-name", "couchlog")
	q.Set("app-version", "1.0")
	return s.base() + path + "?" + q.Encode()
}

func (s *Simkl) auth(ctx context.Context) (map[string]string, error) {
	tok, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + tok, "simkl-api-key": s.ClientID}, nil
}

type ids struct {
	Simkl int    `json:"simkl,omitempty"`
	IMDb  string `json:"imdb,omitempty"`
	TMDB  any    `json:"tmdb,omitempty"`
	TVDB  any    `json:"tvdb,omitempty"`
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return fmt.Sprint(int64(x))
	default:
		return fmt.Sprint(x)
	}
}

// same compares IDs as strings: Simkl sends numeric IDs as text or numbers.
func same(it model.Item, x ids) bool {
	return it.IMDb != "" && it.IMDb == x.IMDb ||
		it.TMDB != 0 && fmt.Sprint(it.TMDB) == str(x.TMDB) ||
		it.TVDB != 0 && fmt.Sprint(it.TVDB) == str(x.TVDB)
}

type listEntry struct {
	Status        string `json:"status"`
	LastWatchedAt string `json:"last_watched_at"`
	Movie         *struct {
		IDs ids `json:"ids"`
	} `json:"movie"`
	Show *struct {
		IDs ids `json:"ids"`
	} `json:"show"`
	Seasons []struct {
		Number   int `json:"number"`
		Episodes []struct {
			Number int `json:"number"`
		} `json:"episodes"`
	} `json:"seasons"`
}

type lists struct{ movies, shows []listEntry }

// all reads your Simkl lists (cached five minutes).
func (s *Simkl) all(ctx context.Context) (*lists, error) {
	s.mu.Lock()
	if s.lists != nil && time.Since(s.listAt) < 5*time.Minute {
		l := s.lists
		s.mu.Unlock()
		return l, nil
	}
	s.mu.Unlock()
	h, err := s.auth(ctx)
	if err != nil {
		return nil, err
	}
	l := &lists{}
	for _, typ := range []string{"movies", "shows", "anime"} {
		var res map[string][]listEntry
		q := url.Values{"extended": {"full"}, "episode_watched_at": {"yes"}}
		if err := s.Do(ctx, "GET", s.url("/sync/all-items/"+typ, q), h, nil, &res, false); err != nil {
			return nil, fmt.Errorf("simkl: reading your lists: %w", err)
		}
		if typ == "movies" {
			l.movies = res[typ]
		} else {
			l.shows = append(l.shows, res[typ]...)
		}
	}
	s.mu.Lock()
	s.lists, s.listAt = l, time.Now()
	s.mu.Unlock()
	return l, nil
}

func (l *lists) has(it model.Item) bool {
	if it.Kind == model.KindMovie {
		for _, x := range l.movies {
			if x.Movie != nil && (x.Status == "completed" || x.LastWatchedAt != "") && same(it, x.Movie.IDs) {
				return true
			}
		}
		return false
	}
	for _, x := range l.shows {
		if x.Show == nil || !same(it, x.Show.IDs) {
			continue
		}
		for _, se := range x.Seasons {
			if se.Number != it.Season {
				continue
			}
			for _, e := range se.Episodes {
				if e.Number == it.Episode {
					return true
				}
			}
		}
		// Simkl lists no episodes for shows marked completed as a whole.
		if x.Status == "completed" && len(x.Seasons) == 0 {
			return true
		}
	}
	return false
}

// idsFor lists the unambiguous IDs first (IMDb, then TVDB for shows, then TMDB).
func idsFor(it model.Item) ids {
	x := ids{IMDb: it.IMDb}
	if it.TMDB != 0 {
		x.TMDB = fmt.Sprint(it.TMDB)
	}
	if it.TVDB != 0 && it.Kind == model.KindEpisode {
		x.TVDB = fmt.Sprint(it.TVDB)
	}
	return x
}

func (s *Simkl) exists(ctx context.Context, it model.Item) (bool, error) {
	x := idsFor(it)
	typ := "movie"
	if it.Kind == model.KindEpisode {
		typ = "show"
	}
	tried := false
	for _, q := range []struct{ src, id string }{{"imdb", x.IMDb}, {"tvdb", str(x.TVDB)}, {"tmdb", str(x.TMDB)}} {
		if q.id == "" {
			continue
		}
		tried = true
		v := url.Values{q.src: {q.id}}
		if q.src != "imdb" {
			v.Set("type", typ)
		}
		var res []map[string]any
		if err := s.Do(ctx, "GET", s.url("/search/id", v), nil, nil, &res, false); err != nil {
			return false, err
		}
		if len(res) > 0 {
			return true, nil
		}
	}
	if !tried {
		return false, fmt.Errorf("simkl: no external IDs")
	}
	return false, nil
}

// Check implements sink.Sink.
func (s *Simkl) Check(ctx context.Context, entries []sink.Entry) ([]sink.Result, error) {
	l, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sink.Result, 0, len(entries))
	for _, e := range entries {
		r := sink.Result{ItemID: e.Item.ID}
		if l.has(e.Item) {
			r.Status, r.Error = model.Added, "Already in your history"
		} else if ok, err := s.exists(ctx, e.Item); err != nil {
			r.Status, r.Error = model.Failed, err.Error()
		} else if ok {
			r.Status = model.Ready
		} else {
			r.Status, r.Error = model.NotFound, "Not on Simkl yet"
		}
		out = append(out, r)
	}
	return out, nil
}

func body(e sink.Entry, withTime bool) map[string]any {
	it := e.Item
	entry := map[string]any{"title": it.Title, "ids": idsFor(it)}
	if it.Year != 0 {
		entry["year"] = it.Year
	}
	at := time.UnixMilli(e.WatchedAt).UTC().Format("2006-01-02T15:04:05Z")
	if it.Kind == model.KindMovie {
		if withTime {
			entry["watched_at"] = at
		}
		return map[string]any{"movies": []any{entry}}
	}
	ep := map[string]any{"number": it.Episode}
	if withTime {
		ep["watched_at"] = at
	}
	entry["seasons"] = []any{map[string]any{"number": it.Season, "episodes": []any{ep}}}
	return map[string]any{"shows": []any{entry}}
}

type syncResult struct {
	Added    map[string]int `json:"added"`
	Deleted  map[string]int `json:"deleted"`
	NotFound map[string]any `json:"not_found"`
}

func (r syncResult) notFound() bool {
	for _, v := range r.NotFound {
		if l, ok := v.([]any); ok && len(l) > 0 {
			return true
		}
	}
	return false
}

func sum(m map[string]int) int { return m["movies"] + m["shows"] + m["episodes"] }

func failed(err error) sink.Result {
	msg := "Simkl: " + err.Error()
	if strings.Contains(strings.ToUpper(err.Error()), "RATE_LIMIT") {
		msg = "Simkl asks to wait a few seconds between writes; it will be retried"
	}
	return sink.Result{Status: model.Failed, Error: msg}
}

// Push implements sink.Sink.
func (s *Simkl) Push(ctx context.Context, entries []sink.Entry) ([]sink.Result, error) {
	h, err := s.auth(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sink.Result, 0, len(entries))
	for _, e := range entries {
		var r sink.Result
		var res syncResult
		switch {
		case e.Item.IMDb == "" && e.Item.TMDB == 0 && e.Item.TVDB == 0:
			r = sink.Result{Status: model.NotFound, Error: "No external IDs"}
		default:
			if err := s.Do(ctx, "POST", s.url("/sync/history", nil), h, body(e, true), &res, true); err != nil {
				r = failed(err)
			} else if res.notFound() {
				r = sink.Result{Status: model.NotFound, Error: "Not on Simkl yet"}
			} else if sum(res.Added) >= 1 {
				r = sink.Result{Status: model.Added}
			} else {
				r = sink.Result{Status: model.Skipped, Error: "Simkl added nothing (already marked?)"}
			}
		}
		r.ItemID = e.Item.ID
		out = append(out, r)
	}
	s.mu.Lock()
	s.lists = nil // your lists changed
	s.mu.Unlock()
	return out, nil
}

// Remove implements sink.Sink: Simkl keeps one mark per title or episode,
// so the mark goes.
func (s *Simkl) Remove(ctx context.Context, entries []sink.Entry) ([]sink.Result, error) {
	h, err := s.auth(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sink.Result, 0, len(entries))
	for _, e := range entries {
		r := sink.Result{Status: model.Removed}
		var res syncResult
		if err := s.Do(ctx, "POST", s.url("/sync/history/remove", nil), h, body(e, false), &res, true); err != nil {
			r = failed(err)
		} else if sum(res.Deleted) == 0 {
			r.Error = "Not in your Simkl history"
		}
		r.ItemID = e.Item.ID
		out = append(out, r)
	}
	s.mu.Lock()
	s.lists = nil
	s.mu.Unlock()
	return out, nil
}

var _ sink.Sink = (*Simkl)(nil)
