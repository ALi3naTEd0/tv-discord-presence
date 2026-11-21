# Gotchas

Things that cost real debugging time. Check here first.

## LG webOS

- `com.webos.media/getForegroundAppInfo` + `media_state` only give `appId`, `playState`, `type`, `mediaId`, `windowId`. **No title, no position, no duration** for third-party apps. That is why sessions need identifying.
- `mediaId` changes when the content changes, but also on some in-app navigation; the recorder only trusts it after the player was left.
- Hulu in Mexico lives inside the Disney+ app and shows up as Disney+.

## Trakt

- Always send a custom `User-Agent`. urllib's default gets Cloudflare `403 error code: 1010`.
- `/sync/watched` is truncated (100 items, no seasons for some shapes). Use paginated `/sync/history`.
- Only a scrobble **stop at ≥ 80 %** writes history. Below that it stays in `/sync/playback` (resume points), which is why "Trakt notified me but nothing is in my history".
- Titles that exist on TMDB/IMDb/TVDB can still be missing on Trakt for days; search does not "pull" them on demand and VIP does not change that. Mark them `not_found` and push later.
- **Never refresh the token from this project.** Refresh tokens are single-use and trakt-scrobbler owns them; refreshing from two places breaks its session.

## trakt-scrobbler (`trakts`)

- It covers **Plex only** here, not the TV apps. The Universal Trakt Scrobbler browser extension is a different thing.
- `get_trakt_id(title, item_type, year)` takes the top fuzzy hit; its score guard is ineffective, so similar titles mis-match.
- Its cache (`~/.local/share/trakt-scrobbler/trakt_cache.json`) is permanent; `-1` means "no match". Wrong entries stick until removed by hand (back it up first).
- `item_type` must be `episode` or `movie`. Passing `show` silently writes bad cache entries.
- `422` lines in its log are benign (duplicate scrobble).

## Simkl

- `client_id` goes in the query string as well as the header.
- `POST /sync/history` returns `201` even when nothing matched — always read `not_found` in the body.
- ~1 POST/s; free tier ~500 writes/day; no rewatches.
- A title with **no release date on TMDB** is `upcoming` on Simkl; pushing it as watched returns `plantowatch` and only adds it to *Plan to Watch*. Fix the date on TMDB first, wait for Simkl to refresh, then push again.
- Completed shows do not list their episodes in some list endpoints; use `/sync/all-items/{movies,shows,anime}?extended=full&episode_watched_at=yes`.

## Plex

- `metadata_item_views` keeps `guid`/`grandparent_guid` after files are deleted; resolve with `https://metadata.provider.plex.tv/library/metadata/<id>` to get IMDb/TMDB/TVDB. That is why the importer runs every few minutes: it survives deletions, but the HTTP history alone does not.
- Brand-new titles can get a Plex GUID that `metadata.provider.plex.tv` still 404s, and episodes that only carry a `tmdb://` ID (no TVDB/IMDb). Simkl's Plex webhook then fails to match them silently; refreshing the show's metadata in Plex later usually fills the missing IDs.
- `/status/sessions` includes every account on the server and music; filter by `User.id` and type.
- The Plex webOS app (`cdp-30`) is ignored by the TV recorder to avoid duplicates.

## Android TV (Mi Box)

- **HDMI-CEC (LG remote controlling the box):** the box reads its CEC *physical address* from the TV only at boot or HDMI hotplug. Turning `hdmi_control_enabled` on at runtime leaves it at `0xFFFFFFFF` (no address), and in that half state Netflix lost its sound. Fix: `settings put global hdmi_control_enabled 1`, then **reboot the box**; it comes back with `physical_address: 0x1000` and the TV's key presses show up as `<User Control Pressed>` in `dumpsys hdmi_control`. SIMPLINK must be on in the LG (it was, the whole time).
- YouTube uploads of films often carry a misleading first line and the real title in the second one (e.g. `Bodega El Capricho de José Gordón` · `nº1 en Steak Revolution` = *Steak (R)evolution*). Both lines are stored (`hint_title`, `hint_extra`) and the now card offers the second line as one-click searches. Never match automatically from these titles: the user decides.
- HBO Max (and likely Disney+ / Prime Video) publish the **episode** name first and the **show** second (`Duty Calls` · `The Mad Pooper`). The dashboard pre-fills the search with the show for those apps and pre-checks the episode whose TMDB name matches; the user still confirms.
- Netflix registers a media session but with `metadata: null`: state and position only, no title.

## HBO Max

- There is no watch-history endpoint at all (only *Continue Watching*, ~90 days). The legacy `api.hbo.com` endpoints browser scrobblers used are dead. The TV recorder + dashboard is the workaround.

## Borrowed runtimes are estimates (2026-11-20)

When an episode has no runtime on TMDB, the show's typical length is used; failing that, for a
one-episode series, the runtime of TMDB's *movie* listing with the same IMDb id. That last one is
often a placeholder (The Mad Pooper S1E1: a 120-min movie listing for a docuseries episode, so
Discord showed an end of 2:00:00 and auto-send saw 15 %). Such items get `items.runtime_est = 1`:
Discord shows elapsed time only, the Now card says `of ~120 min`, and auto-send skips them with a
note ("send it if you finished it"). The Mi Box media session does not expose the duration
(`dumpsys media_session` prints position and title only), so there is no device-side fallback.

## Played time vs. position on Android TV (2026-11-20)

`played_seconds` adds up every playing interval of a session. Coming back to the same title within
6 h resumes the session, so restarting the episode (or rewinding) counted the same minutes twice:
18 min watched, back to YouTube, then the episode from 0 again showed "42 min played" at 23:00.
`sessions.position` keeps the furthest point reached and `sessions.live_position` the last sample.
For an Android TV session with one title the furthest point is the watched amount (list rows and
auto-send); the Now card shows the live position (`at 24:10 of ~120 min`) plus `reached …` when you
went back more than a minute.
