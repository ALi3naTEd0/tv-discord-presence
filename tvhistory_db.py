"""Shared TV history database.

tvpresence writes the sessions (which app played and for how long); the
dashboard writes the identifications (what it was) and the Trakt/Simkl push
status. The database is the source of truth: neither process keeps the open
session in memory, so the dashboard can split a session ("next episode") and
tvpresence picks it up on its next tick.
"""
from __future__ import annotations

import os
import json
import sqlite3
import sys
from pathlib import Path


def app_dir(kind: str) -> Path:
    """Per-platform config ("config") or data ("data") directory for tvpresence.

    Linux: XDG (~/.config/tvpresence, ~/.local/share/tvpresence)
    macOS: ~/Library/Application Support/tvpresence
    Windows: %APPDATA%\\tvpresence (config), %LOCALAPPDATA%\\tvpresence (data)
    TVPRESENCE_CONFIG_DIR / TVPRESENCE_DATA_DIR override everything.
    """
    override = os.environ.get("TVPRESENCE_CONFIG_DIR" if kind == "config" else "TVPRESENCE_DATA_DIR")
    if override:
        return Path(override).expanduser()
    home = Path.home()
    if sys.platform == "win32":
        var, fallback = ("APPDATA", "Roaming") if kind == "config" else ("LOCALAPPDATA", "Local")
        return Path(os.environ.get(var) or home / "AppData" / fallback) / "tvpresence"
    if sys.platform == "darwin":
        return home / "Library" / "Application Support" / "tvpresence"
    var, fallback = ("XDG_CONFIG_HOME", ".config") if kind == "config" else ("XDG_DATA_HOME", ".local/share")
    return Path(os.environ.get(var) or home / fallback) / "tvpresence"


CONFIG_DIR = app_dir("config")
DATA_DIR = app_dir("data")


def log_to_file_if_windowless(name: str) -> None:
    """Under pythonw.exe (Windows, no console) stdout/stderr are None: send them to a log file."""
    if sys.stderr is None or sys.stdout is None:
        DATA_DIR.mkdir(parents=True, exist_ok=True)
        f = open(DATA_DIR / f"{name}.log", "a", buffering=1, encoding="utf-8")
        sys.stdout = sys.stdout or f
        sys.stderr = sys.stderr or f
DB_PATH = DATA_DIR / "history.db"

SCHEMA = """
CREATE TABLE IF NOT EXISTS sessions(
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  app_id         TEXT    NOT NULL,
  app_name       TEXT,
  app_logo       TEXT,
  started_at     REAL    NOT NULL,
  last_seen_at   REAL    NOT NULL,
  last_play_at   REAL    NOT NULL,
  ended_at       REAL,
  played_seconds REAL    NOT NULL DEFAULT 0,
  state          TEXT    NOT NULL DEFAULT 'open',   -- open | closed
  ignored        INTEGER NOT NULL DEFAULT 0,
  media_id       TEXT                               -- webOS mediaId of the content
);
CREATE INDEX IF NOT EXISTS sessions_state   ON sessions(state);
CREATE INDEX IF NOT EXISTS sessions_started ON sessions(started_at);

CREATE TABLE IF NOT EXISTS items(
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id    INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  kind          TEXT    NOT NULL,                   -- movie | episode
  title         TEXT    NOT NULL,                   -- movie, or the show
  year          INTEGER,
  season        INTEGER,
  episode       INTEGER,
  episode_title TEXT,
  tmdb          INTEGER,                            -- of the show for episodes
  tvdb          INTEGER,
  imdb          TEXT,
  poster        TEXT,
  trakt_status  TEXT    NOT NULL DEFAULT 'pending', -- pending|added|not_found|error
  simkl_status  TEXT    NOT NULL DEFAULT 'pending',
  trakt_error   TEXT,
  simkl_error   TEXT,
  pushed_at     REAL
);
CREATE INDEX IF NOT EXISTS items_session ON items(session_id);
"""


