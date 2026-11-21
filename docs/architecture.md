# Architecture

```
 LG webOS TV ──SSAP :3001──▶ tvpresence.py ──IPC──▶ Discord Rich Presence
                                  │  ▲
                     sessions     │  │ identified item (title, S/E, poster)
                                  ▼  │
                        ~/.local/share/tvpresence/history.db  (SQLite, WAL)
                                  ▲  │
             identify / split /   │  │  state
             push status          │  ▼
 Plex :32400 ──plays/sessions──▶ tvhistory_dashboard.py :7790 ──by ID──▶ Trakt, Simkl
                                  ▲
                    TMDB + IMDb search, ID/link resolution
```

Both processes open the same database; neither keeps the open session in memory. That is what makes the loop two-way: the dashboard can identify or split the session that is playing right now, and on its next tick `tvpresence` reads the item and updates Discord with the real title and the TMDB poster.

## Capture rules (`HistoryRecorder` in `tvpresence.py`)

webOS only reports `appId` and `playState` (plus an opaque `mediaId`). No title, no position. So a "session" is a stretch of playback inside one app:

- **Opens** when a streaming app reports `playing`. Apps in `exclude_apps` (YouTube, Twitch) are never recorded.
- **Counts** only time spent `playing`; gaps longer than 30 s (suspend, network loss) are not counted.
- **Closes** when the app leaves the foreground for more than `close_grace` (60 s), or stays paused / in menus for more than `pause_timeout` (30 min).
- **Splits** when the `mediaId` changes *after* the player was left (back to the app's menu, then something else is played). Autoplay into the next episode without a menu gap keeps the same session — use **Next episode** in the dashboard for that.
- **Drops** sessions shorter than `min_seconds` (120 s): trailers, menu autoplay.
- **Hides** unidentified sessions under an app's own minimum (`min_seconds_by_app`, YouTube = 20 min) instead of dropping them (`auto_hidden = 1`, dashboard filter *Short*). The dashboard purges them after 7 days. There is no automatic matching.
- **Resumes** the open session after a service restart if it was seen less than `resume_window` (10 min) ago, so restarts do not split a viewing.

## Database (`tvhistory_db.py`)

- `sessions`: one row per viewing: `app_id`, `app_name`, `started_at`, `last_play_at`, `ended_at`, `played_seconds`, `state` (`open|closed`), `ignored`, `media_id`, `source` (`tv|plex`), `source_key` (unique, dedupes Plex imports), and `hint_title/season/episode` (what Plex said, used to pre-fill the picker).
- `items`: what a session actually was. One session can hold several episodes. `kind` (`movie|episode`), `title`, `year`, `season`, `episode`, `episode_title`, `tmdb`/`tvdb`/`imdb` (of the show for episodes), `poster`, and per-service status: `trakt_status`, `simkl_status` ∈ `pending | ready | added | not_found | error`.

Columns are added by in-place migrations on `connect()`.

## Dashboard (`tvhistory_dashboard.py`)

Stdlib HTTP server bound to `127.0.0.1:7790`. POST endpoints require `Content-Type: application/json` and a same-origin `Origin` (tolerant of `X-Forwarded-Host` for `tailscale serve`) as a CSRF guard.

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | the UI |
| GET | `/api/state` | now-playing on the TV, *Now on Plex*, recent sessions, pending counts, Plex import status |
| GET | `/api/search?q=` | TMDB + IMDb merged by ID; a pasted IMDb/TMDB/TVDB ID or URL resolves directly. Each hit carries ✓ badges for "exists on Trakt / Simkl" (all ID sources are tried). |
| GET | `/api/tmdb/tv/{id}`, `/api/tmdb/tv/{id}/season/{n}` | seasons and episodes for the picker |
| POST | `/api/sessions/{id}/identify` | attach one or more items |
| POST | `/api/sessions/{id}/next` | close the open session at "now" and open the next episode |
| POST | `/api/sessions/{id}/ignore`, `/delete` | hide or remove a session |
| POST | `/api/items/{id}/delete` | remove one item |
| POST | `/api/push` | send pending items to Trakt and Simkl by ID (1.1 s spacing) |
| POST | `/api/verify` | check Trakt history (±12 h) and Simkl lists; marks items `added`, `ready` (exists there, not in your history yet) or `not_found` (title missing on that service) |
| POST | `/api/plex/import` | import Plex plays now |

### Auto-send

Every 2 minutes (`auto_loop`), closed, identified, non-ignored sessions with something still pending are sent like a scrobble would: first `api_verify` marks plays already in your history as added (no duplicates), then `api_push`. LG and Android TV sessions qualify only if `played_seconds` ≥ `auto_push_min_ratio` (0.8) × the TMDB runtime of their items; below that, or with an unknown runtime, they stay pending with the reason in the chip tooltip. Plex views always qualify (Plex marks them watched at ~90 %). `not_found` items are retried at most once a day. Disable with `[history] auto_push = false`.

### Plex

- **Import** (`plex_loop`, every `plex_interval` s): reads `metadata_item_views` from the Plex library DB (falls back to the HTTP history). Those rows keep the `guid`/`grandparent_guid` even after the file is deleted from disk, and the GUID resolves through `metadata.provider.plex.tv` to IMDb/TMDB/TVDB, so items arrive already identified. Runtime comes from the provider, falling back to TMDB.
- **Dedupe**: before pushing a Plex-sourced item to Trakt, the dashboard checks whether trakt-scrobbler already recorded it.
- **Now on Plex**: `/status/sessions` filtered to `plex_account_id` (and optionally `plex_user`), movies and episodes only, with real position, duration, state and player. Cached 3 s.
- **Match chips** on both now-playing cards: `Trakt ✓/✗`, `Simkl ✓/✗` = the title exists on that service (by external ID, cached 1 h), so a scrobble or push can land. On Plex, `● scrobbling to Trakt` appears when Trakt's `/users/me/watching` shows the same title, i.e. trakt-scrobbler is live.
- **Local ID fallback**: if Plex's metadata provider 404s on a new GUID, the IDs come from the library's own `tags` (type 314), completed with TMDB `external_ids`.

## Services

| | Linux | macOS | Windows |
|---|---|---|---|
| Capture + presence | `tvpresence.service` (systemd user, needs the graphical session for Discord IPC) | launchd agent `local.tvpresence` | Scheduled Task `tvpresence` at logon (`pythonw.exe`) |
| Dashboard | `tvhistory.service` (`default.target`) | launchd agent `local.tvhistory` | Scheduled Task `tvhistory` at logon |

Under `pythonw.exe` there is no console, so both scripts redirect output to `<data dir>/<name>.log`.
