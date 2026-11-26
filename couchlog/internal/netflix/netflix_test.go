package netflix

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// fakeStore writes a Firefox-like cookies.sqlite.
func fakeStore(t *testing.T, dir string, cookies map[string]int64) string {
	t.Helper()
	p := filepath.Join(dir, "cookies.sqlite")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`CREATE TABLE moz_cookies (id INTEGER PRIMARY KEY, name TEXT, value TEXT, host TEXT, expiry INTEGER, lastAccessed INTEGER)`)
	future := time.Now().Add(time.Hour).Unix()
	for name, at := range cookies {
		db.Exec("INSERT INTO moz_cookies(name, value, host, expiry, lastAccessed) VALUES (?, ?, '.netflix.com', ?, ?)", name, "v-"+name, future, at)
	}
	db.Exec("INSERT INTO moz_cookies(name, value, host, expiry, lastAccessed) VALUES ('sid', 'x', '.example.com', ?, 9)", future)
	db.Exec("INSERT INTO moz_cookies(name, value, host, expiry, lastAccessed) VALUES ('old', 'x', '.netflix.com', 1, 9)")
	return p
}

func TestCookiesFromMostRecentProfile(t *testing.T) {
	a := fakeStore(t, t.TempDir(), map[string]int64{"NetflixId": 100, "SecureNetflixId": 100})
	b := fakeStore(t, t.TempDir(), map[string]int64{"NetflixId": 200})
	none := fakeStore(t, t.TempDir(), map[string]int64{"nfvdid": 300})
	c, src, err := cookiesFrom([]string{a, none, b})
	if err != nil {
		t.Fatal(err)
	}
	if src != filepath.Dir(b) || len(c) != 1 || c["NetflixId"] != "v-NetflixId" {
		t.Errorf("cookies %v from %s", c, src)
	}
	if _, _, err := cookiesFrom([]string{none}); err != ErrNoSession {
		t.Errorf("err %v, want ErrNoSession", err)
	}
}

func fakeNetflix(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "NetflixId=abc") {
			w.Write([]byte("<html>login</html>"))
			return
		}
		switch {
		case r.URL.Path == "/settings/viewed/":
			w.Write([]byte(`<script>netflix.reactContext = {"models":{"userInfo":{"data":{"name":"Gordo","authURL":"a\x2Fb\x3D\x3D","userGuid":"GUID1"}},` +
				`"serverDefs":{"data":{"BUILD_IDENTIFIER":"v123"}}}}</script>`))
		case strings.HasPrefix(r.URL.Path, "/api/aui/pathEvaluator"):
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "GUID1") || r.Header.Get("x-netflix.request.routing") == "" {
				t.Errorf("activity request body %q", body)
			}
			w.Write([]byte(`{"jsonGraph":{"aui":{"viewingActivity":{"$type":"atom","value":{"viewedItems":[
				{"movieID":82980714,"title":"One Last Order","date":1795599641000},
				{"movieID":81949112,"series":81777231,"seriesTitle":"The Trials of Karen Read","episodeTitle":"The Evidence","title":"The Trials of Karen Read: \"The Evidence\"","date":1795409175000}]}}}}}`))
		case r.URL.Path == "/nq/website/memberapi/release/metadata":
			switch r.URL.Query().Get("movieid") {
			case "81777231":
				w.Write([]byte(`{"video":{"id":81777231,"type":"show","title":"The Trials of Karen Read","hiddenEpisodeNumbers":false,
					"seasons":[{"seq":1,"episodes":[{"id":81949110,"seq":1,"title":"A Broken Taillight"},{"id":81949112,"seq":3,"title":"The Evidence"}]}]}}`))
			case "82980714":
				w.Write([]byte(`{"video":{"id":82980714,"type":"movie","title":"One Last Order","year":2026}}`))
			default:
				w.WriteHeader(404)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func client(srv *httptest.Server) *Client {
	return &Client{BaseURL: srv.URL, Cookies: func() (map[string]string, string, error) {
		return map[string]string{"NetflixId": "abc"}, "test", nil
	}}
}

func TestProfileAndActivity(t *testing.T) {
	c := client(fakeNetflix(t))
	ctx := context.Background()
	p, err := c.Profile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Gordo" || p.GUID != "GUID1" || p.AuthURL != "a/b==" || p.Build != "v123" {
		t.Errorf("profile %+v", p)
	}
	v, err := c.Activity(ctx, 0, 50)
	if err != nil || len(v) != 2 || v[1].Series != 81777231 || v[0].Started().Unix() != 1795599641 {
		t.Fatalf("activity %+v %v", v, err)
	}
	md, err := c.Metadata(ctx, 81777231)
	if err != nil || md.Type != "show" || len(md.Seasons[0].Episodes) != 2 {
		t.Fatalf("metadata %+v %v", md, err)
	}
}

