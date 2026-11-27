// Package engine turns what a device reports (observations) into playback
// sessions. It applies tvpresence's rules unchanged: those rules are the
// reference the parallel run compares against.
//
// The database is the source of truth, not the tracker: the dashboard may
// close or split a session (next episode) at any moment, and the tracker
// picks that up on its next observation. Time comes from the observations,
// never from the wall clock, so every rule is testable with made-up times.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// Config holds the session rules. Zero fields take the defaults.
type Config struct {
	MinPlayed      time.Duration            // shorter unidentified sessions are dropped (120 s)
	MinPlayedByApp map[string]time.Duration // per-app minimum; under it kept hidden as Short (YouTube 20 min)
	PauseTimeout   time.Duration            // paused or in menus this long ends the session (30 min)
	CloseGrace     time.Duration            // no recorded app in front this long closes it (60 s)
	ResumeWindow   time.Duration            // after a restart, keep the open session if seen this recently (10 min)
	ReopenWithin   time.Duration            // same title back within this continues the session, where media ids are titles (6 h)
	MaxGap         time.Duration            // a longer gap between observations is not counted as playback (30 s)
	ExcludeApps    map[string]bool          // never recorded (music, Twitch)
}

// Defaults are tvpresence's values.
var Defaults = Config{
	MinPlayed: 120 * time.Second,
	MinPlayedByApp: map[string]time.Duration{
		"com.google.android.youtube.tv": 20 * time.Minute,
		"youtube.leanback.v4":           20 * time.Minute,
	},
	PauseTimeout: 30 * time.Minute,
	CloseGrace:   60 * time.Second,
	ResumeWindow: 10 * time.Minute,
	ReopenWithin: 6 * time.Hour,
	MaxGap:       30 * time.Second,
	// tvpresence's lists. Plex is excluded on the box because the Plex
	// source records it with exact IDs.
	ExcludeApps: map[string]bool{
		"tv.twitch.tv.starshot.lg":           true,
		"com.spotify.tv.android":             true,
		"tv.twitch.android.app":              true,
		"com.plexapp.android":                true,
		"com.google.android.youtube.tvmusic": true,
	},
}

func (c Config) withDefaults() Config {
	d := Defaults
	if c.MinPlayed > 0 {
		d.MinPlayed = c.MinPlayed
	}
	if c.MinPlayedByApp != nil {
		d.MinPlayedByApp = c.MinPlayedByApp
	}
	if c.PauseTimeout > 0 {
		d.PauseTimeout = c.PauseTimeout
	}
	if c.CloseGrace > 0 {
		d.CloseGrace = c.CloseGrace
	}
	if c.ResumeWindow > 0 {
		d.ResumeWindow = c.ResumeWindow
	}
	if c.ReopenWithin > 0 {
		d.ReopenWithin = c.ReopenWithin
	}
	if c.MaxGap > 0 {
		d.MaxGap = c.MaxGap
	}
	if c.ExcludeApps != nil {
		d.ExcludeApps = c.ExcludeApps
	}
	return d
}

// Device says how far the tracker can trust what a device reports.
type Device struct {
	ID int64
	// TitleMediaIDs: the media id is the published title (Android TV), so a
	// new one is new content even without passing through a menu, and coming
	// back to it within ReopenWithin continues the old session. On webOS the
	// media id also changes on credits and ads, so it only splits after the
	// app's menu.
	TitleMediaIDs bool
}

// Event is what the tracker did with an observation, for logs and the
// dashboard's live view.
type Event struct {
	Kind    EventKind
	Session int64
	Reason  string // why it closed
	Outcome store.CloseOutcome
}

// EventKind names a session change.
type EventKind string

const (
	Opened   EventKind = "opened"
	Resumed  EventKind = "resumed" // same title came back, or kept after a restart
	AutoNext EventKind = "next"    // the app autoplayed the next episode: split onto it
	Ended    EventKind = "closed"
)

// Tracker follows one device. It is not safe for concurrent use: feed it
// from the one goroutine that reads the device's source.
type Tracker struct {
	st  *store.Store
	dev Device
	cfg Config
	log *slog.Logger

	lastAt      time.Time // previous observation
	prevPlaying bool      // the previous observation was playing
	idleSince   time.Time // since when no recorded app is in front; zero if one is
	leftPlayer  bool      // the app is in front but its player is not (menu)
	hdr         string    // dynamic range playing now

	// OnEvent, if set, is called for every session change.
	OnEvent func(Event)

	startPos map[int64]time.Duration // session -> first real position seen in it

	lastMu   sync.Mutex
	lastObs  model.Observation
	lastSess *model.Session

	// Next, if set, returns the episode after an identified one
	// (identify.Identifier.Next). With it the tracker splits the session
	// when an app autoplays the next episode; Sinks get its deliveries.
	Next  func(ctx context.Context, cur model.Item) (model.Item, error)
	Sinks []string
}

