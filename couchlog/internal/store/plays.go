package store

import (
	"context"
	"fmt"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/source"
)

// AddPlay records a viewing a source already recorded and identified (Plex):
// a closed session with its item and a pending delivery per sink. A play
// seen before (same SourceKey) is skipped. It reports whether it was new.
// A play without any external ID is kept unidentified, for the picker.
func (s *Store) AddPlay(ctx context.Context, p source.Play, appID, appName string, sinks []string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	var userID int64
	if err := tx.QueryRowContext(ctx, "SELECT user_id FROM devices WHERE id = ?", p.DeviceID).Scan(&userID); err != nil {
		return false, fmt.Errorf("store: device %d: %w", p.DeviceID, err)
	}
	played := p.EndedAt.Sub(p.StartedAt)
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sessions(user_id, device_id, source_key, app_id, app_name,
		started_at, last_seen_at, last_play_at, ended_at, played_ms, state, hint_title, hint_season, hint_episode)
		VALUES (?,?,?,?,?,?,?,?,?,?, 'closed', ?,?,?)`,
		userID, p.DeviceID, p.SourceKey, appID, appName, ms(p.StartedAt), ms(p.EndedAt), ms(p.EndedAt), ms(p.EndedAt),
		played.Milliseconds(), p.Identity.Title, nullInt(p.Identity.Season), nullInt(p.Identity.Episode))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	sid, _ := res.LastInsertId()
	id := p.Identity
	if id.Title != "" && (id.TMDB != 0 || id.IMDb != "" || id.TVDB != 0) {
		it := modelItem(p)
		if err := identifyTx(ctx, tx, sid, it, sinks, now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func modelItem(p source.Play) []model.Item {
	return []model.Item{{Identity: p.Identity, Poster: p.Poster}}
}
