package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Watch is a closed session with its current items and the kind of device
// it came from (Plex plays are sent as they are; TV sessions only when
// enough was watched).
type Watch struct {
	Session    *model.Session
	DeviceKind string
	Items      []model.Item
}

// Watches lists closed, shown sessions with items that started after since
// and ended before endedBefore, oldest first.
func (s *Store) Watches(ctx context.Context, since, endedBefore time.Time) ([]Watch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed("s", sessionCols)+`, d.kind FROM sessions s
		JOIN devices d ON d.id = s.device_id
		WHERE s.state = 'closed' AND s.hidden = '' AND s.started_at > ? AND s.ended_at < ?
		AND EXISTS (SELECT 1 FROM items WHERE session_id = s.id AND replaced_at IS NULL)
		ORDER BY s.started_at`, ms(since), ms(endedBefore))
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	var out []Watch
	for rows.Next() {
		var kind string
		sess, err := scanSession(scanWith{rows, &kind})
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Watch{Session: sess, DeviceKind: kind})
	}
	rows.Close()
	for i := range out {
		its, err := s.Items(ctx, out[i].Session.ID)
		if err != nil {
			return nil, err
		}
		out[i].Items = its
	}
	return out, nil
}

// Replaced lists items a correction replaced whose delivery to sink is
// still "remove", with their sessions (for when they were watched).
func (s *Store) Replaced(ctx context.Context, sink string) ([]Watch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.session_id, i.id FROM items i
		JOIN deliveries d ON d.item_id = i.id
		WHERE d.sink = ? AND d.status = 'remove' AND i.replaced_at IS NOT NULL ORDER BY i.id`, sink)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	type pair struct{ sid, iid int64 }
	var ps []pair
	for rows.Next() {
		var p pair
		rows.Scan(&p.sid, &p.iid)
		ps = append(ps, p)
	}
	rows.Close()
	var out []Watch
	for _, p := range ps {
		sess, err := s.Session(ctx, p.sid)
		if err != nil {
			return nil, err
		}
		it, err := s.item(ctx, p.iid)
		if err != nil {
			return nil, err
		}
		out = append(out, Watch{Session: sess, Items: []model.Item{it}})
	}
	return out, nil
}

func (s *Store) item(ctx context.Context, id int64) (model.Item, error) {
	var (
		it                                         model.Item
		kind                                       string
		year, season, episode, tmdb, tvdb, runtime sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, session_id, kind, title, year, season, episode, episode_title,
		tmdb, tvdb, imdb, poster, runtime_min, runtime_estimated FROM items WHERE id = ?`, id).Scan(
		&it.ID, &it.SessionID, &kind, &it.Title, &year, &season, &episode, &it.EpisodeTitle,
		&tmdb, &tvdb, &it.IMDb, &it.Poster, &runtime, &it.RuntimeEstimated)
	if err != nil {
		return it, fmt.Errorf("store: item %d: %w", id, err)
	}
	it.Kind = model.ItemKind(kind)
	it.Year, it.Season, it.Episode = int(year.Int64), int(season.Int64), int(episode.Int64)
	it.TMDB, it.TVDB, it.RuntimeMinutes = int(tmdb.Int64), int(tvdb.Int64), int(runtime.Int64)
	return it, nil
}

// SetDelivery records what a destination said about an item. A failure
// counts an attempt; anything else resets them.
func (s *Store) SetDelivery(ctx context.Context, itemID int64, sink string, st model.DeliveryStatus, msg string, now time.Time) error {
	return s.exec(ctx, `UPDATE deliveries SET status = ?, error = ?, updated_at = ?,
		attempts = CASE WHEN ? = 'error' THEN attempts + 1 ELSE 0 END WHERE item_id = ? AND sink = ?`,
		string(st), msg, ms(now), string(st), itemID, sink)
}

// SetDeliveryNote leaves the status as it is and says why it waits.
func (s *Store) SetDeliveryNote(ctx context.Context, itemID int64, sink, msg string, now time.Time) error {
	return s.exec(ctx, "UPDATE deliveries SET error = ?, updated_at = ? WHERE item_id = ? AND sink = ? AND error <> ?",
		msg, ms(now), itemID, sink, msg)
}

// scanWith scans a session row followed by extra columns.
type scanWith struct {
	r     rowScanner
	extra *string
}

func (w scanWith) Scan(dest ...any) error { return w.r.Scan(append(dest, w.extra)...) }

func prefixed(alias, cols string) string {
	out := ""
	for i, c := range splitCols(cols) {
		if i > 0 {
			out += ", "
		}
		out += alias + "." + c
	}
	return out
}

