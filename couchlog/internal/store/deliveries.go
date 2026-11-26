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
