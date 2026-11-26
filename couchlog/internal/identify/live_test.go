package identify

import (
	"context"
	"os"
	"testing"
)

// TestLiveTMDB talks to the real TMDB. It runs only with a key:
//
//	COUCHLOG_TMDB_KEY=... go test ./internal/identify -run Live -v
func TestLiveTMDB(t *testing.T) {
	key := os.Getenv("COUCHLOG_TMDB_KEY")
	if key == "" {
		t.Skip("COUCHLOG_TMDB_KEY not set")
	}
	ctx := context.Background()
	id := &Identifier{Catalog: &TMDB{Key: key}}
	found, err := id.Search(ctx, "tt13623608") // The Curse, by IMDb id
	if err != nil || len(found) == 0 || found[0].Kind != Show {
		t.Fatalf("find by IMDb: %+v %v", found, err)
	}
	it, err := id.Item(ctx, Show, found[0].TMDB, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("item: %s S%02dE%02d %q, %d min (estimated %v), imdb %s tvdb %d",
		it.Title, it.Season, it.Episode, it.EpisodeTitle, it.RuntimeMinutes, it.RuntimeEstimated, it.IMDb, it.TVDB)
	if it.IMDb == "" || it.TVDB == 0 || it.EpisodeTitle == "" {
		t.Errorf("incomplete item %+v", it)
	}
	if _, err := id.Next(ctx, it); err != nil {
		t.Logf("after S1E10: %v", err)
	}
	auto, how, err := id.Auto(ctx, 1, session("netflix", "Les enfants vont bien", 0, 0))
	t.Logf("auto 'Les enfants vont bien': %+v %q %v", auto, how, err)
}
