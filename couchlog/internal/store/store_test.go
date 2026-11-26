package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "couchlog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "couchlog.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(context.Background()); v != len(migrations) {
		t.Errorf("schema version %d, want %d", v, len(migrations))
	}
	s.Close()

	// Opening again does not re-run migrations or duplicate the local user.
	s, err = Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	var users int
	_ = s.DB().QueryRow("SELECT COUNT(*) FROM users").Scan(&users)
	if users != 1 {
		t.Errorf("%d users after reopening, want 1", users)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := openTemp(t)
	_, err := s.DB().Exec("INSERT INTO items(session_id, kind, title) VALUES (999, 'movie', 'x')")
	if err == nil {
		t.Fatal("item for a missing session was accepted: foreign keys are off")
	}
}

func TestRefusesNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "couchlog.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(context.Background(), path); !errors.Is(err, ErrTooNew) {
		t.Fatalf("err = %v, want ErrTooNew", err)
	}
}

// legacySchema is tvpresence's history.db as of its latest migrations.
const legacySchema = `
CREATE TABLE sessions(
  id INTEGER PRIMARY KEY AUTOINCREMENT, app_id TEXT NOT NULL, app_name TEXT, app_logo TEXT,
  started_at REAL NOT NULL, last_seen_at REAL NOT NULL, last_play_at REAL NOT NULL, ended_at REAL,
  played_seconds REAL NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'open', ignored INTEGER NOT NULL DEFAULT 0,
  media_id TEXT, source TEXT NOT NULL DEFAULT 'tv', source_key TEXT, hint_title TEXT, hint_season INTEGER,
  hint_episode INTEGER, auto_hidden INTEGER NOT NULL DEFAULT 0, hint_extra TEXT, position REAL, live_position REAL,
  video_codec TEXT, video_width INTEGER, video_height INTEGER, video_hdr TEXT, video_hdr_live TEXT,
  video_hdr_secs TEXT, video_hdr_since REAL, auto_next_from INTEGER);
CREATE TABLE items(
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id INTEGER NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL,
  year INTEGER, season INTEGER, episode INTEGER, episode_title TEXT, tmdb INTEGER, tvdb INTEGER, imdb TEXT,
  poster TEXT, trakt_status TEXT NOT NULL DEFAULT 'pending', simkl_status TEXT NOT NULL DEFAULT 'pending',
  trakt_error TEXT, simkl_error TEXT, pushed_at REAL, runtime INTEGER, runtime_est INTEGER NOT NULL DEFAULT 0);
`

// legacyRows covers every mapping: three sources, ignored and short
// sessions, an open one, an autoplay split, HDR time, Plex history keys,
// and every delivery status.
const legacyRows = `
INSERT INTO sessions(id, app_id, app_name, started_at, last_seen_at, last_play_at, ended_at, played_seconds, state,
  ignored, media_id, source, source_key, hint_title, auto_hidden, position, video_codec, video_width, video_height,
  video_hdr, video_hdr_secs, auto_next_from) VALUES
 (1, 'netflix', 'Netflix', 1700000000.5, 1700003600, 1700003600, 1700003600, 3500.25, 'closed', 0, 'm1', 'tv', NULL, NULL, 0, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
 (2, 'com.hbo', 'HBO Max', 1700010000, 1700013000, 1700013000, 1700013000, 2900, 'closed', 0, 's1', 'androidtv', NULL, 'The Hotline', 0, 2950.5, 'HEVC', 3840, 2160, 'HDR10', '{"HDR10": 2800.4, "SDR": 99.6}', NULL),
 (3, 'com.hbo', 'HBO Max', 1700013000, 1700016000, 1700016000, 1700016000, 2950, 'closed', 0, 's2', 'androidtv', NULL, NULL, 0, NULL, NULL, NULL, NULL, NULL, NULL, 2),
 (4, 'youtube', 'YouTube', 1700020000, 1700020600, 1700020600, 1700020600, 600, 'closed', 0, NULL, 'androidtv', NULL, NULL, 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
 (5, 'netflix', 'Netflix', 1700030000, 1700030300, 1700030300, 1700030300, 300, 'closed', 1, NULL, 'tv', NULL, NULL, 0, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
 (6, 'plex', 'Plex', 1700040000, 1700045000, 1700045000, 1700045000, 5000, 'closed', 0, NULL, 'plex', 'plex:12345', NULL, 0, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
 (7, 'netflix', 'Netflix', 1700050000, 1700050100, 1700050100, NULL, 100, 'open', 0, NULL, 'tv', NULL, NULL, 0, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
INSERT INTO items(session_id, kind, title, year, season, episode, episode_title, tmdb, imdb, trakt_status, simkl_status, trakt_error, pushed_at, runtime, runtime_est) VALUES
 (1, 'movie', 'Heat', 1995, NULL, NULL, NULL, 949, 'tt0113277', 'added', 'added', NULL, 1700004000, 170, 0),
 (2, 'episode', 'The Hotline', 2026, 1, 1, 'Pilot', 1001, NULL, 'ready', 'not_found', NULL, NULL, 50, 1),
 (3, 'episode', 'The Hotline', 2026, 1, 2, 'Second', 1001, NULL, 'error', 'weird', 'HTTP 502', NULL, 50, 1),
 (6, 'movie', 'Alien', 1979, NULL, NULL, NULL, 348, 'tt0078748', 'pending', 'pending', NULL, NULL, 117, 0);
`

