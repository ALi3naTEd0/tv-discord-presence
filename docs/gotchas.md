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
- `/status/sessions` includes every account on the server and music; filter by `User.id` and type.
- The Plex webOS app (`cdp-30`) is ignored by the TV recorder to avoid duplicates.

## HBO Max

- There is no watch-history endpoint at all (only *Continue Watching*, ~90 days). The legacy `api.hbo.com` endpoints browser scrobblers used are dead. The TV recorder + dashboard is the workaround.
