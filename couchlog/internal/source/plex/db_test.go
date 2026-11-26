package plex

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// fakePlexDB builds the three tables Couchlog reads from Plex's database.
func fakePlexDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "com.plexapp.plugins.library.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE metadata_item_views (id INTEGER PRIMARY KEY, account_id INTEGER, guid TEXT, metadata_type INTEGER,
			grandparent_guid TEXT, title TEXT, grandparent_title TEXT, parent_index INTEGER, "index" INTEGER,
			viewed_at INTEGER, originally_available_at)`,
		`CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, guid TEXT, title TEXT, year INTEGER)`,
		`CREATE TABLE tags (id INTEGER PRIMARY KEY, tag TEXT, tag_type INTEGER)`,
		`CREATE TABLE taggings (id INTEGER PRIMARY KEY, metadata_item_id INTEGER, tag_id INTEGER)`,
		// A movie deleted from the library: only the view and its plex:// GUID remain.
		`INSERT INTO metadata_item_views VALUES (101, 1, 'plex://movie/aaa', 1, NULL, 'You Can See Everything', NULL, NULL, NULL, 1764000000, '2025-10-01')`,
		// An episode; the show is still in the library.
		`INSERT INTO metadata_item_views VALUES (102, 1, 'plex://episode/eee', 4, 'plex://show/sss', 'Green Queen', 'The Curse', 1, 10, 1764100000, NULL)`,
		// A brand-new film the provider does not know yet; the library stored its IDs.
		`INSERT INTO metadata_item_views VALUES (103, 1, 'plex://movie/new', 1, NULL, 'Listen', NULL, NULL, NULL, 1764200000, 1735689600)`,
		// Another account, and a song.
		`INSERT INTO metadata_item_views VALUES (104, 2, 'plex://movie/aaa', 1, NULL, 'x', NULL, NULL, NULL, 1764300000, NULL)`,
		`INSERT INTO metadata_item_views VALUES (105, 1, 'plex://track/ttt', 10, NULL, 'song', NULL, NULL, NULL, 1764300000, NULL)`,
		// Old, before the cut-off.
		`INSERT INTO metadata_item_views VALUES (100, 1, 'plex://movie/aaa', 1, NULL, 'old', NULL, NULL, NULL, 1700000000, NULL)`,
		`INSERT INTO metadata_items VALUES (7, 'plex://movie/new', 'Listen', 2025)`,
		`INSERT INTO tags VALUES (1, 'tmdb://1703223', 314), (2, 'imdb://tt39110874', 314), (3, 'Drama', 1)`,
		`INSERT INTO taggings VALUES (1, 7, 1), (2, 7, 2), (3, 7, 3)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func fakeProvider(t *testing.T) *httptest.Server {
	pages := map[string]string{
		"/library/metadata/aaa": `{"MediaContainer":{"Metadata":[{"title":"You Can See Everything","year":2025,"duration":10500000,
			"Guid":[{"id":"imdb://tt45368240"},{"id":"tmdb://1765072"}]}]}}`,
		"/library/metadata/sss": `{"MediaContainer":{"Metadata":[{"title":"The Curse","year":2023,
			"Guid":[{"id":"imdb://tt13623608"},{"id":"tmdb://114655"},{"id":"tvdb://377278"}]}]}}`,
		"/library/metadata/eee": `{"MediaContainer":{"Metadata":[{"title":"Green Queen","duration":4140000}]}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := pages[r.URL.Path]; ok && r.Header.Get("X-Plex-Token") == "tok" {
			w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPlaysFromDatabase(t *testing.T) {
	prov := fakeProvider(t)
	c := &Client{Token: "tok", AccountID: 1, DeviceID: 5, DBPath: fakePlexDB(t), ProviderURL: prov.URL,
		URL: "http://127.0.0.1:1"} // the HTTP history must not be needed
	plays, err := c.Plays(context.Background(), time.Unix(1763000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 3 {
		t.Fatalf("%d plays, want 3: %+v", len(plays), plays)
	}
	mv, ep, listen := plays[0], plays[1], plays[2]
	if mv.SourceKey != "/status/sessions/history/101" || mv.Identity.TMDB != 1765072 || mv.Identity.IMDb != "tt45368240" ||
		mv.Identity.Year != 2025 || mv.EndedAt.Sub(mv.StartedAt) != 175*time.Minute {
		t.Errorf("deleted movie %+v (%v)", mv.Identity, mv.EndedAt.Sub(mv.StartedAt))
	}
	want := model.Identity{Kind: model.KindEpisode, Title: "The Curse", Year: 2023, Season: 1, Episode: 10,
		EpisodeTitle: "Green Queen", TMDB: 114655, TVDB: 377278, IMDb: "tt13623608"}
	if ep.Identity != want || ep.EndedAt.Sub(ep.StartedAt) != 69*time.Minute {
		t.Errorf("episode %+v (%v)", ep.Identity, ep.EndedAt.Sub(ep.StartedAt))
	}
	if listen.Identity.TMDB != 1703223 || listen.Identity.IMDb != "tt39110874" || listen.Identity.Year != 2025 {
		t.Errorf("new film from the library's own IDs: %+v", listen.Identity)
	}
}

func TestPlaysFallBackToHTTP(t *testing.T) {
	srv := plexServer(t)
	c := &Client{URL: srv.URL, Token: "tok", AccountID: 1, DBPath: filepath.Join(t.TempDir(), "missing.db")}
	plays, err := c.Plays(context.Background(), time.Unix(1763000000, 0))
	if err != nil || len(plays) != 3 {
		t.Fatalf("plays %d err %v", len(plays), err)
	}
}

func TestYearOfReleased(t *testing.T) {
	for v, want := range map[any]int{"2025-10-01": 2025, int64(1735689600): 2025, nil: 0, "": 0} {
		if got := yearOfReleased(v); got != want {
			t.Errorf("%v: %d", v, got)
		}
	}
}
