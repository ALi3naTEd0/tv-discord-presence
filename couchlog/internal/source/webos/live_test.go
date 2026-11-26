package webos

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// TestLiveTV reads the real LG TV for a few seconds. It runs only when told
// where it is; with tvpresence's key file the TV does not ask again:
//
//	COUCHLOG_LG_HOST=192.168.1.220 COUCHLOG_LG_KEY=~/.config/tvpresence/key go test ./internal/source/webos -run Live -v
func TestLiveTV(t *testing.T) {
	host, key := os.Getenv("COUCHLOG_LG_HOST"), os.Getenv("COUCHLOG_LG_KEY")
	if host == "" || key == "" {
		t.Skip("COUCHLOG_LG_HOST / COUCHLOG_LG_KEY not set")
	}
	s := &Source{Host: host, KeyPath: key, Poll: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	got := make(chan model.Observation, 8)
	go s.Run(ctx, func(o model.Observation) { got <- o })
	for i := 0; i < 3; i++ {
		select {
		case o := <-got:
			t.Logf("state=%s app=%q (%s) media=%q", o.State, o.App.ID, o.App.Name, o.MediaID)
		case <-ctx.Done():
			t.Fatal("no observation from the TV")
		}
	}
}
