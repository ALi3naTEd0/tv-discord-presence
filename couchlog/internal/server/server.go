// Package server wires Couchlog together: each configured TV or box feeds
// its own engine tracker, what a device publishes is identified when that
// is certain, and Plex's own history is imported every few minutes.
package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/api"
	"github.com/ALi3naTEd0/couchlog/internal/config"
	"github.com/ALi3naTEd0/couchlog/internal/deliver"
	"github.com/ALi3naTEd0/couchlog/internal/engine"
	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/netflix"
	"github.com/ALi3naTEd0/couchlog/internal/presence"
	"github.com/ALi3naTEd0/couchlog/internal/source"
	"github.com/ALi3naTEd0/couchlog/internal/source/androidtv"
	"github.com/ALi3naTEd0/couchlog/internal/source/plex"
	"github.com/ALi3naTEd0/couchlog/internal/source/webos"
	"github.com/ALi3naTEd0/couchlog/internal/store"
	"github.com/ALi3naTEd0/couchlog/internal/web"
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
	// DataDir holds what Couchlog keeps next to its database (the saved
	// Netflix session).
	DataDir string
	// Version is shown by the dashboard.
	Version string

	mu        sync.Mutex
	splitDone map[string]bool // Netflix splits already made (session/entry)
	nf        *netflix.Client // the Netflix connection, when Netflix is on
	trackers  []deviceTracker // one per TV or box, guarded by mu
	runCtx    context.Context
	wg        *sync.WaitGroup
	// ConfigPath is the settings file, rewritten when a device is added
	// from the dashboard; "" = devices added there last until a restart.
	ConfigPath string
	connect    connectState // Connect Netflix from the dashboard

	// newSource builds a device's source; tests replace it.
	newSource func(d config.Device, id int64) source.Source
}

// New prepares a server from the settings: the TMDB key and Plex token are
// read from the environment or the settings' env files.
func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Server {
	s := &Server{Config: cfg, Store: st, Log: log}
	key := cfg.Secret("COUCHLOG_TMDB_KEY", "TMDB_API_KEY")
	if key == "" {
		key = identify.DefaultKey
	}
	if key != "" {
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
	var plexClient *plex.Client
	s.mu.Lock()
	s.runCtx, s.wg = ctx, &wg
	devices := append([]config.Device(nil), s.Config.Devices...)
	s.mu.Unlock()
	for _, d := range devices {
		if err := s.startDevice(ctx, &wg, d); err != nil {
			return err
		}
	}
	if s.Ident != nil {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.posterLoop(ctx)
		}()
		go func() {
			defer wg.Done()
			s.retryIdentifyLoop(ctx)
		}()
	}
	if len(s.Config.Sinks) > 0 {
		sinks, errs := deliver.Sinks(s.Config)
		for _, e := range errs {
			s.Log.Warn("destination not set up", "err", e)
		}
		if len(sinks) > 0 {
			d := &deliver.Deliverer{Store: s.Store, Sinks: sinks, ReadOnly: s.Config.ReadOnly, Log: s.Log}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.deliverLoop(ctx, d)
			}()
		}
	}
	if s.Config.Netflix && s.Ident != nil {
		path := netflix.SessionPath(s.DataDir)
		s.nf = &netflix.Client{Cookies: netflix.SessionCookies(path), SessionPath: path}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.netflixLoop(ctx, s.nf)
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
			plexClient = c
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.importPlex(ctx, c, p.ImportDays)
			}()
		}
	}
	if addr := s.Config.Listen; addr != "off" {
		if addr == "" {
			addr = config.DefaultListen
		}
		if err := s.serveHTTP(ctx, &wg, addr); err != nil {
			s.Log.Error("dashboard not served", "addr", addr, "err", err)
		}
	}
	if mode := s.Config.Presence; mode == "on" || mode == "dry-run" {
		id := s.Config.DiscordClientID
		if id == "" {
			id = s.Config.Secret("COUCHLOG_DISCORD_CLIENT_ID")
		}
		if id == "" {
			id = presence.DefaultClientID
		}
		if id == "" && mode == "on" {
			s.Log.Warn("no Discord application id: the Discord status stays off")
		} else {
			r := &presence.Runner{ClientID: id, DryRun: mode == "dry-run", Log: s.Log}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.presenceLoop(ctx, r, plexClient)
			}()
		}
	}
	wg.Wait()
	return nil
}

type deviceTracker struct {
	id      int64
	kind    string
	name    string
	keyFile string
	tr      *engine.Tracker
}

