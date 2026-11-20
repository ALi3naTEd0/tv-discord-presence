#!/usr/bin/env python3
"""TV Discord Presence — shows in Discord which streaming app is open on an LG webOS TV,
and records viewing sessions for the TV History dashboard."""
from __future__ import annotations

import asyncio
import logging
import os
import re
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
    # Optional Android TV box over ADB:  [androidtv] host = "192.168.1.117"
    "androidtv": {},
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

# ---------- Android TV (ADB over the network) ----------
# package -> (name, domain for the logo)
ANDROID_APPS: dict[str, tuple[str, str]] = {
    "com.netflix.ninja": ("Netflix", "www.netflix.com"),
    "com.wbd.stream": ("HBO Max", "www.hbomax.com"),
    "com.hbo.hbonow": ("HBO Max", "www.hbomax.com"),
    "com.disney.disneyplus": ("Disney+", "www.disneyplus.com"),
    "com.amazon.amazonvideo.livingroom": ("Prime Video", "www.primevideo.com"),
    "com.apple.atve.androidtv.appletv": ("Apple TV", "tv.apple.com"),
    "com.univision.prendetv": ("ViX", "vix.com"),
    "com.tubitv": ("Tubi", "tubitv.com"),
    "com.mercadolibre.mercadoplay": ("Mercado Play", "play.mercadolibre.com.mx"),
    "com.clarovideo.androidtv": ("Claro Video", "www.clarovideo.com"),
    "com.crunchyroll.crunchyroid": ("Crunchyroll", "www.crunchyroll.com"),
    "com.google.android.youtube.tv": ("YouTube", "www.youtube.com"),
    "com.plexapp.android": ("Plex", "www.plex.tv"),
    "com.spotify.tv.android": ("Spotify", "www.spotify.com"),
}
# Never recorded (music, YouTube/Twitch, Plex: the Plex importer and DRPP cover it)
ANDROID_EXCLUDE = ["com.spotify.tv.android", "com.google.android.youtube.tv", "tv.twitch.android.app",
                   "com.plexapp.android", "com.google.android.youtube.tvmusic"]
ANDROID_SYSTEM = ("com.android.", "com.google.android.tvlauncher", "com.google.android.apps.tv.launcherx",
                  "com.google.android.katniss", "com.mitv.", "com.xiaomi.", "android")