// NewTracker starts following a device: open sessions left by a previous run
// are closed, except the latest when it was seen within ResumeWindow.
func NewTracker(ctx context.Context, st *store.Store, dev Device, cfg Config, now time.Time, log *slog.Logger) (*Tracker, error) {
	if log == nil {
		log = slog.Default()
	}
	t := &Tracker{st: st, dev: dev, cfg: cfg.withDefaults(), log: log.With("device", dev.ID)}
	closed, kept, err := st.Recover(ctx, dev.ID, t.cfg.ResumeWindow, now, t.cfg.MinPlayed, t.appMin)
	if err != nil {
		return nil, err
	}
	if closed > 0 {
		t.log.Info("closed sessions left open", "count", closed)
	}
	if kept != nil {
		t.log.Info("session resumed after restart", "session", kept.ID)
	}
	return t, nil
}

func (t *Tracker) appMin(appID string) time.Duration { return t.cfg.MinPlayedByApp[appID] }

func (t *Tracker) emit(e Event) {
	t.log.Info("session "+string(e.Kind), "session", e.Session, "reason", e.Reason)
	if t.OnEvent != nil {
		t.OnEvent(e)
	}
}

func (t *Tracker) close(ctx context.Context, s *model.Session, why string) error {
	out, err := t.st.CloseSession(ctx, s.ID, t.cfg.MinPlayed, t.appMin(s.App.ID))
	if err != nil {
		return err
	}
	t.prevPlaying, t.leftPlayer = false, false
	if out != store.NotOpen {
		t.emit(Event{Kind: Ended, Session: s.ID, Reason: why, Outcome: out})
	}
	return nil
}

// Observe applies one observation and returns the device's open session
// afterwards (nil if none).
func (t *Tracker) Observe(ctx context.Context, o model.Observation) (*model.Session, error) {
	sess, err := t.observe(ctx, o)
	t.lastMu.Lock()
	t.lastObs, t.lastSess = o, sess
	t.lastMu.Unlock()
	return sess, err
}

// Last is the latest observation and the open session after it (nil if
// none): what plays on the device now, for the Discord status.
func (t *Tracker) Last() (model.Observation, *model.Session) {
	t.lastMu.Lock()
	defer t.lastMu.Unlock()
	return t.lastObs, t.lastSess
}

