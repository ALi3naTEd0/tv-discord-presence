# Couchlog in Go: design

Status: **draft, stage 2 (Design)**. The Python version keeps running until the
Go version gives the same results on the same TVs (see *Migration*).

## Goals

1. **Installs in one step, anywhere.** One static binary for Linux, Windows and
   macOS on amd64 and arm64, a multi-arch Docker image and an AUR package. No
   Python, no venv, no trakt-scrobbler, no hand-made API keys.
2. **Your own history first, useful with no accounts.** Couchlog records what
   you watch on any TV or player and works with zero sign-ins. Trakt, Simkl,
   Letterboxd (CSV export) and Discord are optional *outputs*, never required.
3. **Server anywhere, presence on the desktop.** The history can live on a
   NAS or a Raspberry Pi that is always on, while Discord Rich Presence (which
   only works through Discord's local IPC) runs on the PC where Discord is.
4. **Room for a hosted mode later**, without building it now.

## Product direction

Couchlog is not another scrobbler and not another title database. It uses
what already exists (TMDB, TVDB, where-to-watch data) to organize a history
that is captured automatically, for any user, passive or hardcore. The value
is what the history makes possible: time and cost per service, what's next
and where, alerts for new seasons or watchlist titles arriving on a service
you pay for, a yearly summary. The local product is complete and free; a
paid tier, if any, covers infrastructure only (sync across homes and phone,
push alerts, backup). Open question: household profiles (who watched what),
see the project note.

## One binary, two modes

```
couchlog server    24/7: sources, session engine, identification, sync, dashboard
couchlog agent     on the PC with Discord: shows what the server says is playing
couchlog import    one-off: brings an existing tvpresence history.db in
```

Running `couchlog server --with-agent` on a single PC gives today's setup.

```
 LG webOS ──SSAP──┐
 Android TV ─ADB──┤                         ┌── Trakt  (by ID)
 Plex ──HTTP──────┼─▶ Sources ─▶ Engine ─▶ Store ─▶ Sinks ──┤
 Jellyfin/Kodi/…──┘   (server     (sessions) (SQLite)     └── Simkl  (by ID)
        ▲              or agent)        │
        │                               ├─▶ Events bus ─▶ API (HTTP + WebSocket)
   agent-hosted                         │                    ├─▶ Dashboard (Svelte PWA, embedded)
   sources push                         │                    └─▶ Agent ─▶ Discord Rich Presence
   observations                         └─▶ Identify (TMDB, IMDb, TVmaze)
```

## Packages

| Package | Job |
|---|---|
| `cmd/couchlog` | subcommands, flags, signals, wiring |
| `internal/model` | the shared types: `Observation`, `Session`, `Item`, `Delivery`, `Device` |
| `internal/source` | the `Source` interface and one subpackage per device kind (`webos`, `androidtv`, `plex`, later `jellyfin`, `kodi`, `roku`) |
| `internal/engine` | turns a stream of observations per device into sessions, with today's rules |
| `internal/store` | SQLite schema, migrations, queries, the tvpresence importer |
| `internal/identify` | TMDB + IMDb + TVmaze search and ID resolution |
| `internal/sink` | the `Sink` interface; `trakt`, `simkl` |
| `internal/auth` | device-code logins (Trakt, Simkl), token refresh, credential storage |
| `internal/presence` | Discord Rich Presence (agent side) |
| `internal/events` | typed event bus (the one from CatchMe: slow subscribers are dropped, not waited for) |
| `internal/api` | HTTP + WebSocket API for the dashboard and agents; loopback by default, tokens always |

## Sources

A source watches one device and reports **observations**: what the device
says right now. It knows nothing about sessions or the database.

```go
type Source interface {
    Kind() string                                        // "webos", "androidtv", "plex", ...
    Run(ctx context.Context, emit func(model.Observation)) error
}

type Observation struct {
    DeviceID  string
    At        time.Time
    State     PlayState      // off, idle, playing, paused
    App       App            // id + display name
    MediaID   string         // device's own content id, if any (webOS mediaId, Android session id)
    Hint      Hint           // title / subtitle / season / episode the device published, if any
    Position  *time.Duration // real playback position, if the device knows it
    Duration  *time.Duration
    Video     *Video         // codec, size, dynamic range, when the device reports them
    Identity  *Identity      // set only by sources that already know what it is (Plex)
}
```

`Run` blocks until `ctx` ends and reconnects by itself (backoff, the same
watchdog idea as CatchMe's fetcher). Sources that know exactly what played
(Plex history) also produce finished plays for import; that is a second,
optional interface (`Importer`).

### Implemented sources

- **`source/webos`** (LG): SSAP over WebSocket, `wss://:3001` first (newer
  firmware), then `ws://:3000`. Registers with the public LG Remote App
  manifest, signed first and unsigned when the firmware rejects the signature;
  the first time the TV shows a prompt and the key is saved (tvpresence's
  `~/.config/tvpresence/key` works as is). Subscribes to power, foreground
  app, launch points (app names) and media state, and reports every poll
  (5 s). The TV publishes no titles. HDMI inputs and system apps are not
  apps; unreachable means off, so the engine's grace closes the session.
- **`source/androidtv`**: a small ADB client in Go (no adb binary): RSA auth
  with the box's "Allow USB debugging?" prompt the first time (tvpresence's
  `adbkey` works as is; a new key gets its Android-format `.pub`). One shell
  call per poll reads wakefulness, the resumed activity, uptime and
  `dumpsys media_session` (state, title, subtitle, position advanced to now);
  every 6 polls, decoder records from `media.metrics` (codec, size) and the
  content's dynamic range from the Amlogic driver's kernel log.
- **`source/plex`**: not polled into sessions; `Plays` imports what Plex
  recorded (history API, IDs from the library's GUIDs, legacy agents too) and
  `store.AddPlay` stores each as a closed, identified session. `NowPlaying`
  lists this account's current sessions with the file's format (Plex writes
  Dolby Vision as "DoVi"). When Plex's own database is readable (Couchlog on
  the Plex machine), plays come from its views table instead: it keeps each
  viewing's GUID after the file is deleted (watch-then-delete is the common
  case), resolved through Plex's metadata provider, or the IDs the library
  stored for titles the provider does not know yet. Same source keys as the
  HTTP history, so both and the tvpresence import dedupe.

