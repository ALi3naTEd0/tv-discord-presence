// Package model holds the types every part of Couchlog shares: what a device
// reports (Observation), what the engine records (Session, Item) and where
// each item has been sent (Delivery).
package model

import "time"

// PlayState is what a device says it is doing.
type PlayState string

const (
	StateOff     PlayState = "off"     // device off or unreachable
	StateIdle    PlayState = "idle"    // on, in a menu or a non-video app
	StatePlaying PlayState = "playing" // a player is playing
	StatePaused  PlayState = "paused"  // a player is paused
)

// App is the foreground app on a device.
type App struct {
	ID   string // device's own id, e.g. "netflix", "com.google.android.youtube.tv"
	Name string // display name, e.g. "Netflix"
}

// Hint is what the device itself says is playing, when it says anything.
// It pre-fills identification; it is never trusted as an identification.
type Hint struct {
	Title    string
	Subtitle string // channel, tagline; YouTube hides real titles here
	Season   int    // 0 if unknown
	Episode  int    // 0 if unknown
}

// Video is the format the device decoded.
type Video struct {
	Codec  string // AV1, HEVC, H.264, VP9, Dolby Vision
	Width  int
	Height int
	HDR    string // SDR, HDR10, HLG, DV
}

// Identity is an exact identification, from sources that know what played
// (Plex) or from the user picking a title.
type Identity struct {
	Kind         ItemKind
	Title        string // the movie, or the show for episodes
	Year         int
	Season       int
	Episode      int
	EpisodeTitle string
	TMDB         int // of the show for episodes
	TVDB         int
	IMDb         string
}

// Observation is one report from a source: the state of one device at one
// moment. Sources emit them; the engine turns them into sessions.
type Observation struct {
	DeviceID int64
	At       time.Time
	State    PlayState
	App      App
	MediaID  string         // device's content id (webOS mediaId, Android session), "" if none
	Hint     Hint           // zero if the device publishes nothing
	Position *time.Duration // real playback position, nil if unknown
	Duration *time.Duration // length of what plays, nil if unknown
	Video    *Video         // nil if the device does not report it
	Identity *Identity      // nil unless the source knows exactly what this is
}

// ItemKind is movie or episode.
type ItemKind string

const (
	KindMovie   ItemKind = "movie"
	KindEpisode ItemKind = "episode"
)

// Hidden says why a session is not shown in the main history.
type Hidden string

const (
	Shown   Hidden = ""
	Ignored Hidden = "ignored" // the user hid it
	Short   Hidden = "short"   // under the app's own minimum (YouTube 20 min), kept 7 days
)

// Session is one stretch of playback in one app on one device.
type Session struct {
	ID           int64
	UserID       int64
	DeviceID     int64
	SourceKey    string // unique per source; dedupes imports
	App          App
	StartedAt    time.Time
	LastSeenAt   time.Time
	LastPlayAt   time.Time
	EndedAt      *time.Time // nil while open
	Played       time.Duration
	Open         bool
	Hidden       Hidden
	MediaID      string
	Hint         Hint
	Position     *time.Duration // furthest real position reached
	LivePosition *time.Duration // last sample, for "now playing"
	Video        *Video
	HDRTime      map[string]time.Duration // time per dynamic range; Video.HDR is the longest
	AutoNextFrom *int64                   // session this one was split from on autoplay
}

// Item is what a session actually was; a session may hold several episodes.
type Item struct {
	ID        int64
	SessionID int64
	Identity
	Poster           string
	RuntimeMinutes   int  // from TMDB, 0 if unknown
	RuntimeEstimated bool // borrowed from another listing
	// Guessed: the episode is a guess (the one after your last), never sent
	// until you confirm it by identifying the session.
	Guessed    bool
	ReplacedAt *time.Time // a correction replaced it; nil for current items
}

// SameTitle reports whether two identities are the same movie or episode,
// by external ID when both have one, so a correction to the same thing does
// not take it out of Trakt and send it again.
func (a Identity) SameTitle(b Identity) bool {
	if a.Kind != b.Kind || a.Season != b.Season || a.Episode != b.Episode {
		return false
	}
	switch {
	case a.TMDB != 0 && b.TMDB != 0:
		return a.TMDB == b.TMDB
	case a.IMDb != "" && b.IMDb != "":
		return a.IMDb == b.IMDb
	case a.TVDB != 0 && b.TVDB != 0:
		return a.TVDB == b.TVDB
	}
	return a.Title == b.Title && a.Year == b.Year
}

// Alias says that what a device (or Plex) calls MatchTitle is Identity.
// Season and episode in Identity are ignored: an alias names the movie or the
// show, and the episode comes from the device or the picker.
type Alias struct {
	ID         int64
	UserID     int64
	AppID      string // "" = any app
	MatchTitle string // as the device says it; stored normalized
	MatchYear  int    // 0 = any year
	Identity
	Hits      int
	CreatedAt time.Time
}

// DeliveryStatus is where an item stands with one destination.
type DeliveryStatus string

const (
	Pending  DeliveryStatus = "pending"   // not sent yet
	Ready    DeliveryStatus = "ready"     // the title exists there; not in your history yet
	Added    DeliveryStatus = "added"     // in your history there
	NotFound DeliveryStatus = "not_found" // the service does not have this title (yet)
	Skipped  DeliveryStatus = "skipped"   // nothing to do (already marked by other means)
	Failed   DeliveryStatus = "error"     // the last attempt failed; see Error
	Remove   DeliveryStatus = "remove"    // a correction replaced it: take it out of your history there
	Removed  DeliveryStatus = "removed"   // taken out; the replaced item can go once all are
)

// Delivery is one item's status with one destination ("trakt", "simkl").
type Delivery struct {
	ItemID    int64
	Sink      string
	Status    DeliveryStatus
	Error     string
	Attempts  int
	UpdatedAt time.Time
}