func (t *Tracker) observe(ctx context.Context, o model.Observation) (*model.Session, error) {
	now := o.At
	var gap time.Duration
	if !t.lastAt.IsZero() {
		gap = now.Sub(t.lastAt)
	}
	t.lastAt = now
	sess, err := t.st.OpenSession(ctx, t.dev.ID)
	if err != nil {
		return nil, err
	}

	// No recorded app in front: tolerated for a while (app switches, drops).
	if o.State == model.StateOff || o.App.ID == "" || t.cfg.ExcludeApps[o.App.ID] {
		t.prevPlaying = false
		t.leftPlayer = sess != nil // leaving the app also leaves the player
		if t.idleSince.IsZero() {
			t.idleSince = now
		}
		if sess != nil && now.Sub(t.idleSince) > t.cfg.CloseGrace {
			return nil, t.close(ctx, sess, "no streaming app")
		}
		return sess, nil
	}
	t.idleSince = time.Time{}

	playing := o.State == model.StatePlaying
	if sess != nil && sess.App.ID != o.App.ID {
		if err := t.close(ctx, sess, "switched to "+o.App.Name); err != nil {
			return nil, err
		}
		sess = nil
	}
	// New content in the same app. Only after going back to the menu, unless
	// the media id is a title: credits, ads and autoplay change webOS's id
	// without being new content (autoplay has its own split, "next episode").
	if sess != nil && o.MediaID != "" && o.MediaID != sess.MediaID {
		if sess.MediaID != "" && (t.leftPlayer || t.dev.TitleMediaIDs) {
			if err := t.close(ctx, sess, "new content"); err != nil {
				return nil, err
			}
			sess = nil
		} else if err := t.st.SetMedia(ctx, sess.ID, o.MediaID); err != nil {
			return nil, err
		}
	}
	if o.MediaID != "" {
		t.leftPlayer = false
	} else if sess != nil {
		t.leftPlayer = true
	}

	if sess == nil {
		if !playing {
			return nil, nil
		}
		if t.dev.TitleMediaIDs && o.MediaID != "" {
			if sess, err = t.st.ReopenRecent(ctx, t.dev.ID, o.App.ID, o.MediaID, t.cfg.ReopenWithin, now); err != nil {
				return nil, err
			}
			if sess != nil {
				t.emit(Event{Kind: Resumed, Session: sess.ID, Reason: "same title"})
			}
		}
		if sess == nil {
			if sess, err = t.st.StartSession(ctx, t.dev.ID, o.App, o.MediaID, now); err != nil {
				return nil, err
			}
			t.emit(Event{Kind: Opened, Session: sess.ID})
		}
		t.prevPlaying = false // the interval before this one was not playback
	}

	if o.Hint.Title != "" && sess.Hint.Title == "" {
		if err := t.st.SetHint(ctx, sess.ID, o.Hint); err != nil {
			return nil, err
		}
	}
	if o.Position != nil {
		if t.startPos == nil {
			t.startPos = map[int64]time.Duration{}
		}
		if _, ok := t.startPos[sess.ID]; !ok {
			t.startPos[sess.ID] = *o.Position // where this session picked the title up
		}
	}
	if playing && o.Position != nil && sess.LivePosition != nil && t.Next != nil {
		if next := t.autoplay(ctx, sess, *sess.LivePosition, *o.Position, now); next != nil {
			sess = next
		}
	}
	if o.Position != nil {
		if err := t.st.SetPosition(ctx, sess.ID, *o.Position); err != nil {
			return nil, err
		}
	}
	if o.Video != nil {
		if o.Video.Width > 0 {
			if err := t.st.SetVideo(ctx, sess.ID, *o.Video); err != nil {
				return nil, err
			}
		}
		if o.Video.HDR != "" {
			t.hdr = o.Video.HDR
		}
	}

	if playing {
		var add time.Duration
		if t.prevPlaying && gap > 0 && gap <= t.cfg.MaxGap {
			add = gap
		}
		if err := t.st.Touch(ctx, sess.ID, now, add, true); err != nil {
			return nil, err
		}
		if add > 0 && t.hdr != "" {
			if err := t.st.AddHDRTime(ctx, sess.ID, t.hdr, add); err != nil {
				return nil, err
			}
		}
	} else if now.Sub(sess.LastPlayAt) > t.cfg.PauseTimeout {
		return nil, t.close(ctx, sess, "inactive")
	} else if err := t.st.Touch(ctx, sess.ID, now, 0, false); err != nil {
		return nil, err
	}
	t.prevPlaying = playing
	return t.st.Session(ctx, sess.ID)
}

// autoplay splits the session onto the next episode when the app moved on
// by itself, which only a real position shows (Android TV): the identified
// episode was past 80 % of its runtime and playback restarted near zero.
// It returns the new session, or nil when nothing changed. Failures (TMDB
// down) are logged and leave the session as it is; the dashboard's "next
// episode" button does the same by hand.
func (t *Tracker) autoplay(ctx context.Context, sess *model.Session, prev, pos time.Duration, now time.Time) *model.Session {
	if pos >= 5*time.Minute || prev-pos < 10*time.Minute {
		return nil
	}
	items, err := t.st.Items(ctx, sess.ID)
	if err != nil || len(items) == 0 {
		return nil
	}
	cur := items[len(items)-1]
	runtime := time.Duration(cur.RuntimeMinutes) * time.Minute
	if cur.Kind != model.KindEpisode || runtime == 0 || cur.RuntimeEstimated || prev < runtime*8/10 {
		return nil
	}
	// Real watching, not a skip to the end: Netflix can jump back to an
	// earlier episode while you are away, and seeking that one to its end
	// must not look like this episode finishing.
	// Picking the episode up again counts: the position went from where this
	// session started to the end by playing, not by seeking.
	continuous := prev-t.startPos[sess.ID] <= sess.Played*5/4+time.Minute
	if sess.Played < runtime/2 && !continuous {
		t.log.Info("position jumped back but little was played: not the next episode",
			"session", sess.ID, "from", prev, "to", pos, "played", sess.Played)
		return nil
	}
	next, err := t.Next(ctx, cur)
	if err != nil {
		t.log.Warn("autoplay: no next episode", "session", sess.ID, "err", err)
		return nil
	}
	ns, err := t.st.SplitNext(ctx, sess.ID, next, t.Sinks, now)
	if err != nil {
		t.log.Warn("autoplay: split failed", "session", sess.ID, "err", err)
		return nil
	}
	t.emit(Event{Kind: AutoNext, Session: ns.ID, Reason: fmt.Sprintf("S%dE%d", next.Season, next.Episode)})
	return ns
}
