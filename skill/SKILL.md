---
name: scrobbler-dashboard
description: Operate the self-hosted TV watch-history stack — tvpresence (LG webOS TV → Discord Rich Presence + session capture), the TV History dashboard (:7790, tailnet https :8444) that identifies sessions and pushes them to Trakt and Simkl by ID, the Plex importer and "Now on Plex" card, and tvsync_reconcile.py for full Trakt↔Simkl reconciliation. Trigger when the user says "dashboard de historial", "scrobbler dashboard", "qué estoy viendo", "no se scrobbleó", "no aparece en Trakt/Simkl", "identifica lo de HBO/Netflix", "envía los pendientes", "revisa Trakt/Simkl", "importa de Plex", "now playing de Plex", "el Discord no muestra el título", "concilia Trakt y Simkl", or invokes /scrobbler-dashboard.
---

# Scrobbler dashboard

Repo: `~/tv-discord-presence` (GitHub `ALi3naTEd0/tv-discord-presence`). Full docs live there:
`README.md`, `docs/architecture.md`, `docs/gotchas.md`, `docs/reconcile.md`. Read `docs/gotchas.md` before debugging anything.

**If `local.md` exists next to this file, read it first**: it holds this machine's specifics (tailnet URL, current `tailscale serve` map, which Discord app is in use, config quirks). It is git-ignored on purpose.

## Pieces

| Piece | Where |
|---|---|
| Capture + Discord presence | `tvpresence.py`, `systemctl --user … tvpresence` (venv in `~/.local/share/tvpresence/venv`) |
| Dashboard + Plex importer | `tvhistory_dashboard.py` + `.html`, `systemctl --user … tvhistory`, `http://127.0.0.1:7790` |
| Remote access | `https://<machine>.<tailnet>.ts.net:8444` via `tailscale serve`, tailnet only. **Never Funnel.** (exact URL in `local.md`) |
| Android TV | Mi Box over ADB (`[androidtv] host` in config, key `adbkey`); `source = androidtv` sessions, own dashboard tab. Reader: `AndroidTv` in `tvpresence.py`. Never toggle HDMI-CEC on the box without SIMPLINK on in the LG (kills audio; see gotchas). |
| Shared DB | `~/.local/share/tvpresence/history.db` (SQLite WAL) |
| Config | `~/.config/tvpresence/config.toml` (`[history]` section), secrets in `~/.config/tvpresence/.env` |
| Discord app | `client_id` in config: either DRPP's public app or the user's own (see `local.md`). If presence breaks or reads "Plex", see README → Discord application; creating an app happens in the user's Discord account, so they do it. |
| Full reconciliation | `tvsync_reconcile.py`, output in `reconcile/` (git-ignored, personal data) |

## Hard rules

1. **No writes to Trakt or Simkl without the user's explicit OK** for that specific push/delete. `/api/push`, `apply --yes` and `delete --yes` all write. Verify (`/api/verify`) and dry runs are fine.
2. **Never refresh the Trakt token.** It belongs to trakt-scrobbler (single-use refresh tokens). If expired: tell the user to run `trakts auth`.
3. Never print tokens or `.env` contents.
4. Keep the dashboard bound to `127.0.0.1`; expose only via `tailscale serve`.
5. Do not commit or push unless asked.

## Dashboard (UI)

Local `http://127.0.0.1:7790`; from phone/other devices the tailnet URL on port 8444 (see `local.md`). UI is English, refreshes every 5 s.

