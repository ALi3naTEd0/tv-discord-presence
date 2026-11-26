package presence

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

var now = time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)

func dur(d time.Duration) *time.Duration { return &d }

func TestBuildIdentifiedEpisode(t *testing.T) {
	n := &Now{App: model.App{ID: "com.netflix.ninja", Name: "Netflix"}, State: model.StatePlaying, Position: dur(10 * time.Minute),
		Item: &model.Item{Identity: model.Identity{Kind: model.KindEpisode, Title: "The Trials of Karen Read", Season: 1, Episode: 3,
			EpisodeTitle: "The Evidence"}, Poster: "https://image.tmdb.org/p.jpg", RuntimeMinutes: 48}}
	a := Build(n, now)
	if a.Type != 3 || a.Name != "Netflix" || a.Details != "The Trials of Karen Read" || a.State != "S1 · E3 — The Evidence" {
		t.Errorf("%+v", a)
	}
	if a.Assets.LargeImage != "https://image.tmdb.org/p.jpg" || !strings.Contains(a.Assets.SmallImage, "netflix.com") || a.Assets.SmallText != "Netflix on TV" {
		t.Errorf("assets %+v", a.Assets)
	}
	if a.Timestamps.Start != now.Add(-10*time.Minute).Unix() || a.Timestamps.End != now.Add(38*time.Minute).Unix() {
		t.Errorf("timestamps %+v (want a progress bar)", a.Timestamps)
	}
}

func TestBuildMoviePausedAndEstimated(t *testing.T) {
	n := &Now{App: model.App{ID: "plex", Name: "Plex"}, State: model.StatePaused, Position: dur(time.Hour), Device: "Mi Box",
		Item: &model.Item{Identity: model.Identity{Kind: model.KindMovie, Title: "Dune", Year: 2021}, Poster: "p", RuntimeMinutes: 155}}
	a := Build(n, now)
	if a.Details != "Dune (2021)" || a.State != "Paused · Movie" || a.Timestamps != nil || a.Assets.SmallText != "Plex · Mi Box" {
		t.Errorf("%+v %+v", a, a.Assets)
	}
	n.State = model.StatePlaying
	n.Item.RuntimeEstimated = true
	if a := Build(n, now); a.Timestamps.End != 0 {
		t.Error("progress bar from an estimated runtime")
	}
}

func TestBuildHintAndBare(t *testing.T) {
	yt := &Now{App: model.App{ID: "com.google.android.youtube.tv", Name: "YouTube"}, State: model.StatePlaying, Played: 5 * time.Minute,
		Hint: model.Hint{Title: "RESUMEN: PSG 2-2 AS ROMA", Subtitle: "TNT Sports México"}}
	a := Build(yt, now)
	if a.Details != "RESUMEN: PSG 2-2 AS ROMA" || a.State != "TNT Sports México" || a.Timestamps.Start != now.Add(-5*time.Minute).Unix() {
		t.Errorf("%+v", a)
	}
	bare := &Now{App: model.App{ID: "netflix", Name: "Netflix"}, State: model.StatePlaying}
	if a := Build(bare, now); a.Details != "Watching Netflix" || a.State != "Playing on TV" {
		t.Errorf("%+v", a)
	}
	if Build(&Now{App: model.App{ID: "netflix"}, State: model.StateIdle}, now) != nil || Build(nil, now) != nil {
		t.Error("status for nothing playing")
	}
	long := strings.Repeat("x", 200)
	if a := Build(&Now{App: model.App{ID: "a", Name: "A"}, State: model.StatePlaying, Hint: model.Hint{Title: long}}, now); len(a.Details) != 128 {
		t.Errorf("details %d chars", len(a.Details))
	}
}

// fakeDiscord answers the handshake and SET_ACTIVITY on a unix socket.
func fakeDiscord(t *testing.T, got chan<- map[string]any) string {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("TMPDIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "discord-ipc-0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		frame := func(op uint32, v any) {
			b, _ := json.Marshal(v)
			h := make([]byte, 8)
			binary.LittleEndian.PutUint32(h, op)
			binary.LittleEndian.PutUint32(h[4:], uint32(len(b)))
			c.Write(append(h, b...))
		}
		for {
			h := make([]byte, 8)
			if _, err := io.ReadFull(c, h); err != nil {
				return
			}
			b := make([]byte, binary.LittleEndian.Uint32(h[4:]))
			io.ReadFull(c, b)
			var m map[string]any
			json.Unmarshal(b, &m)
			if binary.LittleEndian.Uint32(h) == opHandshake {
				if m["client_id"] != "123" {
					frame(opClose, map[string]any{"code": 4000, "message": "Invalid Client ID"})
					return
				}
				frame(opFrame, map[string]any{"cmd": "DISPATCH", "evt": "READY", "data": map[string]any{}})
				continue
			}
			got <- m
			frame(opFrame, map[string]any{"cmd": "SET_ACTIVITY", "nonce": m["nonce"], "data": map[string]any{}})
		}
	}()
	return dir
}

func TestRunnerSendsOnlyChanges(t *testing.T) {
	got := make(chan map[string]any, 8)
	fakeDiscord(t, got)
	r := &Runner{ClientID: "123", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n := &Now{App: model.App{ID: "netflix", Name: "Netflix"}, State: model.StatePlaying, Played: time.Minute}
	r.Show(n, now)
	r.Show(n, now.Add(5*time.Second)) // same status: not sent again
	n.State = model.StatePaused
	r.Show(n, now.Add(10*time.Second))
	r.Show(nil, now.Add(15*time.Second))
	var msgs []map[string]any
	for len(msgs) < 3 {
		select {
		case m := <-got:
			msgs = append(msgs, m)
		case <-time.After(3 * time.Second):
			t.Fatalf("got %d messages, want 3", len(msgs))
		}
	}
	act := msgs[0]["args"].(map[string]any)["activity"].(map[string]any)
	if msgs[0]["cmd"] != "SET_ACTIVITY" || act["details"] != "Watching Netflix" || act["type"].(float64) != 3 {
		t.Errorf("first %v", msgs[0])
	}
	if _, ok := msgs[2]["args"].(map[string]any)["activity"]; ok {
		t.Error("clearing sent an activity")
	}
	select {
	case m := <-got:
		t.Errorf("extra message %v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDialWrongApp(t *testing.T) {
	fakeDiscord(t, make(chan map[string]any, 1))
	if _, err := Dial("999"); err == nil || !strings.Contains(err.Error(), "Invalid Client ID") {
		t.Errorf("err %v", err)
	}
}

func TestNoDiscord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("TMPDIR", dir)
	if _, err := Dial("123"); err == nil {
		t.Error("connected with no Discord")
	}
	_ = fs.ModePerm
	_ = os.Getpid
}
