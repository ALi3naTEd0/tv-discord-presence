package identify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// tmdbServer answers like TMDB for a few canned paths and counts requests.
func tmdbServer(t *testing.T, check func(*http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	pages := map[string]string{
		"/search/multi": `{"results":[
			{"id":438631,"media_type":"movie","title":"Dune","original_title":"Dune","release_date":"2021-09-15","poster_path":"/d.jpg"},
			{"id":17,"media_type":"person","name":"Someone"},
			{"id":90228,"media_type":"tv","name":"Dune: Prophecy","first_air_date":"2024-11-17"}]}`,
		"/movie/438631": `{"id":438631,"title":"Dune","release_date":"2021-09-15","runtime":155,"poster_path":"/d.jpg",
			"imdb_id":"tt1160419","external_ids":{"imdb_id":"tt1160419"}}`,
		"/tv/114655": `{"id":114655,"name":"The Curse","first_air_date":"2023-11-10","episode_run_time":[60],
			"seasons":[{"season_number":1,"episode_count":10},{"season_number":0,"episode_count":1}],
			"external_ids":{"imdb_id":"tt13623608","tvdb_id":377278}}`,
		"/tv/114655/season/1": `{"episodes":[{"episode_number":1,"name":"Land of Enchantment","runtime":62},
			{"episode_number":2,"name":"Pressure Washer","runtime":0}]}`,
		"/find/tt1160419": `{"movie_results":[{"id":438631,"title":"Dune","release_date":"2021-09-15"}],"tv_results":[]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if check != nil {
			check(r)
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status_code":34,"status_message":"The resource you requested could not be found."}`))
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestTMDBSearchSkipsPeople(t *testing.T) {
	srv, _ := tmdbServer(t, nil)
	c := &TMDB{Key: "v3key", BaseURL: srv.URL}
	got, err := c.Search(context.Background(), "dune")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != Movie || got[0].Year != 2021 || got[0].Poster != PosterBase+"/d.jpg" ||
		got[1].Kind != Show || got[1].Title != "Dune: Prophecy" || got[1].Year != 2024 {
		t.Fatalf("search %+v", got)
	}
}

func TestTMDBKeyKinds(t *testing.T) {
	var auth, apiKey string
	srv, _ := tmdbServer(t, func(r *http.Request) {
		auth, apiKey = r.Header.Get("Authorization"), r.URL.Query().Get("api_key")
	})
	ctx := context.Background()
	(&TMDB{Key: "eyJhbGciOi.token", BaseURL: srv.URL}).Get(ctx, Movie, 438631)
	if auth != "Bearer eyJhbGciOi.token" || apiKey != "" {
		t.Errorf("v4 token sent as auth=%q api_key=%q", auth, apiKey)
	}
	(&TMDB{Key: "abc123", BaseURL: srv.URL, Language: "es-MX"}).Get(ctx, Movie, 438631)
	if auth != "" || apiKey != "abc123" {
		t.Errorf("v3 key sent as auth=%q api_key=%q", auth, apiKey)
	}
}

func TestTMDBGetShowAndCache(t *testing.T) {
	srv, hits := tmdbServer(t, nil)
	c := &TMDB{Key: "k", BaseURL: srv.URL}
	ctx := context.Background()
	s, err := c.Get(ctx, Show, 114655)
	if err != nil {
		t.Fatal(err)
	}
	if s.IMDb != "tt13623608" || s.TVDB != 377278 || s.Runtime != 60 || len(s.Seasons) != 2 || s.Seasons[0].Number != 0 {
		t.Fatalf("show %+v", s)
	}
	c.Get(ctx, Show, 114655)
	if n := hits.Load(); n != 1 {
		t.Errorf("%d requests for the same show, want 1 (cached)", n)
	}
}

func TestTMDBNotFound(t *testing.T) {
	srv, _ := tmdbServer(t, nil)
	_, err := (&TMDB{Key: "k", BaseURL: srv.URL}).Get(context.Background(), Movie, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err %v, want ErrNotFound", err)
	}
}

func TestTMDBFindAndSeason(t *testing.T) {
	srv, _ := tmdbServer(t, nil)
	c := &TMDB{Key: "k", BaseURL: srv.URL}
	ctx := context.Background()
	f, err := c.Find(ctx, "tt1160419", "imdb_id")
	if err != nil || len(f) != 1 || f[0].TMDB != 438631 || f[0].Kind != Movie {
		t.Fatalf("find %+v %v", f, err)
	}
	eps, err := c.Season(ctx, 114655, 1)
	if err != nil || len(eps) != 2 || eps[0].Name != "Land of Enchantment" || eps[0].Runtime != 62 {
		t.Fatalf("season %+v %v", eps, err)
	}
}

func TestTMDBServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := (&TMDB{Key: "k", BaseURL: srv.URL}).Search(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("err %v, want HTTP 429", err)
	}
}

func TestTMDBBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status_code":7,"status_message":"Invalid API key"}`))
	}))
	defer srv.Close()
	_, err := (&TMDB{Key: "nope", BaseURL: srv.URL}).Search(context.Background(), "x")
	if !errors.Is(err, ErrBadKey) {
		t.Errorf("err %v, want ErrBadKey", err)
	}
}
