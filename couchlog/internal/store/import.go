package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ImportStats says what an import of a tvpresence database did.
type ImportStats struct {
	Devices    int // devices created
	Sessions   int // sessions imported
	Skipped    int // sessions already imported before
	Items      int
	Deliveries int
}

// legacyDevice maps tvpresence's sessions.source to a Couchlog device.
var legacyDevice = map[string]struct{ kind, name string }{
	"tv":        {"webos", "LG TV"},
	"androidtv": {"androidtv", "Android TV"},
	"plex":      {"plex", "Plex"},
}

// legacySessionColumns are the columns read from tvpresence's sessions table.
// Older databases lack some of them; those read as NULL.
var legacySessionColumns = []string{
	"id", "app_id", "app_name", "started_at", "last_seen_at", "last_play_at", "ended_at",
	"played_seconds", "state", "ignored", "media_id", "source", "source_key",
	"hint_title", "hint_season", "hint_episode", "auto_hidden", "hint_extra",
	"position", "live_position", "video_codec", "video_width", "video_height",
	"video_hdr", "video_hdr_secs", "auto_next_from",
}

var legacyItemColumns = []string{
	"id", "session_id", "kind", "title", "year", "season", "episode", "episode_title",
	"tmdb", "tvdb", "imdb", "poster", "trakt_status", "simkl_status", "trakt_error",
	"simkl_error", "pushed_at", "runtime", "runtime_est",
}

// ImportTvpresence copies the history from a tvpresence history.db into this
// store for the local user. The old file is opened read-only and never
// changed. Running it again imports only sessions it has not seen: each old
// session keeps its source_key (Plex's history key) or gets
// "tvpresence:<old id>".
func (s *Store) ImportTvpresence(ctx context.Context, legacyPath string) (ImportStats, error) {
	var st ImportStats
	old, err := sql.Open("sqlite", "file:"+legacyPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return st, fmt.Errorf("import: %w", err)
	}
	defer old.Close()

	sessCols, err := existingColumns(ctx, old, "sessions")
	if err != nil {
		return st, err
	}
	itemCols, err := existingColumns(ctx, old, "items")
	if err != nil {
		return st, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, fmt.Errorf("import: %w", err)
	}
	defer tx.Rollback()

	devices := map[string]int64{} // old source -> device id
	idMap := map[int64]int64{}    // old session id -> new session id (imported now)
	autoNext := map[int64]int64{} // new session id -> old auto_next_from

	rows, err := old.QueryContext(ctx, selectList(legacySessionColumns, sessCols, "sessions")+" ORDER BY id")
	if err != nil {
		return st, fmt.Errorf("import: read sessions: %w", err)
	}
	for rows.Next() {
		r, err := scanRow(rows, legacySessionColumns)
		if err != nil {
			rows.Close()
			return st, err
		}
		source := r.str("source", "tv")
		devID, ok := devices[source]
		if !ok {
			created := false
			devID, created, err = ensureDevice(ctx, tx, source)
			if err != nil {
				rows.Close()
				return st, err
			}
			devices[source] = devID
			if created {
				st.Devices++
			}
		}

		oldID := r.int64("id")
		key := r.str("source_key", "")
		if key == "" {
			key = "tvpresence:" + strconv.FormatInt(oldID, 10)
		}
		hidden := ""
		switch {
		case r.int64("ignored") != 0:
			hidden = "ignored"
		case r.int64("auto_hidden") != 0:
			hidden = "short"
		}
		state := r.str("state", "closed")
		if state != "open" {
			state = "closed"
		}

		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sessions(
			user_id, device_id, source_key, app_id, app_name, started_at, last_seen_at, last_play_at,
			ended_at, played_ms, state, hidden, media_id, hint_title, hint_extra, hint_season, hint_episode,
			position_ms, live_position_ms, video_codec, video_width, video_height, video_hdr, video_hdr_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			LocalUserID, devID, key, r.str("app_id", ""), r.str("app_name", ""),
			r.ms("started_at"), r.ms("last_seen_at"), r.ms("last_play_at"), r.msOrNil("ended_at"),
			r.ms("played_seconds"), state, hidden, r.str("media_id", ""), r.str("hint_title", ""),
			r.str("hint_extra", ""), r.intOrNil("hint_season"), r.intOrNil("hint_episode"),
			r.msOrNil("position"), r.msOrNil("live_position"), r.str("video_codec", ""),
			r.intOrNil("video_width"), r.intOrNil("video_height"), r.str("video_hdr", ""),
			hdrSecondsToMS(r.str("video_hdr_secs", "")))
		if err != nil {
			rows.Close()
			return st, fmt.Errorf("import: session %d: %w", oldID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			st.Skipped++
			continue
		}
		newID, _ := res.LastInsertId()
		idMap[oldID] = newID
		if from := r.int64("auto_next_from"); from != 0 {
			autoNext[newID] = from
		}
		st.Sessions++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return st, fmt.Errorf("import: %w", err)
	}
	rows.Close()

	// Links between sessions, now that every new id is known.
	for newID, oldFrom := range autoNext {
		if target, ok := idMap[oldFrom]; ok {
			if _, err := tx.ExecContext(ctx, "UPDATE sessions SET auto_next_from=? WHERE id=?", target, newID); err != nil {
				return st, fmt.Errorf("import: %w", err)
			}
		}
	}

	irows, err := old.QueryContext(ctx, selectList(legacyItemColumns, itemCols, "items")+" ORDER BY id")
	if err != nil {
		return st, fmt.Errorf("import: read items: %w", err)
	}
	defer irows.Close()
	now := ms(time.Now())
	for irows.Next() {
		r, err := scanRow(irows, legacyItemColumns)
		if err != nil {
			return st, err
		}
		sessID, ok := idMap[r.int64("session_id")]
		if !ok {
			continue // its session was imported before (or is missing)
		}
		kind := r.str("kind", "movie")
		if kind != "episode" {
			kind = "movie"
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO items(session_id, kind, title, year, season, episode,
			episode_title, tmdb, tvdb, imdb, poster, runtime_min, runtime_estimated)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sessID, kind, r.str("title", ""), r.intOrNil("year"), r.intOrNil("season"), r.intOrNil("episode"),
			r.str("episode_title", ""), r.intOrNil("tmdb"), r.intOrNil("tvdb"), r.str("imdb", ""),
			r.str("poster", ""), r.intOrNil("runtime"), r.int64("runtime_est"))
		if err != nil {
			return st, fmt.Errorf("import: item %d: %w", r.int64("id"), err)
		}
		itemID, _ := res.LastInsertId()
		st.Items++

		updated := now
		if p := r.msOrNil("pushed_at"); p != nil {
			updated = p.(int64)
		}
		for _, sink := range []string{"trakt", "simkl"} {
			status := legacyStatus(r.str(sink+"_status", "pending"))
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO deliveries(item_id, sink, status, error, updated_at) VALUES (?,?,?,?,?)",
				itemID, sink, status, r.str(sink+"_error", ""), updated); err != nil {
				return st, fmt.Errorf("import: delivery: %w", err)
			}
			st.Deliveries++
		}
	}
	if err := irows.Err(); err != nil {
		return st, fmt.Errorf("import: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return st, fmt.Errorf("import: %w", err)
	}
	return st, nil
}

func ensureDevice(ctx context.Context, tx *sql.Tx, source string) (id int64, created bool, err error) {
	d, ok := legacyDevice[source]
	if !ok {
		d = struct{ kind, name string }{source, source}
	}
	res, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO devices(user_id, kind, name, created_at) VALUES (?,?,?,?)",
		LocalUserID, d.kind, d.name, ms(time.Now()))
	if err != nil {
		return 0, false, fmt.Errorf("import: device: %w", err)
	}
	n, _ := res.RowsAffected()
	err = tx.QueryRowContext(ctx, "SELECT id FROM devices WHERE user_id=? AND kind=? AND name=?",
		LocalUserID, d.kind, d.name).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("import: device: %w", err)
	}
	return id, n == 1, nil
}

