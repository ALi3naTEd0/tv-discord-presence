// Package sink defines the destinations Couchlog sends your history to:
// Trakt and Simkl today. Each lives in its own subpackage.
package sink

import (
	"context"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Entry is one viewing to send: what it was and when it was watched.
type Entry struct {
	Item      model.Item
	WatchedAt int64 // Unix milliseconds
}

// Result is what a destination said about one entry.
type Result struct {
	ItemID int64
	Status model.DeliveryStatus
	Error  string // set when Status is Failed; a reason for people otherwise
}

// Sink is a destination for watch history. Everything goes by external ID
// (TMDB, IMDb, TVDB), never by title.
type Sink interface {
	// Name is the delivery key: "trakt", "simkl".
	Name() string

	// Check reports, without changing anything, whether each entry is
	// already in your history there (Added), exists there but is not in your
	// history (Ready), or is unknown to the service (NotFound). Auto-send
	// calls it first so nothing is ever sent twice.
	Check(ctx context.Context, entries []Entry) ([]Result, error)

	// Push adds the entries to your history. Implementations respect the
	// service's rate limits and Retry-After.
	Push(ctx context.Context, entries []Entry) ([]Result, error)
}
