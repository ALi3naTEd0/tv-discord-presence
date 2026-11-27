package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/api"
	"github.com/ALi3naTEd0/couchlog/internal/config"
	"github.com/ALi3naTEd0/couchlog/internal/discover"
	"github.com/ALi3naTEd0/couchlog/internal/engine"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/netflix"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// startDevice records a device and starts listening to it.
func (s *Server) startDevice(ctx context.Context, wg *sync.WaitGroup, d config.Device) error {
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
	s.mu.Lock()
	s.trackers = append(s.trackers, deviceTracker{id: id, kind: d.Kind, name: d.Name, keyFile: d.KeyFile, tr: tr})
	s.mu.Unlock()
	src := s.source(d, id)
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.follow(ctx, d.Name, src, tr)
	}()
	return nil
}

func (s *Server) deviceTrackers() []deviceTracker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]deviceTracker(nil), s.trackers...)
}

var reHost = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]{1,255}$`)

// AddDevice starts listening to a new TV or box and saves it in the
// settings. The TV asks to accept the connection the first time (an LG
// shows a prompt; Android TV asks "Allow network debugging?").
func (s *Server) AddDevice(ctx context.Context, kind, host, name string) (int64, error) {
	host, name = strings.TrimSpace(host), strings.TrimSpace(name)
	if kind != "webos" && kind != "androidtv" {
		return 0, errors.New("kind must be webos or androidtv")
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	if !reHost.MatchString(host) || h == "" {
		return 0, errors.New("write the IP address the TV shows in its network settings")
	}
	if name == "" {
		name = map[string]string{"webos": "LG TV", "androidtv": "Android TV"}[kind]
	}
	if len([]rune(name)) > 60 {
		return 0, errors.New("names are up to 60 characters")
	}
	s.mu.Lock()
	runCtx, wg := s.runCtx, s.wg
	if runCtx == nil {
		s.mu.Unlock()
		return 0, errors.New("the server is not running")
	}
	for _, d := range s.Config.Devices {
		if d.Host == host || strings.EqualFold(d.Name, name) {
			s.mu.Unlock()
			return 0, fmt.Errorf("%s (%s) is already added", d.Name, d.Host)
		}
	}
	d := config.Device{Kind: kind, Name: name, Host: host, KeyFile: s.keyFile(kind, h)}
	s.Config.Devices = append(s.Config.Devices, d)
	cfg := *s.Config
	s.mu.Unlock()
	if s.ConfigPath != "" {
		if err := cfg.Save(s.ConfigPath); err != nil {
			return 0, fmt.Errorf("could not save the settings: %w", err)
		}
	}
	if err := s.startDevice(runCtx, wg, d); err != nil {
		return 0, err
	}
	s.Log.Info("device added from the dashboard", "kind", kind, "name", name, "host", host)
	return s.Store.EnsureDevice(ctx, store.LocalUserID, kind, name, host)
}

// keyFile is where a new device's pairing key goes: one adb key for every
// Android TV (a box trusts the key once), one client key per LG.
func (s *Server) keyFile(kind, host string) string {
	dir := s.DataDir
	if dir == "" {
		dir = "."
	}
	if kind == "androidtv" {
		return filepath.Join(dir, "adbkey")
	}
	return filepath.Join(dir, "webos-"+strings.NewReplacer(":", "_", "[", "", "]", "").Replace(host)+".key")
}

func (s *Server) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runCtx != nil
}

// DeviceStates says what each device is doing now, by device id.
func (s *Server) DeviceStates() map[int64]api.DeviceState {
	out := map[int64]api.DeviceState{}
	// a device that recorded sessions before was paired: no answer now means off
	paired, _ := s.Store.DevicesWithSessions(context.Background())
	for _, d := range s.deviceTrackers() {
		o, _ := d.tr.Last()
		st := api.DeviceState{State: "waiting"}
		// so does an LG whose client key is saved (it is given on accepting)
		if fi, err := os.Stat(d.keyFile); paired[d.id] || (d.kind == "webos" && err == nil && fi.Size() > 0) {
			st.State = "off"
		}
		if !o.At.IsZero() {
			st.State, st.Since = string(o.State), o.At
			if o.App.Name != "" && (o.State == model.StatePlaying || o.State == model.StatePaused) {
				st.App = o.App.Name
			}
		}
		out[d.id] = st
	}
	return out
}

// Scan finds TVs and boxes on the network, marking the ones already added.
func (s *Server) Scan(ctx context.Context) ([]api.FoundDevice, error) {
	found, err := discover.Scan(ctx, 0)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	have := map[string]string{}
	for _, d := range s.Config.Devices {
		h := d.Host
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		have[h] = d.Name
	}
	s.mu.Unlock()
	out := make([]api.FoundDevice, 0, len(found))
	for _, f := range found {
		out = append(out, api.FoundDevice{Found: f, AddedAs: have[f.Host]})
	}
	return out, nil
}

// connectState is Connect Netflix as the dashboard follows it.
type connectState struct {
	mu      sync.Mutex
	running bool
	message string
	err     string
}

// ConnectNetflix opens Couchlog's own browser window to sign in to Netflix
// (or none, if it is still signed in) and saves the session; it returns at
// once and the dashboard follows NetflixStatus.
func (s *Server) ConnectNetflix() error {
	c := &s.connect
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return nil
	}
	c.running, c.message, c.err = true, "Opening Netflix…", ""
	dir := s.DataDir
	s.mu.Lock()
	base := s.runCtx
	s.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(base, 15*time.Minute)
		defer cancel()
		p, err := netflix.Connect(ctx, filepath.Join(dir, "netflix-browser"), netflix.SessionPath(dir), func(msg string) {
			c.mu.Lock()
			c.message = msg
			c.mu.Unlock()
		})
		c.mu.Lock()
		defer c.mu.Unlock()
		c.running = false
		if err != nil {
			c.err = err.Error()
			return
		}
		c.message = "Connected as " + p.Name
	}()
	return nil
}

// NetflixStatus says whether Netflix is connected and how a connect goes.
func (s *Server) NetflixStatus(ctx context.Context) api.NetflixStatus {
	c := &s.connect
	c.mu.Lock()
	st := api.NetflixStatus{Enabled: s.Config.Netflix, Connecting: c.running, Message: c.message, Error: c.err}
	c.mu.Unlock()
	if saved, err := netflix.LoadSaved(netflix.SessionPath(s.DataDir)); err == nil {
		st.Saved = true
		st.SavedAt = &saved.SavedAt
	}
	if s.nf != nil && st.Saved && !st.Connecting {
		pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		if p, err := s.nf.Profile(pctx); err == nil {
			st.Profile = p.Name
		} else if !st.Connecting && st.Error == "" {
			st.Error = "The saved Netflix session no longer works: connect again"
		}
	}
	return st
}
