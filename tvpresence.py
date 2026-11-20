#!/usr/bin/env python3
"""TV Discord Presence — shows in Discord which streaming app is open on an LG webOS TV,
and records viewing sessions for the TV History dashboard."""
from __future__ import annotations

import asyncio
import logging
import os
import sys
import time
import tomllib
from pathlib import Path

from aiowebostv import WebOsClient
from pypresence import AioPresence
from pypresence.types import ActivityType, StatusDisplayType

import tvhistory_db as db

CONFIG_DIR = db.CONFIG_DIR
CONFIG_FILE = CONFIG_DIR / "config.toml"
KEY_FILE = CONFIG_DIR / "key"

FAVICON = "https://t0.gstatic.com/faviconV2?client=SOCIAL&type=FAVICON&fallback_opts=TYPE,SIZE,URL&url=https://{}&size=256"

# webOS appId -> (name, domain for the logo)
KNOWN_APPS: dict[str, tuple[str, str]] = {
    "netflix": ("Netflix", "www.netflix.com"),
    "com.wbd.stream": ("HBO Max", "www.hbomax.com"),
    "com.hbo.hbomax": ("HBO Max", "www.hbomax.com"),
    "com.disney.disneyplus-prod": ("Disney+", "www.disneyplus.com"),
    "hulu": ("Hulu", "www.hulu.com"),
    "amazon": ("Prime Video", "www.primevideo.com"),
    "com.apple.appletv": ("Apple TV", "tv.apple.com"),
    "youtube.leanback.v4": ("YouTube", "www.youtube.com"),
    "vix": ("ViX", "vix.com"),
    "com.clarovideov9": ("Claro Video", "www.clarovideo.com"),
    "com.mercadolibre.mercadoplay": ("Mercado Play", "play.mercadolibre.com.mx"),
    "com.tubitv.ott.tubi": ("Tubi", "tubitv.com"),
    "tv.twitch.tv.starshot.lg": ("Twitch", "www.twitch.tv"),
    "com.izzi.tv": ("izzi tv", "www.izzi.mx"),
}

DEFAULT_CONFIG = {
    "tv_host": "192.168.1.220",
    "client_id": "",
    "poll_seconds": 5,
    # Apps never shown (Plex is covered by the Plex importer / DRPP)
    "ignore_apps": ["cdp-30"],
    # Only show apps in KNOWN_APPS (true) or any non-system app (false)
    "only_known": False,
    # Also show when the app is open but nothing is playing
    "show_when_browsing": False,
}

# Local history of what plays on the TV (identified in the dashboard)
DEFAULT_HISTORY = {
    "enabled": True,
    # Apps not recorded (not movies/shows)
    "exclude_apps": ["youtube.leanback.v4", "tv.twitch.tv.starshot.lg"],
    # Sessions shorter than this are dropped (trailers, menu autoplay)
    "min_seconds": 120,
    # Paused / in menus longer than this ends the session
    "pause_timeout": 1800,
    # No streaming app in front (TV off, other input) longer than this closes it
    "close_grace": 60,
    # After a service restart, resume the open session if seen less than this ago
    "resume_window": 600,
}
MAX_TICK_GAP = 30  # s; after suspend or a dropout the gap is not counted as playback

SYSTEM_PREFIXES = ("com.webos.", "com.lge.", "lge.", "com.lg.", "webos.")

log = logging.getLogger("tvpresence")


def load_config() -> dict:
    cfg = dict(DEFAULT_CONFIG)
    if CONFIG_FILE.exists():
        with CONFIG_FILE.open("rb") as f:
            cfg.update(tomllib.load(f))
    env_id = os.environ.get("TVPRESENCE_CLIENT_ID")
    if env_id:
        cfg["client_id"] = env_id
    return cfg


