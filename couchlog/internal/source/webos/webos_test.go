package webos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// fakeTV speaks enough SSAP: registration (with or without a key, and
// rejecting the signed manifest when told to) and the four subscriptions.
type fakeTV struct {
	t            *testing.T
	key          string // the key it hands out
	rejectSigned bool
	updates      chan map[string]any // pushed to the open connection: {"id": ..., "payload": ...}

	mu         sync.Mutex
	registered []map[string]any // register payloads received
}

func (f *fakeTV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	subscribed := 0
	for {
		var m map[string]any
		if err := wsjson.Read(ctx, c, &m); err != nil {
			return
		}
		switch m["type"] {
		case "register":
			p := m["payload"].(map[string]any)
			f.mu.Lock()
			f.registered = append(f.registered, p)
			f.mu.Unlock()
			_, signed := p["manifest"].(map[string]any)["signatures"]
			if signed && f.rejectSigned {
				wsjson.Write(ctx, c, map[string]any{"type": "error", "id": "register_0", "error": "500 Application error"})
				continue
			}
			if p["client-key"] == nil {
				wsjson.Write(ctx, c, map[string]any{"type": "response", "id": "register_0", "payload": map[string]any{"pairingType": "PROMPT"}})
			}
			wsjson.Write(ctx, c, map[string]any{"type": "registered", "id": "register_0", "payload": map[string]any{"client-key": f.key}})
		case "request":
			wsjson.Write(ctx, c, map[string]any{"type": "response", "id": m["id"], "payload": map[string]any{"returnValue": true}})
		case "subscribe":
			wsjson.Write(ctx, c, map[string]any{"type": "response", "id": m["id"], "payload": map[string]any{"subscribed": true}})
			// Like a real TV: updates only flow once subscribed.
			if subscribed++; subscribed == len(subscriptions) {
				go func() {
					for u := range f.updates {
						if wsjson.Write(ctx, c, map[string]any{"type": "response", "id": u["id"], "payload": u["payload"]}) != nil {
							return
						}
					}
				}()
			}
		}
	}
}

