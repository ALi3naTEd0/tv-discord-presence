// Package server wires Couchlog together: each configured TV or box feeds
// its own engine tracker, what a device publishes is identified when that
// is certain, and Plex's own history is imported every few minutes.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/config"
	"github.com/ALi3naTEd0/couchlog/internal/engine"
	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/source"
	"github.com/ALi3naTEd0/couchlog/internal/source/androidtv"
	"github.com/ALi3naTEd0/couchlog/internal/source/plex"
	"github.com/ALi3naTEd0/couchlog/internal/source/webos"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// Server is a running Couchlog.
type Server struct {
	Config *config.Config
	Store  *store.Store
	Log    *slog.Logger
	// Ident identifies titles; nil without a TMDB key (sessions are still
	// recorded, and identified later).
	Ident *identify.Identifier
	// PlexEvery is how often Plex's history is imported; 0 = 5 min.
	PlexEvery time.Duration

	// newSource builds a device's source; tests replace it.
	newSource func(d config.Device, id int64) source.Source
}

// New prepares a server from the settings: the TMDB key and Plex token are
// read from the environment or the settings' env files.
func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Server {
	s := &Server{Config: cfg, Store: st, Log: log}
	if key := cfg.Secret("COUCHLOG_TMDB_KEY", "TMDB_API_KEY"); key != "" {
		s.Ident = &identify.Identifier{Catalog: &identify.TMDB{Key: key, Language: cfg.Language}, Aliases: st,
			Posters: &identify.IMDb{}, History: st}
	} else {
		log.Warn("no TMDB key (COUCHLOG_TMDB_KEY): sessions are recorded but not identified")
	}
	return s
}

func (s *Server) source(d config.Device, id int64) source.Source {
	if s.newSource != nil {
		return s.newSource(d, id)
	}
	switch d.Kind {
	case "webos":
		return &webos.Source{Host: d.Host, DeviceID: id, KeyPath: d.KeyFile, IgnoreApps: []string{"cdp-30"},
			Log: s.Log.With("device", d.Name)}
	default:
		return &androidtv.Source{Addr: d.Host, DeviceID: id, KeyPath: d.KeyFile, Log: s.Log.With("device", d.Name)}
	}
}

// Run listens to every device and imports Plex until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, d := range s.Config.Devices {
		id, err := s.Store.EnsureDevice(ctx, store.LocalUserID, d.Kind, d.Name, d.Host)
		if err != nil {
			return err
		}
		tr, err := engine.NewTracker(ctx, s.Store, engine.Device{ID: id, TitleMediaIDs: d.Kind == "androidtv"},
			engine.Config{}, time.Now(), s.Log.With("device", d.Name))
		if err != nil {
			return err
		}
		tr.Sinks = s.Config.Sinks
		if s.Ident != nil {
			tr.Next = s.Ident.Next
		}
		src := s.source(d, id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.follow(ctx, d.Name, src, tr)
		}()
	}
	if s.Ident != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.posterLoop(ctx)
		}()
	}
	if p := s.Config.Plex; p != nil {
		if token := s.Config.Secret("COUCHLOG_PLEX_TOKEN", "PLEX_TOKEN"); token == "" {
			s.Log.Warn("no Plex token (COUCHLOG_PLEX_TOKEN): Plex is not imported")
		} else {
			id, err := s.Store.EnsureDevice(ctx, store.LocalUserID, "plex", "Plex", p.URL)
			if err != nil {
				return err
			}
			db := p.DB
			if db == "" {
				db = plex.DefaultDBPath()
			}
			c := &plex.Client{URL: p.URL, Token: token, AccountID: p.Account, User: p.User, DeviceID: id, DBPath: db}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.importPlex(ctx, c, p.ImportDays)
			}()
		}
	}
	wg.Wait()
	return nil
}