// legacyStatus keeps the known statuses and turns anything else into error.
func legacyStatus(s string) string {
	switch s {
	case "pending", "ready", "added", "not_found", "skipped", "error":
		return s
	}
	return "error"
}

// hdrSecondsToMS turns tvpresence's {"HDR10": 1234.5} into {"HDR10": 1234500}.
func hdrSecondsToMS(js string) string {
	if js == "" {
		return "{}"
	}
	var secs map[string]float64
	if json.Unmarshal([]byte(js), &secs) != nil {
		return "{}"
	}
	out := make(map[string]int64, len(secs))
	for k, v := range secs {
		out[k] = int64(math.Round(v * 1000))
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func existingColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("import: no %s table: is this a tvpresence history.db?", table)
	}
	return cols, rows.Err()
}

// selectList selects wanted columns, reading missing ones as NULL so every
// version of the old schema scans the same way. Column names come from the
// fixed lists above, never from input.
func selectList(wanted []string, have map[string]bool, table string) string {
	parts := make([]string, len(wanted))
	for i, c := range wanted {
		if have[c] {
			parts[i] = c
		} else {
			parts[i] = "NULL AS " + c
		}
	}
	return "SELECT " + strings.Join(parts, ", ") + " FROM " + table
}

// legacyRow is one scanned row with typed, NULL-tolerant accessors.
type legacyRow map[string]any

func scanRow(rows *sql.Rows, cols []string) (legacyRow, error) {
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	r := legacyRow{}
	for i, c := range cols {
		r[c] = vals[i]
	}
	return r, nil
}

func (r legacyRow) str(col, def string) string {
	switch v := r[col].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case nil:
		return def
	default:
		return fmt.Sprint(v)
	}
}

func (r legacyRow) float(col string) (float64, bool) {
	switch v := r[col].(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	return 0, false
}

func (r legacyRow) int64(col string) int64 {
	f, _ := r.float(col)
	return int64(f)
}

func (r legacyRow) intOrNil(col string) any {
	if f, ok := r.float(col); ok {
		return int64(f)
	}
	return nil
}

// ms reads seconds (REAL in tvpresence) as Unix milliseconds.
func (r legacyRow) ms(col string) int64 {
	f, _ := r.float(col)
	return int64(math.Round(f * 1000))
}

func (r legacyRow) msOrNil(col string) any {
	if f, ok := r.float(col); ok {
		return int64(math.Round(f * 1000))
	}
	return nil
}
