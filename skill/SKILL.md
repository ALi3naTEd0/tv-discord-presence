---
name: scrobbler-dashboard
description: Operate Couchlog, the self-hosted TV watch-history stack — tvpresence (LG webOS TV + Android TV box → Discord Rich Presence + session capture), the Couchlog dashboard (:7790, tailnet https :8444, installable PWA) that identifies sessions and sends them to Trakt and Simkl by ID (auto-send when a session ends), the Plex importer and "Now on Plex" card, the Mi Box (ADB, HDMI-CEC, HDR/format info) and tvsync_reconcile.py for full Trakt↔Simkl reconciliation. Trigger when the user says "Couchlog", "dashboard de historial", "scrobbler dashboard", "qué estoy viendo", "no se scrobbleó", "no aparece en Trakt/Simkl", "identifica lo de HBO/Netflix/YouTube", "envía los pendientes", "revisa Trakt/Simkl", "importa de Plex", "now playing", "el Discord no muestra el título", "la Mi Box / Android TV", "resolución / HDR / códec", "el control de la LG", "concilia Trakt y Simkl", or invokes /scrobbler-dashboard.
---

# Couchlog (scrobbler dashboard)

Repo: `~/tv-discord-presence` (GitHub `ALi3naTEd0/tv-discord-presence`, public, MIT). Full docs live there:
`README.md`, `docs/architecture.md`, `docs/gotchas.md`, `docs/reconcile.md`. **Read `docs/gotchas.md` before debugging anything.**

**If `local.md` exists next to this file, read it first**: this machine's specifics (tailnet URL, `tailscale serve` map, which Discord app, config quirks). It is git-ignored on purpose. Other sessions may commit to this repo too: `git pull --rebase` before editing, and check `git log` for changes you did not make.

## Pieces

| Piece | Where |
|---|---|
| Capture + Discord presence | `tvpresence.py`, `systemctl --user … tvpresence` (venv `~/.local/share/tvpresence/venv`, has `aiowebostv`, `pypresence`, `adb-shell`) |
| Dashboard + Plex importer + auto-send | `tvhistory_dashboard.py` + `.html` + `static/`, `systemctl --user … tvhistory`, `http://127.0.0.1:7790` |
| Remote access | `https://<machine>.<tailnet>.ts.net:8444` via `tailscale serve`, tailnet only. **Never Funnel.** (exact URL in `local.md`) |
| LG webOS TV | SSAP (`tv_host`, key `key` in the config dir): app + playing/paused only |
| Android TV box | Mi Box over ADB (`[androidtv] host`, key `adbkey`): app, state, real position, title when published, resolution/codec. Reader: `AndroidTv` in `tvpresence.py` |
| Shared DB | `~/.local/share/tvpresence/history.db` (SQLite WAL); `source = tv` (LG) / `androidtv` / `plex` |
| Config | `~/.config/tvpresence/config.toml` (`[history]`, `[androidtv]`), secrets in `.env` or `[history] env_files` |
| Discord app | `client_id`: DRPP's public app or the user's own (see `local.md`). Plex presence comes from `tvpresence` too (TMDB posters, real position); DRPP is not used. Creating an app happens in the user's Discord account, so they do it |
| Full reconciliation | `tvsync_reconcile.py`, output in `reconcile/` (git-ignored, personal data; known mismatches in `reconcile/known-mismatches.json`) |

## Hard rules

1. **No writes to Trakt or Simkl without the user's explicit OK** for that specific push/delete, except the dashboard's own **auto-send** of identified sessions (user-approved 2026-11-20). `/api/push`, `apply --yes` and `delete --yes` all write. Verify (`/api/verify`) and dry runs are fine.
2. **Never match/identify automatically, not even via the API.** The user picks every match (YouTube/HBO titles are unreliable). Pre-filling the search, pre-checking an episode and offering chips is fine; calling `/identify` yourself is not, unless the user explicitly tells you which match to set.
3. **Never refresh the Trakt token**, and **never suggest stopping trakt-scrobbler**: it owns the token the dashboard reuses (single-use refresh tokens). If expired: the user runs `trakts auth`.
4. Never print tokens or `.env` contents.
5. Keep the dashboard bound to `127.0.0.1`; expose only via `tailscale serve`.
6. Device changes on the Mi Box (settings, reboots) and the LG: ask first, and warn when it interrupts playback (HDMI re-sync blanks the screen).
7. Do not commit or push unless asked.

## Dashboard — Couchlog

