package simkl

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/sink"
)

var at = time.Date(2026, 11, 24, 4, 31, 0, 0, time.UTC)

func newSimkl(t *testing.T, writes *[]string) *Simkl {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_id") != "cid" {
			w.WriteHeader(403)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/sync/all-items/"):
			switch strings.TrimPrefix(r.URL.Path, "/sync/all-items/") {
			case "movies":
				w.Write([]byte(`{"movies":[{"status":"completed","movie":{"ids":{"imdb":"tt45368240","tmdb":"1765072"}}}]}`))
			case "shows":
				w.Write([]byte(`{"shows":[{"status":"watching","show":{"ids":{"tmdb":340615}},"seasons":[{"number":1,"episodes":[{"number":1},{"number":2}]}]},
					{"status":"completed","show":{"ids":{"tvdb":"377278"}}}]}`))
			default:
				w.Write([]byte(`{}`))
			}
		case r.URL.Path == "/search/id":
			if r.URL.Query().Get("tmdb") == "340615" || r.URL.Query().Get("imdb") == "tt1" {
				w.Write([]byte(`[{"ids":{"simkl":5}}]`))
				return
			}
			w.Write([]byte(`[]`))
		case r.URL.Path == "/sync/history", r.URL.Path == "/sync/history/remove":
			b, _ := io.ReadAll(r.Body)
			*writes = append(*writes, r.URL.Path+" "+string(b))
			switch {
			case strings.Contains(string(b), `"404"`):
				w.Write([]byte(`{"added":{"movies":0},"not_found":{"movies":[{"ids":{"tmdb":"404"}}]}}`))
			case strings.Contains(string(b), `"777"`):
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"RATE_LIMIT"}`))
			case r.URL.Path == "/sync/history/remove":
				w.Write([]byte(`{"deleted":{"movies":1}}`))
			default:
				w.Write([]byte(`{"added":{"movies":1,"shows":0,"episodes":0},"not_found":{"movies":[],"shows":[]}}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	s := &Simkl{ClientID: "cid", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	s.Sleep = func(context.Context, time.Duration) error { return nil }
	return s
}

func ep(id int64, tmdb, tvdb, s, e int) sink.Entry {
	return sink.Entry{Item: model.Item{ID: id, Identity: model.Identity{Kind: model.KindEpisode, Title: "x", TMDB: tmdb, TVDB: tvdb, Season: s, Episode: e}}, WatchedAt: at.UnixMilli()}
}
func mv(id int64, tmdb int, imdb string) sink.Entry {
	return sink.Entry{Item: model.Item{ID: id, Identity: model.Identity{Kind: model.KindMovie, Title: "m", Year: 2025, TMDB: tmdb, IMDb: imdb}}, WatchedAt: at.UnixMilli()}
}

func TestCheck(t *testing.T) {
	var w []string
	s := newSimkl(t, &w)
	res, err := s.Check(context.Background(), []sink.Entry{
		mv(1, 1765072, ""),     // in your list (TMDB as text there)
		ep(2, 340615, 0, 1, 2), // watched episode
		ep(3, 340615, 0, 1, 3), // on Simkl, not watched
		ep(4, 0, 377278, 4, 9), // a show completed as a whole
		mv(5, 99, ""),          // not on Simkl
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []model.DeliveryStatus{model.Added, model.Added, model.Ready, model.Added, model.NotFound}
	for i, r := range res {
		if r.Status != want[i] {
			t.Errorf("entry %d: %s (%s), want %s", i, r.Status, r.Error, want[i])
		}
	}
	if len(w) != 0 {
		t.Errorf("check wrote %v", w)
	}
}

func TestPushAndRemove(t *testing.T) {
	var w []string
	s := newSimkl(t, &w)
	ctx := context.Background()
	res, _ := s.Push(ctx, []sink.Entry{mv(1, 1765072, "tt45368240"), mv(2, 404, ""), mv(3, 777, ""), ep(4, 340615, 0, 1, 3)})
	want := []model.DeliveryStatus{model.Added, model.NotFound, model.Failed, model.Added}
	for i, r := range res {
		if r.Status != want[i] {
			t.Errorf("push %d: %s (%s)", i, r.Status, r.Error)
		}
	}
	if !strings.Contains(res[2].Error, "wait") {
		t.Errorf("rate limit message %q", res[2].Error)
	}
	if !strings.Contains(w[0], `"watched_at":"2026-11-24T04:31:00Z"`) || !strings.Contains(w[0], `"imdb":"tt45368240"`) || !strings.Contains(w[0], `"year":2025`) {
		t.Errorf("movie body %s", w[0])
	}
	rr, _ := s.Remove(ctx, []sink.Entry{mv(1, 1765072, "")})
	if rr[0].Status != model.Removed || !strings.HasPrefix(w[len(w)-1], "/sync/history/remove") || strings.Contains(w[len(w)-1], "watched_at") {
		t.Errorf("remove %+v %s", rr, w[len(w)-1])
	}
}