// notShownOnTV are apps the TV status skips: music, and Plex, which shows
// from the Plex server with exact IDs instead.
var notShownOnTV = map[string]bool{"com.plexapp.android": true, "com.spotify.tv.android": true,
	"com.google.android.youtube.tvmusic": true}

// presenceLoop updates the Discord status every five seconds: the LG's own
// app first, then the box, then Plex on any device.
func (s *Server) presenceLoop(ctx context.Context, r *presence.Runner, px *plex.Client) {
	defer r.Close()
	for {
		// read each time: devices added from the dashboard join without a restart
		trackers := s.deviceTrackers()
		order := append(append([]deviceTracker{}, filterKind(trackers, "webos")...), filterKind(trackers, "androidtv")...)
		r.Show(s.nowPlaying(ctx, order, px), time.Now())
		select {
		case <-ctx.Done():
			r.Show(nil, time.Now())
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func filterKind(ts []deviceTracker, kind string) []deviceTracker {
	var out []deviceTracker
	for _, t := range ts {
		if t.kind == kind {
			out = append(out, t)
		}
	}
	return out
}

// nowPlaying is what the Discord status shows, or nil.
func (s *Server) nowPlaying(ctx context.Context, order []deviceTracker, px *plex.Client) *presence.Now {
	for _, d := range order {
		o, sess := d.tr.Last()
		if o.App.ID == "" || notShownOnTV[o.App.ID] || (o.State != model.StatePlaying && o.State != model.StatePaused) {
			continue
		}
		n := &presence.Now{App: o.App, State: o.State, Hint: o.Hint, Position: o.Position}
		if sess != nil {
			n.Played = sess.Played
			if its, err := s.Store.Items(ctx, sess.ID); err == nil && len(its) > 0 {
				n.Item = &its[len(its)-1]
			}
		}
		return n
	}
	if px == nil {
		return nil
	}
	now, err := px.NowPlaying(ctx)
	if err != nil || len(now) == 0 {
		return nil
	}
	p := now[0]
	it := &model.Item{Identity: p.Identity, RuntimeMinutes: int(p.Duration.Minutes())}
	if s.Ident != nil && (it.TMDB != 0 || it.IMDb != "") {
		it.Poster = s.Ident.Poster(ctx, *it)
	}
	off := p.Offset
	return &presence.Now{App: model.App{ID: "plex", Name: "Plex"}, State: p.State, Item: it, Position: &off, Device: p.Device}
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

// retryIdentifyLoop tries automatic identification again on the last three
// days' unidentified sessions: half a minute after start (so a new rule
// reaches sessions recorded before it) and every 15 minutes, each session
// and title at most once an hour.
func (s *Server) retryIdentifyLoop(ctx context.Context) {
	tried := map[string]time.Time{}
	wait := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 15 * time.Minute
		list, err := s.Store.UnidentifiedWithHint(ctx, store.LocalUserID, time.Now().Add(-72*time.Hour))
		if err != nil {
			s.Log.Warn("identify again", "err", err)
			continue
		}
		for _, sess := range list {
			key := fmt.Sprintf("%d\x00%s\x00%s", sess.ID, sess.Hint.Title, sess.Hint.Subtitle)
			if time.Since(tried[key]) < time.Hour {
				continue
			}
			tried[key] = time.Now()
			s.autoIdentify(ctx, sess)
		}
	}
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

// netflixLoop identifies Netflix sessions from the account's viewing
// activity every two minutes, asking Netflix only while some from the last
// week wait (one request; titles' metadata is cached).
func (s *Server) netflixLoop(ctx context.Context, c *netflix.Client) {
	for {
		if n, err := s.IdentifyNetflix(ctx, c); err != nil {
			s.Log.Warn("Netflix", "err", err)
		} else if n > 0 {
			s.Log.Info("Netflix sessions identified", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
	}
}

// IdentifyNetflix matches unidentified Netflix sessions of the last week
// with the viewing activity and identifies the ones it is sure about.
func (s *Server) IdentifyNetflix(ctx context.Context, c *netflix.Client) (int, error) {
	pending, err := s.Store.Unidentified(ctx, netflix.Apps, time.Now().AddDate(0, 0, -7))
	if err != nil {
		return 0, err
	}
	open, err := s.Store.OpenIn(ctx, netflix.Apps, time.Now())
	if err != nil || (len(pending) == 0 && open == nil) {
		return 0, err
	}
	viewed, err := c.Activity(ctx, 0, 50)
	if err != nil {
		return 0, err
	}
	n := s.splitNetflix(ctx, c, viewed)
	pairs := netflix.Match(pending, viewed)
	pairs = append(pairs, netflix.Continuations(pending, viewed, pairs, time.Now())...)
	for _, p := range pairs {
		it, err := netflix.Resolve(ctx, c, s.Ident, p.Viewed)
		if err != nil {
			s.Log.Warn("Netflix", "title", p.Viewed.Title, "err", err)
			continue
		}
		if it == nil {
			continue
		}
		if err := s.Store.Identify(ctx, p.Session.ID, []model.Item{*it}, s.Config.Sinks, time.Now()); err != nil {
			return n, err
		}
		s.Log.Info("identified", "session", p.Session.ID, "title", it.Title, "season", it.Season, "episode", it.Episode, "by", "netflix")
		n++
	}
	return n, nil
}

// deliverLoop checks and sends to the destinations every half minute, so a
// finished viewing reaches Trakt and Simkl right after it ends.
func (s *Server) deliverLoop(ctx context.Context, d *deliver.Deliverer) {
	if d.ReadOnly {
		s.Log.Info("destinations are read-only: checking, never sending")
	}
	for {
		rep, err := d.Run(ctx)
		if err != nil && ctx.Err() == nil {
			s.Log.Warn("deliveries", "err", err)
		}
		for name, counts := range rep {
			s.Log.Info("deliveries", "to", name, "result", counts)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// splitNetflix: Netflix moved on to another title inside the session that
// is playing (an autoplay the box did not show): its newest entry started
// after the session did. The session is split there onto that title, as
// the autoplay split would.
func (s *Server) splitNetflix(ctx context.Context, c *netflix.Client, viewed []netflix.Viewed) int {
	if len(viewed) == 0 {
		return 0
	}
	newest := viewed[0]
	for _, v := range viewed {
		if v.DateMS > newest.DateMS {
			newest = v
		}
	}
	sess, err := s.Store.OpenIn(ctx, netflix.Apps, newest.Started().Add(-netflix.Window))
	if err != nil || sess == nil {
		return 0
	}
	key := fmt.Sprintf("%d/%d", sess.ID, newest.DateMS)
	s.mu.Lock()
	if s.splitDone == nil {
		s.splitDone = map[string]bool{}
	}
	done := s.splitDone[key]
	s.splitDone[key] = true
	s.mu.Unlock()
	if done {
		return 0
	}
	items, err := s.Store.Items(ctx, sess.ID)
	if err != nil || len(items) == 0 || items[len(items)-1].Kind != model.KindEpisode {
		return 0 // a movie or an unidentified session: the usual rules handle it
	}
	it, err := netflix.Resolve(ctx, c, s.Ident, newest)
	if err != nil || it == nil || items[len(items)-1].SameTitle(it.Identity) {
		return 0
	}
	// split where the new title started, not when Netflix got around to listing it
	ns, err := s.Store.SplitNextAt(ctx, sess.ID, *it, s.Config.Sinks, newest.Started(), time.Now())
	if err != nil {
		s.Log.Warn("Netflix split", "session", sess.ID, "err", err)
		return 0
	}
	s.Log.Info("Netflix moved on to another title", "session", sess.ID, "new", ns.ID, "title", it.Title, "season", it.Season, "episode", it.Episode)
	return 1
}

// serveHTTP serves the dashboard and its API on addr until ctx ends.
func (s *Server) serveHTTP(ctx context.Context, wg *sync.WaitGroup, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	a := &api.API{Store: s.Store, Ident: s.Ident, Version: s.Version, Sinks: s.Config.Sinks, ReadOnly: s.Config.ReadOnly,
		Presence: s.Config.Presence, Log: s.Log.With("part", "api")}
	a.Devices = api.DeviceHooks{States: s.DeviceStates, Scan: s.Scan, Add: s.AddDevice,
		ConnectNetflix: s.ConnectNetflix, NetflixStatus: s.NetflixStatus}
	if nf := s.nf; nf != nil {
		a.NetflixProfile = func(ctx context.Context) (string, error) {
			p, err := nf.Profile(ctx)
			if err != nil {
				return "", err
			}
			return p.Name, nil
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", a.Handler())
	mux.Handle("/", web.Handler())
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.Log.Info("dashboard", "url", "http://"+ln.Addr().String()+"/")
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Log.Error("dashboard stopped", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(sc)
	}()
	return nil
}