class HistoryRecorder:
    """Turns the TV state into playback sessions in the database."""

    def __init__(self, cfg: dict):
        h = {**DEFAULT_HISTORY, **(cfg.get("history") or {})}
        self.exclude = set(h["exclude_apps"])
        self.min_seconds = float(h["min_seconds"])
        self.pause_timeout = float(h["pause_timeout"])
        self.close_grace = float(h["close_grace"])
        self.conn = db.connect()
        closed, resumed = db.recover(self.conn, self.min_seconds, float(h["resume_window"]))
        if closed:
            log.info("History: closed %d open session(s) on startup", closed)
        if resumed:
            log.info("History: session %d resumed after restart", resumed)
        self._last_mono: float | None = None
        self._prev_playing = False
        self._idle_since: float | None = None
        self._left_player = False  # the app is in front but has no media (in its menu)

    def _close(self, sess, why: str) -> None:
        db.close_session(self.conn, sess["id"], self.min_seconds)
        log.info("History: session %d closed (%s)", sess["id"], why)
        self._prev_playing = False
        self._left_player = False

    def tick(self, info, media_id: str | None = None):
        now, mono = time.time(), time.monotonic()
        gap = 0.0 if self._last_mono is None else mono - self._last_mono
        self._last_mono = mono
        sess = db.get_open_session(self.conn)

        # No streaming app in front: tolerated for a while (app switches, network drops)
        if info is None or info[0] in self.exclude:
            self._prev_playing = False
            self._left_player = bool(sess)  # leaving the app also means leaving the player
            self._idle_since = self._idle_since or mono
            if sess and mono - self._idle_since > self.close_grace:
                self._close(sess, "no streaming app")
                return None
            return sess
        self._idle_since = None

        app_id, name, logo, play = info
        playing = play == "playing"
        if sess and sess["app_id"] != app_id:
            self._close(sess, f"switched to {name}")
            sess = None
        # New content in the same app: you went back to the menu and other media started.
        # If the media changes WITHOUT going through the menu (credits, ads, autoplay) it is not
        # split: for episode autoplay there is the dashboard's "Next episode" button.
        if sess and media_id:
            if sess["media_id"] and media_id != sess["media_id"] and self._left_player:
                self._close(sess, "contenido nuevo")
                sess = None
            elif media_id != sess["media_id"]:
                db.set_media(self.conn, sess["id"], media_id)
        if media_id:
            self._left_player = False
        elif sess:
            self._left_player = True
        if sess is None:
            if not playing:
                return None
            sess = db.open_session(self.conn, app_id, name, logo, now, media_id)
            log.info("History: session %d opened (%s)", sess["id"], name)
            self._prev_playing = False  # the previous interval was not playback

        if playing:
            add = gap if self._prev_playing and gap <= MAX_TICK_GAP else 0.0
            db.touch(self.conn, sess["id"], now, add, True)
        elif now - sess["last_play_at"] > self.pause_timeout:
            self._close(sess, "inactiva")
            return None
        else:
            db.touch(self.conn, sess["id"], now, 0.0, False)
        self._prev_playing = playing
        return sess

    def identification(self, sess):
        return db.current_item(self.conn, sess["id"]) if sess else None


