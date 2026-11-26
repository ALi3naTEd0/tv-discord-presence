package androidtv

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// TestLiveBox reads the real box once. It runs only when told where it is:
//
//	COUCHLOG_ATV_ADDR=192.168.1.117 COUCHLOG_ADB_KEY=~/.config/tvpresence/adbkey \
//	  go test ./internal/source/androidtv -run Live -v
func TestLiveBox(t *testing.T) {
	addr, keyPath := os.Getenv("COUCHLOG_ATV_ADDR"), os.Getenv("COUCHLOG_ADB_KEY")
	if addr == "" || keyPath == "" {
		t.Skip("COUCHLOG_ATV_ADDR / COUCHLOG_ADB_KEY not set")
	}
	s := &Source{Addr: addr, KeyPath: keyPath, Poll: time.Second, FormatsEvery: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	got := make(chan model.Observation, 8)
	go s.Run(ctx, func(o model.Observation) { got <- o })
	for i := 0; i < 3; i++ {
		select {
		case o := <-got:
			t.Logf("state=%s app=%q (%s) media=%q hint=%+v pos=%v video=%+v",
				o.State, o.App.ID, o.App.Name, o.MediaID, o.Hint, o.Position, o.Video)
		case <-ctx.Done():
			t.Fatal("no observation from the box")
		}
	}
}
