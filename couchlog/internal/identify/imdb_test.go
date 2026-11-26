package identify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

func imdbServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	pages := map[string]string{
		"tt50522528": `{"d":[{"id":"tt50522528","l":"The Swallowed","y":2026,"qid":"movie","i":{"imageUrl":"https://m.media-amazon.com/images/M/swallowed._V1_.jpg"}}]}`,
		"andrew tate: toxic empire": `{"d":[
			{"id":"tt50289581","l":"Andrew Tate: Toxic Empire","y":2026,"qid":"tvMiniSeries","i":{"imageUrl":"https://m.media-amazon.com/images/M/tate._V1_.jpg"}},
			{"id":"tt32651219","l":"Andrew Tate: Icon or Toxic?","y":2024,"qid":"movie","i":{"imageUrl":"https://x/other.jpg"}}]}`,
		"dune": `{"d":[
			{"id":"tt1160419","l":"Dune","y":2021,"qid":"movie","i":{"imageUrl":"https://x/dune21.jpg"}},
			{"id":"tt0087182","l":"Dune","y":1984,"qid":"movie","i":{"imageUrl":"https://x/dune84.jpg"}}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		q := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/suggestion/x/"), ".json")
		w.Write([]byte(pages[q]))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestIMDbPoster(t *testing.T) {
	srv, hits := imdbServer(t)
	m := &IMDb{BaseURL: srv.URL}
	ctx := context.Background()
	if p := m.Poster(ctx, model.KindMovie, "The Swallowed", 2026, "tt50522528"); p != "https://m.media-amazon.com/images/M/swallowed._V1_QL75_UX300_.jpg" {
		t.Errorf("by id: %q", p)
	}
	if p := m.Poster(ctx, model.KindEpisode, "Andrew Tate: Toxic Empire", 0, ""); !strings.Contains(p, "tate._V1_QL75") {
		t.Errorf("by title: %q", p)
	}
	if p := m.Poster(ctx, model.KindEpisode, "Andrew Tate: Toxic Empire", 2019, ""); p != "" {
		t.Errorf("wrong year matched: %q", p)
	}
	if p := m.Poster(ctx, model.KindMovie, "Andrew Tate: Toxic Empire", 0, ""); p != "" {
		t.Errorf("a series matched as a movie: %q", p)
	}
	if p := m.Poster(ctx, model.KindMovie, "Dune", 0, ""); p != "" {
		t.Errorf("ambiguous title guessed: %q", p)
	}
	if p := m.Poster(ctx, model.KindMovie, "Dune", 1984, ""); p != "https://x/dune84.jpg" {
		t.Errorf("year disambiguates: %q", p)
	}
	before := hits.Load()
	m.Poster(ctx, model.KindMovie, "Dune", 2021, "")
	if hits.Load() != before {
		t.Error("same query fetched again; should be cached")
	}
}

func TestItemPosterFromIMDbWhenTMDBHasNone(t *testing.T) {
	srv, _ := imdbServer(t)
	cat := catalog()
	tate := Title{Kind: Show, TMDB: 343245, Title: "Andrew Tate: Toxic Empire", Year: 2026, Seasons: []Season{{Number: 1, Episodes: 4}}}
	cat.titles["tv:343245"] = tate
	id := &Identifier{Catalog: cat, Posters: &IMDb{BaseURL: srv.URL}}
	it, err := id.Item(context.Background(), Show, 343245, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(it.Poster, "tate._V1_QL75") {
		t.Errorf("poster %q", it.Poster)
	}
	// A TMDB poster wins.
	if p := id.Poster(context.Background(), model.Item{Identity: model.Identity{Kind: model.KindMovie, Title: "Dune", TMDB: 438631}}); p != "p" {
		t.Errorf("TMDB poster %q", p)
	}
}