// follow feeds one device's observations to its tracker. The source keeps
// running while the tracker works: observations wait in a buffer, and the
// oldest are dropped if the database stalls for minutes.
func (s *Server) follow(ctx context.Context, name string, src source.Source, tr *engine.Tracker) {
	obs := make(chan model.Observation, 256)
	go func() {
		defer close(obs)
		if err := src.Run(ctx, func(o model.Observation) {
			select {
			case obs <- o:
			default:
				s.Log.Warn("observations dropped: the tracker is behind", "device", name)
			}
		}); err != nil {
			s.Log.Error("source stopped", "device", name, "err", err)
		}
	}()
	tried := map[string]bool{} // session+title already sent to Auto
	for o := range obs {
		sess, err := tr.Observe(ctx, o)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Error("observe", "device", name, "err", err)
			}
			continue
		}
		if sess != nil && sess.Hint.Title != "" && s.Ident != nil {
			key := fmt.Sprintf("%d\x00%s", sess.ID, sess.Hint.Title)
			if !tried[key] {
				tried[key] = true
				s.autoIdentify(ctx, sess)
			}
		}
	}
}

// autoIdentify identifies a session from what its device published, when
// identify.Auto is sure. A session that already has items is left alone.
func (s *Server) autoIdentify(ctx context.Context, sess *model.Session) {
	items, err := s.Store.Items(ctx, sess.ID)
	if err != nil || len(items) > 0 {
		return
	}
	it, how, err := s.Ident.Auto(ctx, sess.UserID, sess)
	if err != nil {
		s.Log.Warn("identify", "session", sess.ID, "title", sess.Hint.Title, "err", err)
		return
	}
	if it == nil {
		return
	}
	sinks := s.Config.Sinks
	if how.Guessed() {
		sinks = nil // a guessed episode waits for you to confirm it before going anywhere
	}
	if err := s.Store.Identify(ctx, sess.ID, []model.Item{*it}, sinks, time.Now()); err != nil {
		s.Log.Error("identify", "session", sess.ID, "err", err)
		return
	}
	s.Log.Info("identified", "session", sess.ID, "title", it.Title, "season", it.Season, "episode", it.Episode, "by", how)
}

// importPlex brings in Plex's own history now and every PlexEvery.
func (s *Server) importPlex(ctx context.Context, c *plex.Client, days int) {
	every := s.PlexEvery
	if every <= 0 {
		every = 5 * time.Minute
	}
	for {
		if n, err := s.ImportPlex(ctx, c, days); err != nil {
			s.Log.Warn("Plex import", "err", err)
		} else if n > 0 {
			s.Log.Info("Plex import", "added", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// ImportPlex imports Plex plays since the last one it has (minus two days,
// for plays recorded late), or the last days on the first run.
func (s *Server) ImportPlex(ctx context.Context, c *plex.Client, days int) (int, error) {
	since := time.Now().AddDate(0, 0, -days)
	var last sql.NullInt64
	if err := s.Store.DB().QueryRowContext(ctx, "SELECT MAX(ended_at) FROM sessions WHERE device_id = ?",
		c.DeviceID).Scan(&last); err != nil {
		return 0, err
	}
	if last.Valid {
		if t := time.UnixMilli(last.Int64).Add(-48 * time.Hour); t.After(since) {
			since = t
		}
	}
	plays, err := c.Plays(ctx, since)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, p := range plays {
		ok, err := s.Store.AddPlay(ctx, p, "plex", "Plex", s.Config.Sinks, time.Now())
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}

// posterLoop fills missing posters every two minutes; a title without any
// is retried every six hours.
func (s *Server) posterLoop(ctx context.Context) {
	tried := map[int64]time.Time{}
	for {
		if n, err := s.FillPosters(ctx, tried); err != nil {
			s.Log.Warn("posters", "err", err)
		} else if n > 0 {
			s.Log.Info("posters filled", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
	}
}

// FillPosters gives a poster to recent items without one: TMDB again, then
// IMDb (see identify.Identifier.Poster). tried remembers attempts.
func (s *Server) FillPosters(ctx context.Context, tried map[int64]time.Time) (int, error) {
	items, err := s.Store.ItemsWithoutPoster(ctx, time.Now().AddDate(0, 0, -30))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		if time.Since(tried[it.ID]) < 6*time.Hour {
			continue
		}
		tried[it.ID] = time.Now()
		if p := s.Ident.Poster(ctx, it); p != "" {
			if err := s.Store.SetPoster(ctx, it.ID, p); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