func startTV(t *testing.T, f *fakeTV) string {
	t.Helper()
	f.t = t
	if f.updates == nil {
		f.updates = make(chan map[string]any, 16)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func payload(js string) map[string]any {
	var m map[string]any
	json.Unmarshal([]byte(js), &m)
	return m
}

// collect runs the source until it has reported n observations.
func collect(t *testing.T, s *Source, n int, before func()) []model.Observation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var got []model.Observation
	done := make(chan struct{})
	go func() {
		s.Run(ctx, func(o model.Observation) {
			mu.Lock()
			defer mu.Unlock()
			if len(got) < n {
				got = append(got, o)
				if len(got) == n {
					close(done)
				}
			}
		})
	}()
	if before != nil {
		before()
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("only %d of %d observations", len(got), n)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestPairsAndSavesKey(t *testing.T) {
	tv := &fakeTV{key: "abc123", rejectSigned: true}
	url := startTV(t, tv)
	keyPath := filepath.Join(t.TempDir(), "key")
	s := &Source{DeviceID: 1, KeyPath: keyPath, URLs: []string{url}, Poll: 20 * time.Millisecond}
	collect(t, s, 1, nil)
	if b, _ := os.ReadFile(keyPath); string(b) != "abc123" {
		t.Errorf("key file %q", b)
	}
	tv.mu.Lock()
	defer tv.mu.Unlock()
	if len(tv.registered) != 2 {
		t.Fatalf("%d registrations, want signed then unsigned", len(tv.registered))
	}
	if _, ok := tv.registered[1]["manifest"].(map[string]any)["signatures"]; ok {
		t.Error("second registration still signed")
	}
}

func TestReusesKey(t *testing.T) {
	tv := &fakeTV{key: "abc123"}
	url := startTV(t, tv)
	keyPath := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyPath, []byte("abc123\n"), 0o600)
	s := &Source{DeviceID: 1, KeyPath: keyPath, URLs: []string{url}, Poll: 20 * time.Millisecond}
	collect(t, s, 1, nil)
	tv.mu.Lock()
	defer tv.mu.Unlock()
	if tv.registered[0]["client-key"] != "abc123" {
		t.Errorf("registered with %v, want the saved key", tv.registered[0]["client-key"])
	}
}

func TestReportsPlayback(t *testing.T) {
	tv := &fakeTV{key: "k"}
	url := startTV(t, tv)
	s := &Source{DeviceID: 7, KeyPath: filepath.Join(t.TempDir(), "key"), URLs: []string{url}, Poll: 30 * time.Millisecond}
	obs := collect(t, s, 8, func() {
		tv.updates <- map[string]any{"id": "power", "payload": payload(`{"state":"Active"}`)}
		tv.updates <- map[string]any{"id": "apps", "payload": payload(`{"launchPoints":[{"id":"com.some.app","title":"Some App"}]}`)}
		tv.updates <- map[string]any{"id": "app", "payload": payload(`{"appId":"netflix"}`)}
		tv.updates <- map[string]any{"id": "media", "payload": payload(`{"foregroundAppInfo":[{"appId":"netflix","playState":"playing","mediaId":"_m1"}]}`)}
	})
	for _, o := range obs {
		t.Logf("%s %s %s", o.State, o.App.ID, o.MediaID)
	}
	last := obs[len(obs)-1]
	if last.DeviceID != 7 || last.State != model.StatePlaying || last.App.ID != "netflix" || last.App.Name != "Netflix" || last.MediaID != "_m1" {
		t.Errorf("last observation %+v", last)
	}
}

func TestObserve(t *testing.T) {
	s := &Source{DeviceID: 1, IgnoreApps: []string{"cdp-30"}}
	now := time.Now()
	for name, c := range map[string]struct {
		st    *state
		want  model.PlayState
		appID string
		media string
	}{
		"off":                {&state{power: "Suspend", gotPower: true, appID: "netflix"}, model.StateOff, "", ""},
		"active standby":     {&state{power: "Active Standby", gotPower: true}, model.StateOff, "", ""},
		"old firmware, on":   {&state{appID: "netflix"}, model.StateIdle, "netflix", ""},
		"old firmware, off":  {&state{}, model.StateOff, "", ""},
		"home screen":        {&state{power: "Active", gotPower: true, appID: "com.webos.app.home"}, model.StateIdle, "", ""},
		"hdmi console":       {&state{power: "Active", gotPower: true, appID: "com.webos.app.hdmi2"}, model.StateIdle, "", ""},
		"ignored app":        {&state{power: "Active", gotPower: true, appID: "cdp-30"}, model.StateIdle, "", ""},
		"app menu":           {&state{power: "Active", gotPower: true, appID: "netflix"}, model.StateIdle, "netflix", ""},
		"paused":             {&state{power: "Active", gotPower: true, appID: "netflix", media: []mediaInfo{{"netflix", "paused", "_m"}}}, model.StatePaused, "netflix", "_m"},
		"screen off playing": {&state{power: "Screen Off", gotPower: true, appID: "netflix", media: []mediaInfo{{"netflix", "playing", "_m"}}}, model.StatePlaying, "netflix", "_m"},
		"other app's media":  {&state{power: "Active", gotPower: true, appID: "netflix", media: []mediaInfo{{"hulu", "playing", "_x"}}}, model.StateIdle, "netflix", ""},
	} {
		c.st.apps = map[string]string{}
		o := s.observe(c.st, now)
		if o.State != c.want || o.App.ID != c.appID || o.MediaID != c.media {
			t.Errorf("%s: %+v", name, o)
		}
	}
	unknown := &state{appID: "com.some.app", apps: map[string]string{"com.some.app": "Some App"}}
	if o := s.observe(unknown, now); o.App.Name != "Some App" {
		t.Errorf("unknown app named %q, want the TV's title", o.App.Name)
	}
}

func TestOffWhileUnreachable(t *testing.T) {
	s := &Source{DeviceID: 3, Host: "127.0.0.1", URLs: []string{"ws://127.0.0.1:1"}, Poll: 10 * time.Millisecond,
		KeyPath: filepath.Join(t.TempDir(), "key")}
	for _, o := range collect(t, s, 2, nil) {
		if o.State != model.StateOff || o.DeviceID != 3 {
			t.Errorf("observation %+v, want off", o)
		}
	}
}