func makeLegacy(t *testing.T, schema string, rows string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if rows != "" {
		if _, err := db.Exec(rows); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestImportTvpresence(t *testing.T) {
	legacy := makeLegacy(t, legacySchema, legacyRows)
	s := openTemp(t)
	ctx := context.Background()

	st, err := s.ImportTvpresence(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	want := ImportStats{Devices: 3, Sessions: 7, Items: 4, Deliveries: 8}
	if st != want {
		t.Errorf("stats %+v, want %+v", st, want)
	}

	q := func(query string, args ...any) string {
		t.Helper()
		var v sql.NullString
		if err := s.DB().QueryRow(query, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return v.String
	}
	checks := []struct{ name, query, want string }{
		{"devices by kind", "SELECT group_concat(kind || ':' || name, ',') FROM (SELECT * FROM devices ORDER BY kind)", "androidtv:Android TV,plex:Plex,webos:LG TV"},
		{"seconds become milliseconds", "SELECT started_at || '/' || played_ms FROM sessions WHERE source_key='tvpresence:1'", "1700000000500/3500250"},
		{"Plex key kept", "SELECT app_name FROM sessions WHERE source_key='plex:12345'", "Plex"},
		{"ignored", "SELECT hidden FROM sessions WHERE source_key='tvpresence:5'", "ignored"},
		{"short", "SELECT hidden FROM sessions WHERE source_key='tvpresence:4'", "short"},
		{"open session stays open", "SELECT state || ':' || COALESCE(ended_at, 'null') FROM sessions WHERE source_key='tvpresence:7'", "open:null"},
		{"HDR time in ms", "SELECT video_hdr_ms FROM sessions WHERE source_key='tvpresence:2'", `{"HDR10":2800400,"SDR":99600}`},
		{"position in ms", "SELECT position_ms FROM sessions WHERE source_key='tvpresence:2'", "2950500"},
		{"autoplay link points at the new id", `SELECT (SELECT source_key FROM sessions p WHERE p.id = s.auto_next_from) FROM sessions s WHERE source_key='tvpresence:3'`, "tvpresence:2"},
		{"empty strings, not NULLs", "SELECT hint_title || '|' || media_id FROM sessions WHERE source_key='plex:12345'", "|"},
		{"item fields", "SELECT kind || ' ' || title || ' S' || season || 'E' || episode || ' ' || episode_title || ' ' || runtime_min || ' ' || runtime_estimated FROM items WHERE episode = 1", "episode The Hotline S1E1 Pilot 50 1"},
		{"deliveries", "SELECT group_concat(sink || '=' || status, ',') FROM (SELECT * FROM deliveries d JOIN items i ON i.id = d.item_id WHERE i.title = 'Heat' ORDER BY sink)", "simkl=added,trakt=added"},
		{"pushed_at kept", "SELECT DISTINCT d.updated_at FROM deliveries d JOIN items i ON i.id = d.item_id WHERE i.title = 'Heat'", "1700004000000"},
		{"unknown status becomes error", "SELECT d.status FROM deliveries d JOIN items i ON i.id = d.item_id WHERE i.episode = 2 AND d.sink = 'simkl'", "error"},
		{"error text kept", "SELECT d.error FROM deliveries d JOIN items i ON i.id = d.item_id WHERE i.episode = 2 AND d.sink = 'trakt'", "HTTP 502"},
		{"all belong to the local user", "SELECT COUNT(*) FROM sessions WHERE user_id <> 1", "0"},
	}
	for _, c := range checks {
		if got := q(c.query); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	// Again: nothing new, nothing duplicated.
	st2, err := s.ImportTvpresence(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if st2 != (ImportStats{Skipped: 7}) {
		t.Errorf("second import %+v, want only 7 skipped", st2)
	}
	if n := q("SELECT COUNT(*) FROM items"); n != "4" {
		t.Errorf("%s items after importing twice, want 4", n)
	}
}

// The first tvpresence versions had none of the later columns.
func TestImportOldestLegacySchema(t *testing.T) {
	oldest := `
CREATE TABLE sessions(id INTEGER PRIMARY KEY AUTOINCREMENT, app_id TEXT NOT NULL, app_name TEXT, app_logo TEXT,
  started_at REAL NOT NULL, last_seen_at REAL NOT NULL, last_play_at REAL NOT NULL, ended_at REAL,
  played_seconds REAL NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'open', ignored INTEGER NOT NULL DEFAULT 0);
CREATE TABLE items(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id INTEGER NOT NULL, kind TEXT NOT NULL,
  title TEXT NOT NULL, year INTEGER, season INTEGER, episode INTEGER, episode_title TEXT, tmdb INTEGER,
  tvdb INTEGER, imdb TEXT, poster TEXT, trakt_status TEXT NOT NULL DEFAULT 'pending',
  simkl_status TEXT NOT NULL DEFAULT 'pending', trakt_error TEXT, simkl_error TEXT, pushed_at REAL);`
	rows := `INSERT INTO sessions VALUES (1, 'netflix', 'Netflix', NULL, 1, 2, 2, 2, 1, 'closed', 0);
INSERT INTO items(session_id, kind, title) VALUES (1, 'movie', 'Old');`
	s := openTemp(t)
	st, err := s.ImportTvpresence(context.Background(), makeLegacy(t, oldest, rows))
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 1 || st.Items != 1 || st.Devices != 1 {
		t.Errorf("stats %+v", st)
	}
}

func TestImportRejectsOtherFiles(t *testing.T) {
	s := openTemp(t)
	notTvpresence := makeLegacy(t, "CREATE TABLE notes(x TEXT);", "")
	if _, err := s.ImportTvpresence(context.Background(), notTvpresence); err == nil {
		t.Error("imported a database without tvpresence tables")
	}
}

// TestImportRealHistory imports a copy of a real tvpresence database when
// COUCHLOG_REAL_DB points at one. It never touches the original.
func TestImportRealHistory(t *testing.T) {
	src := os.Getenv("COUCHLOG_REAL_DB")
	if src == "" {
		t.Skip("set COUCHLOG_REAL_DB=~/.local/share/tvpresence/history.db to run")
	}
	dir := t.TempDir()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(src+suffix, filepath.Join(dir, "history.db"+suffix)); err != nil && suffix == "" {
			t.Fatal(err)
		}
	}
	legacy := filepath.Join(dir, "history.db")
	old, err := sql.Open("sqlite", "file:"+legacy+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var wantSessions, wantItems int
	_ = old.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&wantSessions)
	_ = old.QueryRow("SELECT COUNT(*) FROM items").Scan(&wantItems)
	old.Close()

	s := openTemp(t)
	st, err := s.ImportTvpresence(context.Background(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real history: %+v", st)
	if st.Sessions != wantSessions || st.Items != wantItems || st.Deliveries != 2*wantItems {
		t.Errorf("imported %+v from %d sessions and %d items", st, wantSessions, wantItems)
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func TestOpenPathWithURICharacters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "50% off?#x", "couchlog.db")
	s, err := Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("database not at the given path: %v", err)
	}
}
