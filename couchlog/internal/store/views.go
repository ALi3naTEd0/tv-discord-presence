package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Device is a TV, box or server as the dashboard shows it.
type Device struct {
	ID   int64
	Kind string // webos | androidtv | plex
	Name string
}

// Devices returns the user's devices by id.
func (s *Store) Devices(ctx context.Context, userID int64) (map[int64]Device, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, kind, name FROM devices WHERE user_id = ?", userID)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	out := map[int64]Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Kind, &d.Name); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		out[d.ID] = d
	}
	return out, rows.Err()
}

// SessionsSince returns the user's sessions that started at or after since,
// plus any still open, newest first.
func (s *Store) SessionsSince(ctx context.Context, userID int64, since time.Time) ([]*model.Session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+sessionCols+` FROM sessions
		WHERE user_id = ? AND (started_at >= ? OR state = 'open') ORDER BY started_at DESC`, userID, ms(since))
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	var out []*model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// OpenSessions returns the user's open sessions, newest first.
func (s *Store) OpenSessions(ctx context.Context, userID int64) ([]*model.Session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+sessionCols+` FROM sessions
		WHERE user_id = ? AND state = 'open' ORDER BY started_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	var out []*model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Deliveries returns one item's status with every destination.
func (s *Store) Deliveries(ctx context.Context, itemID int64) ([]model.Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sink, status, error, attempts, updated_at FROM deliveries
		WHERE item_id = ? ORDER BY sink`, itemID)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	var out []model.Delivery
	for rows.Next() {
		d := model.Delivery{ItemID: itemID}
		var status string
		var updated int64
		if err := rows.Scan(&d.Sink, &status, &d.Error, &d.Attempts, &updated); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		d.Status, d.UpdatedAt = model.DeliveryStatus(status), fromMS(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// WatchedEpisodes returns the (season, episode) pairs of a show in the
// user's history.
func (s *Store) WatchedEpisodes(ctx context.Context, userID int64, tmdb int) (map[[2]int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.season, i.episode FROM items i JOIN sessions s ON s.id = i.session_id
		WHERE s.user_id = ? AND i.tmdb = ? AND i.kind = 'episode' AND i.replaced_at IS NULL
		AND i.season IS NOT NULL AND i.episode IS NOT NULL`, userID, tmdb)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	out := map[[2]int]bool{}
	for rows.Next() {
		var se, ep int
		if err := rows.Scan(&se, &ep); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		out[[2]int{se, ep}] = true
	}
	return out, rows.Err()
}

// FirstSession is when the user's oldest session started, or nil.
func (s *Store) FirstSession(ctx context.Context, userID int64) (*time.Time, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(started_at) FROM sessions WHERE user_id = ?", userID).Scan(&v); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if !v.Valid {
		return nil, nil
	}
	t := fromMS(v.Int64)
	return &t, nil
}

// ServiceDevices returns, per app id, the devices it was watched on.
func (s *Store) ServiceDevices(ctx context.Context, userID int64) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT s.app_id, d.name FROM sessions s
		JOIN devices d ON d.id = s.device_id WHERE s.user_id = ? AND s.hidden = ''`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var app, dev string
		if err := rows.Scan(&app, &dev); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		out[app] = append(out[app], dev)
	}
	return out, rows.Err()
}