class AndroidTv:
    """Reads an Android TV box over ADB: foreground app plus the media session it publishes
    (state, position and, when the app provides it, title/subtitle). Blocking: run in a thread."""

    def __init__(self, host: str, port: int = 5555):
        self.host, self.port = host, port
        self.dev = None
        self.retry_at = 0.0
        self.key_file = CONFIG_DIR / "adbkey"

    def _connect(self) -> bool:
        if self.dev is not None and self.dev.available:
            return True
        if time.monotonic() < self.retry_at:
            return False
        from adb_shell.adb_device import AdbDeviceTcp
        from adb_shell.auth.keygen import keygen
        from adb_shell.auth.sign_pythonrsa import PythonRSASigner
        if not self.key_file.exists():
            CONFIG_DIR.mkdir(parents=True, exist_ok=True)
            keygen(str(self.key_file))
            self.key_file.chmod(0o600)
        signer = PythonRSASigner(Path(f"{self.key_file}.pub").read_text(), self.key_file.read_text())
        dev = AdbDeviceTcp(self.host, self.port, default_transport_timeout_s=8.0)
        try:
            # first time: the box shows "Allow USB debugging?"; accept it (up to 60 s)
            dev.connect(rsa_keys=[signer], auth_timeout_s=60)
        except Exception as e:
            log.debug("Android TV unavailable: %s", e)
            self.retry_at = time.monotonic() + 30
            return False
        self.dev = dev
        log.info("Connected to Android TV %s", self.host)
        return True

    def read(self):
        """-> (info, media_id, hint) like the LG reader, or (None, None, None)."""
        if not self._connect():
            return None, None, None
        try:
            power = self.dev.shell("dumpsys power | grep -m1 mWakefulness=")
            if "Awake" not in power:
                return None, None, None
            top = self.dev.shell("dumpsys activity activities | grep -m1 mResumedActivity")
            m = re.search(r" ([\w.]+)/", top)
            pkg = m.group(1) if m else ""
            sessions = self.dev.shell("dumpsys media_session")
        except Exception as e:
            log.debug("Android TV read failed: %s", e)
            try:
                self.dev.close()
            except Exception:
                pass
            self.dev = None
            return None, None, None
        if not pkg or pkg.startswith(ANDROID_SYSTEM):
            return None, None, None
        play, title, subtitle = None, None, None
        for block in re.split(r"\n    (?=\S)", sessions):
            if f"package={pkg}" not in block:
                continue
            st = re.search(r"state=PlaybackState \{state=(\d+)", block)
            code = int(st.group(1)) if st else 0
            if code == 3 or (play is None and code in (2, 6)):  # 3 playing, 2 paused, 6 buffering
                play = "playing" if code in (3, 6) else "paused"
                d = re.search(r"description=([^\n]*)", block)
                if d:
                    parts = [p.strip() for p in d.group(1).split(",")]
                    title = parts[0] if parts and parts[0] not in ("", "null") else None
                    subtitle = parts[1] if len(parts) > 1 and parts[1] not in ("", "null") else None
        if pkg in ANDROID_APPS:
            name, domain = ANDROID_APPS[pkg]
            logo = FAVICON.format(domain)
        elif play:
            name, logo = pkg.rsplit(".", 1)[-1].capitalize(), None
        else:
            return None, None, None  # unknown app with no media session: not a player
        return (pkg, name, logo, play), title, (title, subtitle)

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

    def __init__(self, cfg: dict, source: str = "tv"):
        h = {**DEFAULT_HISTORY, **(cfg.get("history") or {})}
        self.source = source
        self.exclude = set(h["exclude_apps"]) | set(h.get("exclude_android_apps", ANDROID_EXCLUDE))
        self.min_seconds = float(h["min_seconds"])
        self.pause_timeout = float(h["pause_timeout"])
        self.close_grace = float(h["close_grace"])
        self.conn = db.connect()
        closed, resumed = db.recover(self.conn, self.min_seconds, float(h["resume_window"]), source=source)
        if closed:
            log.info("History[%s]: closed %d open session(s) on startup", source, closed)
        if resumed:
            log.info("History[%s]: session %d resumed after restart", source, resumed)
        self._last_mono: float | None = None
        self._prev_playing = False
        self._idle_since: float | None = None
        self._left_player = False  # the app is in front but has no media (in its menu)

    def _close(self, sess, why: str) -> None:
        db.close_session(self.conn, sess["id"], self.min_seconds)
        log.info("History[%s]: session %d closed (%s)", self.source, sess["id"], why)
        self._prev_playing = False
        self._left_player = False

    def tick(self, info, media_id: str | None = None, hint: tuple | None = None):
        now, mono = time.time(), time.monotonic()
        gap = 0.0 if self._last_mono is None else mono - self._last_mono
        self._last_mono = mono
        sess = db.get_open_session(self.conn, self.source)

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
            sess = db.open_session(self.conn, app_id, name, logo, now, media_id, source=self.source)
            log.info("History[%s]: session %d opened (%s)", self.source, sess["id"], name)
            self._prev_playing = False  # the previous interval was not playback
        if hint and hint[0] and not sess["hint_title"]:
            db.set_hint(self.conn, sess["id"], *hint)

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
        a = {"port": 5555, "ignore_apps": ["com.plexapp.android", "com.spotify.tv.android"], **(cfg.get("androidtv") or {})}
        self.atv: AndroidTv | None = AndroidTv(a["host"], int(a["port"])) if a.get("host") else None
        self.atv_ignore = set(a["ignore_apps"])
        self.atv_history: HistoryRecorder | None = None
        if self.atv and self.history:
            try:
                self.atv_history = HistoryRecorder(cfg, source="androidtv")
            except Exception as e:
                log.warning("Android TV history disabled: %s", e)

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

    async def push(self, info, ident=None, hint=None) -> None:
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
        hint = hint if hint and hint[0] else None
        key = (app_id, state, ident_key, hint)
        if key == self.current_key:
            return
        if ident:
            await self.push_identified(info, ident, paused, start)
            self.current_key = key
            return
        if hint:  # the app itself publishes what is playing (Android TV media session, e.g. YouTube)
            title, sub = hint
            await self.rpc.update(
                activity_type=ActivityType.WATCHING,
                status_display_type=StatusDisplayType.DETAILS,
                name=name,
                details=title[:128],
                state=(f"Paused · {sub}" if paused else sub)[:128] if sub else ("Paused" if paused else f"{name} on TV"),
                large_image=logo,
                large_text=name,
                start=None if paused else start,
            )
            self.current_key = key
            log.info("Presence: %s — %s — %s", name, title, sub or "")
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
                ident = hint = None
                if self.history:
                    try:
                        media = getattr(self, "media_id", None) if info else None
                        ident = self.history.identification(self.history.tick(info, media))
                    except Exception as e:
                        log.warning("History: %s", e)
                # Android TV box: its own session; shown in Discord when the LG shows nothing
                if self.atv:
                    atv_info = atv_media = atv_hint = atv_ident = None
                    try:
                        atv_info, atv_media, atv_hint = await asyncio.wait_for(asyncio.to_thread(self.atv.read), 25)
                    except Exception as e:
                        log.debug("Android TV: %s", e)
                    if self.atv_history:
                        try:
                            atv_ident = self.atv_history.identification(
                                self.atv_history.tick(atv_info, atv_media, atv_hint))
                        except Exception as e:
                            log.warning("History[androidtv]: %s", e)
                    if info is None and atv_info and atv_info[0] not in self.atv_ignore:
                        info, ident, hint = atv_info, atv_ident, atv_hint
                # reset the timer when the app leaves the foreground
                active = info[0] if info else None
                for a in list(self.app_since):
                    if a != active:
                        del self.app_since[a]
                if await self.ensure_rpc():
                    await self.push(info, ident, hint)
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