Each has a live test that runs only with the device's address and key.

## Engine

The engine keeps one open session per device and applies today's rules
(from `docs/architecture.md`, unchanged):

- opens when a recorded app reports `playing`; excluded apps never record
- counts only time spent `playing`; gaps over 30 s are not counted
- closes after `close_grace` (60 s) in another app, or `pause_timeout` (30 min) paused
- splits when the media id changes after leaving the player
- drops sessions under `min_seconds` (120 s); hides unidentified ones under an app's own minimum (YouTube 20 min) as *short*
- resumes the open session after a restart within `resume_window` (10 min)
- on Android TV, coming back to the same title within 6 h continues the session

The engine is a state machine over observations: time comes from each
observation, never the wall clock, so every rule has a test with made-up
times, no TV needed (`internal/engine`, done). The database stays the source
of truth, because the dashboard can close or split a session at any moment.
The Python version's behaviour is the reference: the parallel run compares both.

The autoplay split is ported too: with a real position (Android TV), when the
identified episode passed 80 % of its TMDB runtime (not an estimate), at least
half of it was played, and the position restarts under 5 min, the tracker
asks `identify` for the next episode and `store.SplitNext` closes the session
and opens the next one with `auto_next_from` set. `store.UndoNext` gives the
time back when it was the same episode from the start.

## Identification

`internal/identify` uses what already exists, TMDB, instead of a catalog of
its own. One key per Couchlog build (the v4 read token or a v3 key), responses
cached 6 h.

- **Search** (the picker): a pasted IMDb id, TMDB link / `tmdb:movie:123` or
  TheTVDB link / `tvdb:123` resolves directly; anything else searches movies
  and shows by title.
- **Item**: the picked title becomes an item with TMDB, IMDb and TVDB ids,
  poster, the episode's title and runtime. When TMDB has no runtime for the
  episode yet, the show's usual length is used and marked *estimated* (never
  auto-sent).
- **Auto**, when a device publishes a title (Android TV): an alias wins;
  otherwise exactly one TMDB title whose name or original name is the
  published one, a show when the device gave episode numbers and a movie when
  not. Two films with that name (remakes) or no exact match: left for the
  user, because a wrong guess reaches Trakt. YouTube titles are video names,
  so there only aliases identify.
- **Next**: the episode after this one, rolling over to the next season with
  episodes.

Not ported (yet): the IMDb suggestion search the Python picker merges in (an
unofficial endpoint) and TVmaze as a fallback for episodes TMDB has not added.

## Corrections and aliases (instead of a remap file)

trakt-scrobbler needs `remap_rules.toml` because it sends *titles* and lets
Trakt's search pick the item, which picks wrong for foreign titles,
remakes and same-name films. Couchlog sends **IDs** (TMDB / IMDb / TVDB), so
that whole class of error does not happen: the films in the current remap
file that also went through Couchlog are in Trakt and Simkl correctly with no
rule at all.

What can still be wrong is the identification itself: Plex matched the wrong
film, the picker's first result was the wrong one, or you chose badly. Two
pieces cover it:

1. **Correcting reaches the services.** `store.Identify` replaces a session's
   items. An old item that is the same title (same ID) is kept with its
   deliveries, so fixing a year or poster sends nothing again. An old item
   never sent is deleted. An old item already *added* somewhere is kept as
   replaced and those deliveries become `remove`: the sync takes that one play
   (at its watched time, never other viewings) back out of Trakt / Simkl,
   then `removed`, and the item is deleted once no service has it.
   The Python dashboard today deletes the old items and leaves the wrong
   entry in Trakt; this fixes that.
