package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

func TestQualityInPlainWords(t *testing.T) {
	cases := []struct {
		v    model.Video
		want string
	}{
		{model.Video{Codec: "HEVC", Width: 3840, Height: 2160, HDR: "SDR"}, "4K UHD · HEVC"},
		{model.Video{Codec: "AV1", Width: 3840, Height: 1606, HDR: "HDR10"}, "4K UHD · AV1 · HDR10"},
		{model.Video{Codec: "HEVC", Width: 3840, Height: 2160, HDR: "DV"}, "4K UHD · HEVC · Dolby Vision"},
		{model.Video{Codec: "Dolby Vision", Width: 3840, Height: 2160}, "4K UHD · Dolby Vision"},
		{model.Video{Codec: "H.264", Width: 1440, Height: 1080, HDR: "SDR"}, "1080p · H.264"},
		{model.Video{Codec: "HEVC", Width: 1024, Height: 576}, "576p · HEVC"},
		{model.Video{HDR: "SDR"}, ""},
	}
	for _, c := range cases {
		q := QualityOf(&c.v)
		if q == nil || q.Label != c.want {
			t.Errorf("%+v: got %+v, want %q", c.v, q, c.want)
		}
	}
	if QualityOf(nil) != nil {
		t.Error("nil video should have no quality")
	}
}

func TestLinksFromIDs(t *testing.T) {
	ep := LinksOf(model.Identity{Kind: model.KindEpisode, TMDB: 277831, IMDb: "tt1", Season: 1, Episode: 4})
	if len(ep) != 3 || ep[1].URL != "https://www.themoviedb.org/tv/277831/season/1/episode/4" ||
		ep[2].URL != "https://trakt.tv/search/tmdb/277831?id_type=show" || ep[0].Site != "imdb" {
		t.Errorf("episode: %+v", ep)
	}
	mv := LinksOf(model.Identity{Kind: model.KindMovie, TMDB: 603})
	if len(mv) != 2 || mv[0].URL != "https://www.themoviedb.org/movie/603" {
		t.Errorf("movie: %+v", mv)
	}
	if n := LinksOf(model.Identity{Kind: model.KindMovie, Title: "x"}); len(n) != 0 {
		t.Errorf("no ids: %+v", n)
	}
}

func TestNowShowsWhenItEnds(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 11, 27, 1, 40, 0, 0, time.UTC)
	dev, _ := st.EnsureDevice(ctx, store.LocalUserID, "androidtv", "Box", "")
	sess, err := st.StartSession(ctx, dev, model.App{ID: "com.wbd.stream", Name: "HBO Max"}, "", now.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	st.SetVideo(ctx, sess.ID, model.Video{Codec: "HEVC", Width: 3840, Height: 2160, HDR: "SDR"})
	st.SetPosition(ctx, sess.ID, 20*time.Minute)
	it := model.Item{Identity: model.Identity{Kind: model.KindEpisode, Title: "The Yogurt Shop Murders", Season: 1,
		Episode: 4, EpisodeTitle: "In Your Time", TMDB: 277831}, RuntimeMinutes: 60}
	if err := st.Identify(ctx, sess.ID, []model.Item{it}, []string{"trakt"}, now); err != nil {
		t.Fatal(err)
	}
	a := &API{Store: st, Version: "test", Now: func() time.Time { return now }}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/now", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got Sessions
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("%+v", got)
	}
	s := got.Sessions[0]
	if s.Quality == nil || s.Quality.Label != "4K UHD · HEVC" || s.Device.Name != "Box" || s.App.Name != "HBO Max" {
		t.Errorf("session: %+v", s)
	}
	if s.EndsAt == nil || !s.EndsAt.Equal(now.Add(40*time.Minute)) {
		t.Errorf("ends at %v, want %v", s.EndsAt, now.Add(40*time.Minute))
	}
	if len(s.Items) != 1 || s.Items[0].Deliveries["trakt"].Status != "pending" || len(s.Items[0].Links) != 2 {
		t.Errorf("items: %+v", s.Items)
	}
	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/history?days=abc", nil))
	if rec.Code != 400 {
		t.Errorf("bad days: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/sessions/999", nil))
	if rec.Code != 404 {
		t.Errorf("missing session: %d", rec.Code)
	}
}
