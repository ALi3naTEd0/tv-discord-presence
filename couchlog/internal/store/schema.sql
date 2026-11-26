-- Couchlog schema, version 1. Times are Unix milliseconds (UTC).
-- Every row belongs to a user so a hosted mode later only adds users.

CREATE TABLE users (
  id         INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE devices (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind       TEXT    NOT NULL,            -- webos | androidtv | plex | jellyfin | kodi | roku
  name       TEXT    NOT NULL,
  address    TEXT    NOT NULL DEFAULT '', -- host or URL; '' when not applicable
  created_at INTEGER NOT NULL,
  UNIQUE (user_id, kind, name)
);

CREATE TABLE sessions (
  id               INTEGER PRIMARY KEY,
  user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id        INTEGER NOT NULL REFERENCES devices(id),
  source_key       TEXT    NOT NULL UNIQUE,      -- dedupes imports and re-imports
  app_id           TEXT    NOT NULL,
  app_name         TEXT    NOT NULL DEFAULT '',
  started_at       INTEGER NOT NULL,
  last_seen_at     INTEGER NOT NULL,
  last_play_at     INTEGER NOT NULL,
  ended_at         INTEGER,                      -- NULL while open
  played_ms        INTEGER NOT NULL DEFAULT 0,
  state            TEXT    NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
  hidden           TEXT    NOT NULL DEFAULT '' CHECK (hidden IN ('', 'ignored', 'short')),
  media_id         TEXT    NOT NULL DEFAULT '',
  hint_title       TEXT    NOT NULL DEFAULT '',
  hint_extra       TEXT    NOT NULL DEFAULT '',
  hint_season      INTEGER,
  hint_episode     INTEGER,
  position_ms      INTEGER,                      -- furthest real position reached
  live_position_ms INTEGER,                      -- last sample
  video_codec      TEXT    NOT NULL DEFAULT '',
  video_width      INTEGER,
  video_height     INTEGER,
  video_hdr        TEXT    NOT NULL DEFAULT '',  -- the range that played longest
  video_hdr_ms     TEXT    NOT NULL DEFAULT '{}',-- JSON {range: ms}
  auto_next_from   INTEGER REFERENCES sessions(id) ON DELETE SET NULL
);
CREATE INDEX sessions_user_started ON sessions(user_id, started_at);
CREATE INDEX sessions_open ON sessions(device_id) WHERE state = 'open';

CREATE TABLE items (
  id                INTEGER PRIMARY KEY,
  session_id        INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  kind              TEXT    NOT NULL CHECK (kind IN ('movie', 'episode')),
  title             TEXT    NOT NULL,
  year              INTEGER,
  season            INTEGER,
  episode           INTEGER,
  episode_title     TEXT    NOT NULL DEFAULT '',
  tmdb              INTEGER,
  tvdb              INTEGER,
  imdb              TEXT    NOT NULL DEFAULT '',
  poster            TEXT    NOT NULL DEFAULT '',
  runtime_min       INTEGER,
  runtime_estimated INTEGER NOT NULL DEFAULT 0,
  replaced_at       INTEGER                      -- set when a correction replaced it; kept until
                                                 -- every service it reached has removed it
);
CREATE INDEX items_session ON items(session_id) WHERE replaced_at IS NULL;

CREATE TABLE deliveries (
  item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  sink       TEXT    NOT NULL,                   -- trakt | simkl
  status     TEXT    NOT NULL DEFAULT 'pending'
             CHECK (status IN ('pending', 'ready', 'added', 'not_found', 'skipped', 'error',
                               'remove', 'removed')),   -- remove: a correction; take it back out
  error      TEXT    NOT NULL DEFAULT '',
  attempts   INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (item_id, sink)
);
CREATE INDEX deliveries_open ON deliveries(sink, status) WHERE status <> 'added';

-- Aliases: "when a device or Plex calls it this, it is that". Couchlog's
-- answer to trakt-scrobbler's remap_rules.toml, but made from the dashboard
-- when you correct an identification, and resolved to IDs, never to titles.
CREATE TABLE aliases (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  app_id      TEXT    NOT NULL DEFAULT '',     -- '' = any app; 'plex' for Plex's own (mis)matches
  match_title TEXT    NOT NULL,                -- normalized: lower case, single spaces
  match_year  INTEGER NOT NULL DEFAULT 0,      -- 0 = any year
  kind        TEXT    NOT NULL CHECK (kind IN ('movie', 'episode')),
  title       TEXT    NOT NULL,
  year        INTEGER,
  tmdb        INTEGER,
  tvdb        INTEGER,
  imdb        TEXT    NOT NULL DEFAULT '',
  hits        INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  UNIQUE (user_id, app_id, match_title, match_year)
);

CREATE TABLE credentials (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  service    TEXT    NOT NULL,                   -- trakt | simkl | plex | ...
  data       TEXT    NOT NULL,                   -- JSON: tokens, expiry
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, service)
);
