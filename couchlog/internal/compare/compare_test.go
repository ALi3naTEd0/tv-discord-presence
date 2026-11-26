package compare

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/source"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

var t0 = time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)

func legacy(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "history.db")
	db, _ := sql.Open("sqlite", p)
	defer db.Close()
	at := func(min int) float64 { return float64(t0.Add(time.Duration(min) * time.Minute).Unix()) }
	for _, q := range []string{
		`CREATE TABLE sessions(id INTEGER PRIMARY KEY, app_id TEXT, started_at REAL, ended_at REAL, played_seconds REAL,
			state TEXT, ignored INTEGER DEFAULT 0, source TEXT, source_key TEXT, auto_hidden INTEGER DEFAULT 0)`,
		`CREATE TABLE items(id INTEGER PRIMARY KEY, session_id INTEGER, title TEXT, season INTEGER, episode INTEGER)`,
	} {
		db.Exec(q)
	}
	ins := func(id int, app string, start, end int, played float64, src, key string) {
		if _, err := db.Exec("INSERT INTO sessions(id, app_id, started_at, ended_at, played_seconds, state, source, source_key) VALUES (?,?,?,?,?,'closed',?,?)",
			id, app, at(start), at(end), played, src, key); err != nil {
			t.Fatal(err)
		}
	}
	ins(1, "com.netflix.ninja", 0, 50, 2900, "androidtv", "")
	db.Exec("INSERT INTO items VALUES (1, 1, 'Ozark', 1, 2)")
	ins(2, "netflix", 60, 90, 1700, "tv", "") // Go counted much more
	ins(3, "plex", 100, 160, 3600, "plex", "/status/sessions/history/9")
	db.Exec("INSERT INTO items VALUES (2, 3, 'Dune', NULL, NULL)")
	ins(4, "hulu", 200, 230, 1500, "tv", "")                             // only Python
	ins(5, "netflix", -600, -500, 100, "tv", "")                         // before since
	ins(6, "netflix", -30, -10, 1100, "tv", "")                          // before Couchlog listened: not compared
	ins(7, "plex", -40, -10, 1700, "plex", "/status/sessions/history/8") // Plex counts from since: only Python
	return p
}

func couchlog(t *testing.T) string {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "couchlog.db")
	st, err := store.Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	atv, _ := st.EnsureDevice(ctx, store.LocalUserID, "androidtv", "Android TV", "")
	lg, _ := st.EnsureDevice(ctx, store.LocalUserID, "webos", "LG TV", "")
	px, _ := st.EnsureDevice(ctx, store.LocalUserID, "plex", "Plex", "")
	play := func(dev int64, app string, startMin int, playedSec int) int64 {
		s, err := st.StartSession(ctx, dev, model.App{ID: app}, "", t0.Add(time.Duration(startMin)*time.Minute+20*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		st.Touch(ctx, s.ID, s.StartedAt.Add(time.Duration(playedSec)*time.Second), time.Duration(playedSec)*time.Second, true)
		st.CloseSession(ctx, s.ID, 0, 0)
		return s.ID
	}
	s1 := play(atv, "com.netflix.ninja", 0, 2890)
	st.Identify(ctx, s1, []model.Item{{Identity: model.Identity{Kind: model.KindEpisode, Title: "Ozark", Season: 1, Episode: 2, TMDB: 1}}}, nil, t0)
	play(lg, "netflix", 61, 1790)
	st.AddPlay(ctx, source.Play{SourceKey: "/status/sessions/history/9", DeviceID: px, StartedAt: t0.Add(100 * time.Minute),
		EndedAt: t0.Add(160 * time.Minute), Identity: model.Identity{Kind: model.KindMovie, Title: "Dune", TMDB: 2}}, "plex", "Plex", nil, t0)
	play(lg, "youtube.leanback.v4", 300, 1300) // only Go
	// Couchlog started listening five minutes before t0.
	st.DB().Exec("UPDATE devices SET created_at = ?", t0.Add(-5*time.Minute).UnixMilli())
	return p
}

func TestCompare(t *testing.T) {
	r, err := Run(context.Background(), couchlog(t), legacy(t), t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	same, differ, onlyPy, onlyGo := r.Summary()
	if same != 2 || differ != 1 || onlyPy != 2 || onlyGo != 1 {
		var b bytes.Buffer
		r.Write(&b, false)
		t.Fatalf("same %d differ %d onlyPy %d onlyGo %d\n%s", same, differ, onlyPy, onlyGo, b.String())
	}
	var b bytes.Buffer
	r.Write(&b, true)
	out := b.String()
	if !strings.Contains(out, "played +90s") || !strings.Contains(out, "only Python") || strings.Contains(out, "Ozark") {
		t.Errorf("diff-only report:\n%s", out)
	}
}
