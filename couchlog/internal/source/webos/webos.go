// Package webos listens to an LG webOS TV over its local SSAP WebSocket API:
// power, the app in front and the media state it reports (play state and
// media id). The TV publishes no titles, so observations carry no hint.
package webos

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// KnownApps names the streaming apps; others take the TV's own title.
var KnownApps = map[string]string{
	"netflix":                      "Netflix",
	"com.wbd.stream":               "HBO Max",
	"com.hbo.hbomax":               "HBO Max",
	"com.disney.disneyplus-prod":   "Disney+",
	"hulu":                         "Hulu",
	"amazon":                       "Prime Video",
	"com.apple.appletv":            "Apple TV",
	"youtube.leanback.v4":          "YouTube",
	"vix":                          "ViX",
	"com.clarovideov9":             "Claro Video",
	"com.mercadolibre.mercadoplay": "Mercado Play",
	"com.tubitv.ott.tubi":          "Tubi",
	"tv.twitch.tv.starshot.lg":     "Twitch",
	"com.izzi.tv":                  "izzi tv",
}

// systemPrefixes are the TV's own apps (home, settings, inputs).
var systemPrefixes = []string{"com.webos.", "com.lge.", "lge.", "com.lg.", "webos."}

// ErrPairing means the TV refused pairing (the prompt was declined or timed out).
var ErrPairing = errors.New("webos: pairing refused")

// Source watches one LG TV.
type Source struct {
	Host     string
	DeviceID int64
	// KeyPath holds the client key the TV gives on pairing. The first
	// connection shows a prompt on the TV; accept it with the remote.
	KeyPath    string
	Poll       time.Duration // 0 = 5 s
	IgnoreApps []string      // never reported (e.g. "cdp-30")
	Log        *slog.Logger

	// URLs overrides wss://host:3001 then ws://host:3000 (tests).
	URLs []string
}

// Kind implements source.Source.
func (s *Source) Kind() string { return "webos" }

func (s *Source) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Source) poll() time.Duration {
	if s.Poll <= 0 {
		return 5 * time.Second
	}
	return s.Poll
}

// Run implements source.Source. While the TV cannot be reached it reports
// it as off on every poll, so the engine closes the session after its grace.
func (s *Source) Run(ctx context.Context, emit func(model.Observation)) error {
	for {
		conn, err := s.connect(ctx)
		if err == nil {
			s.log().Info("connected to LG TV", "host", s.Host)
			err = s.listen(ctx, conn, emit)
			conn.CloseNow()
			s.log().Info("LG TV connection ended", "err", err)
		} else {
			s.log().Debug("LG TV unavailable", "err", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrPairing) {
			s.log().Warn("LG TV pairing refused; accept the prompt on the TV", "host", s.Host)
		}
		// Off until the next attempt; retry every poll (the TV answers
		// within seconds of turning on).
		emit(model.Observation{DeviceID: s.DeviceID, At: time.Now(), State: model.StateOff})
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.poll()):
		}
	}
}

func (s *Source) urls() []string {
	if len(s.URLs) > 0 {
		return s.URLs
	}
	return []string{"wss://" + s.Host + ":3001", "ws://" + s.Host + ":3000"}
}

