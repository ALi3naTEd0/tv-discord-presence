// Package compare puts the Go and Python histories side by side for the
// parallel run: the same TVs recorded by both, session by session, so any
// difference in the rules shows before tvpresence is retired.
package compare

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Session is one session from either side, reduced to what is compared.
type Session struct {
	Source  string // tv | androidtv | plex (tvpresence's names)
	Key     string // source key (matches Plex plays)
	App     string
	Start   time.Time
	End     time.Time // zero while open
	Played  time.Duration
	Hidden  bool // hidden as short
	Open    bool
	Title   string // "Ozark S1E2", "" if unidentified
	Comment string
}

// Pair is a matched session, or one side alone.
type Pair struct {
	Py, Go *Session
}

// Report is the comparison.
type Report struct {
	Since   time.Time
	GoSince time.Time // when Couchlog started listening to the TVs
	Pairs   []Pair
}

// Run reads both databases (read-only) from since on and matches them.
// Plex plays match by source key; TV sessions by device, app and a start
// within two minutes.
func Run(ctx context.Context, couchlogDB, legacyDB string, since time.Time) (*Report, error) {
	py, err := readLegacy(ctx, legacyDB, since)
	if err != nil {
		return nil, fmt.Errorf("tvpresence: %w", err)
	}
	gs, goSince, err := readCouchlog(ctx, couchlogDB, since)
	if err != nil {
		return nil, fmt.Errorf("couchlog: %w", err)
	}
	// TVs are only compared from when Couchlog started listening to them
	// (plus the match window, for a session already running then); Plex is
	// imported backwards, so all of it counts.
	var kept []*Session
	for _, p := range py {
		if p.Source == "plex" || goSince.IsZero() || !p.Start.Before(goSince.Add(-MatchWindow)) {
			kept = append(kept, p)
		}
	}
	return &Report{Since: since, GoSince: goSince, Pairs: match(kept, gs)}, nil
}

func open(path string) (*sql.DB, error) {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, db.PingContext(context.Background())
}

func label(title string, season, episode sql.NullInt64) string {
	if title == "" {
		return ""
	}
	if season.Valid && episode.Valid {
		return fmt.Sprintf("%s S%dE%d", title, season.Int64, episode.Int64)
	}
	return title
}

