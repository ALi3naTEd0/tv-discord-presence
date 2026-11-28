package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Identify sets what a session was, replacing what it had. This is also how
// a wrong identification is corrected, and the reason Couchlog needs no
// remap file: nothing goes out by title, so a correction is about the item,
// and it reaches every service the old item had reached.
//
//   - An old item that is the same title as a new one (SameTitle) stays, with
//     its deliveries: correcting the poster or the year sends nothing again.
//   - An old item that was never added anywhere is deleted.
//   - An old item already in someone's history is kept as replaced, its
//     "added" deliveries become "remove", so the sync takes that play back out
//     of Trakt / Simkl; the rest of its deliveries go. RemoveDone deletes it
//     once every service has removed it.
//   - New items get a pending delivery for each sink.
func (s *Store) Identify(ctx context.Context, sessionID int64, items []model.Item, sinks []string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	if err := identifyTx(ctx, tx, sessionID, items, sinks, now); err != nil {
		return err
	}
	return tx.Commit()
}

func identifyTx(ctx context.Context, tx *sql.Tx, sessionID int64, items []model.Item, sinks []string, now time.Time) error {
	if err := tx.QueryRowContext(ctx, "SELECT id FROM sessions WHERE id = ?", sessionID).Scan(new(int64)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSession
		}
		return fmt.Errorf("store: %w", err)
	}
	old, err := currentItems(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	kept := map[int]bool{} // indexes into items already present
	for _, o := range old {
		match := -1
		for i, it := range items {
			if !kept[i] && o.Identity.SameTitle(it.Identity) {
				match = i
				break
			}
		}
		if match >= 0 {
			kept[match] = true
			n := items[match]
			// the same title again: identifying it yourself confirms a guess
			if _, err := tx.ExecContext(ctx, `UPDATE items SET title = ?, year = ?, episode_title = ?,
				tmdb = ?, tvdb = ?, imdb = ?, poster = COALESCE(NULLIF(?, ''), poster), guessed = ? WHERE id = ?`,
				n.Title, nullInt(n.Year), n.EpisodeTitle, nullInt(n.TMDB), nullInt(n.TVDB), n.IMDb, n.Poster, n.Guessed, o.ID); err != nil {
				return fmt.Errorf("store: %w", err)
			}
			continue
		}
		var added int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries WHERE item_id = ? AND status = 'added'",
			o.ID).Scan(&added); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if added == 0 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM items WHERE id = ?", o.ID); err != nil {
				return fmt.Errorf("store: %w", err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE items SET replaced_at = ? WHERE id = ?", ms(now), o.ID); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM deliveries WHERE item_id = ? AND status <> 'added'", o.ID); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET status = 'remove', error = '', attempts = 0,
			updated_at = ? WHERE item_id = ?`, ms(now), o.ID); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	for i, it := range items {
		if kept[i] {
			continue
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO items(session_id, kind, title, year, season, episode,
			episode_title, tmdb, tvdb, imdb, poster, runtime_min, runtime_estimated, guessed) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sessionID, string(it.Kind), it.Title, nullInt(it.Year), nullInt(it.Season), nullInt(it.Episode),
			it.EpisodeTitle, nullInt(it.TMDB), nullInt(it.TVDB), it.IMDb, it.Poster, nullInt(it.RuntimeMinutes),
			it.RuntimeEstimated, it.Guessed)
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		id, _ := res.LastInsertId()
		if it.Guessed {
			continue // nowhere until you confirm it
		}
		for _, sk := range sinks {
			if _, err := tx.ExecContext(ctx, "INSERT INTO deliveries(item_id, sink, updated_at) VALUES (?,?,?)",
				id, sk, ms(now)); err != nil {
				return fmt.Errorf("store: %w", err)
			}
		}
	}
	// Identifying a session shows it, even if it was hidden as short.
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET hidden = '' WHERE id = ? AND hidden = 'short'", sessionID); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// RemoveDone records that a sink took a replaced item's play out of your
// history; when no service still has it, the replaced item is deleted.
func (s *Store) RemoveDone(ctx context.Context, itemID int64, sink string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET status = 'removed', error = '', updated_at = ?
		WHERE item_id = ? AND sink = ? AND status = 'remove'`, ms(now), itemID, sink); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE id = ? AND replaced_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM deliveries WHERE item_id = items.id AND status <> 'removed')`, itemID); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return tx.Commit()
}

// Items returns the session's current items (not the replaced ones).
func (s *Store) Items(ctx context.Context, sessionID int64) ([]model.Item, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer tx.Rollback()
	return currentItems(ctx, tx, sessionID)
}

// DeliveryOf returns one item's status with one sink.
func (s *Store) DeliveryOf(ctx context.Context, itemID int64, sink string) (model.Delivery, error) {
	d := model.Delivery{ItemID: itemID, Sink: sink}
	var status string
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT status, error, attempts, updated_at FROM deliveries
		WHERE item_id = ? AND sink = ?`, itemID, sink).Scan(&status, &d.Error, &d.Attempts, &updated)
	if err != nil {
		return d, fmt.Errorf("store: delivery %d/%s: %w", itemID, sink, err)
	}
	d.Status, d.UpdatedAt = model.DeliveryStatus(status), fromMS(updated)
	return d, nil
}

