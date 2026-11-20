"""Shared TV history database.

tvpresence writes the sessions (which app played and for how long); the
dashboard writes the identifications (what it was) and the Trakt/Simkl push
status. The database is the source of truth: neither process keeps the open
session in memory, so the dashboard can split a session ("next episode") and
tvpresence picks it up on its next tick.
"""
from __future__ import annotations

import os
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
                      ("source", "TEXT NOT NULL DEFAULT 'tv'"),   # tv | plex
                      ("source_key", "TEXT"),                     # e.g. Plex historyKey
                      ("hint_title", "TEXT"),                     # what the source said, when it could not be identified
                      ("hint_season", "INTEGER"),
                      ("hint_episode", "INTEGER")):
        if col not in cols:
            try:
                conn.execute(f"ALTER TABLE sessions ADD COLUMN {col} {decl}")
            except sqlite3.OperationalError:
                pass  # the other process added it
    conn.execute("CREATE UNIQUE INDEX IF NOT EXISTS sessions_source_key ON sessions(source_key)")
    return conn


def get_open_session(conn: sqlite3.Connection) -> sqlite3.Row | None:
    return conn.execute(
        "SELECT * FROM sessions WHERE state='open' ORDER BY id DESC LIMIT 1"
    ).fetchone()


def open_session(conn, app_id: str, app_name: str, app_logo: str | None, now: float,
                 media_id: str | None = None) -> sqlite3.Row:
    cur = conn.execute(
        "INSERT INTO sessions(app_id, app_name, app_logo, started_at, last_seen_at, last_play_at, media_id)"
        " VALUES (?,?,?,?,?,?,?)",
        (app_id, app_name, app_logo, now, now, now, media_id),
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


def set_media(conn, sid: int, media_id: str) -> None:
    conn.execute("UPDATE sessions SET media_id=? WHERE id=? AND state='open'", (media_id, sid))


def has_items(conn, sid: int) -> bool:
    return conn.execute("SELECT 1 FROM items WHERE session_id=? LIMIT 1", (sid,)).fetchone() is not None


def close_session(conn, sid: int, min_seconds: float, ended_at: float | None = None) -> None:
    """Close the session. If it lasted less than min_seconds and nobody
    identified it, delete it (trailers, menu autoplay)."""
    row = conn.execute("SELECT * FROM sessions WHERE id=? AND state='open'", (sid,)).fetchone()
    if row is None:
        return
    if row["played_seconds"] < min_seconds and not has_items(conn, sid):
        conn.execute("DELETE FROM sessions WHERE id=?", (sid,))
        return
    conn.execute(
        "UPDATE sessions SET state='closed', ended_at=? WHERE id=? AND state='open'",
        (ended_at if ended_at is not None else row["last_seen_at"], sid),
    )


def recover(conn, min_seconds: float, resume_window: float = 0, now: float | None = None) -> tuple[int, int | None]:
    """On startup after a restart: the most recent open session is resumed if it
    was seen less than resume_window ago (the first tick decides whether it is
    still the same app or closes it); the rest are closed. Returns (closed, resumed_id)."""
    import time as _t
    now = _t.time() if now is None else now
    rows = conn.execute("SELECT id, last_seen_at FROM sessions WHERE state='open' ORDER BY id DESC").fetchall()
    keep = rows[0]["id"] if rows and now - rows[0]["last_seen_at"] <= resume_window else None
    closed = 0
    for r in rows:
        if r["id"] != keep:
            close_session(conn, r["id"], min_seconds)
            closed += 1
    return closed, keep


def current_item(conn, sid: int) -> sqlite3.Row | None:
    """The last identified item of the session (the most recent episode)."""
    return conn.execute(
        "SELECT * FROM items WHERE session_id=?"
        " ORDER BY COALESCE(season,0) DESC, COALESCE(episode,0) DESC, id DESC LIMIT 1",
        (sid,),
    ).fetchone()