Logo `static/icon.svg`, favicon, `manifest.webmanifest`, pass-through `sw.js` served from `/`: installable as a PWA (Chrome *Install app*; iOS *Add to Home Screen*) over HTTPS (Tailscale URL) or localhost. UI in English, refreshes every 5 s.

- **Header:** stats incl. `auto-send on (≥80% watched)` · *Check Trakt/Simkl* (read-only) · *Import from Plex* · *Send pending (N)* (writes).
- **Device tabs:** All · LG TV · Android TV · Plex (remembered per browser). Each device has its own *Now on…* card.
- **Now on Plex:** poster, state, device, position bar, `Trakt ✓/Simkl ✓` match chips, `● scrobbling to Trakt` when trakt-scrobbler is live, and the file format from the Plex session (`4K · HEVC` `HDR10+` `EAC3 5.1` `direct play`/`transcode`, bitrate on hover). The box capture skips the Plex app, so this comes from the server, not ADB.
- **Now on LG TV / Android TV:** app, state, time (Android TV: real position, progress bar against the TMDB runtime, `4K · AV1` format chip). Unidentified → search pre-filled from the device's title (HBO-style apps: the **show** from the second line; YouTube: the title, with the second line as *Search instead* chips). Identified → what Discord shows, *Next episode*, *Change*.
- **Filters:** All · Unidentified · Pending · Sent · Ignored · **Short** (YouTube under 20 min, kept 7 days in case one was a short film).
- **Status chips:** `unchecked`, `to send`, `✓`, `✓ already there`, `not listed yet`, `held · N% watched` / `held · runtime unknown` (auto-send waiting; *Send* forces it), `error`.
- **Search:** TMDB + IMDb merged, Trakt/Simkl ✓/✗ per hit, IDs/links resolve directly. The **same work listed twice** (TMDB movie + one-episode series, same IMDb id) is one card; the main listing is the one on most services, or the **series** when the device reported an episode; the other is an *Also listed as… →* button.
- **Episode picker:** episodes merged from **TMDB + TVmaze** (`/api/show/{tmdb}/episodes`), season tabs; an episode the app reported (or picked earlier) that no catalog lists yet appears as a row "reported by …"; manual entry behind *+ Episode not listed?*. TheTVDB integration is pending a `TVDB_API_KEY` the user must request (free tier, attribution, needs approval).

## Capture and auto-send (what to expect)

- Sessions are per device. LG: no title ever. Android TV: Netflix and izzi publish no title; YouTube, HBO Max (episode · show), Spotify do.
- **YouTube** (LG + box) is recorded but kept only if ≥20 min or identified; shorter → *Short* 7 days, then purged. No automatic promotion.
- **Android TV resume:** coming back to the same title within 6 h reopens the old session (identification and time kept).
- **Autoplay next episode (Android TV, user-approved 2026-11-23):** an identified episode past 80 % of its TMDB runtime whose real position drops back under 5 min (without leaving the app), with at least 50 % of the runtime actually played in that session (a seek to the end does not count: Netflix can jump back to an earlier episode while you are away), is split onto the next episode automatically (`api_next(auto=True)`, `sessions.auto_next_from`). The Now card then offers **Change** (wrong episode or show) and **Undo** (same episode restarted: time merged back, new session dropped). Format (resolution/HDR) carries over. This is the one sanctioned automatic identification: it only follows a show the user identified.
- **Auto-send** every 2 min: closed + identified + something pending. LG/Android TV need ≥80 % of the TMDB runtime watched (Android TV counts the furthest real position); Plex always qualifies. Verify runs first, so nothing already present is re-sent; `not_found` retried once a day. Runtimes borrowed from a movie listing are estimates (`~`) and never gate auto-send.
- **Plex + Trakt:** trakt-scrobbler scrobbles live; the dashboard's Plex push dedupes against Trakt history (±12 h, by ID) and only fills what trakts missed. Simkl's own Plex webhook drops new titles without TVDB/IMDb ids; auto-send fills those.

## Mi Box / LG facts (verified 2026-11)

