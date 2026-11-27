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
    # Apps never shown (Plex is shown from the Plex server, see [plex])
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
    "exclude_apps": ["tv.twitch.tv.starshot.lg"],
    # Sessions shorter than this are dropped (trailers, menu autoplay)
    "min_seconds": 120,
    # Per-app minimum: YouTube (LG and Android TV) is recorded, but only kept if it ran this long
    # (documentaries, films) or you identified it; clips and music videos are dropped
    "min_seconds_by_app": {"com.google.android.youtube.tv": 1200, "youtube.leanback.v4": 1200},
    # Paused / in menus longer than this ends the session
    "pause_timeout": 1800,
    # No streaming app in front (TV off, other input) longer than this closes it
    "close_grace": 60,
    # After a service restart, resume the open session if seen less than this ago
    "resume_window": 600,
}
REOPEN_WITHIN = 6 * 3600  # s; same title back on Android TV within this: continue the old session
MAX_TICK_GAP = 30  # s; after suspend or a dropout the gap is not counted as playback

SYSTEM_PREFIXES = ("com.webos.", "com.lge.", "lge.", "com.lg.", "webos.")

# Game consoles on an HDMI input: the LG only knows which device is in front, not the game.
# Detected from the input's label (LG names SIMPLINK devices, or rename the input yourself)
# or set explicitly in config:  [consoles]  hdmi2 = "Nintendo Switch 2"
CONSOLE_HINTS = (("switch 2", "Nintendo Switch 2", "www.nintendo.com"), ("nintendo", "Nintendo Switch", "www.nintendo.com"),
                 ("switch", "Nintendo Switch", "www.nintendo.com"), ("playstation", "PlayStation", "www.playstation.com"),
                 ("ps5", "PlayStation 5", "www.playstation.com"), ("xbox", "Xbox", "www.xbox.com"))

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
    "tv.mirada.iris.android.inspire.izzitv.byod": ("izzi tv", "www.izzi.mx"),   # live TV: no title, no position
}
# Never recorded (music, Twitch, Plex: the Plex importer covers Plex, and Discord shows it from the server).
# YouTube IS recorded on Android TV (it publishes titles); short sessions are dropped, see min_seconds_by_app
ANDROID_EXCLUDE = ["com.spotify.tv.android", "tv.twitch.android.app",
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

    MIME = {"video/av01": "AV1", "video/hevc": "HEVC", "video/avc": "H.264",
            "video/x-vnd.on2.vp9": "VP9", "video/dolby-vision": "Dolby Vision", "video/mpeg2": "MPEG-2"}

    def video_formats(self) -> list:
        """New video decoder records since the last call: [(package, codec, width, height)].
        Android logs them in media.metrics when an app releases the decoder (end, pause, seek)."""
        if self.dev is None:
            return []
        try:
            out = self.dev.shell("dumpsys media.metrics | grep 'mediacodec.height' | grep 'mime=video' | tail -15",
                                 timeout_s=20)
        except Exception:
            return []
        seen, first = getattr(self, "_seen_codecs", None), not hasattr(self, "_seen_codecs")
        seen = seen if seen is not None else set()
        new = []
        for line in out.splitlines():
            key = line.split(":", 1)[0].strip()   # record number
            if key in seen:
                continue
            seen.add(key)
            f = dict(re.findall(r"android\.media\.mediacodec\.([\w-]+)=([^,)]+)", line))
            app = re.search(r"\((\S+), \d+, \d+\)", line)
            if first or f.get("encoder") == "1" or not app or not f.get("height"):
                continue
            new.append((app.group(1), self.MIME.get(f.get("mime", ""), f.get("mime", "").split("/")[-1].upper()),
                        int(f.get("width", 0)), int(f["height"])))
        self._seen_codecs = seen
        return new

    KCODEC = {"hevc": "HEVC", "h265": "HEVC", "av1": "AV1", "vp9": "VP9", "h264": "H.264", "avc": "H.264",
              "mpeg2": "MPEG-2", "dv": "Dolby Vision"}

    def resolution_events(self) -> list:
        """Live picture sizes from the box's video driver: [(epoch, codec or None, width, height)].
        Amlogic logs `Parse from ucode, visible(W x H)` when a stream starts and every time adaptive
        streaming switches quality, so HBO's ramp 576p -> 1080p -> 4K shows up within seconds, while
        media.metrics only gets a record once the app releases the decoder."""
        if self.dev is None:
            return []
        try:
            out = self.dev.shell("logcat -d -b kernel -v epoch | grep -E 'visible\\(|video changed to' | tail -40",
                                 timeout_s=20)
        except Exception:
            return []
        last = getattr(self, "_res_last", 0.0)
        new, codec, codec_at = [], None, 0.0
        for line in out.splitlines():
            t = re.match(r"\s*(\d+\.\d+)", line)
            if not t:
                continue
            ts = float(t.group(1))
            c = re.search(r"\]\s+(\w+) video changed to", line)
            if c:
                codec, codec_at = self.KCODEC.get(c.group(1).lower()), ts
                continue
            m = re.search(r"visible\((\d+) x (\d+)\)", line)
            if m and ts > last:
                last = ts
                new.append((ts, codec if ts - codec_at < 15 else None, int(m.group(1)), int(m.group(2))))
        self._res_last = last
        return new

    def hdr_events(self) -> list:
        """New 'content dynamic range' reports from the box's video driver: [(epoch, 'SDR'|'HDR10'|'HLG'|'DV')].
        Amlogic logs `DV: [.., bitdepth, FORMAT, ..]` in the kernel log each time a video starts; this is the
        source's format, unlike the HDMI output (the box always outputs HDR)."""
        if self.dev is None:
            return []
        try:
            out = self.dev.shell("logcat -d -b kernel -v epoch | grep 'DV: \\[' | tail -20", timeout_s=20)
        except Exception:
            return []
        last = getattr(self, "_hdr_last", 0.0)   # first call: everything still in the buffer (matched by time)
        new = []
        for line in out.splitlines():
            m = re.match(r"\s*(\d+\.\d+).*DV: \[[^,]*,[^,]*,\s*\d+,\s*([A-Za-z0-9+]+)", line)
            if m and float(m.group(1)) > last:
                last = float(m.group(1))
                new.append((last, m.group(2).upper()))
        self._hdr_last = last
        return new

    def read(self):
        """-> (info, media_id, hint, position_seconds) like the LG reader, or all None."""
        if not self._connect():
            return None, None, None, None
        try:
            power = self.dev.shell("dumpsys power | grep -m1 mWakefulness=")
            if "Awake" not in power:
                return None, None, None, None
            top = self.dev.shell("dumpsys activity activities | grep -m1 mResumedActivity")
            m = re.search(r" ([\w.]+)/", top)
            pkg = m.group(1) if m else ""
            sessions = self.dev.shell("dumpsys media_session")
            uptime_ms = float(self.dev.shell("cat /proc/uptime").split()[0]) * 1000  # = elapsedRealtime
        except Exception as e:
            log.debug("Android TV read failed: %s", e)
            try:
                self.dev.close()
            except Exception:
                pass
            self.dev = None
            return None, None, None, None
        if not pkg or pkg.startswith(ANDROID_SYSTEM):
            return None, None, None, None
        play, title, subtitle, pos = None, None, None, None
        for block in re.split(r"\n    (?=\S)", sessions):
            if f"package={pkg}" not in block:
                continue
            st = re.search(r"state=PlaybackState \{state=(\d+)", block)
            code = int(st.group(1)) if st else 0
            if code == 3 or (play is None and code in (2, 6)):  # 3 playing, 2 paused, 6 buffering
                play = "playing" if code in (3, 6) else "paused"
                pm = re.search(r"position=(-?\d+).*?speed=([\d.]+), updated=(\d+)", block)
                if pm and int(pm.group(1)) >= 0:
                    # position was sampled at `updated` (elapsedRealtime): advance it to now
                    p_ms, speed, upd = int(pm.group(1)), float(pm.group(2)), int(pm.group(3))
                    pos = (p_ms + (max(0.0, uptime_ms - upd) * speed if code == 3 else 0)) / 1000
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
            return None, None, None, None  # unknown app with no media session: not a player
        return (pkg, name, logo, play), title, (title, subtitle), pos

log = logging.getLogger("tvpresence")
PLEX_LOGO = FAVICON.format("www.plex.tv")


def plex_now(port: int) -> dict | None:
    """What this account plays on Plex right now, from the Couchlog dashboard (title, episode,
    real position, TMDB poster). The dashboard already resolves IDs and posters, so Discord gets
    a plain https poster URL: nothing to upload. None when nothing plays or the dashboard is down."""
    import json
    import urllib.request
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/plex_now", timeout=4) as r:
            items = json.load(r).get("plex_now") or []
    except Exception as e:
        log.debug("Plex now: %s", e)
        return None
    items.sort(key=lambda p: p.get("state") != "playing")   # playing before paused
    return items[0] if items else None


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
        self.min_by_app = {k: float(v) for k, v in (h.get("min_seconds_by_app") or {}).items()}
        self.dashboard_port = int(h.get("dashboard_port", 7790))
        self.conn = db.connect()
        closed, resumed = db.recover(self.conn, self.min_seconds, float(h["resume_window"]), source=source,
                                     min_by_app=self.min_by_app)
        if closed:
            log.info("History[%s]: closed %d open session(s) on startup", source, closed)
        if resumed:
            log.info("History[%s]: session %d resumed after restart", source, resumed)
        self._last_mono: float | None = None
        self._prev_playing = False
        self._idle_since: float | None = None
        self._left_player = False  # the app is in front but has no media (in its menu)
        self._start_pos: dict = {}  # session id -> first real position seen in it

    def _close(self, sess, why: str) -> None:
        db.close_session(self.conn, sess["id"], self.min_seconds, min_by_app=self.min_by_app)
        log.info("History[%s]: session %d closed (%s)", self.source, sess["id"], why)
        self._prev_playing = False
        self._left_player = False

    def _autoplay_next(self, sess, pos: float):
        """Apps like Netflix autoplay the next episode without saying so. With a real position (Android TV)
        it shows: the identified episode was past 80 % of its runtime and playback restarts near zero.
        Then split the session onto the next episode (same as the dashboard's "Next episode"), flagged
        as automatic so the dashboard offers Change / Undo. Returns the new open session, or None."""
        prev = sess["live_position"]
        if prev is None or pos >= 300 or prev - pos < 600:
            return None
        it = db.current_item(self.conn, sess["id"])
        if not it or it["kind"] != "episode" or not it["runtime"] or it["runtime_est"] or prev < 0.8 * it["runtime"] * 60:
            return None
        # Real watching, not a skip to the end: Netflix can jump back to an earlier episode while you are away,
        # and seeking that one to its end must not look like this episode finishing. Picking the episode up
        # again counts: the position went from where this session started to the end by playing, not seeking.
        start_pos = self._start_pos.get(sess["id"], 0.0)
        continuous = prev - start_pos <= sess["played_seconds"] * 1.25 + 60
        if sess["played_seconds"] < 0.5 * it["runtime"] * 60 and not continuous:
            log.info("History[%s]: position jumped %.0f → %.0f s but only %.0f s played: not treated as the next episode",
                     self.source, prev, pos, sess["played_seconds"])
            return None
        import json
        import urllib.request
        try:
            req = urllib.request.Request(
                f"http://127.0.0.1:{self.dashboard_port}/api/sessions/{sess['id']}/next", data=b'{"auto": true}',
                headers={"Content-Type": "application/json", "Origin": f"http://127.0.0.1:{self.dashboard_port}"})
            with urllib.request.urlopen(req, timeout=15) as r:
                res = json.load(r)
        except Exception as e:
            log.warning("History[%s]: autoplay next episode failed: %s", self.source, e)
            return None
        log.info("History[%s]: autoplay detected (%.0f → %.0f s): S%s · E%s, session %s",
                 self.source, prev, pos, res.get("season"), res.get("episode"), res.get("session"))
        return db.get_open_session(self.conn, self.source)

    def tick(self, info, media_id: str | None = None, hint: tuple | None = None, pos: float | None = None):
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
            # Android TV media ids are the published titles: a new title is new content, no menu needed
            reliable = self.source == "androidtv"
            if sess["media_id"] and media_id != sess["media_id"] and (self._left_player or reliable):
                self._close(sess, "new content")
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
            if self.source == "androidtv" and media_id:
                sess = db.reopen_recent(self.conn, self.source, app_id, media_id, REOPEN_WITHIN, now)
                if sess:
                    log.info("History[%s]: session %d resumed (%s, same title)", self.source, sess["id"], name)
            if sess is None:
                sess = db.open_session(self.conn, app_id, name, logo, now, media_id, source=self.source)
                log.info("History[%s]: session %d opened (%s)", self.source, sess["id"], name)
            self._prev_playing = False  # the previous interval was not playback
        if hint and hint[0] and not sess["hint_title"]:
            db.set_hint(self.conn, sess["id"], *hint)
        if pos is not None:
            self._start_pos.setdefault(sess["id"], pos)   # where this session picked the title up
        if pos is not None and playing and self.source == "androidtv":
            sess = self._autoplay_next(sess, pos) or sess
        if pos is not None:
            db.set_position(self.conn, sess["id"], pos)

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
        px = {"presence": True, **(cfg.get("plex") or {})}
        self.plex_presence = bool(px["presence"])
        self.dashboard_port = int({**DEFAULT_HISTORY, **(cfg.get("history") or {})}.get("dashboard_port", 7790))
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

    def console_for(self, app_id: str, st) -> tuple[str, str] | None:
        """(name, logo domain) if the foreground HDMI input is a game console."""
        m = re.fullmatch(r"com\.webos\.app\.(hdmi\d)", app_id or "")
        if not m:
            return None
        cfg = {k.lower(): v for k, v in (self.cfg.get("consoles") or {}).items()}
        explicit = cfg.get(m.group(1)) or cfg.get(app_id.lower())
        label = ((getattr(st, "inputs", None) or {}).get(app_id) or {}).get("label") or ""
        for text in (explicit, label):
            low = (text or "").lower()
            for hint, name, domain in CONSOLE_HINTS:
                if hint in low:
                    return (explicit or name), domain
        return (explicit, "www.nintendo.com") if explicit else None

    def read_tv(self) -> tuple[str, str, str | None, str | None] | None:
        """Returns (app_id, name, logo) and play state, or None when there is nothing to show."""
        assert self.tv
        st = self.tv.tv_state
        if not st.is_on:
            return None
        app_id = st.current_app_id or ""
        console = self.console_for(app_id, st)
        if console:   # an HDMI input with a game console in front: shown as "Playing …", never recorded
            name, domain = console
            self.media_id = None
            return app_id, name, FAVICON.format(domain), "playing"
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

    async def push(self, info, ident=None, hint=None, pos=None, played=None) -> None:
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
        now = time.time()
        # Elapsed time: the real playback position when the device reports it (Android TV),
        # else the session's accumulated play time (survives restarts), else time in the app.
        if pos is not None:
            start = int(now - pos)
        elif played:
            start = int(now - played)
        else:
            start = self.app_since.setdefault(app_id, int(now))
        runtime = (ident["runtime"] if ident is not None and "runtime" in ident.keys() else None)
        if ident is not None and "runtime_est" in ident.keys() and ident["runtime_est"]:
            runtime = None   # borrowed placeholder (e.g. 120 min): elapsed only, no wrong end time
        # A progress bar only with a real position (Android TV) and a known runtime
        self._end = int(start + runtime * 60) if pos is not None and runtime else None
        state = "Paused" if paused else ("Playing" if playing else "Browsing")
        ident_key = (
            (ident["id"], ident["title"], ident["season"], ident["episode"], ident["poster"])
            if ident else None
        )
        hint = hint if hint and hint[0] else None
        key = (app_id, state, ident_key, hint, None if paused else start // 20)  # re-send after a seek
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
        console = app_id.startswith("com.webos.app.hdmi")
        await self.rpc.update(
            activity_type=ActivityType.PLAYING if console else ActivityType.WATCHING,
            status_display_type=StatusDisplayType.NAME,
            name=name,
            details=f"Playing on {name}" if console else f"Watching {name}",
            state="On TV" if console else f"{state} on TV",
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
        imdb = ident["imdb"] if "imdb" in ident.keys() else None
        # A link to the title on IMDb under the status (Discord shows it as a button to others)
        buttons = [{"label": "IMDb", "url": f"https://www.imdb.com/title/{imdb}/"}] if imdb else None
        await self.rpc.update(
            buttons=buttons,
            activity_type=ActivityType.WATCHING,
            status_display_type=StatusDisplayType.DETAILS,
            name=name,
            details=title[:128],
            state=line[:128],
            large_image=poster or logo,
            large_text=title[:128],
            small_image=logo if poster else None,
            small_text=(f"{name} · {ident['device']}" if "device" in ident.keys() and ident["device"]
                        else f"{name} on TV") if poster else None,
            start=None if paused else start,
            end=None if paused else getattr(self, "_end", None),
        )
        log.info("Presence: %s — %s — %s", name, title, line)

    def _box_video(self, ts: float, fmt: str, app: str | None) -> None:
        """Last format the box decoded, for apps that have no box session (Plex): the dashboard reads it."""
        import json
        try:
            (db.DATA_DIR / "box_video.json").write_text(json.dumps({"ts": ts, "fmt": fmt, "app": app}))
        except Exception as e:
            log.debug("box_video: %s", e)

    async def run(self) -> None:
        if not self.cfg["client_id"]:
            sys.exit(f"Missing client_id in {CONFIG_FILE} (Application ID from discord.com/developers)")
        while True:
            try:
                info = await self.ensure_tv() and self.read_tv()
                if not info:
                    info = None
                # History is recorded even when Discord is not running
                ident = hint = pos = played = None
                if self.history:
                    try:
                        media = getattr(self, "media_id", None) if info else None
                        is_console = bool(info and info[0].startswith("com.webos.app.hdmi"))
                        sess = self.history.tick(None if is_console else info, media)
                        ident = self.history.identification(sess)
                        played = sess["played_seconds"] if sess else None
                    except Exception as e:
                        log.warning("History: %s", e)
                # Android TV box: its own session; shown in Discord when the LG shows nothing
                if self.atv:
                    atv_info = atv_media = atv_hint = atv_ident = atv_pos = atv_played = None
                    try:
                        atv_info, atv_media, atv_hint, atv_pos = await asyncio.wait_for(asyncio.to_thread(self.atv.read), 25)
                    except Exception as e:
                        log.debug("Android TV: %s", e)
                    if self.atv_history:
                        self._atv_n = getattr(self, "_atv_n", 0) + 1
                        if self._atv_n % 6 == 1:   # about every 30 s: decoder formats (resolution, codec)
                            try:
                                fmts = await asyncio.wait_for(asyncio.to_thread(self.atv.video_formats), 25)
                                for pkg, codec, w, h in fmts:
                                    row = self.atv_history.conn.execute(
                                        "SELECT id FROM sessions WHERE source='androidtv' AND app_id=? AND"
                                        " (state='open' OR ended_at > ?) ORDER BY id DESC LIMIT 1",
                                        (pkg, time.time() - 600)).fetchone()
                                    if row:
                                        db.set_video(self.atv_history.conn, row["id"], codec, w, h)
                                for ts, codec, w, h in await asyncio.wait_for(
                                        asyncio.to_thread(self.atv.resolution_events), 25):
                                    row = self.atv_history.conn.execute(
                                        "SELECT id, video_codec FROM sessions WHERE source='androidtv' AND started_at <= ?"
                                        " AND (state='open' OR ended_at >= ?) ORDER BY id DESC LIMIT 1",
                                        (ts + 90, ts - 5)).fetchone()
                                    if row and w >= 320:
                                        db.set_video(self.atv_history.conn, row["id"], row["video_codec"] or codec, w, h)
                                for ts, fmt in await asyncio.wait_for(asyncio.to_thread(self.atv.hdr_events), 25):
                                    row = self.atv_history.conn.execute(
                                        "SELECT id FROM sessions WHERE source='androidtv' AND started_at <= ? AND"
                                        " (state='open' OR ended_at >= ?) ORDER BY id DESC LIMIT 1",
                                        # the box logs the format when the decoder starts, while the app is
                                        # still buffering: up to 90 s before the session counts as playing
                                        (ts + 90, ts - 5)).fetchone()
                                    if row:
                                        db.set_hdr(self.atv_history.conn, row["id"], fmt, ts)
                                    self._box_video(ts, fmt, atv_info[0] if atv_info else None)
                                open_row = self.atv_history.conn.execute(
                                    "SELECT id FROM sessions WHERE source='androidtv' AND state='open'"
                                    " AND video_hdr_live IS NOT NULL ORDER BY id DESC LIMIT 1").fetchone()
                                if open_row:
                                    db.tick_hdr(self.atv_history.conn, open_row["id"], time.time())
                                # closed sessions: count the last format up to the end
                                for r in self.atv_history.conn.execute(
                                        "SELECT id, ended_at FROM sessions WHERE source='androidtv' AND state='closed'"
                                        " AND video_hdr_since IS NOT NULL AND ended_at > video_hdr_since").fetchall():
                                    db.tick_hdr(self.atv_history.conn, r["id"], r["ended_at"])
                            except Exception as e:
                                log.debug("Android TV formats: %s", e)
                        try:
                            s2 = self.atv_history.tick(atv_info, atv_media, atv_hint, atv_pos)
                            atv_ident = self.atv_history.identification(s2)
                            atv_played = s2["played_seconds"] if s2 else None
                        except Exception as e:
                            log.warning("History[androidtv]: %s", e)
                    if info is None and atv_info and atv_info[0] not in self.atv_ignore:
                        info, ident, hint, pos, played = atv_info, atv_ident, atv_hint, atv_pos, atv_played
                # Plex (any device, this account): shown when the LG has no app of its own in front
                # and the box is not playing something else. Wins over the box's Plex app, which is ignored.
                if info is None and self.plex_presence:
                    p = await asyncio.to_thread(plex_now, self.dashboard_port)
                    if p and p.get("title"):
                        info = ("com.plexapp.android", "Plex", PLEX_LOGO, p.get("state") or "playing")
                        ident = {"id": "plex", "kind": p.get("kind"), "title": p["title"], "year": p.get("year"),
                                 "season": p.get("season"), "episode": p.get("episode"),
                                 "episode_title": p.get("episode_title"), "poster": p.get("poster"),
                                 "imdb": (p.get("ids") or {}).get("imdb"),
                                 "runtime": (p.get("duration") or 0) / 60 or None, "runtime_est": 0,
                                 "device": p.get("device")}
                        hint, pos, played = None, p.get("offset"), None
                # reset the timer when the app leaves the foreground
                active = info[0] if info else None
                for a in list(self.app_since):
                    if a != active:
                        del self.app_since[a]
                if await self.ensure_rpc():
                    await self.push(info, ident, hint, pos, played)
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
