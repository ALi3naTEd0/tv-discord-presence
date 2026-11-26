package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

func plexServer(t *testing.T) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"/status/sessions/history/all": `{"MediaContainer":{"Metadata":[
			{"historyKey":"/status/sessions/history/3","ratingKey":"20","type":"episode","title":"Green Queen","grandparentTitle":"The Curse","grandparentRatingKey":"10","parentIndex":1,"index":10,"viewedAt":1764100000},
			{"historyKey":"/status/sessions/history/2","ratingKey":"30","type":"movie","title":"Dune","year":2021,"viewedAt":1764000000},
			{"historyKey":"/status/sessions/history/1","ratingKey":"40","type":"track","title":"A song","viewedAt":1763900000},
			{"ratingKey":"99","type":"movie","title":"Deleted film","originallyAvailableAt":"1999-03-31","viewedAt":1763800000,"duration":5400000}]}}`,
		"/library/metadata/20": `{"MediaContainer":{"Metadata":[{"ratingKey":"20","type":"episode","title":"Green Queen","parentIndex":1,"index":10,"duration":4140000,"grandparentRatingKey":"10"}]}}`,
		"/library/metadata/10": `{"MediaContainer":{"Metadata":[{"ratingKey":"10","type":"show","title":"The Curse","year":2023,
			"Guid":[{"id":"imdb://tt13623608"},{"id":"tmdb://114655"},{"id":"tvdb://377278"}]}]}}`,
		"/library/metadata/30": `{"MediaContainer":{"Metadata":[{"ratingKey":"30","type":"movie","title":"Dune","year":2021,"duration":9300000,
			"guid":"com.plexapp.agents.imdb://tt1160419?lang=en","Guid":[{"id":"tmdb://438631"},{"id":"tvdb://123"}]}]}}`,
		"/status/sessions": `{"MediaContainer":{"Metadata":[
			{"ratingKey":"30","type":"movie","title":"Dune","year":2021,"viewOffset":600000,"duration":9300000,
			 "User":{"id":"2","title":"guest"},"Player":{"title":"Living room","state":"playing"}},
			{"ratingKey":"20","type":"episode","title":"Green Queen","grandparentTitle":"The Curse","grandparentRatingKey":"10","parentIndex":1,"index":10,
			 "viewOffset":120000,"duration":4140000,"User":{"id":"1","title":"me"},"Player":{"title":"Mi Box","state":"paused"},
			 "Media":[{"width":3840,"height":2160,"videoCodec":"hevc","Part":[{"Stream":[{"streamType":1,"displayTitle":"4K DoVi/HDR10 (HEVC Main 10)"}]}]}]},
			{"ratingKey":"30","type":"movie","title":"Dune","year":2021,"viewOffset":5000,"duration":9300000,
			 "User":{"id":1,"title":"me"},"Player":{"product":"Plex Web","state":"playing"},
			 "Media":[{"width":1920,"height":1080,"videoCodec":"h264","Part":[{"Stream":[{"streamType":1,"displayTitle":"1080p (H.264)"}]}]}]}]}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/status/sessions/history/all" && (r.URL.Query().Get("accountID") != "1" || r.URL.Query().Get("viewedAt>") == "") {
			t.Errorf("history query %s", r.URL.RawQuery)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPlays(t *testing.T) {
	srv := plexServer(t)
	c := &Client{URL: srv.URL, Token: "tok", AccountID: 1, DeviceID: 5}
	plays, err := c.Plays(context.Background(), time.Unix(1763000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 3 {
		t.Fatalf("%d plays, want 3 (music skipped): %+v", len(plays), plays)
	}
	gone, dune, ep := plays[0], plays[1], plays[2] // oldest first
	if ep.SourceKey != "/status/sessions/history/3" || ep.DeviceID != 5 {
		t.Errorf("episode play %+v", ep)
	}
	want := model.Identity{Kind: model.KindEpisode, Title: "The Curse", Year: 2023, Season: 1, Episode: 10,
		EpisodeTitle: "Green Queen", TMDB: 114655, TVDB: 377278, IMDb: "tt13623608"}
	if ep.Identity != want {
		t.Errorf("episode identity %+v", ep.Identity)
	}
	if got := ep.EndedAt.Sub(ep.StartedAt); got != 69*time.Minute {
		t.Errorf("episode length %v, want the library's 69 min", got)
	}
	if dune.Identity.TMDB != 438631 || dune.Identity.IMDb != "tt1160419" || dune.Identity.TVDB != 0 {
		t.Errorf("movie ids %+v (legacy guid for IMDb, no TVDB for movies)", dune.Identity)
	}
	if gone.SourceKey != "plex:99:1763800000" || gone.Identity.Year != 1999 || gone.Identity.TMDB != 0 ||
		gone.EndedAt.Sub(gone.StartedAt) != 90*time.Minute {
		t.Errorf("deleted item play %+v", gone)
	}
}

func TestNowPlaying(t *testing.T) {
	srv := plexServer(t)
	c := &Client{URL: srv.URL, Token: "tok", AccountID: 1}
	now, err := c.NowPlaying(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(now) != 2 {
		t.Fatalf("%d sessions, want this account's 2: %+v", len(now), now)
	}
	if now[0].State != model.StatePlaying || now[0].Device != "Plex Web" || now[0].Video.HDR != "SDR" || now[0].Video.Codec != "H.264" {
		t.Errorf("first (playing) %+v %+v", now[0], now[0].Video)
	}
	ep := now[1]
	if ep.State != model.StatePaused || ep.Identity.TMDB != 114655 || ep.Offset != 2*time.Minute || ep.Video.HDR != "Dolby Vision" ||
		ep.Video.Width != 3840 || ep.Video.Codec != "HEVC" {
		t.Errorf("episode %+v %+v", ep, ep.Video)
	}
}

func TestBadToken(t *testing.T) {
	srv := plexServer(t)
	_, err := (&Client{URL: srv.URL, Token: "nope", AccountID: 1}).NowPlaying(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err %v", err)
	}
}