func readLegacy(ctx context.Context, path string, since time.Time) ([]*Session, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT s.source, COALESCE(s.source_key, ''), s.app_id, s.started_at,
		COALESCE(s.ended_at, 0), s.played_seconds, s.auto_hidden, s.state,
		COALESCE((SELECT title FROM items WHERE session_id = s.id ORDER BY id LIMIT 1), ''),
		(SELECT season FROM items WHERE session_id = s.id ORDER BY id LIMIT 1),
		(SELECT episode FROM items WHERE session_id = s.id ORDER BY id LIMIT 1)
		FROM sessions s WHERE s.started_at >= ? AND s.ignored = 0 ORDER BY s.started_at`, float64(since.Unix()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var s Session
		var start, end, played float64
		var hidden int
		var state, title string
		var season, ep sql.NullInt64
		if err := rows.Scan(&s.Source, &s.Key, &s.App, &start, &end, &played, &hidden, &state, &title, &season, &ep); err != nil {
			return nil, err
		}
		s.Start = secs(start)
		if end > 0 {
			s.End = secs(end)
		}
		s.Played = time.Duration(played * float64(time.Second))
		s.Hidden, s.Open, s.Title = hidden != 0, state == "open", label(title, season, ep)
		out = append(out, &s)
	}
	return out, rows.Err()
}

func secs(f float64) time.Time {
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9))
}

var kindToSource = map[string]string{"webos": "tv", "androidtv": "androidtv", "plex": "plex"}

func readCouchlog(ctx context.Context, path string, since time.Time) ([]*Session, time.Time, error) {
	db, err := open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer db.Close()
	var goSince time.Time
	var first sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT MIN(created_at) FROM devices WHERE kind <> 'plex'").Scan(&first); err != nil {
		return nil, goSince, err
	}
	if first.Valid {
		goSince = time.UnixMilli(first.Int64)
	}
	out, err := readCouchlogSessions(ctx, db, since)
	return out, goSince, err
}

func readCouchlogSessions(ctx context.Context, db *sql.DB, since time.Time) ([]*Session, error) {
	// Sessions imported from tvpresence are tvpresence's own: not compared.
	rows, err := db.QueryContext(ctx, `SELECT d.kind, s.source_key, s.app_id, s.started_at, COALESCE(s.ended_at, 0),
		s.played_ms, s.hidden, s.state,
		COALESCE((SELECT title FROM items WHERE session_id = s.id AND replaced_at IS NULL ORDER BY id LIMIT 1), ''),
		(SELECT season FROM items WHERE session_id = s.id AND replaced_at IS NULL ORDER BY id LIMIT 1),
		(SELECT episode FROM items WHERE session_id = s.id AND replaced_at IS NULL ORDER BY id LIMIT 1)
		FROM sessions s JOIN devices d ON d.id = s.device_id
		WHERE s.started_at >= ? AND s.hidden <> 'ignored' AND s.source_key NOT LIKE 'tvpresence:%'
		ORDER BY s.started_at`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var s Session
		var kind, hidden, state, title string
		var start, end, played int64
		var season, ep sql.NullInt64
		if err := rows.Scan(&kind, &s.Key, &s.App, &start, &end, &played, &hidden, &state, &title, &season, &ep); err != nil {
			return nil, err
		}
		s.Source = kindToSource[kind]
		s.Start = time.UnixMilli(start)
		if end > 0 {
			s.End = time.UnixMilli(end)
		}
		s.Played = time.Duration(played) * time.Millisecond
		s.Hidden, s.Open, s.Title = hidden == "short", state == "open", label(title, season, ep)
		out = append(out, &s)
	}
	return out, rows.Err()
}

// MatchWindow is how far apart two starts may be and still be one session.
const MatchWindow = 2 * time.Minute

func match(py, gs []*Session) []Pair {
	used := map[*Session]bool{}
	var pairs []Pair
	for _, p := range py {
		var best *Session
		bestGap := MatchWindow + 1
		for _, g := range gs {
			if used[g] || g.Source != p.Source {
				continue
			}
			if p.Source == "plex" {
				if g.Key == p.Key {
					best = g
					break
				}
				continue
			}
			if g.App != p.App {
				continue
			}
			gap := g.Start.Sub(p.Start)
			if gap < 0 {
				gap = -gap
			}
			if gap <= MatchWindow && gap < bestGap {
				best, bestGap = g, gap
			}
		}
		if best != nil {
			used[best] = true
		}
		pairs = append(pairs, Pair{Py: p, Go: best})
	}
	for _, g := range gs {
		if !used[g] {
			pairs = append(pairs, Pair{Go: g})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].start().Before(pairs[j].start()) })
	return pairs
}

func (p Pair) start() time.Time {
	if p.Py != nil {
		return p.Py.Start
	}
	return p.Go.Start
}

// PlayedTolerance is how much the played time may differ without being
// called a difference (polls are 5 s apart and start a few seconds apart).
const PlayedTolerance = 30 * time.Second

// Differs reports what is different in a matched pair ("" when they agree).
func (p Pair) Differs() string {
	switch {
	case p.Py == nil:
		return "only Go"
	case p.Go == nil:
		return "only Python"
	}
	var d []string
	tol := PlayedTolerance
	if p.Py.Source == "plex" {
		// A Plex play's length is the title's runtime, which Plex's provider
		// and TMDB round differently by a minute or two: not a rule.
		tol = 2 * time.Minute
	}
	if diff := p.Go.Played - p.Py.Played; diff > tol || diff < -tol {
		d = append(d, fmt.Sprintf("played %+ds", int(diff.Seconds())))
	}
	if p.Py.Hidden != p.Go.Hidden {
		d = append(d, "hidden")
	}
	if p.Py.Open != p.Go.Open {
		d = append(d, "open")
	}
	if p.Py.Title != p.Go.Title {
		d = append(d, "title")
	}
	return strings.Join(d, ", ")
}

// Summary counts the outcome.
func (r *Report) Summary() (same, differ, onlyPy, onlyGo int) {
	for _, p := range r.Pairs {
		switch {
		case p.Py == nil:
			onlyGo++
		case p.Go == nil:
			onlyPy++
		case p.Differs() == "":
			same++
		default:
			differ++
		}
	}
	return
}

func dur(d time.Duration) string {
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func side(s *Session) string {
	if s == nil {
		return "—"
	}
	t := s.Title
	if t == "" {
		t = "(unidentified)"
	}
	flags := ""
	if s.Hidden {
		flags += " [short]"
	}
	if s.Open {
		flags += " [open]"
	}
	return fmt.Sprintf("%s %s%s", dur(s.Played), t, flags)
}

// Write prints the report as a plain table.
func (r *Report) Write(w io.Writer, onlyDiffs bool) {
	same, differ, onlyPy, onlyGo := r.Summary()
	since := r.Since
	if r.GoSince.After(since) {
		since = r.GoSince
	}
	fmt.Fprintf(w, "TVs since %s, Plex since %s: %d match, %d differ, %d only in Python, %d only in Go\n\n",
		since.Format("2006-01-02 15:04"), r.Since.Format("2006-01-02 15:04"), same, differ, onlyPy, onlyGo)
	for _, p := range r.Pairs {
		d := p.Differs()
		if onlyDiffs && d == "" {
			continue
		}
		s := p.Py
		if s == nil {
			s = p.Go
		}
		mark := "  ="
		if d != "" {
			mark = "  ≠"
		}
		fmt.Fprintf(w, "%s %s %-9s %-30s\n      Python: %s\n      Go:     %s\n", mark, s.Start.Format("01-02 15:04"),
			s.Source, s.App, side(p.Py), side(p.Go))
		if d != "" {
			fmt.Fprintf(w, "      → %s\n", d)
		}
	}
}
