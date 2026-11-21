# TV Discord Presence + TV History

A small self-hosted stack for people who watch on an **LG webOS TV** and track what they watch on **Trakt** and **Simkl**:

| Piece | What it does |
|---|---|
| `tvpresence.py` | Reads the foreground app and play/pause state from the TV, shows it as Discord Rich Presence (`Watching HBO Max`), and records every viewing session into a local SQLite history. |
| `tvhistory_dashboard.py` + `.html` | Local web dashboard (port 7790) to identify those sessions (search TMDB + IMDb at once, or paste an ID/link), push them to Trakt and Simkl **by external ID**, verify they landed, import Plex plays, and show a live *Now on Plex* card. Once a session is identified, the Discord presence shows the real title and poster. |
| `tvhistory_db.py` | Shared SQLite schema (`~/.local/share/tvpresence/history.db`). The database is the source of truth between the two processes. |
| `tvsync_reconcile.py` | One-off CLI that reconciles the **full** Trakt ↔ Simkl history, with Plex as a witness. Dry-run by default. |

Why: webOS never exposes *what* is playing inside Netflix/HBO Max/Disney+, and scrobblers miss things or match the wrong title. This keeps your own record so nothing is lost, and lets you send it later once the title exists on Trakt/Simkl.

```
Watching HBO Max                  Watching HBO Max
Playing on TV · 00:12:34    →     The Hotline
                                  Playing on TV · 00:12:34   [poster]
   (unidentified)                    (after picking it in the dashboard)
```

## Requirements