def connect(path: Path = DB_PATH) -> sqlite3.Connection:
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(path, timeout=10, isolation_level=None)  # autocommit
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA busy_timeout=5000")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.executescript(SCHEMA)
    # Migrations from older databases (tvpresence and the dashboard may run them at the same time)
    cols = {r[1] for r in conn.execute("PRAGMA table_info(sessions)")}
    for col, decl in (("media_id", "TEXT"),
                      ("source", "TEXT NOT NULL DEFAULT 'tv'"),   # tv (LG) | androidtv | plex
                      ("source_key", "TEXT"),                     # e.g. Plex historyKey
                      ("hint_title", "TEXT"),                     # what the source said, when it could not be identified
                      ("hint_season", "INTEGER"),
                      ("hint_episode", "INTEGER"),
                      ("auto_hidden", "INTEGER NOT NULL DEFAULT 0"),
                      ("hint_extra", "TEXT"),
                      ("position", "REAL"),
                      ("live_position", "REAL"),
                      ("video_codec", "TEXT"),    # Android TV decoder: AV1 / HEVC / H.264 / VP9 / Dolby Vision
                      ("video_width", "INTEGER"),
                      ("video_height", "INTEGER"),
                      ("video_hdr", "TEXT"),      # dynamic range that played longest (SDR / HDR10 / HLG / DV), for the history
                      ("video_hdr_live", "TEXT"), # what is playing right now, per the box's video driver
                      ("video_hdr_secs", "TEXT"), # JSON {format: seconds} behind video_hdr
                      ("video_hdr_since", "REAL"),
                      ("auto_next_from", "INTEGER")):  # session this one was split from when the app autoplayed the next episode  # last sample, for the Now card (position keeps the furthest)     # last real playback position in seconds (Android TV)   # the device's second line (channel / tagline): YouTube hides real titles there  # short clip kept 7 days under "Short"
        if col not in cols:
            try:
                conn.execute(f"ALTER TABLE sessions ADD COLUMN {col} {decl}")
            except sqlite3.OperationalError:
                pass  # the other process added it
    icols = {r[1] for r in conn.execute("PRAGMA table_info(items)")}
    for col, decl in (("runtime", "INTEGER"),                       # minutes, from TMDB
                      ("runtime_est", "INTEGER NOT NULL DEFAULT 0")):  # 1 = borrowed from another listing
        if col not in icols:
            try:
                conn.execute(f"ALTER TABLE items ADD COLUMN {col} {decl}")
            except sqlite3.OperationalError:
                pass  # the other process added it
    conn.execute("CREATE UNIQUE INDEX IF NOT EXISTS sessions_source_key ON sessions(source_key)")
    return conn


def get_open_session(conn: sqlite3.Connection, source: str = "tv") -> sqlite3.Row | None:
    return conn.execute(
        "SELECT * FROM sessions WHERE state='open' AND source=? ORDER BY id DESC LIMIT 1", (source,)
    ).fetchone()


def open_session(conn, app_id: str, app_name: str, app_logo: str | None, now: float,
                 media_id: str | None = None, source: str = "tv") -> sqlite3.Row:
    cur = conn.execute(
        "INSERT INTO sessions(app_id, app_name, app_logo, started_at, last_seen_at, last_play_at, media_id, source)"
        " VALUES (?,?,?,?,?,?,?,?)",
        (app_id, app_name, app_logo, now, now, now, media_id, source),
    )
    return conn.execute("SELECT * FROM sessions WHERE id=?", (cur.lastrowid,)).fetchone()


def touch(conn, sid: int, now: float, played_add: float, playing: bool) -> None:
    # WHERE state='open': no-op if the dashboard already closed it (next episode)
    if playing:
        conn.execute(
            "UPDATE sessions SET last_seen_at=?, last_play_at=?, played_seconds=played_seconds+?"
            " WHERE id=? AND state='open'",
            (now, now, played_add, sid),
        )
    else:
        conn.execute(
            "UPDATE sessions SET last_seen_at=? WHERE id=? AND state='open'", (now, sid)
        )


def reopen_recent(conn, source: str, app_id: str, media_id: str, within: float, now: float) -> sqlite3.Row | None:
    """Coming back to the same title (Android TV publishes it) after a pause or another app:
    continue that session instead of starting a new, unidentified one."""
    row = conn.execute(
        "SELECT * FROM sessions WHERE state='closed' AND source=? AND app_id=? AND media_id=? AND ignored=0"
        " AND ended_at > ? ORDER BY ended_at DESC LIMIT 1", (source, app_id, media_id, now - within)).fetchone()
    if row is None:
        return None
    conn.execute("UPDATE sessions SET state='open', ended_at=NULL, auto_hidden=0, last_seen_at=? WHERE id=?",
                 (now, row["id"]))
    return conn.execute("SELECT * FROM sessions WHERE id=?", (row["id"],)).fetchone()


def set_video(conn, sid: int, codec: str, width: int, height: int) -> None:
    """Video format the box decoded for this session (keep the widest = highest resolution seen)."""
    conn.execute("UPDATE sessions SET video_codec=?, video_width=?, video_height=? WHERE id=?"
                 " AND COALESCE(video_width, 0) <= ?", (codec, width, height, sid, width))


