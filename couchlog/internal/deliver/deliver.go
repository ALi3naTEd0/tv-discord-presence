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
	Log      *slog.Logger
	Now      func() time.Time
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

// Eligible says whether a session is sent on its own, and why not. Plex
// already marked its plays as watched; TV sessions need most of the
// runtime watched (a real runtime, not an estimate).
func (d *Deliverer) Eligible(w store.Watch) (bool, string) {
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
	// the measure: played time adds up every attempt and rewind.
	if w.Session.Position != nil {
		if len(w.Items) == 1 && w.DeviceKind == "androidtv" {
			watched = *w.Session.Position
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
	watches, err := d.Store.Watches(ctx, now.AddDate(0, 0, -60), now.Add(-time.Minute))
	if err != nil {
		return nil, err
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
	for _, w := range watches {
		ok, note := d.Eligible(w)
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
