// Package deliver sends identified viewings to the destinations (Trakt,
// Simkl) with the same rules the dashboard used: Plex plays always; TV
// sessions once you watched most of the title; always checking first that
// it is not already in your history, so nothing is ever sent twice.
package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/config"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/sink"
	"github.com/ALi3naTEd0/couchlog/internal/sink/simkl"
	"github.com/ALi3naTEd0/couchlog/internal/sink/trakt"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// Deliverer runs the deliveries.
type Deliverer struct {
	Store *store.Store
	Sinks []sink.Sink
	// ReadOnly only checks: what is already in your history is marked as
	// added, nothing is sent or removed.
	ReadOnly bool
	MinRatio float64 // share of the runtime to send a TV session; 0 = 0.8
	// Rewatch: the same title again within this long after an earlier
	// session is not sent on its own (usually the same viewing, resumed or
	// recorded twice); you confirm a real second viewing. 0 = 24 h.
	Rewatch time.Duration
	Log     *slog.Logger
	Now     func() time.Time
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// WatchedTimes spreads a session's items over its length: each ends at its
// share of the session (a session may hold several episodes).
func WatchedTimes(w store.Watch) map[int64]int64 {
	s := w.Session
	end := s.LastSeenAt
	if s.EndedAt != nil {
		end = *s.EndedAt
	}
	span := end.Sub(s.StartedAt)
	if span <= 0 {
		span = time.Second
	}
	out := map[int64]int64{}
	n := len(w.Items)
	for k, it := range w.Items {
		out[it.ID] = s.StartedAt.Add(span * time.Duration(k+1) / time.Duration(n)).UnixMilli()
	}
	return out
}

// WatchedInOrder says whether a guessed episode (the next one in order,
// when the app did not say which) was watched as that: one title with a
// real runtime, from the start (the furthest point is no more than 3 min
// past the time played, so nothing was skipped) to 80 %. Binge watching in
// order is what this is; a wrong one is taken back out by correcting it.
func (d *Deliverer) WatchedInOrder(w store.Watch) (model.Item, bool) {
	if len(w.Items) != 1 || !w.Items[0].Guessed || w.Session.Position == nil {
		return model.Item{}, false
	}
	it := w.Items[0]
	if it.RuntimeMinutes == 0 || it.RuntimeEstimated {
		return model.Item{}, false
	}
	ratio := d.MinRatio
	if ratio == 0 {
		ratio = 0.8
	}
	runtime := time.Duration(it.RuntimeMinutes) * time.Minute
	pos, played := *w.Session.Position, w.Session.Played
	if pos < time.Duration(float64(runtime)*ratio) || pos-played > 3*time.Minute {
		return model.Item{}, false
	}
	return it, true
}

// Eligible says whether a session is sent on its own, and why not. Plex
// already marked its plays as watched; TV sessions need most of the
// runtime watched (a real runtime, not an estimate).
func (d *Deliverer) Eligible(w store.Watch) (bool, string) {
	for _, it := range w.Items {
		if it.Guessed {
			return false, "Episode guessed from your progress: confirm it with Identify to send it"
		}
	}
	if w.DeviceKind == "plex" {
		return true, ""
	}
	var expected time.Duration
	for _, it := range w.Items {
		if it.RuntimeEstimated {
			return false, "Runtime is only an estimate: not sent automatically. Send it if you finished it"
		}
		if it.RuntimeMinutes == 0 {
			return false, "Runtime unknown: not sent automatically"
		}
		expected += time.Duration(it.RuntimeMinutes) * time.Minute
	}
	watched := w.Session.Played
	// With one title and a real position (Android TV) the furthest point is
	// the measure: played time adds up every attempt and rewind. An episode
	// picked up over several sessions is one viewing: its furthest point in
	// any of them counts.
	if w.Session.Position != nil {
		if len(w.Items) == 1 && w.DeviceKind == "androidtv" {
			watched = *w.Session.Position
			for _, p := range d.parts(w) {
				if p.Position > watched {
					watched = p.Position
				}
			}
		} else if *w.Session.Position > watched {
			watched = *w.Session.Position
		}
	}
	min := d.MinRatio
	if min == 0 {
		min = 0.8
	}
	ratio := float64(watched) / float64(expected)
	if ratio < min {
		return false, fmt.Sprintf("Watched %d%% of the runtime (< %d%%): not sent automatically", int(ratio*100), int(min*100))
	}
	return true, ""
}

// PartsWindow is how long after an earlier session an episode picked up
// again is still the same viewing.
const PartsWindow = 24 * time.Hour

func (d *Deliverer) parts(w store.Watch) []store.Part {
	if d.Store == nil || w.Session == nil {
		return nil
	}
	ps, _ := d.Store.Parts(context.Background(), w, PartsWindow)
	return ps
}

// absorbParts marks the earlier partial sessions of a title just sent as
// part of that viewing, never sent on their own.
func (d *Deliverer) absorbParts(ctx context.Context, w store.Watch, sinkName string) {
	if w.Session == nil {
		return
	}
	note := "Part of the viewing sent with the session at " + w.Session.StartedAt.Local().Format("15:04")
	for _, p := range d.parts(w) {
		d.Store.SetDelivery(ctx, p.ItemID, sinkName, model.Skipped, note, d.now())
	}
}

// Report counts what a run did, per destination.
type Report map[string]map[model.DeliveryStatus]int

func (r Report) add(sinkName string, st model.DeliveryStatus) {
	if r[sinkName] == nil {
		r[sinkName] = map[model.DeliveryStatus]int{}
	}
	r[sinkName][st]++
}

// Run checks and sends what is due, and takes out what corrections replaced.
func (d *Deliverer) Run(ctx context.Context) (Report, error) {
	now := d.now()
	watches, err := d.Store.Watches(ctx, now.AddDate(0, 0, -60), now.Add(-15*time.Second))
	if err != nil {
		return nil, err
	}
	for _, w := range watches {
		if it, ok := d.WatchedInOrder(w); ok {
			if err := d.Store.ConfirmGuess(ctx, it.ID); err != nil {
				return nil, err
			}
			for i := range w.Items {
				w.Items[i].Guessed = false
			}
			if d.Log != nil {
				d.Log.Info("guessed episode watched from the start: sending it", "session", w.Session.ID,
					"title", it.Title, "season", it.Season, "episode", it.Episode)
			}
		}
	}
	names := make([]string, 0, len(d.Sinks))
	for _, sk := range d.Sinks {
		names = append(names, sk.Name())
	}
	if n, err := d.Store.EnsureDeliveries(ctx, names, now.AddDate(0, 0, -60), now); err != nil {
		return nil, err
	} else if n > 0 && d.Log != nil {
		d.Log.Info("titles waiting for a destination are now checked", "deliveries", n)
	}
	rep := Report{}
	var errs []error
	for _, sk := range d.Sinks {
		if err := d.runSink(ctx, sk, watches, rep); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sk.Name(), err))
		}
	}
	return rep, errors.Join(errs...)
}