class TvPresence:
    def __init__(self, cfg: dict):
        self.cfg = cfg
        self.tv: WebOsClient | None = None
        self.rpc: AioPresence | None = None
        self.current_key: tuple | None = None
        self.app_since: dict[str, int] = {}
        self._rpc_retry_at = 0.0
        self.history: HistoryRecorder | None = None
        if {**DEFAULT_HISTORY, **(cfg.get("history") or {})}["enabled"]:
            try:
                self.history = HistoryRecorder(cfg)
            except Exception as e:  # history must never take the presence down
                log.warning("History disabled: %s", e)

    # ---------- TV ----------
    async def ensure_tv(self) -> bool:
        if self.tv and self.tv.is_connected():
            return True
        key = KEY_FILE.read_text().strip() if KEY_FILE.exists() else None
        self.tv = WebOsClient(self.cfg["tv_host"], key)
        try:
            await asyncio.wait_for(self.tv.connect(), timeout=60 if not key else 10)
        except Exception as e:  # TV off / no network
            log.debug("TV unavailable: %s", e)
            self.tv = None
            return False
        if self.tv.client_key and self.tv.client_key != key:
            CONFIG_DIR.mkdir(parents=True, exist_ok=True)
            KEY_FILE.write_text(self.tv.client_key)
            KEY_FILE.chmod(0o600)
            log.info("TV paired, key saved")
        log.info("Connected to TV %s", self.cfg["tv_host"])
        return True

    def read_tv(self) -> tuple[str, str, str | None, str | None] | None:
        """Returns (app_id, name, logo) and play state, or None when there is nothing to show."""
        assert self.tv
        st = self.tv.tv_state
        if not st.is_on:
            return None
        app_id = st.current_app_id or ""
        if not app_id or app_id in self.cfg["ignore_apps"] or app_id.startswith(SYSTEM_PREFIXES):
            return None
        if app_id in KNOWN_APPS:
            name, domain = KNOWN_APPS[app_id]
            logo = FAVICON.format(domain)
        elif self.cfg["only_known"]:
            return None
        else:
            name = (st.apps or {}).get(app_id, {}).get("title") or app_id
            logo = None
        play = media_id = None
        for m in st.media_state or []:
            if m.get("appId") == app_id:
                play = m.get("playState")
                media_id = m.get("mediaId")
        # Diagnostics: log mediaId changes (used to split sessions on new content)
        if media_id != getattr(self, "_last_media", None):
            if media_id:
                log.info("Media in %s: %s (%s)", name, media_id, play)
            self._last_media = media_id
        self.media_id = media_id
        return app_id, name, logo, play

    # ---------- Discord ----------
    async def ensure_rpc(self) -> bool:
        if self.rpc:
            return True
        if time.monotonic() < self._rpc_retry_at:
            return False
        rpc = AioPresence(self.cfg["client_id"])
        try:
            await asyncio.wait_for(rpc.connect(), timeout=10)
        except Exception as e:
            log.debug("Discord unavailable: %s", e)
            self._rpc_retry_at = time.monotonic() + 30
            return False
        self.rpc = rpc
        log.info("Connected to Discord")
        return True

    async def push(self, info, ident=None) -> None:
        if info is None:
            if self.current_key is not None:
                await self.rpc.clear()
                log.info("Presence cleared")
            self.current_key = None
            return
        app_id, name, logo, play = info
        playing = play == "playing"
        paused = play in ("paused", "pause")
        if not playing and not paused and not self.cfg["show_when_browsing"]:
            await self.push(None)
            return
        start = self.app_since.setdefault(app_id, int(time.time()))
        state = "Paused" if paused else ("Playing" if playing else "Browsing")
        ident_key = (
            (ident["id"], ident["title"], ident["season"], ident["episode"], ident["poster"])
            if ident else None
        )
        key = (app_id, state, ident_key)
        if key == self.current_key:
            return
        if ident:
            await self.push_identified(info, ident, paused, start)
            self.current_key = key
            return
        await self.rpc.update(
            activity_type=ActivityType.WATCHING,
            status_display_type=StatusDisplayType.NAME,
            name=name,
            details=f"Watching {name}",
            state=f"{state} on TV",
            large_image=logo,
            large_text=name,
            start=None if paused else start,
        )
        self.current_key = key
        log.info("Presence: %s — %s", name, state)

    async def push_identified(self, info, ident, paused: bool, start: int) -> None:
        """Presence with title, episode and poster (what was identified in the dashboard)."""
        _, name, logo, _ = info
        title = ident["title"]
        if ident["kind"] == "movie" and ident["year"]:
            title += f" ({ident['year']})"
        if ident["kind"] == "episode" and ident["season"] is not None:
            line = f"S{ident['season']} · E{ident['episode']}"
            if ident["episode_title"]:
                line += f" — {ident['episode_title']}"
        else:
            line = "Movie"
        if paused:
            line = f"Paused · {line}"
        poster = ident["poster"]
        await self.rpc.update(
            activity_type=ActivityType.WATCHING,
            status_display_type=StatusDisplayType.DETAILS,
            name=name,
            details=title[:128],
            state=line[:128],
            large_image=poster or logo,
            large_text=title[:128],
            small_image=logo if poster else None,
            small_text=f"{name} on TV" if poster else None,
            start=None if paused else start,
        )
        log.info("Presence: %s — %s — %s", name, title, line)

    async def run(self) -> None:
        if not self.cfg["client_id"]:
            sys.exit(f"Missing client_id in {CONFIG_FILE} (Application ID from discord.com/developers)")
        while True:
            try:
                info = await self.ensure_tv() and self.read_tv()
                if not info:
                    info = None
                # History is recorded even when Discord is not running
                ident = None
                if self.history:
                    try:
                        media = getattr(self, "media_id", None) if info else None
                        ident = self.history.identification(self.history.tick(info, media))
                    except Exception as e:
                        log.warning("History: %s", e)
                # reset the timer when the app leaves the foreground
                active = info[0] if info else None
                for a in list(self.app_since):
                    if a != active:
                        del self.app_since[a]
                if await self.ensure_rpc():
                    await self.push(info, ident)
            except Exception as e:
                log.warning("Error: %s — reconnecting", e)
                self.current_key = None
                if self.rpc:
                    try:
                        self.rpc.close()
                    except Exception:
                        pass
                self.rpc = None
                if self.tv:
                    try:
                        await self.tv.disconnect()
                    except Exception:
                        pass
                self.tv = None
            await asyncio.sleep(self.cfg["poll_seconds"])


def main() -> None:
    db.log_to_file_if_windowless("tvpresence")
    logging.basicConfig(
        level=logging.DEBUG if os.environ.get("TVPRESENCE_DEBUG") else logging.INFO,
        format="%(asctime)s [%(levelname)s] %(message)s",
    )
    asyncio.run(TvPresence(load_config()).run())


if __name__ == "__main__":
    main()