2. **Aliases remember a correction.** "What this app (or any app, or Plex)
   calls *X* (in year *Y*, or any year) is *this ID*." Made from the dashboard
   when you correct something ("remember for next time"), matched with
   normalized titles (case, accents, punctuation do not matter), the most
   specific first (this app over any, this year over any), with a hit count so
   dead ones can be cleaned up. They always resolve to an external ID.
   Identification checks aliases before searching; the Plex importer checks
   `plex` aliases before trusting Plex's own match.

Importing `remap_rules.toml` is not worth it: its rules point at Trakt slugs,
the files are a handful, and those plays are already correct.

## Sinks

```go
type Sink interface {
    Name() string                                              // "trakt", "simkl"
    Check(ctx context.Context, items []model.Item) ([]Result, error)  // already in your history? exists there?
    Push(ctx context.Context, items []model.Item) ([]Result, error)
}
```

Statuses per item and sink are the ones in use today: `pending`, `ready`
(exists on the service, not in your history yet), `added`, `not_found`,
`skipped`, `error`, plus `remove` / `removed` for corrections (see below). Auto-send keeps today's rule: identified, closed, watched
time ≥ 80 % of the runtime (Plex always), check first so nothing is
duplicated, `not_found` retried at most daily. Requests respect `Retry-After`
and back off like CatchMe's downloader.

## Store (SQLite)

Pure-Go SQLite (`modernc.org/sqlite`, no CGO, so it cross-compiles). Times are
Unix milliseconds. Every row belongs to a user, so a hosted mode later is a
matter of more users, not a new schema.

```
users        id, name, created_at
devices      id, user_id, kind, name, address, created_at
sessions     id, user_id, device_id, source_key UNIQUE, app_id, app_name,
             started_at, last_seen_at, last_play_at, ended_at, played_ms,
             state (open|closed), hidden (''|ignored|short),
             media_id, hint_title, hint_extra, hint_season, hint_episode,
             position_ms, live_position_ms,
             video_codec, video_width, video_height, video_hdr, video_hdr_ms (JSON),
             auto_next_from
items        id, session_id, kind (movie|episode), title, year, season, episode,
             episode_title, tmdb, tvdb, imdb, poster, runtime_min, runtime_estimated,
             replaced_at (a correction replaced it; kept until every service removed it)
deliveries   item_id, sink, status, error, attempts, updated_at   PRIMARY KEY (item_id, sink)
aliases      id, user_id, app_id, match_title, match_year, kind, title, year,
             tmdb, tvdb, imdb, hits, created_at   UNIQUE (user_id, app_id, match_title, match_year)
credentials  user_id, service, data (JSON), updated_at            PRIMARY KEY (user_id, service)
schema_version
```

`deliveries` replaces the `trakt_status` / `simkl_status` columns: adding a
destination is new rows, not a new column.

## API and the agent protocol

Same security model as CatchMe: loopback by default, `Authorization: Bearer`,
Origin and Host checks. Listening on the LAN (for a NAS) is an explicit flag
and then requires HTTPS or a reverse proxy.

- Dashboard: REST for actions (`identify`, `next`, `ignore`, `push`, …) plus
  `/v1/ws` for live events, with a full snapshot on connect.
- Agent: `/v1/agent` WebSocket. The server sends the *presence state* per
  device (title, poster, position, playing/paused); the agent may also host
  sources and send their observations up the same connection, which is what a
  hosted mode would need: the TVs stay on the home network.
- Agents pair with a one-time code shown in the dashboard and get their own
  token, revocable per agent.

## Accounts and keys

Users never create apps or paste tokens:

| Service | How Couchlog gets in |
|---|---|
| Trakt | Couchlog's own Trakt app, **device code** login (code on screen, approve on trakt.tv) |
| Simkl | Couchlog's own Simkl app, **PIN** login |
| TMDB | Couchlog's own API key, with the attribution TMDB requires in the UI |
| Discord | Couchlog's own application id (public by design) |

Tokens go in `credentials`, refreshed automatically. Desktop apps cannot keep
a client secret truly secret; Trakt and Simkl accept that for device flows.

## Migration from tvpresence

`couchlog import ~/.local/share/tvpresence/history.db` (also offered by the
setup wizard when it finds one):

- one device per old `source` (`tv` → LG webOS, `androidtv`, `plex`)
- seconds → milliseconds; `ignored` / `auto_hidden` → `hidden`
- `trakt_status` / `simkl_status` (+ errors, `pushed_at`) → `deliveries` rows
- read-only on the old file; running it twice imports nothing new
  (`source_key` and a stable mapping of old ids)

Parallel run: both versions record the same TVs for a week; a comparison
command lists sessions that differ in start, end, played time or split.

## Distribution

GoReleaser on a tag: binaries for linux/windows/darwin × amd64/arm64, a
multi-arch image on GHCR, AUR package, systemd/launchd/Windows service
install via `couchlog service install`. Self-hosting templates (Unraid,
CasaOS, Umbrel, Home Assistant add-on) come after the first release. CI
copies CatchMe's: tests with race and leak checks on three systems, and an
install-and-run smoke test of every package.