- Python ≥ 3.11 on Linux, macOS or Windows (see [Platforms](#platforms)). The dashboard is stdlib only; the presence daemon uses a venv.
- LG webOS TV on the same network (see [TV connection](#tv-connection)).
- Discord desktop running on the same machine, with Settings → Activity Privacy → *Share your detected activities with others* turned on. You also need a Discord **Application ID** (`client_id`); see [Discord application](#discord-application).
- A TMDB API key.
- For pushing: [trakt-scrobbler](https://github.com/iamkroot/trakt-scrobbler) installed with pipx and authenticated (the dashboard reuses its client id and token), and a Simkl app + token.
- Optional: a local Plex Media Server.

## Platforms

| | Linux | macOS | Windows |
|---|---|---|---|
| Status | used daily | should work, **untested** | should work, **untested** |
| Installer | `./install.sh` (systemd user services) | `./install.sh` (launchd agents) | `install.ps1` (Scheduled Tasks at logon) |
| Config dir (`config.toml`, `.env`, TV key) | `~/.config/tvpresence` | `~/Library/Application Support/tvpresence` | `%APPDATA%\tvpresence` |
| Data dir (`history.db`, venv, logs) | `~/.local/share/tvpresence` | same as config | `%LOCALAPPDATA%\tvpresence` |
| Plex DB default | `/var/lib/plex/…` | `~/Library/Application Support/Plex Media Server/…` | `%LOCALAPPDATA%\Plex Media Server\…` |

`TVPRESENCE_CONFIG_DIR` / `TVPRESENCE_DATA_DIR` override both directories. trakt-scrobbler's venv is found through `pipx environment` (override with `[history] trakt_python`), and its token location is asked to trakt-scrobbler itself, so it follows that tool's own per-platform paths. If the Plex DB is not readable, the importer falls back to Plex's HTTP history (works, but cannot recover IDs of files you already deleted).

## Install

**Linux / macOS**

```bash
git clone https://github.com/ALi3naTEd0/tv-discord-presence ~/tv-discord-presence
cd ~/tv-discord-presence && ./install.sh
```

**Windows** (PowerShell)

```powershell
git clone https://github.com/ALi3naTEd0/tv-discord-presence $HOME\tv-discord-presence
cd $HOME\tv-discord-presence
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

Then, in the config dir from the table above:

1. Edit `config.toml`: `tv_host` and `client_id` (see [Discord application](#discord-application)).
2. Create `.env` with `TMDB_API_KEY=...` and, if you use Plex, `PLEX_TOKEN=...`. Keys can also come from environment variables or other `.env` files listed in `[history] env_files`.
3. Restart the services (the installer prints the exact command for your OS).

The first connection shows a pairing prompt on the TV; accept it (details in [TV connection](#tv-connection)).

Open <http://127.0.0.1:7790>. To reach it from your phone over Tailscale (tailnet only, **not** Funnel):

```bash
tailscale serve --bg --https=8444 http://127.0.0.1:7790
tailscale serve --https=8444 off               # to remove it
```

## TV connection

There is no hack involved: `tvpresence` uses webOS's local control API (SSAP), the same one LG's remote apps and Home Assistant talk to, through the [aiowebostv](https://github.com/home-assistant-libs/aiowebostv) library.

**On the TV:** nothing to configure on recent models; the control API is on by default. Just give the TV a fixed IP (a DHCP reservation in your router) and put it in `tv_host`.

Only if the connection is refused: look for **LG Connect Apps** (older webOS, under *Network*) or *Mobile TV On* / *External Devices* (newer webOS) and make sure it is on.

**Pairing (first run):**

- `tvpresence` opens a WebSocket to the TV: `wss://<tv>:3001` (TLS with the TV's self-signed certificate, so it is not verified), falling back to `ws://<tv>:3000` on older firmware.
- It registers as a client and the TV shows an **"Allow this device to connect?"** prompt. Accept it with the remote; the first attempt waits up to 60 s for you.
- The TV answers with a **client key**. It is saved as `key` in the config dir (mode 600) and sent on every later connection, so the prompt never shows again. Delete that file to pair again (e.g. after a factory reset of the TV).

**What it reads** (two subscriptions; the TV pushes changes):

- `com.webos.applicationManager/getForegroundAppInfo` → which app is in front (`netflix`, `com.hbo.hbomax`, …).
- `com.webos.media/getForegroundAppInfo` → for that app: `playState` (playing / paused), `mediaId` and `type`.

That is all webOS exposes for third-party apps: **no title, no position, no duration**. Hence the dashboard to identify what was playing. It never sends commands to the TV (no power, volume or input changes).

## Android TV

An Android TV / Google TV box (tested: Xiaomi Mi Box, Android 11) is read over **ADB on the network** with [adb-shell](https://github.com/JeffLIrion/adb_shell), the library Home Assistant uses. It gets its own session (`source = androidtv`) next to the LG, and its own tab in the dashboard.

**On the box, once:** Settings → Device Preferences → About → press *Build* 7 times; then Developer options → **USB debugging** on (on most boxes that also opens ADB on port 5555 over the network). Set `[androidtv] host` to the box's IP and restart `tvpresence`. The first connection shows **"Allow USB debugging?"** on the TV: tick *Always allow from this computer*. The key is saved as `adbkey` in the config dir.

**What it reads:** the foreground app (`dumpsys activity`), the screen state (`dumpsys power`), and the app's **media session** (`dumpsys media_session`): playing/paused, position and, when the app publishes it, **title and subtitle**. That title is stored as a hint and pre-fills the picker. Netflix does not publish a title (only state and position); music apps like Spotify do.

**YouTube** (on the box and on the LG) is recorded too, but a session is only kept if it ran **20+ minutes** or you identified it, so documentaries and films posted on YouTube can be matched and sent to Trakt/Simkl. Shorter ones are not deleted right away: they sit under the **Short** filter for 7 days in case one was a short film, and on the box a title that exactly matches a TMDB title is moved back to the main list automatically. On the box the picker is pre-filled with the video title; the LG gives no title, so you type it.

**Discord:** when an app publishes its title (YouTube: video + channel), Discord shows it directly, even for apps that are not recorded in the history. The box's activity is shown when the LG itself has nothing streaming in front (e.g. the LG is on the box's HDMI input). Plex and Spotify on the box are never shown or recorded (DRPP and the Plex importer cover Plex).

## Discord application

Rich Presence always runs under a Discord *application*, identified by its Application ID (`client_id`). The app itself does almost nothing here: the code overrides the activity name (`Watching HBO Max`) and passes logos and posters as URLs, so the app needs **no** uploaded art and no bot. Its own name only shows up as a fallback.

**Option A — reuse DRPP's app (what this setup uses today).** If you already run [DRPP](https://github.com/phin05/discord-rich-presence-plex), its public Application ID `413407336082833418` (named "Plex") works as is:

```toml
client_id = "413407336082833418"
```

How it works:

1. `tvpresence` uses [pypresence](https://github.com/qwertyquerty/pypresence) to open Discord's **local IPC socket** (`$XDG_RUNTIME_DIR/discord-ipc-0`) and handshakes with the `client_id`. Nothing goes through the internet and there is no login: Discord only checks that an application with that ID exists, not who owns it. That is why any public Application ID works, DRPP's included.
2. Every update (`SET_ACTIVITY`) sends everything the activity needs, so the app's own settings are never used:
   - `type = WATCHING` and `name = "HBO Max"` → Discord shows **Watching HBO Max** instead of the app's registered name ("Plex").
   - `status_display_type` → what the member list shows: the app name, or the episode/movie title once identified.
   - `details` / `state` → the two text lines (`Watching HBO Max` · `Playing on TV`, or title · `S1 · E2 — name`).
   - `large_image` / `small_image` as **external URLs** (app logo, TMDB poster). Discord fetches them through its media proxy, so the app needs no uploaded Rich Presence assets.
   - `start` → the elapsed-time counter (dropped while paused).
3. DRPP and `tvpresence` each hold their own IPC connection and set their own activity. They do not step on each other in practice because `tvpresence` ignores the Plex app on the TV (`cdp-30`): Plex is DRPP's job, everything else is `tvpresence`'s.

Trade-off: it is someone else's app. If its owner deletes it or changes its ID, the handshake fails and the presence stops (the service keeps retrying every 30 s and history recording is unaffected). Any Discord client that ignores the name override shows "Plex".

**Option B — your own app (2 minutes, recommended long term).**

1. Open <https://discord.com/developers/applications> and sign in with your Discord account.
2. **New Application** → name it (e.g. `TV`) → accept the terms → **Create**.
3. On **General Information**, copy the **Application ID**. (An icon is optional; nothing else needs configuring: no bot, no OAuth2, no Rich Presence assets.)
4. Put it in `~/.config/tvpresence/config.toml` as `client_id = "…"` (or export `TVPRESENCE_CLIENT_ID`), then `systemctl --user restart tvpresence`.

The Application ID is not a secret: it is visible to anyone who sees your activity.

## Credentials

Nothing secret lives in this repo. The dashboard reads:

| Service | Where |
|---|---|
| TMDB | `TMDB_API_KEY` env var, `.env` in the config dir, or a file in `[history] env_files` |
| Trakt | client id and token location asked to trakt-scrobbler (its pipx venv; `trakt_token.json` in its data dir). **Never refreshed here** — refresh tokens are single-use and trakt-scrobbler owns them. If expired, run `trakts auth` or let trakt-scrobbler renew it. |
| Simkl | `SIMKL_CLIENT_ID` in `<simkl_dir>/.env`, token in `<simkl_dir>/token.json` (`simkl_dir` defaults to `~/.config/simkl`) |
| Plex | `PLEX_TOKEN`, looked up like the TMDB key. Plays are read straight from the library DB when it is readable, otherwise over HTTP. |

## Configuration

See [`config.example.toml`](config.example.toml). Top-level keys configure the TV/Discord side; everything under `[history]` is optional.

| Key | Default | Description |
|---|---|---|
| `tv_host` | `192.168.1.220` | TV IP |
| `client_id` | — | Discord Application ID |
| `poll_seconds` | `5` | TV polling interval |
| `ignore_apps` | `["cdp-30"]` | appIds never shown (Plex is handled by the Plex importer) |
| `only_known` | `false` | Only apps in the built-in catalog |
| `show_when_browsing` | `false` | Show presence even when nothing is playing |

## Docs

- [docs/architecture.md](docs/architecture.md) — data flow, session rules, database, dashboard API.
- [docs/gotchas.md](docs/gotchas.md) — hard-won quirks of webOS, Trakt, Simkl, Plex and trakt-scrobbler.
- [docs/reconcile.md](docs/reconcile.md) — full-history Trakt ↔ Simkl reconciliation.
- [skill/SKILL.md](skill/SKILL.md) — Claude Code skill (`/scrobbler-dashboard`) to operate all of this.

## Operation

Linux:

```bash
systemctl --user status tvpresence tvhistory
journalctl --user -u tvpresence -f              # capture + presence
journalctl --user -u tvhistory -f               # dashboard, Plex import, pushes
```

macOS: `launchctl print gui/$(id -u)/local.tvpresence`, logs in `~/Library/Application Support/tvpresence/*.log`.
Windows: Task Scheduler (`tvpresence`, `tvhistory`), logs in `%LOCALAPPDATA%\tvpresence\*.log`.

Debug run in the foreground (any OS): `TVPRESENCE_DEBUG=1 <venv python> tvpresence.py`.

## License

[MIT](LICENSE)