func splitCols(cols string) []string {
	var out []string
	cur := ""
	for _, r := range cols {
		switch r {
		case ',':
			out = append(out, cur)
			cur = ""
		case ' ', '\n', '\t':
		default:
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// SeenShortlyBefore finds an earlier session, ended within window before
// this one started, whose viewing of one of these titles (same movie, or
// same show and episode, by external ID) already reached a destination:
// watching it again that soon is usually the same viewing, so it waits for
// you to confirm it. A start that was never sent does not count.
func (s *Store) SeenShortlyBefore(ctx context.Context, w Watch, window time.Duration) (*model.Session, error) {
	for _, it := range w.Items {
		if it.TMDB == 0 && it.IMDb == "" {
			continue
		}
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT s.id FROM items i JOIN sessions s ON s.id = i.session_id
			WHERE s.id <> ? AND s.hidden <> 'ignored' AND i.replaced_at IS NULL AND i.kind = ?
			AND EXISTS (SELECT 1 FROM deliveries d WHERE d.item_id = i.id AND d.status = 'added')
			AND COALESCE(i.season, -1) = ? AND COALESCE(i.episode, -1) = ?
			AND ((i.tmdb IS NOT NULL AND i.tmdb = ?) OR (i.imdb <> '' AND i.imdb = ?))
			AND s.ended_at <= ? AND s.ended_at >= ? ORDER BY s.ended_at DESC LIMIT 1`,
			w.Session.ID, string(it.Kind), seasonOrNone(it), episodeOrNone(it), it.TMDB, it.IMDb,
			ms(w.Session.StartedAt.Add(time.Minute)), ms(w.Session.StartedAt.Add(-window))).Scan(&id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		return s.Session(ctx, id)
	}
	return nil, nil
}

func seasonOrNone(it model.Item) int {
	if it.Kind == model.KindMovie {
		return -1
	}
	return it.Season
}

func episodeOrNone(it model.Item) int {
	if it.Kind == model.KindMovie {
		return -1
	}
	return it.Episode
}

// Part is an earlier, unsent session of the same single title.
type Part struct {
	SessionID, ItemID int64
	Position          time.Duration
}

// Parts lists the earlier sessions (same app, within window) holding only
// the same title as w, not sent anywhere: an episode picked up again over
// several sessions is one viewing.
func (s *Store) Parts(ctx context.Context, w Watch, window time.Duration) ([]Part, error) {
	if len(w.Items) != 1 {
		return nil, nil
	}
	it := w.Items[0]
	if it.TMDB == 0 && it.IMDb == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, i.id, COALESCE(s.position_ms, 0) FROM sessions s
		JOIN items i ON i.session_id = s.id AND i.replaced_at IS NULL
		WHERE s.id <> ? AND s.app_id = ? AND s.hidden <> 'ignored' AND s.started_at < ? AND s.started_at > ?
		AND i.kind = ? AND COALESCE(i.season, -1) = ? AND COALESCE(i.episode, -1) = ?
		AND ((i.tmdb IS NOT NULL AND i.tmdb = ?) OR (i.imdb <> '' AND i.imdb = ?))
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.item_id = i.id AND d.status IN ('added', 'skipped'))
		AND (SELECT COUNT(*) FROM items WHERE session_id = s.id AND replaced_at IS NULL) = 1`,
		w.Session.ID, w.Session.App.ID, ms(w.Session.StartedAt), ms(w.Session.StartedAt.Add(-window)),
		string(it.Kind), seasonOrNone(it), episodeOrNone(it), it.TMDB, it.IMDb)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		var pos int64
		if err := rows.Scan(&p.SessionID, &p.ItemID, &pos); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		p.Position = time.Duration(pos) * time.Millisecond
		out = append(out, p)
	}
	return out, rows.Err()
}

// EnsureDeliveries gives every current item of the sessions started after
// since a pending delivery for each sink it has none for: items recorded
// before a destination was set up (Plex imported first, Trakt added later)
// would otherwise never be checked or sent. It returns how many it added.
func (s *Store) EnsureDeliveries(ctx context.Context, sinks []string, since, now time.Time) (int64, error) {
	var total int64
	for _, sk := range sinks {
		res, err := s.db.ExecContext(ctx, `INSERT INTO deliveries(item_id, sink, status, updated_at)
			SELECT i.id, ?, 'pending', ? FROM items i JOIN sessions s ON s.id = i.session_id
			WHERE i.replaced_at IS NULL AND s.started_at > ?
			AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.item_id = i.id AND d.sink = ?)`,
			sk, ms(now), ms(since), sk)
		if err != nil {
			return total, fmt.Errorf("store: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