func (d *Deliverer) runSink(ctx context.Context, sk sink.Sink, watches []store.Watch, rep Report) error {
	name, now := sk.Name(), d.now()
	var due []sink.Entry
	watchOf := map[int64]store.Watch{}
	rewatch := d.Rewatch
	if rewatch == 0 {
		rewatch = 24 * time.Hour
	}
	for _, w := range watches {
		ok, note := d.Eligible(w)
		if ok {
			prev, err := d.Store.SeenShortlyBefore(ctx, w, rewatch)
			if err != nil {
				return err
			}
			if prev != nil {
				ok, note = false, fmt.Sprintf("You already watched this (%s): not sent automatically. Send it if it was a second viewing",
					prev.EndedAt.Local().Format("Jan 2 15:04"))
			}
		}
		times := WatchedTimes(w)
		for _, it := range w.Items {
			del, err := d.Store.DeliveryOf(ctx, it.ID, name)
			if err != nil {
				continue // identified while this destination was not set up
			}
			switch del.Status {
			case model.Pending, model.Ready:
			case model.Failed:
				if del.Attempts >= 5 {
					continue
				}
			case model.NotFound: // titles get listed later: ask again daily
				if now.Sub(del.UpdatedAt) < 24*time.Hour {
					continue
				}
			default:
				continue
			}
			if !ok {
				d.Store.SetDeliveryNote(ctx, it.ID, name, note, now)
				continue
			}
			due = append(due, sink.Entry{Item: it, WatchedAt: times[it.ID]})
			watchOf[it.ID] = w
		}
	}
	if len(due) > 0 {
		res, err := sk.Check(ctx, due)
		if err != nil {
			return err
		}
		var send []sink.Entry
		for i, r := range res {
			switch r.Status {
			case model.Ready:
				send = append(send, due[i])
				if d.ReadOnly {
					d.Store.SetDelivery(ctx, r.ItemID, name, model.Ready, "", now)
					rep.add(name, model.Ready)
				}
			default:
				d.Store.SetDelivery(ctx, r.ItemID, name, r.Status, r.Error, now)
				rep.add(name, r.Status)
			}
		}
		if !d.ReadOnly && len(send) > 0 {
			res, err := sk.Push(ctx, send)
			if err != nil {
				return err
			}
			for _, r := range res {
				d.Store.SetDelivery(ctx, r.ItemID, name, r.Status, r.Error, d.now())
				rep.add(name, r.Status)
				if r.Status == model.Added {
					d.absorbParts(ctx, watchOf[r.ItemID], name)
				}
			}
		}
		for i, r := range res { // already in your history counts as sent too
			if r.Status == model.Added {
				d.absorbParts(ctx, watchOf[due[i].Item.ID], name)
			}
		}
	}
	if d.ReadOnly {
		return nil
	}
	replaced, err := d.Store.Replaced(ctx, name)
	if err != nil || len(replaced) == 0 {
		return err
	}
	var gone []sink.Entry
	for _, w := range replaced {
		times := WatchedTimes(w)
		gone = append(gone, sink.Entry{Item: w.Items[0], WatchedAt: times[w.Items[0].ID]})
	}
	res, err := sk.Remove(ctx, gone)
	if err != nil {
		return err
	}
	for _, r := range res {
		if r.Status == model.Removed {
			d.Store.RemoveDone(ctx, r.ItemID, name, d.now())
		} else {
			d.Store.SetDeliveryNote(ctx, r.ItemID, name, r.Error, d.now())
		}
		rep.add(name, r.Status)
	}
	return nil
}

