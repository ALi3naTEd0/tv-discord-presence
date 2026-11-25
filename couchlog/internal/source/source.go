// Package source defines how Couchlog listens to devices. Each device kind
// (LG webOS, Android TV, Plex, later Jellyfin, Kodi, Roku) lives in its own
// subpackage and implements Source. Sources know nothing about sessions or
// the database: they only report what the device says.
package source

import (
	"context"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Source watches one device.
type Source interface {
	// Kind is the device kind: "webos", "androidtv", "plex", ...
	Kind() string

	// Run reports observations until ctx ends. It reconnects by itself when
	// the device goes away (TV turned off, network drop) and returns only
	// when ctx is done or the configuration is unusable. emit must not block
	// for long; the engine buffers.
	Run(ctx context.Context, emit func(model.Observation)) error
}

// Play is a finished, already identified viewing, as Plex records them.
type Play struct {
	SourceKey string // stable id from the source (Plex history key); dedupes re-imports
	DeviceID  int64
	StartedAt time.Time
	EndedAt   time.Time
	Identity  model.Identity
	Poster    string
}

// Importer is implemented by sources that keep their own watch history.
type Importer interface {
	// Plays returns viewings that finished after since, oldest first.
	Plays(ctx context.Context, since time.Time) ([]Play, error)
}