- **Header buttons:** *Check Trakt/Simkl* (verify, read-only) · *Import from Plex* · *Send pending (N)* (writes — only with the user's OK).
- **Now on Plex** card: poster, playing/paused, device, real position bar, minutes left, `Trakt ✓ / Simkl ✓` match chips and `● scrobbling to Trakt` when trakt-scrobbler is live. Account 1 only. The Now on TV card shows the same match chips once identified.
- **Now on TV** card: app + state + played minutes. Unidentified → inline search box. Identified → what Discord shows, plus *Next episode* (split when the app autoplayed the next one) and *Change*.
- **Filters:** All · Unidentified · Pending · Sent · Ignored. Sessions grouped by day.
- **Per-session chips** per service: `unchecked`, `to send` (exists there, not in history), `✓`, `✓ already there`, `not listed yet` (title missing on that service), `error` (hover for the message).
- **Per-session actions:** Identify/Change (opens the picker) · Send · Ignore/Restore · Delete (deleting locally does **not** remove it from Trakt/Simkl).
- **Search/picker:** TMDB + IMDb merged, each hit with Trakt/Simkl ✓/✗ badges; pasting an IMDb/TMDB/TVDB link or ID resolves directly. Picking a show collapses the list ("← other results") and shows season + episode checkboxes; Plex hints pre-select them. *Not listed anywhere: manual entry* for things only on TVDB or nowhere.

## Tailscale exposure

Served with `tailscale serve`, **never** `tailscale funnel` (it carries the user's full watch history and can write to Trakt/Simkl). The machine's current serve/Funnel map is in `local.md`; check it before touching anything.

```bash
tailscale serve status                                   # check; 8444 must say "(tailnet only)"
tailscale serve --bg --https=8444 http://127.0.0.1:7790  # (re)create
tailscale serve --https=8444 off                         # remove
```
- The dashboard stays bound to `127.0.0.1`; Tailscale proxies to it. Don't change `dashboard_bind` to `0.0.0.0`.
- Its CSRF guard accepts the Tailscale host via `X-Forwarded-Host`; if POSTs return `403 Origin not allowed` behind serve, check that header.
- Never touch other entries in the serve config (other projects may be on Funnel) and never run `tailscale funnel` on 8444. Changing serve config is a persistent change: ask first.

## Common tasks

**Health check**
```bash
systemctl --user is-active tvpresence tvhistory
journalctl --user -u tvpresence -n 30 --no-pager
journalctl --user -u tvhistory -n 30 --no-pager
curl -s 127.0.0.1:7790/api/state | python3 -m json.tool | head -80
```
`/api/state` has `now` (TV session open now), `plex_now` (Plex playback for the owner account), `sessions`, `pending` counts and `plex` import status.

**"What am I watching / it doesn't show the title"** — webOS never gives titles. If `now` has no item, the user identifies it in the dashboard (or you do it via the API after confirming the pick). Discord picks up the title on the next tvpresence tick.

**Identify a session from the CLI** (POSTs need JSON + same-origin header):
```bash
H=(-H 'Content-Type: application/json' -H 'Origin: http://127.0.0.1:7790')
curl -s "127.0.0.1:7790/api/search?q=The%20Hotline" | python3 -m json.tool | head -40
# movie:   {"kind":"movie","tmdb":12345}
# episodes:{"kind":"tv","tmdb":67890,"season":1,"episodes":[2,3]}   (imdb/tvdb/title/year optional)
curl -s "${H[@]}" -X POST 127.0.0.1:7790/api/sessions/ID/identify -d '{"kind":"tv","tmdb":67890,"season":1,"episodes":[2]}'
```
Identify only writes the local DB (then runs a background verify). Prefer letting the user click in the UI unless they ask you to do it.

**"Wasn't scrobbled / not on Trakt or Simkl"**
1. Run verify: `curl -s "${H[@]}" -X POST 127.0.0.1:7790/api/verify -d '{}'`.
2. Read statuses: `added` (there), `ready` (title exists on the service, not in history → can push), `not_found` (title missing on that service — wait or request it there), `error`.
3. Only after the user says so: `POST /api/push` with `{"session_ids":[...], "targets":["trakt","simkl"]}`; `{"dry_run":true}` previews without writing.
4. If it came from Plex, remember trakt-scrobbler may already have scrobbled it; the push dedupes Trakt for Plex items.
5. Trakt only writes history on a stop at ≥80 %; partial plays sit in `/sync/playback`.

**Plex** — import runs every 5 min; force it with `POST /api/plex/import`. "Now on Plex" reads `/status/sessions` filtered to account 1.

**Full Trakt↔Simkl reconciliation** — see `docs/reconcile.md`. Always `report` → show the user → `apply BATCH` (dry run) → apply `--yes` only on their OK. Respect Simkl's ~500 writes/day.

## Troubleshooting map

| Symptom | Look at |
|---|---|
| Nothing recorded | `tvpresence` logs; TV IP; app in `exclude_apps`; session < 120 s is dropped |
| Two shows merged in one session | autoplay without menu gap → "Siguiente episodio" button / `/api/sessions/ID/next` |
| Trakt 403 `error code: 1010` | missing custom User-Agent |
| Trakt 401 | token expired → user runs `trakts auth` |
| Simkl says 201 but nothing added | read `not_found` in the response |
| Plex item shows 0 min | provider has no runtime yet; TMDB fallback fills it later |
| Wrong match by trakt-scrobbler | its permanent cache `~/.local/share/trakt-scrobbler/trakt_cache.json`; back up, fix only the wrong keys, with the user's OK |

## Editing the code

Stdlib only for the dashboard (`/usr/bin/python3`). After edits: `python3 -c "import ast;ast.parse(open(F).read())"`, then `systemctl --user restart tvhistory` (or `tvpresence`) and check the journal. UI, code comments and docs are all English.
