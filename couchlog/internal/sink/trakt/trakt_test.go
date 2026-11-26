package trakt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/sink"
)

var at = time.Date(2026, 11, 24, 4, 31, 0, 0, time.UTC)

// fakeTrakt keeps a history and answers the endpoints Couchlog uses.
type fakeTrakt struct {
	mu      sync.Mutex
	plays   []map[string]any
	writes  []string
	removed []int64
	limited bool // first write gets a 429
}

func (f *fakeTrakt) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("trakt-api-key") != "cid" {
		w.WriteHeader(403)
		return
	}
	auth := r.Header.Get("Authorization") == "Bearer tok"
	switch {
	case strings.HasPrefix(r.URL.Path, "/sync/history/") && r.Method == "GET":
		if !auth {
			w.WriteHeader(401)
			return
		}
		kind := strings.TrimPrefix(r.URL.Path, "/sync/history/")
		var out []map[string]any
		for _, p := range f.plays {
			if (kind == "movies") == (p["movie"] != nil) {
				out = append(out, p)
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/sync/history" && r.Method == "POST":
		if f.limited {
			f.limited = false
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.writes = append(f.writes, string(b))
		if strings.Contains(string(b), `"tmdb":404`) {
			w.Write([]byte(`{"added":{"movies":0,"episodes":0},"not_found":{"movies":[{"ids":{"tmdb":404}}]}}`))
			return
		}
		w.Write([]byte(`{"added":{"movies":1,"episodes":0},"not_found":{"movies":[]}}`))
	case r.URL.Path == "/sync/history/remove":
		var b struct{ IDs []int64 }
		json.NewDecoder(r.Body).Decode(&b)
		f.removed = append(f.removed, b.IDs...)
		w.Write([]byte(`{"deleted":{"movies":1}}`))
	case strings.HasPrefix(r.URL.Path, "/search/"):
		// tmdb 340615 is a show; tt1 is both a movie and a show on IMDb
		if strings.Contains(r.URL.Path, "/340615") && r.URL.Query().Get("type") == "show" {
			w.Write([]byte(`[{"type":"show","show":{"ids":{"trakt":1}}}]`))
			return
		}
		if strings.Contains(r.URL.Path, "/tt1") {
			w.Write([]byte(`[{"type":"movie","movie":{"ids":{"trakt":2}}}]`))
			return
		}
		w.Write([]byte(`[]`))
	default:
		w.WriteHeader(404)
	}
}

func newTrakt(t *testing.T, f *fakeTrakt) *Trakt {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tr := &Trakt{ClientID: "cid", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	tr.Sleep = func(context.Context, time.Duration) error { return nil }
	return tr
}

func episode(id int64, tmdb, s, e int) sink.Entry {
	return sink.Entry{Item: model.Item{ID: id, Identity: model.Identity{Kind: model.KindEpisode, Title: "x", TMDB: tmdb, Season: s, Episode: e}},
		WatchedAt: at.UnixMilli()}
}

func movie(id int64, tmdb int, imdb string) sink.Entry {
	return sink.Entry{Item: model.Item{ID: id, Identity: model.Identity{Kind: model.KindMovie, Title: "m", TMDB: tmdb, IMDb: imdb}},
		WatchedAt: at.UnixMilli()}
}

func TestCheck(t *testing.T) {
	f := &fakeTrakt{plays: []map[string]any{
		{"id": 11, "watched_at": at.Add(3 * time.Minute), "show": map[string]any{"ids": map[string]any{"tmdb": 340615}},
			"episode": map[string]any{"season": 1, "number": 1}},
		{"id": 12, "watched_at": at.Add(-20 * time.Hour), "show": map[string]any{"ids": map[string]any{"tmdb": 340615}},
			"episode": map[string]any{"season": 1, "number": 2}}, // another day: not this viewing
	}}
	tr := newTrakt(t, f)
	res, err := tr.Check(context.Background(), []sink.Entry{
		episode(1, 340615, 1, 1), episode(2, 340615, 1, 2), movie(3, 0, "tt1"), episode(4, 0, 1, 1), showByIMDb(5, "tt1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// tt1 is a movie on Trakt: found as a movie, not as the show sharing its IMDb id.
	want := []model.DeliveryStatus{model.Added, model.Ready, model.Ready, model.Failed, model.NotFound}
	for i, r := range res {
		if r.Status != want[i] {
			t.Errorf("entry %d: %s (%s), want %s", i, r.Status, r.Error, want[i])
		}
	}
}

func TestPushAndRateLimit(t *testing.T) {
	f := &fakeTrakt{limited: true}
	tr := newTrakt(t, f)
	res, err := tr.Push(context.Background(), []sink.Entry{movie(1, 438631, "tt1160419"), movie(2, 404, ""), episode(3, 340615, 1, 3)})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != model.Added || res[1].Status != model.NotFound || res[2].Status != model.Added {
		t.Fatalf("results %+v", res)
	}
	if len(f.writes) != 3 || !strings.Contains(f.writes[0], `"watched_at":"2026-11-24T04:31:00.000Z"`) ||
		!strings.Contains(f.writes[0], `"imdb":"tt1160419"`) || !strings.Contains(f.writes[2], `"seasons":[{"episodes":[{"number":3`) {
		t.Errorf("writes %q", f.writes)
	}
}

func TestRemoveTakesOnlyThatPlay(t *testing.T) {
	f := &fakeTrakt{plays: []map[string]any{
		{"id": 21, "watched_at": at.Add(time.Minute), "movie": map[string]any{"ids": map[string]any{"tmdb": 111}}},
		{"id": 22, "watched_at": at.Add(-48 * time.Hour), "movie": map[string]any{"ids": map[string]any{"tmdb": 111}}}, // a rewatch: stays
	}}
	tr := newTrakt(t, f)
	res, err := tr.Remove(context.Background(), []sink.Entry{movie(1, 111, ""), movie(2, 999, "")})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != model.Removed || res[1].Status != model.Removed || res[1].Error == "" {
		t.Errorf("results %+v", res)
	}
	if len(f.removed) != 1 || f.removed[0] != 21 {
		t.Errorf("removed %v, want only play 21", f.removed)
	}
}

func showByIMDb(id int64, imdb string) sink.Entry {
	return sink.Entry{Item: model.Item{ID: id, Identity: model.Identity{Kind: model.KindEpisode, Title: "s", IMDb: imdb, Season: 1, Episode: 1}},
		WatchedAt: at.UnixMilli()}
}