func currentItems(ctx context.Context, tx *sql.Tx, sessionID int64) ([]model.Item, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, title, year, season, episode, episode_title,
		tmdb, tvdb, imdb, poster, runtime_min, runtime_estimated, guessed FROM items
		WHERE session_id = ? AND replaced_at IS NULL ORDER BY COALESCE(season, 0), COALESCE(episode, 0), id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	var out []model.Item
	for rows.Next() {
		var (
			it                                         model.Item
			kind                                       string
			year, season, episode, tmdb, tvdb, runtime sql.NullInt64
		)
		if err := rows.Scan(&it.ID, &kind, &it.Title, &year, &season, &episode, &it.EpisodeTitle,
			&tmdb, &tvdb, &it.IMDb, &it.Poster, &runtime, &it.RuntimeEstimated, &it.Guessed); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		it.SessionID, it.Kind = sessionID, model.ItemKind(kind)
		it.Year, it.Season, it.Episode = int(year.Int64), int(season.Int64), int(episode.Int64)
		it.TMDB, it.TVDB, it.RuntimeMinutes = int(tmdb.Int64), int(tvdb.Int64), int(runtime.Int64)
		out = append(out, it)
	}
	return out, rows.Err()
}

// NormalizeTitle is how aliases compare titles: case, accents, punctuation
// and spacing do not matter ("Les Enfants vont bien!" = "les enfants vont bien").
func NormalizeTitle(t string) string {
	var b strings.Builder
	space := false
	// "&" and "and" are the same title ("Laurie & Jessie" / "Laurie and Jessie").
	for _, r := range norm.NFD.String(strings.ReplaceAll(t, "&", " and ")) {
		switch {
		case unicode.Is(unicode.Mn, r): // combining accent
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(unicode.ToLower(r))
		default:
			space = true
		}
	}
	return b.String()
}

