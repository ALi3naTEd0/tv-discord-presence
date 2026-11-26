package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// ErrNoSession means the session does not exist (or is not open, for calls
// that only touch open sessions).
var ErrNoSession = errors.New("store: no such session")

const sessionCols = `id, user_id, device_id, source_key, app_id, app_name, started_at, last_seen_at,
	last_play_at, ended_at, played_ms, state, hidden, media_id, hint_title, hint_extra,
	hint_season, hint_episode, position_ms, live_position_ms, video_codec, video_width,
	video_height, video_hdr, video_hdr_ms, auto_next_from`

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(r rowScanner) (*model.Session, error) {
	var (
		s                                    model.Session
		started, seen, lastPlay, playedMS    int64
		ended, hintSeason, hintEp, pos, live sql.NullInt64
		width, height, autoNext              sql.NullInt64
		state, hidden, codec, hdr, hdrJSON   string
	)
	err := r.Scan(&s.ID, &s.UserID, &s.DeviceID, &s.SourceKey, &s.App.ID, &s.App.Name,
		&started, &seen, &lastPlay, &ended, &playedMS, &state, &hidden, &s.MediaID,
		&s.Hint.Title, &s.Hint.Subtitle, &hintSeason, &hintEp, &pos, &live, &codec,
		&width, &height, &hdr, &hdrJSON, &autoNext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	s.StartedAt, s.LastSeenAt, s.LastPlayAt = fromMS(started), fromMS(seen), fromMS(lastPlay)
	if ended.Valid {
		t := fromMS(ended.Int64)
		s.EndedAt = &t
	}
	s.Played = time.Duration(playedMS) * time.Millisecond
	s.Open = state == "open"
	s.Hidden = model.Hidden(hidden)
	s.Hint.Season, s.Hint.Episode = int(hintSeason.Int64), int(hintEp.Int64)
	s.Position, s.LivePosition = msPtr(pos), msPtr(live)
	if codec != "" || width.Valid || hdr != "" {
		s.Video = &model.Video{Codec: codec, Width: int(width.Int64), Height: int(height.Int64), HDR: hdr}
	}
	if hdrJSON != "" && hdrJSON != "{}" {
		var m map[string]int64
		if json.Unmarshal([]byte(hdrJSON), &m) == nil {
			s.HDRTime = make(map[string]time.Duration, len(m))
			for k, v := range m {
				s.HDRTime[k] = time.Duration(v) * time.Millisecond
			}
		}
	}
	if autoNext.Valid {
		s.AutoNextFrom = &autoNext.Int64
	}
	return &s, nil
}

func fromMS(v int64) time.Time { return time.UnixMilli(v) }

func msPtr(v sql.NullInt64) *time.Duration {
	if !v.Valid {
		return nil
	}
	d := time.Duration(v.Int64) * time.Millisecond
	return &d
}

// Session returns one session by id.
func (s *Store) Session(ctx context.Context, id int64) (*model.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, "SELECT "+sessionCols+" FROM sessions WHERE id = ?", id))
}

// OpenSession returns the device's open session, or nil if it has none.
func (s *Store) OpenSession(ctx context.Context, deviceID int64) (*model.Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE device_id = ? AND state = 'open' ORDER BY id DESC LIMIT 1", deviceID))
	if errors.Is(err, ErrNoSession) {
		return nil, nil
	}
	return sess, err
}

// StartSession opens a new session for a device that started playing.
func (s *Store) StartSession(ctx context.Context, deviceID int64, app model.App, mediaID string, now time.Time) (*model.Session, error) {
	var userID int64
	if err := s.db.QueryRowContext(ctx, "SELECT user_id FROM devices WHERE id = ?", deviceID).Scan(&userID); err != nil {
		return nil, fmt.Errorf("store: device %d: %w", deviceID, err)
	}
	t := ms(now)
	// The key only has to be unique; device + start time is, and a second
	// session in the same millisecond on one device gets a suffix.
	key := fmt.Sprintf("live:%d:%d", deviceID, t)
	for i := 1; ; i++ {
		res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO sessions(user_id, device_id, source_key, app_id, app_name,
			started_at, last_seen_at, last_play_at, media_id) VALUES (?,?,?,?,?,?,?,?,?)`,
			userID, deviceID, key, app.ID, app.Name, t, t, t, mediaID)
		if err != nil {
			return nil, fmt.Errorf("store: start session: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			id, _ := res.LastInsertId()
			return s.Session(ctx, id)
		}
		key = fmt.Sprintf("live:%d:%d-%d", deviceID, t, i)
	}
}

// Touch records that the open session was seen at now; when playing, it also
// moves last_play_at and adds the played time. A session closed meanwhile
// (the dashboard split it) is left alone.
func (s *Store) Touch(ctx context.Context, id int64, now time.Time, played time.Duration, playing bool) error {
	var err error
	if playing {
		_, err = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, last_play_at = ?,
			played_ms = played_ms + ? WHERE id = ? AND state = 'open'`, ms(now), ms(now), played.Milliseconds(), id)
	} else {
		_, err = s.db.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ? WHERE id = ? AND state = 'open'", ms(now), id)
	}
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// SetMedia records the device's content id for the open session.
func (s *Store) SetMedia(ctx context.Context, id int64, mediaID string) error {
	return s.exec(ctx, "UPDATE sessions SET media_id = ? WHERE id = ? AND state = 'open'", mediaID, id)
}