def _hdr_accumulate(conn, sid: int, now: float, new_fmt: str | None = None) -> None:
    row = conn.execute("SELECT video_hdr_live, video_hdr_secs, video_hdr_since FROM sessions WHERE id=?", (sid,)).fetchone()
    if not row:
        return
    live, since = row[0], row[2]
    secs = json.loads(row[1]) if row[1] else {}
    if live and since and now > since:
        secs[live] = secs.get(live, 0) + (now - since)
    live = new_fmt or live
    if live:
        secs.setdefault(live, 0)
    top = max(secs, key=secs.get) if secs else None
    conn.execute("UPDATE sessions SET video_hdr=?, video_hdr_live=?, video_hdr_secs=?, video_hdr_since=? WHERE id=?",
                 (top, live, json.dumps({k: round(v) for k, v in secs.items()}), now, sid))


def set_hdr(conn, sid: int, fmt: str, ts: float) -> None:
    """A new video started at `ts` with this dynamic range, as actually decoded (the box's video driver):
    a Dolby Vision title on a non-DV setup reports what really plays. The history keeps the format that
    played longest, so an SDR trailer before an HDR film does not win."""
    _hdr_accumulate(conn, sid, ts, fmt)


def tick_hdr(conn, sid: int, now: float) -> None:
    """Count time for the format playing now (called with the periodic format poll)."""
    _hdr_accumulate(conn, sid, now)


def set_position(conn, sid: int, pos: float) -> None:
    # Furthest point reached, not the last sample: players jump back to 0 when a video ends
    conn.execute("UPDATE sessions SET position=MAX(COALESCE(position, 0), ?), live_position=? WHERE id=? AND state='open'",
                 (pos, pos, sid))


def set_media(conn, sid: int, media_id: str) -> None:
    conn.execute("UPDATE sessions SET media_id=? WHERE id=? AND state='open'", (media_id, sid))


def set_hint(conn, sid: int, title: str | None, subtitle: str | None = None) -> None:
    """What the device itself reports as playing (Android media session); pre-fills the picker."""
    if title:  # second line kept apart: often a channel, but YouTube uploads hide the real title there
        conn.execute("UPDATE sessions SET hint_title=?, hint_extra=? WHERE id=? AND state='open'",
                     (title, subtitle, sid))


def has_items(conn, sid: int) -> bool:
    return conn.execute("SELECT 1 FROM items WHERE session_id=? LIMIT 1", (sid,)).fetchone() is not None


def close_session(conn, sid: int, min_seconds: float, ended_at: float | None = None,
                  min_by_app: dict | None = None) -> None:
    """Close the session. If it lasted less than min_seconds (or the app's own minimum,
    e.g. 20 min for YouTube) and nobody identified it, delete it (trailers, menu autoplay, clips)."""
    row = conn.execute("SELECT * FROM sessions WHERE id=? AND state='open'", (sid,)).fetchone()
    if row is None:
        return
    app_min = (min_by_app or {}).get(row["app_id"])
    if not has_items(conn, sid):
        if row["played_seconds"] < min_seconds:
            conn.execute("DELETE FROM sessions WHERE id=?", (sid,))
            return
        if app_min is not None and row["played_seconds"] < app_min:
            # Under the app's own minimum (YouTube < 20 min): may still be a short film.
            # Keep it hidden under "Short"; the dashboard purges it after 7 days if unidentified.
            conn.execute("UPDATE sessions SET state='closed', ended_at=?, auto_hidden=1 WHERE id=? AND state='open'",
                         (ended_at if ended_at is not None else row["last_seen_at"], sid))
            return
    conn.execute(
        "UPDATE sessions SET state='closed', ended_at=? WHERE id=? AND state='open'",
        (ended_at if ended_at is not None else row["last_seen_at"], sid),
    )


def recover(conn, min_seconds: float, resume_window: float = 0, now: float | None = None,
            source: str = "tv", min_by_app: dict | None = None) -> tuple[int, int | None]:
    """On startup after a restart: the most recent open session is resumed if it
    was seen less than resume_window ago (the first tick decides whether it is
    still the same app or closes it); the rest are closed. Returns (closed, resumed_id)."""
    import time as _t
    now = _t.time() if now is None else now
    rows = conn.execute("SELECT id, last_seen_at FROM sessions WHERE state='open' AND source=? ORDER BY id DESC",
                        (source,)).fetchall()
    keep = rows[0]["id"] if rows and now - rows[0]["last_seen_at"] <= resume_window else None
    closed = 0
    for r in rows:
        if r["id"] != keep:
            close_session(conn, r["id"], min_seconds, min_by_app=min_by_app)
            closed += 1
    return closed, keep


def current_item(conn, sid: int) -> sqlite3.Row | None:
    """The last identified item of the session (the most recent episode)."""
    return conn.execute(
        "SELECT * FROM items WHERE session_id=?"
        " ORDER BY COALESCE(season,0) DESC, COALESCE(episode,0) DESC, id DESC LIMIT 1",
        (sid,),
    ).fetchone()