// SaveAlias remembers that what appID (or any app, "") calls a.MatchTitle
// (in a.MatchYear, or any year with 0) is a.Identity. Saving the same match
// again replaces the target: the latest correction wins.
func (s *Store) SaveAlias(ctx context.Context, a model.Alias, now time.Time) (int64, error) {
	key := NormalizeTitle(a.MatchTitle)
	if key == "" {
		return 0, errors.New("store: alias needs a title to match")
	}
	if a.Kind != model.KindMovie && a.Kind != model.KindEpisode {
		return 0, fmt.Errorf("store: alias kind %q", a.Kind)
	}
	if a.TMDB == 0 && a.IMDb == "" && a.TVDB == 0 {
		return 0, errors.New("store: alias needs an external ID: they go by ID, never by title")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO aliases(user_id, app_id, match_title, match_year, kind, title,
		year, tmdb, tvdb, imdb, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (user_id, app_id, match_title, match_year) DO UPDATE SET kind = excluded.kind,
		title = excluded.title, year = excluded.year, tmdb = excluded.tmdb, tvdb = excluded.tvdb,
		imdb = excluded.imdb, hits = 0, created_at = excluded.created_at`,
		a.UserID, a.AppID, key, a.MatchYear, string(a.Kind), a.Title, nullInt(a.Year), nullInt(a.TMDB),
		nullInt(a.TVDB), a.IMDb, ms(now))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM aliases WHERE user_id = ? AND app_id = ? AND match_title = ?
		AND match_year = ?`, a.UserID, a.AppID, key, a.MatchYear).Scan(&id)
	return id, err
}

// FindAlias returns the alias for what appID calls title (year 0 if the
// device gave none), or nil. The most specific wins: this app over any app,
// this year over any year. A hit is counted, so the dashboard can show which
// aliases are in use and which are dead.
func (s *Store) FindAlias(ctx context.Context, userID int64, appID, title string, year int) (*model.Alias, error) {
	key := NormalizeTitle(title)
	if key == "" {
		return nil, nil
	}
	var (
		a              model.Alias
		kind           string
		yr, tmdb, tvdb sql.NullInt64
		created        int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, app_id, match_title, match_year, kind, title, year,
		tmdb, tvdb, imdb, hits, created_at FROM aliases
		WHERE user_id = ? AND app_id IN (?, '') AND match_title = ? AND match_year IN (?, 0)
		ORDER BY app_id = '' , match_year = 0 LIMIT 1`, userID, appID, key, year).Scan(
		&a.ID, &a.UserID, &a.AppID, &a.MatchTitle, &a.MatchYear, &kind, &a.Title, &yr, &tmdb, &tvdb, &a.IMDb,
		&a.Hits, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	a.Kind, a.Year, a.TMDB, a.TVDB = model.ItemKind(kind), int(yr.Int64), int(tmdb.Int64), int(tvdb.Int64)
	a.CreatedAt = fromMS(created)
	if _, err := s.db.ExecContext(ctx, "UPDATE aliases SET hits = hits + 1 WHERE id = ?", a.ID); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	a.Hits++
	return &a, nil
}

// DeleteAlias forgets an alias.
func (s *Store) DeleteAlias(ctx context.Context, userID, id int64) error {
	return s.exec(ctx, "DELETE FROM aliases WHERE user_id = ? AND id = ?", userID, id)
}

// ItemsWithoutPoster lists current items of sessions started after since
// that have no poster yet (TMDB adds posters days after a new listing).
func (s *Store) ItemsWithoutPoster(ctx context.Context, since time.Time) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.session_id, i.kind, i.title, COALESCE(i.year, 0),
		COALESCE(i.season, 0), COALESCE(i.episode, 0), COALESCE(i.tmdb, 0), i.imdb
		FROM items i JOIN sessions s ON s.id = i.session_id
		WHERE i.poster = '' AND i.replaced_at IS NULL AND s.started_at > ? ORDER BY i.id`, ms(since))
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	var out []model.Item
	for rows.Next() {
		var it model.Item
		var kind string
		if err := rows.Scan(&it.ID, &it.SessionID, &kind, &it.Title, &it.Year, &it.Season, &it.Episode, &it.TMDB, &it.IMDb); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		it.Kind = model.ItemKind(kind)
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetPoster sets an item's poster if it still has none.
func (s *Store) SetPoster(ctx context.Context, itemID int64, poster string) error {
	return s.exec(ctx, "UPDATE items SET poster = ? WHERE id = ? AND poster = ''", poster, itemID)
}

// LastEpisode is the furthest episode of a show (by TMDB id) in your
// history, for guessing what comes next.
func (s *Store) LastEpisode(ctx context.Context, userID int64, tmdb int) (season, episode int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT i.season, i.episode FROM items i JOIN sessions s ON s.id = i.session_id
		WHERE s.user_id = ? AND i.tmdb = ? AND i.kind = 'episode' AND i.replaced_at IS NULL AND s.hidden <> 'ignored'
		ORDER BY i.season DESC, i.episode DESC LIMIT 1`, userID, tmdb).Scan(&season, &episode)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("store: %w", err)
	}
	return season, episode, true, nil
}

// RecentShow is the show of the latest identified episode watched in an
// app since a time, or nil.
func (s *Store) RecentShow(ctx context.Context, userID int64, appID string, since time.Time) (*model.Identity, error) {
	var id model.Identity
	var year, tvdb sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT i.title, i.year, i.tmdb, i.tvdb, i.imdb FROM items i JOIN sessions s ON s.id = i.session_id
		WHERE s.user_id = ? AND s.app_id = ? AND i.kind = 'episode' AND i.replaced_at IS NULL AND i.tmdb IS NOT NULL
		AND s.started_at > ? ORDER BY s.started_at DESC, i.id DESC LIMIT 1`, userID, appID, ms(since)).
		Scan(&id.Title, &year, &id.TMDB, &tvdb, &id.IMDb)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	id.Kind, id.Year, id.TVDB = model.KindEpisode, int(year.Int64), int(tvdb.Int64)
	return &id, nil
}

// Unidentified lists sessions in these apps started after since that have
// no current item and are not ignored, oldest first.
func (s *Store) Unidentified(ctx context.Context, appIDs []string, since time.Time) ([]*model.Session, error) {
	if len(appIDs) == 0 {
		return nil, nil
	}
	q := "SELECT " + sessionCols + " FROM sessions WHERE app_id IN (?" + strings.Repeat(",?", len(appIDs)-1) +
		") AND hidden <> 'ignored' AND started_at > ? AND NOT EXISTS (SELECT 1 FROM items WHERE session_id = sessions.id" +
		" AND replaced_at IS NULL) ORDER BY started_at"
	args := make([]any, 0, len(appIDs)+1)
	for _, a := range appIDs {
		args = append(args, a)
	}
	args = append(args, ms(since))
	rows, err := s.db.QueryContext(ctx, q, args...)
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