// SetHint records what the device says is playing; it pre-fills identification.
func (s *Store) SetHint(ctx context.Context, id int64, h model.Hint) error {
	return s.exec(ctx, `UPDATE sessions SET hint_title = ?, hint_extra = ?, hint_season = ?, hint_episode = ?
		WHERE id = ? AND state = 'open'`, h.Title, h.Subtitle, nullInt(h.Season), nullInt(h.Episode), id)
}

// SetPosition records a real playback position: the live one, and the
// furthest reached (players jump back to 0 when a video ends).
func (s *Store) SetPosition(ctx context.Context, id int64, pos time.Duration) error {
	p := pos.Milliseconds()
	return s.exec(ctx, `UPDATE sessions SET position_ms = MAX(COALESCE(position_ms, 0), ?), live_position_ms = ?
		WHERE id = ? AND state = 'open'`, p, p, id)
}

// SetVideo records the decoded format, keeping the widest seen (the highest
// resolution: a low-resolution start of an adaptive stream does not win).
func (s *Store) SetVideo(ctx context.Context, id int64, v model.Video) error {
	return s.exec(ctx, `UPDATE sessions SET video_codec = ?, video_width = ?, video_height = ?
		WHERE id = ? AND COALESCE(video_width, 0) <= ?`, v.Codec, v.Width, v.Height, id, v.Width)
}

// AddHDRTime adds played time to one dynamic range and sets video_hdr to the
// range that has played longest, so an SDR trailer before an HDR film does
// not decide what the film was.
func (s *Store) AddHDRTime(ctx context.Context, id int64, hdr string, d time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT video_hdr_ms FROM sessions WHERE id = ?", id).Scan(&raw); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	m := map[string]int64{}
	_ = json.Unmarshal([]byte(raw), &m)
	m[hdr] += d.Milliseconds()
	top := ""
	for k, v := range m {
		if top == "" || v > m[top] || (v == m[top] && k < top) {
			top = k
		}
	}
	b, _ := json.Marshal(m)
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET video_hdr = ?, video_hdr_ms = ? WHERE id = ?", top, string(b), id); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return tx.Commit()
}

// CloseOutcome says what closing a session did.
type CloseOutcome int

const (
	NotOpen  CloseOutcome = iota // it was already closed
	Closed                       // closed and kept
	Dropped                      // under the minimum and unidentified: deleted
	HidShort                     // under the app's own minimum: kept hidden as short
)

// CloseSession closes an open session at its last_seen_at. An unidentified
// session that played less than minPlayed is deleted (trailers, menu
// autoplay); one under its app's own minimum (appMin > 0: YouTube's 20 min)
// is kept hidden as Short, because it may still be a short film.
func (s *Store) CloseSession(ctx context.Context, id int64, minPlayed, appMin time.Duration) (CloseOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NotOpen, fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	var playedMS, seen int64
	var items int
	err = tx.QueryRowContext(ctx, `SELECT played_ms, last_seen_at,
		(SELECT COUNT(*) FROM items WHERE session_id = sessions.id AND replaced_at IS NULL)
		FROM sessions WHERE id = ? AND state = 'open'`, id).Scan(&playedMS, &seen, &items)
	if errors.Is(err, sql.ErrNoRows) {
		return NotOpen, nil
	}
	if err != nil {
		return NotOpen, fmt.Errorf("store: %w", err)
	}
	played := time.Duration(playedMS) * time.Millisecond
	out, hidden := Closed, ""
	if items == 0 {
		switch {
		case played < minPlayed:
			if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id); err != nil {
				return NotOpen, fmt.Errorf("store: %w", err)
			}
			return Dropped, tx.Commit()
		case appMin > 0 && played < appMin:
			out, hidden = HidShort, string(model.Short)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = 'closed', ended_at = last_seen_at,
		live_position_ms = NULL, hidden = CASE WHEN ? <> '' THEN ? ELSE hidden END WHERE id = ?`,
		hidden, hidden, id); err != nil {
		return NotOpen, fmt.Errorf("store: %w", err)
	}
	return out, tx.Commit()
}

// ReopenRecent continues the device's last closed session in this app with
// this media id if it ended less than within ago: coming back to the same
// title (Android TV publishes it) after a pause or another app. Ignored
// sessions stay closed. Returns nil when there is none.
func (s *Store) ReopenRecent(ctx context.Context, deviceID int64, appID, mediaID string, within time.Duration, now time.Time) (*model.Session, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE device_id = ? AND state = 'closed'
		AND app_id = ? AND media_id = ? AND hidden <> 'ignored' AND ended_at > ?
		ORDER BY ended_at DESC LIMIT 1`, deviceID, appID, mediaID, ms(now.Add(-within))).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := s.exec(ctx, `UPDATE sessions SET state = 'open', ended_at = NULL, hidden = '', last_seen_at = ?
		WHERE id = ?`, ms(now), id); err != nil {
		return nil, err
	}
	return s.Session(ctx, id)
}