- **Remote (HDMI-CEC/SIMPLINK on):** LG remote arrows/OK/Back/play keys drive the box; **Home is the LG's** (switches input). Direct jump: long-press a number on the LG remote while on HDMI 1 to assign it. Box home: press Back repeatedly. If CEC is ever toggled on the box, **reboot the box** afterwards or audio breaks.
- **Volume:** box media volume on HDMI at 15/15 (verified); the LG sets the level.
- **Two Bluetooth headphones:** Android TV can't; pair both to the **LG** and disconnect them from the box, so the box's audio goes HDMI → LG → both.
- **HDR:** the box outputs **Always HDR** (its "Match content dynamic range" page only offers that; the internal switch needs root — don't). The LG's HDR picture mode therefore says nothing about the content. Real content data: resolution/codec from `dumpsys media.metrics` decoder records (after the app releases the decoder); **content dynamic range** from the Amlogic video driver's kernel log, `logcat -b kernel | grep 'DV: \['` → `[.., bitdepth, SDR|HDR10|HLG|DV, ..]`, one line per video start (an SDR trailer/intro often precedes the HDR title; the session keeps the best). Shown as an `HDR10`/`SDR` chip. The LG accepts HDR10 + HLG (no Dolby Vision/HDR10+). **4K needs *HDMI Ultra HD Deep Color* on for the box's LG input** (2026-11-23, box moved to HDMI 2: Netflix capped at 1080p until it was turned on). LG settings readable via SSAP `settings/getSystemSettings` (`other`/`uhdDeepColorHDMIn`, `simplinkEnable`); writable only through the `createAlert`→`closeAlert` luna trick (`com.webos.settingsservice/setSystemSettings`); `tvpower/power/reboot` is blocked on this firmware.
- **Streaming catalogs via VPN (US):** Netflix, Tubi, Plex free, YouTube free, HBO follow the IP; Disney+'s Hulu content needs a US Disney+/Hulu bundle (the "Hulu" hub in a LatAm Disney+ is the LatAm catalog).

## Common tasks

```bash
systemctl --user is-active tvpresence tvhistory
journalctl --user -u tvpresence -n 30 --no-pager
journalctl --user -u tvhistory -n 30 --no-pager
curl -s 127.0.0.1:7790/api/state | python3 -m json.tool | head -80   # nows, sessions, pending, plex_now
H=(-H 'Content-Type: application/json' -H 'Origin: http://127.0.0.1:7790')
curl -s "${H[@]}" -X POST 127.0.0.1:7790/api/verify -d '{}'          # read-only
curl -s "${H[@]}" -X POST 127.0.0.1:7790/api/push -d '{"session_ids":[ID],"dry_run":true}'   # preview
```

Mi Box over ADB (read-only checks): use the venv's `adb_shell` with `~/.config/tvpresence/adbkey`; useful dumps: `dumpsys media_session` (state/title), `dumpsys activity activities | grep mResumedActivity` (app), `dumpsys media.metrics | grep mediacodec.height` (resolution/codec), `dumpsys hdmi_control` (CEC), `dumpsys audio` (volume).

**Full Trakt↔Simkl reconciliation:** `docs/reconcile.md`. `report` → show the user → `apply BATCH` (dry run) → `--yes` only on their OK. Simkl free: ~500 writes/day.

## Troubleshooting map

| Symptom | Look at |
|---|---|
| Nothing recorded | `tvpresence` logs; device IP; app in `exclude_apps`; < 120 s dropped; YouTube < 20 min → *Short* |
| App shown with a weird name ("Byod") | add the package to `ANDROID_APPS` in `tvpresence.py` (izzi was one) |
| Two shows merged in one session | LG autoplay without menu gap → *Next episode* |
| Not auto-sent | chip `held · N%` (under 80 %) or `runtime unknown`; or Trakt `not listed yet` |
| Episode missing in the picker | TMDB/TVmaze lag: use the reported row or manual entry; TVDB needs a key |
| Trakt 403 `error code: 1010` | missing custom User-Agent |
| Trakt 401 | token expired → user runs `trakts auth` |
| Simkl 201 but nothing added | read `not_found`; `plantowatch` = Simkl thinks it's unreleased (fix the date on TMDB) |
| Plex not in Discord | `tvpresence` shows Plex from `/api/plex_now` (dashboard must be up) only when the LG has no app/console in front; `[plex] presence`. DRPP is retired (it needed Litterbox to upload posters and shares the Discord app): keep `drpp.service` stopped |
| Wrong match by trakt-scrobbler | its remap file / cache `~/.local/share/trakt-scrobbler/trakt_cache.json`; back up, fix only the wrong keys, with the user's OK |
| No sound on the box after a settings change | CEC toggled without reboot → reboot the box |

## Editing the code

Stdlib only for the dashboard (`/usr/bin/python3`). After edits: `python3 -c "import ast;ast.parse(open(F).read())"`, restart `tvhistory` / `tvpresence`, check the journal, and check the UI in the browser pane. UI, comments and docs are English. Never put a `<button>` inside the result `<button>` (use a `span[role=button]`).
