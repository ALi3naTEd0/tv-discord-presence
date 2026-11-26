package plex

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLivePlex reads the real server. It runs only with its URL and token:
//
//	COUCHLOG_PLEX_URL=http://127.0.0.1:32400 COUCHLOG_PLEX_TOKEN=... go test ./internal/source/plex -run Live -v
func TestLivePlex(t *testing.T) {
	u, tok := os.Getenv("COUCHLOG_PLEX_URL"), os.Getenv("COUCHLOG_PLEX_TOKEN")
	if u == "" || tok == "" {
		t.Skip("COUCHLOG_PLEX_URL / COUCHLOG_PLEX_TOKEN not set")
	}
	c := &Client{URL: u, Token: tok, AccountID: 1, DBPath: DefaultDBPath()}
	t.Logf("Plex database: %s", c.DBPath)
	ctx := context.Background()
	plays, err := c.Plays(ctx, time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d plays in the last 7 days", len(plays))
	for _, p := range plays[max(0, len(plays)-5):] {
		t.Logf("  %s %s S%dE%d tmdb=%d imdb=%s tvdb=%d (%v)", p.EndedAt.Format("Jan 2 15:04"), p.Identity.Title,
			p.Identity.Season, p.Identity.Episode, p.Identity.TMDB, p.Identity.IMDb, p.Identity.TVDB, p.EndedAt.Sub(p.StartedAt))
	}
	now, err := c.NowPlaying(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range now {
		t.Logf("now: %s on %s, %s at %v, video %+v", n.Identity.Title, n.Device, n.State, n.Offset, n.Video)
	}
}