// Recover runs when a device's tracker starts: the device's most recent open
// session is kept if it was seen within resumeWindow (the next observation
// decides whether it goes on), every other open one is closed. It returns
// how many were closed and the kept session, if any.
func (s *Store) Recover(ctx context.Context, deviceID int64, resumeWindow time.Duration, now time.Time,
	minPlayed time.Duration, appMin func(appID string) time.Duration) (int, *model.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, app_id, last_seen_at FROM sessions
		WHERE device_id = ? AND state = 'open' ORDER BY id DESC`, deviceID)
	if err != nil {
		return 0, nil, fmt.Errorf("store: %w", err)
	}
	type open struct {
		id   int64
		app  string
		seen int64
	}
	var all []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.app, &o.seen); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("store: %w", err)
		}
		all = append(all, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("store: %w", err)
	}
	var keep int64
	if len(all) > 0 && now.Sub(fromMS(all[0].seen)) <= resumeWindow {
		keep = all[0].id
	}
	closed := 0
	for _, o := range all {
		if o.id == keep {
			continue
		}
		if _, err := s.CloseSession(ctx, o.id, minPlayed, appMin(o.app)); err != nil {
			return closed, nil, err
		}
		closed++
	}
	if keep == 0 {
		return closed, nil, nil
	}
	sess, err := s.Session(ctx, keep)
	return closed, sess, err
}

// EnsureDevice returns the id of the user's device of this kind and name,
// creating it if needed.
func (s *Store) EnsureDevice(ctx context.Context, userID int64, kind, name, address string) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO devices(user_id, kind, name, address, created_at)
		VALUES (?,?,?,?,?)`, userID, kind, name, address, ms(time.Now())); err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM devices WHERE user_id = ? AND kind = ? AND name = ?",
		userID, kind, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return id, nil
}

func (s *Store) exec(ctx context.Context, q string, args ...any) error {
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// SplitNext ends the open session now and starts the next one with next as
// its item: the app autoplayed the next episode without leaving the player.
// The new session remembers where it came from (AutoNextFrom) so the
// dashboard can offer Undo, and starts with the format already reported,
// because the device only reports it when a decoder starts.
func (s *Store) SplitNext(ctx context.Context, id int64, next model.Item, sinks []string, now time.Time) (*model.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state = 'closed', ended_at = ?, live_position_ms = NULL
		WHERE id = ? AND state = 'open'`, ms(now), id)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNoSession
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO sessions(user_id, device_id, source_key, app_id, app_name,
		started_at, last_seen_at, last_play_at, media_id, video_codec, video_width, video_height, video_hdr, auto_next_from)
		SELECT user_id, device_id, 'next:' || id, app_id, app_name, ?, ?, ?, media_id,
		       video_codec, video_width, video_height, video_hdr, id
		FROM sessions WHERE id = ?`, ms(now), ms(now), ms(now), id)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	nid, _ := res.LastInsertId()
	if err := identifyTx(ctx, tx, nid, []model.Item{next}, sinks, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return s.Session(ctx, nid)
}

// ErrNotAutoSplit means UndoNext was asked for a session that was not split
// automatically, or whose item already reached a service (use a correction).
var ErrNotAutoSplit = errors.New("store: not an automatic split that can be undone")

// UndoNext reverts an automatic split that was wrong (you went back to the
// start of the same episode): its time goes back to the previous session,
// which takes its state, and the split session is deleted. It returns the
// previous session.
func (s *Store) UndoNext(ctx context.Context, id int64) (*model.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	var prev sql.NullInt64
	var added int
	err = tx.QueryRowContext(ctx, `SELECT auto_next_from, (SELECT COUNT(*) FROM deliveries d JOIN items i ON i.id = d.item_id
		WHERE i.session_id = sessions.id AND d.status = 'added') FROM sessions WHERE id = ?`, id).Scan(&prev, &added)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if !prev.Valid || added > 0 {
		return nil, ErrNotAutoSplit
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET
		state = n.state, ended_at = n.ended_at,
		last_seen_at = MAX(sessions.last_seen_at, n.last_seen_at),
		last_play_at = MAX(sessions.last_play_at, n.last_play_at),
		played_ms = sessions.played_ms + n.played_ms,
		live_position_ms = n.live_position_ms
		FROM (SELECT * FROM sessions WHERE id = ?) AS n WHERE sessions.id = ?`, id, prev.Int64); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return s.Session(ctx, prev.Int64)
}
