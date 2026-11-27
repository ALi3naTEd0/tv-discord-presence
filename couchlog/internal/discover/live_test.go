//go:build live

package discover

import (
	"context"
	"testing"
)

// go test -tags live ./internal/discover -run Live -v
func TestLiveScan(t *testing.T) {
	found, err := Scan(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Logf("%+v", f)
	}
}