func TestExpiredSession(t *testing.T) {
	c := &Client{BaseURL: fakeNetflix(t).URL, Cookies: func() (map[string]string, string, error) {
		return map[string]string{"NetflixId": "stale"}, "test", nil
	}}
	if _, err := c.Profile(context.Background()); err != ErrExpired {
		t.Errorf("err %v, want ErrExpired", err)
	}
}

func TestMatch(t *testing.T) {
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	sessions := []*model.Session{
		{ID: 1, StartedAt: at("2026-11-23T21:44:50Z")}, // an earlier viewing: Netflix kept only the latest
		{ID: 2, StartedAt: at("2026-11-23T23:46:19Z")},
		{ID: 3, StartedAt: at("2026-11-26T00:40:41Z")},
	}
	viewed := []Viewed{
		{MovieID: 10, DateMS: at("2026-11-26T00:41:05Z").UnixMilli()},
		{MovieID: 20, DateMS: at("2026-11-23T23:46:15Z").UnixMilli()},
		{MovieID: 30, DateMS: at("2026-11-23T22:48:51Z").UnixMilli()}, // no session started near it
	}
	got := Match(sessions, viewed)
	if len(got) != 2 || got[0].Session.ID != 3 || got[0].Viewed.MovieID != 10 || got[1].Session.ID != 2 || got[1].Viewed.MovieID != 20 {
		t.Errorf("pairs %+v", got)
	}
}

type catalog struct{}

func (catalog) Search(_ context.Context, q string) ([]identify.Title, error) {
	switch q {
	case "The Trials of Karen Read":
		return []identify.Title{{Kind: identify.Show, TMDB: 340615, Title: q}}, nil
	case "One Last Order":
		return []identify.Title{{Kind: identify.Movie, TMDB: 1655112, Title: q, Year: 2026}, {Kind: identify.Movie, TMDB: 9, Title: q, Year: 1990}}, nil
	}
	return nil, nil
}
func (catalog) Get(_ context.Context, k identify.Kind, id int) (identify.Title, error) {
	return identify.Title{Kind: k, TMDB: id, Title: "x", Seasons: []identify.Season{{Number: 1, Episodes: 3}}}, nil
}
func (catalog) Season(context.Context, int, int) ([]identify.Episode, error) {
	return []identify.Episode{{Number: 3, Name: "The Evidence", Runtime: 48}}, nil
}
func (catalog) Find(context.Context, string, string) ([]identify.Title, error) { return nil, nil }

func TestResolve(t *testing.T) {
	c := client(fakeNetflix(t))
	id := &identify.Identifier{Catalog: catalog{}}
	ctx := context.Background()
	ep, err := Resolve(ctx, c, id, Viewed{MovieID: 81949112, Series: 81777231, SeriesTitle: "The Trials of Karen Read"})
	if err != nil || ep == nil || ep.TMDB != 340615 || ep.Season != 1 || ep.Episode != 3 || ep.EpisodeTitle != "The Evidence" {
		t.Fatalf("episode %+v %v", ep, err)
	}
	// Two films with that name: Netflix's year picks one.
	mv, err := Resolve(ctx, c, id, Viewed{MovieID: 82980714, Title: "One Last Order"})
	if err != nil || mv == nil || mv.TMDB != 1655112 || mv.Kind != model.KindMovie {
		t.Fatalf("movie %+v %v", mv, err)
	}
}