// Sinks builds the configured destinations; one without credentials is
// left out with a reason.
func Sinks(cfg *config.Config) ([]sink.Sink, []error) {
	var out []sink.Sink
	var errs []error
	for _, name := range cfg.Sinks {
		switch name {
		case "trakt":
			s := cfg.Trakt
			if s == nil {
				s = &config.Service{}
			}
			id := s.ClientID
			if id == "" {
				id = cfg.Secret("COUCHLOG_TRAKT_CLIENT_ID")
			}
			if id == "" || s.TokenFile == "" {
				errs = append(errs, errors.New("trakt: no client id (COUCHLOG_TRAKT_CLIENT_ID) or token file"))
				continue
			}
			out = append(out, &trakt.Trakt{ClientID: id, Token: tokenFrom(s.TokenFile)})
		case "simkl":
			s := cfg.Simkl
			if s == nil {
				s = &config.Service{}
			}
			id := s.ClientID
			if id == "" {
				id = cfg.Secret("COUCHLOG_SIMKL_CLIENT_ID", "SIMKL_CLIENT_ID")
			}
			if id == "" || s.TokenFile == "" {
				errs = append(errs, errors.New("simkl: no client id (SIMKL_CLIENT_ID) or token file"))
				continue
			}
			out = append(out, &simkl.Simkl{ClientID: id, Token: tokenFrom(s.TokenFile)})
		default:
			errs = append(errs, fmt.Errorf("unknown destination %q", name))
		}
	}
	return out, errs
}

// tokenFrom reads an OAuth token file. It does not renew tokens: for now
// they belong to trakt-scrobbler and your Simkl setup, which renew them
// (a refresh token is single-use; renewing from two places breaks it).
func tokenFrom(path string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		var t struct {
			AccessToken string  `json:"access_token"`
			ExpiresAt   float64 `json:"expires_at"`
			CreatedAt   float64 `json:"created_at"`
			ExpiresIn   float64 `json:"expires_in"`
		}
		if err := json.Unmarshal(b, &t); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		exp := t.ExpiresAt
		if exp == 0 && t.CreatedAt > 0 && t.ExpiresIn > 0 {
			exp = t.CreatedAt + t.ExpiresIn
		}
		if exp > 0 && float64(time.Now().Unix()) > exp {
			return "", fmt.Errorf("token in %s expired; the app that owns it renews it on its next use", path)
		}
		if strings.TrimSpace(t.AccessToken) == "" {
			return "", fmt.Errorf("no access_token in %s", path)
		}
		return t.AccessToken, nil
	}
}