type message struct {
	ID      any             `json:"id"`
	Type    string          `json:"type"`
	URI     string          `json:"uri,omitempty"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// connect opens the socket (wss first, as newer firmware wants) and registers.
func (s *Source) connect(ctx context.Context) (*websocket.Conn, error) {
	var conn *websocket.Conn
	var err error
	for _, u := range s.urls() {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		// webOS serves a self-signed certificate on 3001.
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		conn, _, err = websocket.Dial(dctx, u, &websocket.DialOptions{HTTPClient: hc})
		cancel()
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20) // the apps list is large
	if err := s.register(ctx, conn); err != nil {
		conn.CloseNow()
		return nil, err
	}
	return conn, nil
}

func (s *Source) register(ctx context.Context, conn *websocket.Conn) error {
	key := ""
	if b, err := os.ReadFile(s.KeyPath); err == nil {
		key = strings.TrimSpace(string(b))
	}
	// Newer firmware wants system info before registration; the answer
	// does not matter.
	wsjson.Write(ctx, conn, map[string]any{"id": "get_sys_info", "type": "request",
		"uri": "ssap://system/getSystemInfo", "payload": map[string]any{}})
attempts:
	for _, signed := range []bool{true, false} {
		if err := wsjson.Write(ctx, conn, registration(key, signed)); err != nil {
			return err
		}
		for {
			// Without a key the TV waits for the prompt to be accepted.
			wait := 10 * time.Second
			if key == "" {
				wait = 60 * time.Second
			}
			rctx, cancel := context.WithTimeout(ctx, wait)
			var m message
			err := wsjson.Read(rctx, conn, &m)
			cancel()
			if err != nil {
				return err
			}
			if m.ID != "register_0" {
				continue // the system info answer
			}
			switch m.Type {
			case "registered":
				var p struct {
					Key string `json:"client-key"`
				}
				json.Unmarshal(m.Payload, &p)
				if p.Key == "" {
					p.Key = key
				}
				if p.Key == "" {
					return ErrPairing
				}
				if p.Key != key && s.KeyPath != "" {
					os.MkdirAll(filepath.Dir(s.KeyPath), 0o700)
					if err := os.WriteFile(s.KeyPath, []byte(p.Key), 0o600); err != nil {
						return fmt.Errorf("webos: saving key: %w", err)
					}
					s.log().Info("LG TV paired, key saved", "path", s.KeyPath)
				}
				return nil
			case "error":
				if signed {
					continue attempts // newer firmware: retry unsigned
				}
				return fmt.Errorf("%w: %s", ErrPairing, m.Error)
			}
			// "response" with pairingType PROMPT: wait for the user.
		}
	}
	return ErrPairing
}

type mediaInfo struct {
	AppID     string `json:"appId"`
	PlayState string `json:"playState"`
	MediaID   string `json:"mediaId"`
}

// state is what the TV last said, from its subscriptions.
type state struct {
	mu       sync.Mutex
	power    string // "" when the TV has not said (older firmware)
	appID    string
	apps     map[string]string // id -> title
	media    []mediaInfo
	gotPower bool
}

var subscriptions = map[string]string{
	"power": "ssap://com.webos.service.tvpower/power/getPowerState",
	"app":   "ssap://com.webos.applicationManager/getForegroundAppInfo",
	"apps":  "ssap://com.webos.applicationManager/listLaunchPoints",
	"media": "ssap://com.webos.media/getForegroundAppInfo",
}

// listen subscribes and reports the state on every poll until the
// connection drops.
func (s *Source) listen(ctx context.Context, conn *websocket.Conn, emit func(model.Observation)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for id, uri := range subscriptions {
		if err := wsjson.Write(ctx, conn, map[string]any{"id": id, "type": "subscribe", "uri": uri, "payload": map[string]any{}}); err != nil {
			return err
		}
	}
	st := &state{apps: map[string]string{}}
	errc := make(chan error, 1)
	go func() {
		for {
			var m message
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				errc <- err
				return
			}
			st.apply(m)
		}
	}()
	tick := time.NewTicker(s.poll())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case now := <-tick.C:
			emit(s.observe(st, now))
		}
	}
}

func (st *state) apply(m message) {
	if m.Type != "response" {
		return
	}
	id, _ := m.ID.(string)
	st.mu.Lock()
	defer st.mu.Unlock()
	switch id {
	case "power":
		var p struct{ State string }
		if json.Unmarshal(m.Payload, &p) == nil && p.State != "" {
			st.power, st.gotPower = p.State, true
		}
	case "app":
		var p struct {
			AppID string `json:"appId"`
		}
		if json.Unmarshal(m.Payload, &p) == nil {
			st.appID = p.AppID
		}
	case "apps":
		var p struct {
			LaunchPoints []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"launchPoints"`
		}
		if json.Unmarshal(m.Payload, &p) == nil {
			for _, a := range p.LaunchPoints {
				st.apps[a.ID] = a.Title
			}
		}
	case "media":
		var p struct {
			ForegroundAppInfo []mediaInfo `json:"foregroundAppInfo"`
		}
		if json.Unmarshal(m.Payload, &p) == nil {
			st.media = p.ForegroundAppInfo
		}
	}
}

// observe turns the TV's state into an observation.
func (s *Source) observe(st *state, now time.Time) model.Observation {
	st.mu.Lock()
	defer st.mu.Unlock()
	o := model.Observation{DeviceID: s.DeviceID, At: now, State: model.StateOff}
	on := st.appID != ""
	if st.gotPower {
		switch st.power {
		case "Power Off", "Suspend", "Active Standby":
			on = false
		default:
			on = true
		}
	}
	if !on {
		return o
	}
	o.State = model.StateIdle
	app := st.appID
	if app == "" || s.ignored(app) {
		return o // home screen, settings, an HDMI input (consoles are never recorded)
	}
	for _, p := range systemPrefixes {
		if strings.HasPrefix(app, p) {
			return o
		}
	}
	name := KnownApps[app]
	if name == "" {
		name = st.apps[app]
	}
	if name == "" {
		name = app
	}
	o.App = model.App{ID: app, Name: name}
	for _, m := range st.media {
		if m.AppID != app {
			continue
		}
		o.MediaID = m.MediaID
		switch m.PlayState {
		case "playing":
			o.State = model.StatePlaying
		case "paused":
			o.State = model.StatePaused
		}
	}
	return o
}

func (s *Source) ignored(app string) bool {
	for _, a := range s.IgnoreApps {
		if a == app {
			return true
		}
	}
	return false
}
