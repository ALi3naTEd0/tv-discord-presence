#!/usr/bin/env python3
"""TV History dashboard.

Shows what tvpresence recorded (which app played and for how long), lets you
identify each session by searching TMDB + IMDb at once (or pasting an ID/link),
and sends what you identified to Trakt and Simkl **by external ID**, never by title.
Stdlib only. Credentials:
  TMDB/Plex -> TMDB_API_KEY / PLEX_TOKEN (env) or <config dir>/.env
  Trakt     -> client id and token from trakt-scrobbler (that service renews it)
  Simkl     -> <simkl_dir>/.env + token.json (default ~/.config/simkl)
"""
from __future__ import annotations

import json
import os
import re
import sqlite3
import subprocess
import sys
import threading
import time
import tomllib
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import tvhistory_db as db  # noqa: E402

HOME = Path.home()
CONFIG_FILE = db.CONFIG_DIR / "config.toml"
UA = "tvhistory/1.0 (+local dashboard)"  # Trakt/Cloudflare blocks the default UA (error 1010)
IMG = "https://image.tmdb.org/t/p/w342"
POOL = ThreadPoolExecutor(max_workers=12)
PUSH_LOCK = threading.Lock()


def load_cfg() -> dict:
    h = {}
    if CONFIG_FILE.exists():
        with CONFIG_FILE.open("rb") as f:
            h = tomllib.load(f).get("history") or {}
    return {
        "bind": h.get("dashboard_bind", "127.0.0.1"),
        "port": int(h.get("dashboard_port", 7790)),
        "lang": h.get("tmdb_language", "en-US"),
        "plex": bool(h.get("plex_enabled", True)),
        "plex_url": h.get("plex_url", "http://127.0.0.1:32400").rstrip("/"),
        "plex_account": int(h.get("plex_account_id", 1)),       # 1 = server owner
        "plex_user": h.get("plex_user", ""),                    # optional: account name
        "plex_interval": int(h.get("plex_interval", 300)),      # often: watched files get deleted and the IDs go with them
        "plex_days": int(h.get("plex_import_days", 7)),         # look-back on the first import
        "plex_db": h.get("plex_db") or default_plex_db(),
        "env_files": [Path(p).expanduser() for p in h.get("env_files", [])],   # extra .env files with keys
        "trakt_python": h.get("trakt_python", ""),              # python of trakt-scrobbler's venv (auto-detected)
        "simkl_dir": Path(h.get("simkl_dir", "~/.config/simkl")).expanduser(),  # .env (SIMKL_CLIENT_ID) + token.json
    }


def default_plex_db() -> str:
    rel = Path("Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db")
    if sys.platform == "win32":
        return str(Path(os.environ.get("LOCALAPPDATA") or HOME / "AppData/Local") / rel)
    if sys.platform == "darwin":
        return str(HOME / "Library/Application Support" / rel)
    return str(Path("/var/lib/plex") / rel)


CFG = load_cfg()


# ---------------------------------------------------------------- credentials
def read_env(path: Path) -> dict:
    out = {}
    if path.exists():
        for line in path.read_text().splitlines():
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                out[k.strip()] = v.strip().strip('"').strip("'")
    return out


_cache: dict = {}


def secret(name: str) -> str:
    """Environment variable, else the first .env file that defines it:
    <config dir>/.env, then [history] env_files."""
    val = os.environ.get(name, "")
    for f in [db.CONFIG_DIR / ".env", *CFG["env_files"]]:
        val = val or read_env(f).get(name, "")
    if not val:
        raise RuntimeError(f"{name} not found (set it in {db.CONFIG_DIR / '.env'})")
    return val


def tmdb_key() -> str:
    if "tmdb" not in _cache:
        _cache["tmdb"] = secret("TMDB_API_KEY")
    return _cache["tmdb"]


def trakt_python() -> Path:
    """Python of trakt-scrobbler's pipx venv: config override, `pipx environment`, then the usual spots."""
    if CFG["trakt_python"]:
        return Path(CFG["trakt_python"]).expanduser()
    exe = Path("Scripts/python.exe") if os.name == "nt" else Path("bin/python")
    roots = []
    try:
        r = subprocess.run(["pipx", "environment", "--value", "PIPX_LOCAL_VENVS"],
                           capture_output=True, text=True, timeout=15)
        if r.returncode == 0 and r.stdout.strip():
            roots.append(Path(r.stdout.strip()))
    except (OSError, subprocess.SubprocessError):
        pass
    roots += [HOME / ".local/share/pipx/venvs", HOME / ".local/pipx/venvs", HOME / "pipx/venvs"]
    for root in roots:
        if (root / "trakt-scrobbler" / exe).exists():
            return root / "trakt-scrobbler" / exe
    raise RuntimeError("trakt-scrobbler's venv not found; set [history] trakt_python in config.toml")


def trakt_scrobbler_info() -> dict:
    """Client id and data dir (where trakt_token.json lives), asked to trakt-scrobbler itself."""
    if "trakt" not in _cache:
        code = ("from trakt_scrobbler import trakt_key_holder as k\n"
                "from trakt_scrobbler.app_dirs import DATA_DIR\n"
                "print('CID=' + k.get_id()); print('DIR=' + str(DATA_DIR))")
        r = subprocess.run([str(trakt_python()), "-c", code], capture_output=True, text=True, timeout=30)
        out = dict(line.split("=", 1) for line in r.stdout.splitlines() if line[:4] in ("CID=", "DIR="))
        if not re.fullmatch(r"[0-9a-f]{40,}", out.get("CID", "")) or not out.get("DIR"):
            raise RuntimeError("Could not read the client id from trakt-scrobbler")
        _cache["trakt"] = {"cid": out["CID"], "dir": Path(out["DIR"])}
    return _cache["trakt"]


def trakt_client_id() -> str:
    return trakt_scrobbler_info()["cid"]


def trakt_headers(auth: bool) -> dict:
    h = {"trakt-api-key": trakt_client_id(), "trakt-api-version": "2"}
    if auth:
        tok = json.loads((trakt_scrobbler_info()["dir"] / "trakt_token.json").read_text())
        exp = tok.get("expires_at") or (tok.get("created_at", 0) + tok.get("expires_in", 0))
        if exp and exp < time.time():
            # Not renewed here: the refresh token is single-use and trakt-scrobbler
            # renews it itself; renewing from two places would break its session.
            raise RuntimeError("Trakt token expired; trakt-scrobbler renews it on its next use (or run `trakts auth`)")
        h["Authorization"] = f"Bearer {tok['access_token']}"
    return h


def simkl_creds() -> tuple[str, str]:
    d = CFG["simkl_dir"]
    cid = read_env(d / ".env").get("SIMKL_CLIENT_ID", "")
    tok = json.loads((d / "token.json").read_text()).get("access_token", "") if (d / "token.json").exists() else ""
    if not cid or not tok:
        raise RuntimeError(f"Missing Simkl credentials in {d}")
    return cid, tok


# ------------------------------------------------------------- network/cache
def http_json(method: str, url: str, headers: dict | None = None, body=None, timeout=20):
    h = {"User-Agent": UA, "Accept": "application/json", **(headers or {})}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw.decode("utf-8", "replace")[:300]


def cached(key, ttl, fn):
    hit = _cache.get(("c",) + key)
    if hit and hit[0] > time.time():
        return hit[1]
    val = fn()
    _cache[("c",) + key] = (time.time() + ttl, val)
    return val


def tmdb(path: str, **params):
    params = {"api_key": tmdb_key(), "language": CFG["lang"], **params}
    url = f"https://api.themoviedb.org/3{path}?{urllib.parse.urlencode(params)}"

    def fetch():
        st, j = http_json("GET", url)
        if st != 200:
            raise RuntimeError(f"TMDB {st} on {path}")
        return j
    return cached(("tmdb", path, tuple(sorted(params.items()))), 6 * 3600, fetch)


def year_of(date: str | None):
    return int(date[:4]) if date and date[:4].isdigit() else None


# --------------------------------------------------------------- search/merge
IMDB_KIND = {"movie": "movie", "short": "movie", "tvMovie": "movie", "video": "movie",
             "tvSpecial": "movie", "tvShort": "movie", "tvSeries": "tv", "tvMiniSeries": "tv"}
IMDB_LABEL = {"short": "Short", "tvMovie": "TV Movie", "video": "Video", "tvSpecial": "TV Special",
              "tvShort": "TV Short", "tvMiniSeries": "Miniseries", "tvSeries": "Series", "movie": "Movie"}


def imdb_img(url: str | None):
    if url and "._V1_" in url:
        return re.sub(r"\._V1_.*\.jpg$", "._V1_QL75_UX300_.jpg", url)
    return url


def search_tmdb(q: str) -> list[dict]:
    out = []
    for r in tmdb("/search/multi", query=q, include_adult="false").get("results", [])[:10]:
        if r.get("media_type") not in ("movie", "tv"):
            continue
        kind = r["media_type"]
        out.append({
            "kind": kind, "label": "Movie" if kind == "movie" else "Series",
            "title": r.get("title") or r.get("name"),
            "year": year_of(r.get("release_date") or r.get("first_air_date")),
            "poster": IMG + r["poster_path"] if r.get("poster_path") else None,
            "overview": (r.get("overview") or "")[:240],
            "ids": {"tmdb": r["id"]}, "src": {"tmdb"},
        })
    return out


def search_imdb(q: str) -> list[dict]:
    st, j = http_json("GET", f"https://v3.sg.media-imdb.com/suggestion/x/{urllib.parse.quote(q.lower())}.json")
    out = []
    for r in (j or {}).get("d", []) if st == 200 and isinstance(j, dict) else []:
        qid = r.get("qid")
        if not str(r.get("id", "")).startswith("tt") or qid not in IMDB_KIND:
            continue
        out.append({
            "kind": IMDB_KIND[qid], "label": IMDB_LABEL.get(qid, qid), "title": r.get("l"),
            "year": r.get("y"), "poster": imdb_img((r.get("i") or {}).get("imageUrl")),
            "overview": r.get("s") or "", "ids": {"imdb": r["id"]}, "src": {"imdb"},
        })
    return out[:8]


def bridge(rec: dict) -> None:
    """Fill in imdb/tmdb/tvdb using TMDB (external_ids or find)."""
    ids = rec["ids"]
    try:
        if ids.get("tmdb"):
            ext = tmdb(f"/{rec['kind']}/{ids['tmdb']}/external_ids")
            ids.setdefault("imdb", ext.get("imdb_id") or None)
            if rec["kind"] == "tv":
                ids.setdefault("tvdb", ext.get("tvdb_id") or None)
        elif ids.get("imdb"):
            f = tmdb(f"/find/{ids['imdb']}", external_source="imdb_id")
            hit = (f.get("movie_results") or [None])[0] if rec["kind"] == "movie" else (f.get("tv_results") or [None])[0]
            if hit:
                ids["tmdb"] = hit["id"]
                rec["src"].add("tmdb")
                rec["poster"] = rec["poster"] or (IMG + hit["poster_path"] if hit.get("poster_path") else None)
                if rec["kind"] == "tv":
                    ids.setdefault("tvdb", tmdb(f"/tv/{hit['id']}/external_ids").get("tvdb_id"))
    except Exception:
        pass
    if ids.get("imdb"):
        rec["src"].add("imdb")


def exists_trakt(rec: dict):
    ids, t = rec["ids"], "movie" if rec["kind"] == "movie" else "show"
    answered = False
    for src in ("imdb", "tmdb", "tvdb"):
        if not ids.get(src) or (src == "tvdb" and t == "movie"):
            continue
        st, j = http_json("GET", f"https://api.trakt.tv/search/{src}/{ids[src]}?type={t}", trakt_headers(False))
        if st == 200 and isinstance(j, list) and j:
            return j[0][t]["ids"].get("trakt")
        answered = answered or st == 200
    return False if answered else None  # False = asked and missing; None = could not ask


def exists_simkl(rec: dict):
    ids = rec["ids"]
    cid, _ = simkl_creds()
    answered = False
    for src in ("imdb", "tvdb", "tmdb"):
        if not ids.get(src) or (src == "tvdb" and rec["kind"] == "movie"):
            continue
        extra = "" if src == "imdb" else f"&type={'movie' if rec['kind'] == 'movie' else 'show'}"
        st, j = http_json("GET", f"https://api.simkl.com/search/id?{src}={ids[src]}{extra}&client_id={cid}")
        if st == 200 and isinstance(j, list) and j:
            return (j[0].get("ids") or {}).get("simkl") or True
        answered = answered or st == 200
    return False if answered else None


ID_PATTERNS = [
    (re.compile(r"(tt\d{6,10})"), "imdb"),
    (re.compile(r"themoviedb\.org/(movie|tv)/(\d+)"), "tmdb_url"),
    (re.compile(r"^(?:tmdb[:/ ]?)(movie|tv)[:/ ](\d+)$", re.I), "tmdb_url"),
    (re.compile(r"(?:thetvdb\.com/\S*?[?&]id=|^tvdb[:/ ]?)(\d+)", re.I), "tvdb"),
]


def resolve_id(q: str) -> list[dict] | None:
    """If the query is an ID or link (IMDb, TMDB, TheTVDB), resolve it directly."""
    for rx, kind in ID_PATTERNS:
        m = rx.search(q.strip())
        if not m:
            continue
        if kind == "imdb":
            f = tmdb(f"/find/{m.group(1)}", external_source="imdb_id")
            hits = [("movie", h) for h in f.get("movie_results", [])] + [("tv", h) for h in f.get("tv_results", [])]
            if not hits:  # on IMDb but not on TMDB: let it through anyway
                st, j = http_json("GET", f"https://v3.sg.media-imdb.com/suggestion/x/{m.group(1)}.json")
                d = (j or {}).get("d", []) if isinstance(j, dict) else []
                r = next((x for x in d if x.get("id") == m.group(1)), None)
                if r and r.get("qid") in IMDB_KIND:
                    return [{"kind": IMDB_KIND[r["qid"]], "label": IMDB_LABEL.get(r["qid"], r["qid"]),
                             "title": r.get("l"), "year": r.get("y"), "poster": imdb_img((r.get("i") or {}).get("imageUrl")),
                             "overview": r.get("s") or "", "ids": {"imdb": m.group(1)}, "src": {"imdb"}}]
                return []
        elif kind == "tvdb":
            f = tmdb(f"/find/{m.group(1)}", external_source="tvdb_id")
            hits = [("tv", h) for h in f.get("tv_results", [])] + [("movie", h) for h in f.get("movie_results", [])]
        else:
            k, i = m.group(1).lower(), int(m.group(2))
            hits = [(k, tmdb(f"/{k}/{i}"))]
        return [{
            "kind": k, "label": "Movie" if k == "movie" else "Series",
            "title": h.get("title") or h.get("name"),
            "year": year_of(h.get("release_date") or h.get("first_air_date")),
            "poster": IMG + h["poster_path"] if h.get("poster_path") else None,
            "overview": (h.get("overview") or "")[:240], "ids": {"tmdb": h["id"]}, "src": {"tmdb"},
        } for k, h in hits]
    return None


def search_all(q: str) -> dict:
    errors = []
    recs = resolve_id(q)
    if recs is None:
        f_t, f_i = POOL.submit(search_tmdb, q), POOL.submit(search_imdb, q)
        recs = []
        for name, fut in (("TMDB", f_t), ("IMDb", f_i)):
            try:
                recs += fut.result(timeout=25)
            except Exception as e:
                errors.append(f"{name}: {e}")
    list(POOL.map(bridge, recs))
    # merge by imdb, then by tmdb
    merged: list[dict] = []
    for r in recs:
        dup = next((m for m in merged if m["kind"] == r["kind"] and (
            (r["ids"].get("imdb") and m["ids"].get("imdb") == r["ids"]["imdb"]) or
            (r["ids"].get("tmdb") and m["ids"].get("tmdb") == r["ids"]["tmdb"]))), None)
        if dup:
            for k, v in r["ids"].items():
                dup["ids"].setdefault(k, v) if v else None
            dup["src"] |= r["src"]
            dup["poster"] = dup["poster"] or r["poster"]
            if r["label"] not in ("Movie", "Series"):
                dup["label"] = r["label"]  # IMDb tells shorts and miniseries apart
        else:
            merged.append(r)
    merged = merged[:10]

    def check(rec):
        out = {}
        for name, fn in (("trakt", exists_trakt), ("simkl", exists_simkl)):
            try:
                out[name] = fn(rec)
            except Exception:
                out[name] = None
        return out
    for rec, ex in zip(merged, POOL.map(check, merged)):
        rec["exists"] = ex
        rec["src"] = sorted(rec["src"])
    return {"results": merged, "errors": errors}


# ----------------------------------------------------------------------- push
def iso(ts: float) -> str:
    return datetime.fromtimestamp(ts, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")


def watched_times(sess, items) -> dict:
    """Spread a session's items across its duration."""
    end = sess["ended_at"] or sess["last_seen_at"]
    span = max(end - sess["started_at"], 1)
    n = len(items)
    return {it["id"]: sess["started_at"] + (k + 1) * span / n for k, it in enumerate(items)}


def ids_of(it, order, movie_skip=("tvdb",)):
    out = {}
    for k in order:
        v = it[k]
        if v and not (it["kind"] == "movie" and k in movie_skip):
            out[k] = v
    return out


def push_trakt(it, when: str) -> tuple[str, str | None]:
    ids = ids_of(it, ("imdb", "tmdb", "tvdb"))
    if not ids:
        return "not_found", "Sin IDs externos"
    if it["kind"] == "movie":
        body = {"movies": [{"watched_at": when, "ids": ids}]}
    else:
        body = {"shows": [{"ids": ids, "seasons": [{"number": it["season"], "episodes": [
            {"number": it["episode"], "watched_at": when}]}]}]}
    st, j = http_json("POST", "https://api.trakt.tv/sync/history", trakt_headers(True), body)
    if st in (200, 201) and isinstance(j, dict):
        added = j.get("added") or {}
        if (added.get("movies") or 0) + (added.get("episodes") or 0) >= 1:
            return "added", None
        nf = j.get("not_found") or {}
        if any(nf.get(k) for k in nf):
            return "not_found", "Not on Trakt yet"
        return "error", f"Trakt added nothing: {json.dumps(j)[:200]}"
    return "error", f"Trakt {st}: {str(j)[:200]}"


def push_simkl(it, when: str) -> tuple[str, str | None]:
    cid, tok = simkl_creds()
    ids = ids_of(it, ("imdb", "tvdb", "tmdb"))  # unambiguous ones first
    if not ids:
        return "not_found", "Sin IDs externos"
    base = {"title": it["title"], "ids": ids, **({"year": it["year"]} if it["year"] else {})}
    if it["kind"] == "movie":
        body = {"movies": [{**base, "watched_at": when}]}
    else:
        body = {"shows": [{**base, "seasons": [{"number": it["season"], "episodes": [
            {"number": it["episode"], "watched_at": when}]}]}]}
    url = f"https://api.simkl.com/sync/history?client_id={cid}&app-name=tvhistory&app-version=1.0"
    st, j = http_json("POST", url, {"Authorization": f"Bearer {tok}"}, body)
    if st in (200, 201) and isinstance(j, dict):
        nf = j.get("not_found") or {}
        if any(nf.get(k) for k in nf):
            return "not_found", "Not on Simkl yet"
        added = j.get("added") or {}
        if sum((added.get(k) or 0) for k in ("movies", "shows", "episodes")) >= 1:
            return "added", None
        return "skipped", "Simkl added nothing (already marked?)"
    if st == 400 and "RATE_LIMIT" in str(j).upper():
        return "error", "Simkl: bloqueo de 20 s por escrituras seguidas; reintenta"
    return "error", f"Simkl {st}: {str(j)[:200]}"


def api_push(body: dict) -> dict:
    targets = [t for t in (body.get("targets") or ["trakt", "simkl"]) if t in ("trakt", "simkl")]
    dry, force, sids = bool(body.get("dry_run")), bool(body.get("force")), body.get("session_ids")
    if not PUSH_LOCK.acquire(blocking=False):
        return {"error": "A push is already running"}
    try:
        conn = db.connect()
        q = "SELECT * FROM sessions WHERE ignored=0 AND id IN (SELECT session_id FROM items)"
        args: list = []
        if sids:
            q += f" AND id IN ({','.join('?' * len(sids))})"
            args = [int(s) for s in sids]
        results, last = [], {"trakt": 0.0, "simkl": 0.0}
        for sess in conn.execute(q + " ORDER BY started_at", args).fetchall():
            items = conn.execute(
                "SELECT * FROM items WHERE session_id=? ORDER BY COALESCE(season,0), COALESCE(episode,0), id",
                (sess["id"],)).fetchall()
            times = watched_times(sess, items)
            for it in items:
                when = iso(times[it["id"]])
                for t in targets:
                    if not force and it[f"{t}_status"] in ("added", "skipped"):
                        continue
                    label = it["title"] + (f" S{it['season']}E{it['episode']}" if it["kind"] == "episode" else "")
                    dup = False
                    if t == "trakt" and sess["source"] == "plex" and not force:
                        try:
                            dup = trakt_already(it, times[it["id"]])
                        except Exception as e:
                            if not dry:
                                conn.execute("UPDATE items SET trakt_status='error', trakt_error=? WHERE id=?", (str(e), it["id"]))
                                results.append({"item": it["id"], "target": t, "label": label, "status": "error", "error": str(e)})
                                continue
                    if dry:
                        results.append({"item": it["id"], "target": t, "label": label, "watched_at": when,
                                        "dry_run": True, "already": dup})
                        continue
                    if dup:
                        conn.execute("UPDATE items SET trakt_status='added', trakt_error=?, pushed_at=? WHERE id=?",
                                     ("Already on Trakt (sent by trakt-scrobbler)", time.time(), it["id"]))
                        results.append({"item": it["id"], "target": t, "label": label, "status": "added", "error": None})
                        continue
                    wait = 1.1 - (time.monotonic() - last[t])  # 1 write/s per service
                    if wait > 0:
                        time.sleep(wait)
                    try:
                        status, err = (push_trakt if t == "trakt" else push_simkl)(it, when)
                    except Exception as e:
                        status, err = "error", str(e)
                    last[t] = time.monotonic()
                    conn.execute(f"UPDATE items SET {t}_status=?, {t}_error=?, pushed_at=? WHERE id=?",
                                 (status, err, time.time(), it["id"]))
                    results.append({"item": it["id"], "target": t, "label": label, "status": status, "error": err})
        _cache.pop(("c", "simkl_lists"), None)  # so the next verify sees what was just sent
        return {"results": results}
    finally:
        PUSH_LOCK.release()


# ---------------------------------------------------------------------- Plex
PLEX_LOGO = "https://t0.gstatic.com/faviconV2?client=SOCIAL&type=FAVICON&fallback_opts=TYPE,SIZE,URL&url=https://www.plex.tv&size=256"
PLEX_IMPORT_LOCK = threading.Lock()
PLEX_STATUS = {"last_run": None, "last_added": 0, "error": None}


def plex_token() -> str:
    return secret("PLEX_TOKEN")


def plex_get(path: str) -> dict:
    st, j = http_json("GET", CFG["plex_url"] + path, {"X-Plex-Token": plex_token()}, timeout=20)
    if st != 200 or not isinstance(j, dict):
        raise RuntimeError(f"Plex {st} on {path.split('?')[0]}")
    return j.get("MediaContainer") or {}


def guid_ids(md: dict | None) -> dict:
    """External IDs of a Plex item (new agents: Guid[]; old ones: legacy guid)."""
    ids: dict = {}
    for g in (md or {}).get("Guid") or []:
        gid = g.get("id", "")
        for pre, key in (("imdb://", "imdb"), ("tmdb://", "tmdb"), ("tvdb://", "tvdb")):
            if gid.startswith(pre):
                v = gid[len(pre):]
                ids[key] = v if key == "imdb" else (int(v) if v.isdigit() else None)
    m = re.match(r"com\.plexapp\.agents\.(imdb|themoviedb|thetvdb)://([^?/]+)", (md or {}).get("guid", ""))
    if m:
        k = {"imdb": "imdb", "themoviedb": "tmdb", "thetvdb": "tvdb"}[m.group(1)]
        v = m.group(2)
        if k == "imdb" and v.startswith("tt"):
            ids.setdefault("imdb", v)
        elif k != "imdb" and v.isdigit():
            ids.setdefault(k, int(v))
    return {k: v for k, v in ids.items() if v}


def plex_import_entry(conn, e: dict) -> bool:
    if e.get("type") not in ("movie", "episode"):
        return False  # music, photos, etc.
    key = e.get("historyKey") or f"plex:{e.get('ratingKey')}:{e.get('viewedAt')}"
    if conn.execute("SELECT 1 FROM sessions WHERE source_key=?", (key,)).fetchone():
        return False
    viewed, ep = float(e["viewedAt"]), e["type"] == "episode"
    md = show = None
    if e.get("ratingKey"):  # if it was removed from the library there is no metadata
        try:
            md = (plex_get(f"/library/metadata/{e['ratingKey']}?includeGuids=1").get("Metadata") or [None])[0]
        except Exception:
            md = None
    if md and ep and md.get("grandparentRatingKey"):
        try:
            show = (plex_get(f"/library/metadata/{md['grandparentRatingKey']}?includeGuids=1").get("Metadata") or [None])[0]
        except Exception:
            show = None
    dur = ((md or {}).get("duration") or 0) / 1000
    if ep:
        title = (show or {}).get("title") or e.get("grandparentTitle")
        season = (md or {}).get("parentIndex", e.get("parentIndex"))
        number = (md or {}).get("index", e.get("index"))
        ep_title = (md or {}).get("title") or e.get("title")
        ids, year = guid_ids(show), (show or {}).get("year")
    else:
        title = (md or {}).get("title") or e.get("title")
        season = number = ep_title = None
        ids = guid_ids(md)
        ids.pop("tvdb", None)
        year = (md or {}).get("year") or year_of(e.get("originallyAvailableAt"))
    poster = None
    if ids.get("tmdb"):
        try:
            d = tmdb(f"/{'tv' if ep else 'movie'}/{ids['tmdb']}")
            poster = IMG + d["poster_path"] if d.get("poster_path") else None
        except Exception:
            pass
    conn.execute("BEGIN IMMEDIATE")
    try:
        cur = conn.execute(
            "INSERT OR IGNORE INTO sessions(app_id, app_name, app_logo, started_at, last_seen_at, last_play_at,"
            " ended_at, played_seconds, state, source, source_key, hint_title, hint_season, hint_episode)"
            " VALUES ('plex','Plex',?,?,?,?,?,?,'closed','plex',?,?,?,?)",
            (PLEX_LOGO, viewed - dur, viewed, viewed, viewed, dur, key, title, season, number))
        if cur.rowcount and ids and title:
            conn.execute(
                "INSERT INTO items(session_id, kind, title, year, season, episode, episode_title, tmdb, tvdb, imdb, poster)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                (cur.lastrowid, "episode" if ep else "movie", title, year, season, number, ep_title,
                 ids.get("tmdb"), ids.get("tvdb"), ids.get("imdb"), poster))
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    return bool(cur.rowcount)


def plex_import_http(conn) -> int:
    """Fallback: history over the HTTP API (cannot recover IDs of deleted items)."""
    last = conn.execute("SELECT MAX(ended_at) FROM sessions WHERE source='plex'").fetchone()[0]
    cutoff = int(max(time.time() - CFG["plex_days"] * 86400, (last or 0) - 2 * 86400))
    added, start = 0, 0
    while True:
        mc = plex_get(f"/status/sessions/history/all?sort=viewedAt:desc&accountID={CFG['plex_account']}"
                      f"&viewedAt%3E={cutoff}&X-Plex-Container-Start={start}&X-Plex-Container-Size=100")
        batch = mc.get("Metadata") or []
        for e in batch:
            added += plex_import_entry(conn, e)
        if len(batch) < 100:
            break
        start += 100
    return added


# --- Main path: Plex's views table keeps each view's Plex GUID even after the
# file is deleted, and Plex's metadata provider resolves it to imdb/tmdb/tvdb
# using Plex's own match (no fuzzy search).
VIEW_COLS = 'id, metadata_type, guid, grandparent_guid, title, grandparent_title, parent_index, "index", viewed_at, originally_available_at'


def plex_db_rows(where: str, args: tuple) -> list[sqlite3.Row]:
    c = sqlite3.connect(Path(CFG["plex_db"]).as_uri() + "?mode=ro", uri=True, timeout=10)  # read-only
    c.row_factory = sqlite3.Row
    try:
        return c.execute(f"SELECT {VIEW_COLS} FROM metadata_item_views WHERE {where}", args).fetchall()
    finally:
        c.close()


def plex_resolve(guid: str | None) -> tuple[dict, dict]:
    """Plex GUID (plex://show/…) -> (external ids, provider metadata)."""
    if not guid:
        return {}, {}
    if guid.startswith("plex://"):
        rk = guid.rsplit("/", 1)[-1]

        def fetch():
            st, j = http_json("GET", f"https://metadata.provider.plex.tv/library/metadata/{rk}",
                              {"X-Plex-Token": plex_token()})
            if st != 200 or not isinstance(j, dict):
                raise RuntimeError(f"Plex metadata provider {st}")
            return ((j.get("MediaContainer") or {}).get("Metadata") or [{}])[0]
        try:
            md = cached(("plexmd", rk), 7 * 86400, fetch)
        except RuntimeError:
            md = {}
        ids = guid_ids(md)
        if not ids:
            # The provider can 404 on a GUID Plex just assigned (new titles) while the
            # local library already holds the external IDs: use those.
            local = plex_local_guids(guid)
            ids, md = guid_ids(local), {**local, **md}
        if not ids:
            raise RuntimeError("Plex: no external IDs for " + guid)
        return ids, md
    return guid_ids({"guid": guid}), {}  # old agent: the IDs are in the guid


def plex_local_guids(guid: str) -> dict:
    """External IDs Plex stored in its own library for a GUID (tags of type 314),
    completed with TMDB external_ids. Empty if the item is no longer in the library."""
    try:
        c = sqlite3.connect(Path(CFG["plex_db"]).as_uri() + "?mode=ro", uri=True, timeout=10)
        try:
            row = c.execute("SELECT id, title, year FROM metadata_items WHERE guid=? LIMIT 1", (guid,)).fetchone()
            if not row:
                return {}
            tags = [t for (t,) in c.execute(
                "SELECT t.tag FROM taggings tg JOIN tags t ON t.id=tg.tag_id"
                " WHERE tg.metadata_item_id=? AND t.tag_type=314", (row[0],))]
        finally:
            c.close()
    except sqlite3.Error:
        return {}
    md = {"title": row[1], "year": row[2], "Guid": [{"id": t} for t in tags]}
    ids = guid_ids(md)
    if ids.get("tmdb") and not (ids.get("imdb") and ids.get("tvdb")):
        kind = "movie" if "/movie/" in guid else "tv"
        try:
            ext = tmdb(f"/{kind}/{ids['tmdb']}/external_ids")
            for k, src in (("imdb", "imdb_id"), ("tvdb", "tvdb_id")):
                if ext.get(src) and k not in ids:
                    md["Guid"].append({"id": f"{k}://{ext[src]}"})
        except Exception:
            pass
    return md


def plex_view_item(v) -> dict | None:
    ep = v["metadata_type"] == 4
    if ep:
        ids, show = plex_resolve(v["grandparent_guid"])
        title, year = show.get("title") or v["grandparent_title"], show.get("year")
    else:
        ids, md = plex_resolve(v["guid"])
        ids.pop("tvdb", None)
        oaa = v["originally_available_at"]
        title = md.get("title") or v["title"]
        year = md.get("year") or (datetime.fromtimestamp(oaa).year if isinstance(oaa, (int, float)) and oaa > 0
                                  else year_of(oaa if isinstance(oaa, str) else None))
    if not ids or not title:
        return None
    poster = None
    if ids.get("tmdb"):
        try:
            d = tmdb(f"/{'tv' if ep else 'movie'}/{ids['tmdb']}")
            poster = IMG + d["poster_path"] if d.get("poster_path") else None
        except Exception:
            pass
    return {"kind": "episode" if ep else "movie", "title": title, "year": year,
            "season": v["parent_index"] if ep else None, "episode": v["index"] if ep else None,
            "episode_title": v["title"] if ep else None, "tmdb": ids.get("tmdb"),
            "tvdb": ids.get("tvdb") if ep else None, "imdb": ids.get("imdb"), "poster": poster}


def insert_item(conn, sid: int, it: dict) -> None:
    conn.execute(
        "INSERT INTO items(session_id, kind, title, year, season, episode, episode_title, tmdb, tvdb, imdb, poster)"
        " VALUES (?,?,?,?,?,?,?,?,?,?,?)",
        (sid, it["kind"], it["title"], it["year"], it["season"], it["episode"], it["episode_title"],
         it["tmdb"], it["tvdb"], it["imdb"], it["poster"]))


def plex_import_db(conn, since: float) -> int:
    added = 0
    for v in plex_db_rows("account_id=? AND viewed_at > ? AND metadata_type IN (1,4) ORDER BY viewed_at",
                          (CFG["plex_account"], since)):
        key = f"/status/sessions/history/{v['id']}"  # same format as the HTTP API
        if conn.execute("SELECT 1 FROM sessions WHERE source_key=?", (key,)).fetchone():
            continue
        try:
            it = plex_view_item(v)
        except Exception:
            it = None  # retried in plex_enrich
        dur = 0.0
        try:
            dur = (plex_resolve(v["guid"])[1].get("duration") or 0) / 1000
        except Exception:
            pass
        viewed, ep = float(v["viewed_at"]), v["metadata_type"] == 4
        conn.execute("BEGIN IMMEDIATE")
        try:
            cur = conn.execute(
                "INSERT OR IGNORE INTO sessions(app_id, app_name, app_logo, started_at, last_seen_at, last_play_at,"
                " ended_at, played_seconds, state, source, source_key, hint_title, hint_season, hint_episode)"
                " VALUES ('plex','Plex',?,?,?,?,?,?,'closed','plex',?,?,?,?)",
                (PLEX_LOGO, viewed - dur, viewed, viewed, viewed, dur, key,
                 v["grandparent_title"] if ep else v["title"], v["parent_index"] if ep else None,
                 v["index"] if ep else None))
            if cur.rowcount and it:
                insert_item(conn, cur.lastrowid, it)
            conn.execute("COMMIT")
        except Exception:
            conn.execute("ROLLBACK")
            raise
        added += cur.rowcount
    return added


def plex_enrich(conn) -> int:
    """Retry identifying Plex sessions that came in without IDs."""
    fixed = 0
    for s_ in conn.execute("SELECT id, source_key FROM sessions WHERE source='plex' AND ignored=0"
                           " AND id NOT IN (SELECT session_id FROM items)").fetchall():
        m = re.search(r"/history/(\d+)$", s_["source_key"] or "")
        if not m:
            continue
        rows = plex_db_rows("id=?", (int(m.group(1)),))
        try:
            it = plex_view_item(rows[0]) if rows else None
        except Exception:
            it = None
        if it and not db.has_items(conn, s_["id"]):
            insert_item(conn, s_["id"], it)
            fixed += 1
    return fixed


def plex_fix_durations(conn) -> None:
    """Plex sessions without a duration (imported after the file was deleted)."""
    for s_ in conn.execute("SELECT id, source_key, ended_at FROM sessions WHERE source='plex'"
                           " AND played_seconds=0").fetchall():
        m = re.search(r"/history/(\d+)$", s_["source_key"] or "")
        rows = plex_db_rows("id=?", (int(m.group(1)),)) if m else []
        try:
            dur = (plex_resolve(rows[0]["guid"])[1].get("duration") or 0) / 1000 if rows else 0
        except Exception:
            dur = 0
        if not dur:  # provider has no runtime yet (just-aired episodes): use TMDB
            it = db.current_item(conn, s_["id"])
            try:
                if it and it["tmdb"] and it["kind"] == "movie":
                    dur = (tmdb(f"/movie/{it['tmdb']}").get("runtime") or 0) * 60
                elif it and it["tmdb"]:
                    eps = tmdb(f"/tv/{it['tmdb']}/season/{it['season']}").get("episodes", [])
                    dur = next(((e.get("runtime") or 0) * 60 for e in eps if e["episode_number"] == it["episode"]), 0)
            except Exception:
                dur = 0
        if dur:
            conn.execute("UPDATE sessions SET played_seconds=?, started_at=? WHERE id=? AND played_seconds=0",
                         (dur, s_["ended_at"] - dur, s_["id"]))


def plex_import() -> int:
    """Import what Plex marked as watched (owner account), already identified."""
    if not PLEX_IMPORT_LOCK.acquire(blocking=False):
        return 0
    try:
        conn = db.connect()
        last = conn.execute("SELECT MAX(ended_at) FROM sessions WHERE source='plex'").fetchone()[0]
        cutoff = int(max(time.time() - CFG["plex_days"] * 86400, (last or 0) - 2 * 86400))
        if os.access(CFG["plex_db"], os.R_OK):
            added = plex_import_db(conn, cutoff)
            added += plex_enrich(conn)
            plex_fix_durations(conn)
        else:
            added = plex_import_http(conn)
        PLEX_STATUS.update(last_run=time.time(), last_added=added, error=None)
        return added
    except Exception as e:
        PLEX_STATUS.update(last_run=time.time(), error=str(e))
        raise
    finally:
        PLEX_IMPORT_LOCK.release()


def plex_now() -> list:
    """What is playing on Plex right now (any device), with real position."""
    def fetch():
        out = []
        for m in plex_get("/status/sessions").get("Metadata") or []:
            if m.get("type") not in ("movie", "episode"):
                continue
            u = m.get("User") or {}
            if str(u.get("id")) != str(CFG["plex_account"]) and not (CFG["plex_user"] and u.get("title") == CFG["plex_user"]):
                continue  # other accounts on the server
            ep = m["type"] == "episode"
            key = m.get("grandparentRatingKey") if ep else m.get("ratingKey")
            ids, poster = {}, None
            if key:
                try:
                    md = cached(("plexlocal", key), 3600, lambda k=key: (
                        plex_get(f"/library/metadata/{k}?includeGuids=1").get("Metadata") or [{}])[0])
                    ids = guid_ids(md)
                    if not ep:
                        ids.pop("tvdb", None)
                    if ids.get("tmdb"):
                        d = tmdb(f"/{'tv' if ep else 'movie'}/{ids['tmdb']}")
                        poster = IMG + d["poster_path"] if d.get("poster_path") else None
                except Exception:
                    pass
            pl = m.get("Player") or {}
            out.append({
                "kind": "episode" if ep else "movie",
                "title": m.get("grandparentTitle") if ep else m.get("title"),
                "year": None if ep else m.get("year"),
                "season": m.get("parentIndex") if ep else None,
                "episode": m.get("index") if ep else None,
                "episode_title": m.get("title") if ep else None,
                "offset": (m.get("viewOffset") or 0) / 1000,
                "duration": (m.get("duration") or 0) / 1000,
                "state": pl.get("state") or "playing",
                "device": pl.get("title") or pl.get("product") or "",
                "poster": poster, "ids": ids,
            })
        return out
    return cached(("plexnow",), 3, fetch)


def plex_loop() -> None:
    first = True
    while True:
        try:
            n = plex_import()
            if n:
                print(f"Plex: {n} new or newly identified sessions", flush=True)
            if n or first:  # always verify on startup
                api_verify()
            first = False
        except Exception as e:
            print(f"Plex: {e}", flush=True)
        time.sleep(CFG["plex_interval"])


WINDOW = 12 * 3600  # a view "is the same" if Trakt has it within ±12 h


def trakt_history(kind: str, start_ts: float, end_ts: float) -> list:
    """Your Trakt history in a range (paginated). /sync/watched is useless: truncated at 100, no seasons."""
    out, page = [], 1
    while page <= 30:
        q = urllib.parse.urlencode({"start_at": iso(start_ts), "end_at": iso(end_ts), "page": page, "limit": 100})
        st, j = http_json("GET", f"https://api.trakt.tv/sync/history/{kind}?{q}", trakt_headers(True), timeout=40)
        if st != 200 or not isinstance(j, list):
            raise RuntimeError(f"Trakt {st} while reading your history")
        out += j
        if len(j) < 100:
            break
        page += 1
    return out


def in_trakt_history(it, ts: float, hist: list) -> bool:
    for h in hist:
        try:
            wt = datetime.strptime(h["watched_at"][:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
        except Exception:
            continue
        if abs(wt - ts) > WINDOW:
            continue
        if it["kind"] == "movie":
            ids = (h.get("movie") or {}).get("ids") or {}
        else:
            e = h.get("episode") or {}
            if e.get("season") != it["season"] or e.get("number") != it["episode"]:
                continue
            ids = (h.get("show") or {}).get("ids") or {}
        if any(it[k] and ids.get(k) == it[k] for k in ("imdb", "tmdb", "tvdb")):
            return True
    return False


def trakt_already(it, ts: float) -> bool:
    """Already in Trakt history around that time? (trakt-scrobbler also sends Plex plays)"""
    kind = "movies" if it["kind"] == "movie" else "episodes"
    return in_trakt_history(it, ts, trakt_history(kind, ts - WINDOW, ts + WINDOW))


def simkl_lists() -> dict:
    """Everything in your Simkl lists (completed, watching, plan to watch) with watched episodes; cached 5 min."""
    def fetch():
        cid, tok = simkl_creds()
        out = {}
        for typ in ("movies", "shows", "anime"):
            url = (f"https://api.simkl.com/sync/all-items/{typ}?extended=full&episode_watched_at=yes"
                   f"&client_id={cid}&app-name=tvhistory&app-version=1.0")
            st, j = http_json("GET", url, {"Authorization": f"Bearer {tok}"}, timeout=60)
            if st != 200:
                raise RuntimeError(f"Simkl {st} while reading your list")
            out[typ] = ((j or {}).get(typ) or []) if isinstance(j, dict) else []
        return out
    return cached(("simkl_lists",), 300, fetch)


def in_simkl(it, lists: dict) -> bool:
    same = lambda ids: any(it[k] and str(ids.get(k) or "") == str(it[k]) for k in ("imdb", "tmdb", "tvdb"))
    if it["kind"] == "movie":
        return any((x.get("status") == "completed" or x.get("last_watched_at"))
                   and same((x.get("movie") or {}).get("ids") or {}) for x in lists["movies"])
    for x in lists["shows"] + lists["anime"]:
        if not same((x.get("show") or {}).get("ids") or {}):
            continue
        for se in x.get("seasons") or []:
            if se.get("number") == it["season"] and any(e.get("number") == it["episode"] for e in se.get("episodes") or []):
                return True
        if x.get("status") == "completed" and not x.get("seasons"):
            return True
    return False


VERIFY_STATUSES = ("pending", "ready", "not_found", "error")


def api_verify(session_ids=None) -> dict:
    """Read-only on Trakt/Simkl: marks ✓ what is already in your history, and for the
    rest whether the title exists on the service ("to send") or not yet ("not listed yet")."""
    if not PUSH_LOCK.acquire(blocking=False):
        return {"error": "A push is running; try again in a moment"}
    try:
        conn = db.connect()
        q = "SELECT * FROM sessions WHERE ignored=0 AND id IN (SELECT session_id FROM items)"
        args: list = []
        if session_ids:
            q += f" AND id IN ({','.join('?' * len(session_ids))})"
            args = [int(x) for x in session_ids]
        work = []  # (item, ts)
        for sess in conn.execute(q, args).fetchall():
            items = conn.execute("SELECT * FROM items WHERE session_id=? ORDER BY COALESCE(season,0),"
                                 " COALESCE(episode,0), id", (sess["id"],)).fetchall()
            times = watched_times(sess, items)
            work += [(it, times[it["id"]]) for it in items
                     if it["trakt_status"] in VERIFY_STATUSES or it["simkl_status"] in VERIFY_STATUSES]
        counts = {"trakt": {}, "simkl": {}}
        if not work:
            return {"checked": 0, "counts": counts}
        lo, hi = min(t for _, t in work) - WINDOW, max(t for _, t in work) + WINDOW
        try:
            hist = {"movie": trakt_history("movies", lo, hi), "episode": trakt_history("episodes", lo, hi)}
        except Exception as e:
            hist, counts["trakt"]["error"] = None, str(e)
        try:
            sl = simkl_lists()
        except Exception as e:
            sl, counts["simkl"]["error"] = None, str(e)

        def decide(job):
            it, ts = job
            rec = {"kind": "movie" if it["kind"] == "movie" else "tv",
                   "ids": {k: it[k] for k in ("imdb", "tmdb", "tvdb") if it[k]}}
            out = {}
            for svc, have, exists in (("trakt", hist is not None and in_trakt_history(it, ts, hist[it["kind"]]), exists_trakt),
                                      ("simkl", sl is not None and in_simkl(it, sl), exists_simkl)):
                if it[f"{svc}_status"] not in VERIFY_STATUSES or (svc == "trakt" and hist is None) or (svc == "simkl" and sl is None):
                    continue
                if have:
                    out[svc] = ("added", "Already in your history")
                    continue
                try:
                    ex = exists(rec)
                except Exception:
                    ex = None
                if ex is False:
                    out[svc] = ("not_found", f"Not on {'Trakt' if svc == 'trakt' else 'Simkl'} yet")
                elif ex:
                    out[svc] = ("ready", None)
            return it["id"], out
        for iid, out in POOL.map(decide, work):
            for svc, (status, msg) in out.items():
                conn.execute(f"UPDATE items SET {svc}_status=?, {svc}_error=? WHERE id=?", (status, msg, iid))
                counts[svc][status] = counts[svc].get(status, 0) + 1
        return {"checked": len(work), "counts": counts}
    finally:
        PUSH_LOCK.release()


def verify_bg(session_ids=None) -> None:
    def run():
        try:
            api_verify(session_ids)
        except Exception as e:
            print(f"Verify: {e}", flush=True)
    threading.Thread(target=run, daemon=True).start()


# ------------------------------------------------------------- identification
def api_identify(sid: int, b: dict) -> dict:
    kind = b.get("kind")
    if kind not in ("movie", "tv"):
        raise ValueError("kind must be movie or tv")
    ids = {k: b.get(k) or None for k in ("tmdb", "imdb", "tvdb")}
    title, year, poster = b.get("title"), b.get("year"), b.get("poster")
    ep_titles: dict = {}
    if ids["tmdb"]:
        det = tmdb(f"/{kind}/{int(ids['tmdb'])}")
        ext = tmdb(f"/{kind}/{int(ids['tmdb'])}/external_ids")
        ids["imdb"] = ids["imdb"] or ext.get("imdb_id")
        if kind == "tv":
            ids["tvdb"] = ids["tvdb"] or ext.get("tvdb_id")
        title = det.get("title") or det.get("name") or title
        year = year_of(det.get("release_date") or det.get("first_air_date")) or year
        poster = (IMG + det["poster_path"]) if det.get("poster_path") else poster
    if not title:
        raise ValueError("Title is required")
    rows = []
    if kind == "movie":
        rows.append(("movie", title, year, None, None, None, ids["tmdb"], None, ids["imdb"], poster))
    else:
        season = int(b["season"])
        eps = sorted({int(e) for e in b.get("episodes") or []})
        if not eps:
            raise ValueError("Pick at least one episode")
        if ids["tmdb"]:
            try:
                ep_titles = {e["episode_number"]: e.get("name") for e in
                             tmdb(f"/tv/{int(ids['tmdb'])}/season/{season}").get("episodes", [])}
            except Exception:
                pass
        for e in eps:
            rows.append(("episode", title, year, season, e, ep_titles.get(e) or b.get("episode_title"),
                         ids["tmdb"], ids["tvdb"], ids["imdb"], poster))
    conn = db.connect()
    conn.execute("BEGIN IMMEDIATE")
    try:
        if not conn.execute("SELECT 1 FROM sessions WHERE id=?", (sid,)).fetchone():
            raise ValueError("No such session")
        conn.execute("DELETE FROM items WHERE session_id=?", (sid,))
        conn.executemany(
            "INSERT INTO items(session_id, kind, title, year, season, episode, episode_title, tmdb, tvdb, imdb, poster)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?)", [(sid, *r) for r in rows])
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    return {"ok": True, "items": len(rows)}


def api_next(sid: int) -> dict:
    """Split the open session: what came before stays as the current episode and
    a new session starts with the next one (apps autoplay without leaving the player)."""
    conn = db.connect()
    sess = conn.execute("SELECT * FROM sessions WHERE id=?", (sid,)).fetchone()
    if not sess or sess["state"] != "open":
        raise ValueError("Only works on the session that is playing now")
    it = db.current_item(conn, sid)
    if not it or it["kind"] != "episode":
        raise ValueError("Identify the current episode first")
    s, e = it["season"], it["episode"] + 1
    ep_title = None
    if it["tmdb"]:
        counts = {x["season_number"]: x.get("episode_count") or 0
                  for x in tmdb(f"/tv/{it['tmdb']}").get("seasons", [])}
        if e > counts.get(s, e):
            nxt = [n for n in sorted(counts) if n > s and counts[n] > 0]
            if not nxt:
                raise ValueError("No next episode on TMDB")
            s, e = nxt[0], 1
        try:
            ep_title = next((x.get("name") for x in tmdb(f"/tv/{it['tmdb']}/season/{s}").get("episodes", [])
                             if x["episode_number"] == e), None)
        except Exception:
            pass
    now = time.time()
    conn.execute("BEGIN IMMEDIATE")
    try:
        conn.execute("UPDATE sessions SET state='closed', ended_at=? WHERE id=? AND state='open'", (now, sid))
        cur = conn.execute(
            "INSERT INTO sessions(app_id, app_name, app_logo, started_at, last_seen_at, last_play_at)"
            " VALUES (?,?,?,?,?,?)", (sess["app_id"], sess["app_name"], sess["app_logo"], now, now, now))
        conn.execute(
            "INSERT INTO items(session_id, kind, title, year, season, episode, episode_title, tmdb, tvdb, imdb, poster)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (cur.lastrowid, "episode", it["title"], it["year"], s, e, ep_title,
             it["tmdb"], it["tvdb"], it["imdb"], it["poster"]))
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    return {"ok": True, "season": s, "episode": e, "session": cur.lastrowid}


def api_state(days: int) -> dict:
    conn = db.connect()
    since = time.time() - days * 86400
    sessions = [dict(r) for r in conn.execute(
        "SELECT * FROM sessions WHERE started_at >= ? OR state='open' ORDER BY started_at DESC", (since,))]
    by: dict = {}
    for r in conn.execute(
            "SELECT * FROM items WHERE session_id IN (SELECT id FROM sessions WHERE started_at >= ? OR state='open')"
            " ORDER BY COALESCE(season,0), COALESCE(episode,0), id", (since,)):
        by.setdefault(r["session_id"], []).append(dict(r))
    pending = waiting = 0   # counted per item, not per item x service
    for s in sessions:
        s["items"] = by.get(s["id"], [])
        if s["ignored"]:
            continue
        for i in s["items"]:
            left = {i[f"{t}_status"] for t in ("trakt", "simkl")} & set(VERIFY_STATUSES)
            if left:
                pending += 1
                waiting += left == {"not_found"}   # nothing to do until the title is listed
    now = next((s for s in sessions if s["state"] == "open"), None)
    try:
        pnow = plex_now() if CFG["plex"] else []
    except Exception:
        pnow = []
    return {"now": now, "sessions": sessions, "pending": pending, "waiting": waiting, "server_time": time.time(),
            "plex": {"enabled": CFG["plex"], **PLEX_STATUS}, "plex_now": pnow}


# -------------------------------------------------------------------- server
class Handler(BaseHTTPRequestHandler):
    server_version = "tvhistory/1.0"

    def log_message(self, fmt, *args):  # quiet except for errors
        pass

    def send(self, code: int, obj=None, ctype="application/json; charset=utf-8", raw: bytes | None = None):
        data = raw if raw is not None else json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        qs = urllib.parse.parse_qs(u.query)
        try:
            if u.path == "/":
                return self.send(200, raw=PAGE.encode(), ctype="text/html; charset=utf-8")
            if u.path == "/api/state":
                return self.send(200, api_state(int(qs.get("days", ["60"])[0])))
            if u.path == "/api/search":
                q = (qs.get("q", [""])[0]).strip()
                return self.send(200, search_all(q) if len(q) >= 2 else {"results": [], "errors": []})
            m = re.fullmatch(r"/api/tmdb/tv/(\d+)", u.path)
            if m:
                d = tmdb(f"/tv/{m.group(1)}")
                return self.send(200, {"seasons": [
                    {"number": s["season_number"], "name": s.get("name"), "count": s.get("episode_count") or 0}
                    for s in d.get("seasons", []) if (s.get("episode_count") or 0) > 0]})
            m = re.fullmatch(r"/api/tmdb/tv/(\d+)/season/(\d+)", u.path)
            if m:
                d = tmdb(f"/tv/{m.group(1)}/season/{m.group(2)}")
                return self.send(200, {"episodes": [
                    {"number": e["episode_number"], "name": e.get("name"), "runtime": e.get("runtime"),
                     "air_date": e.get("air_date")} for e in d.get("episodes", [])]})
            return self.send(404, {"error": "not found"})
        except Exception as e:
            return self.send(500, {"error": str(e)})

    def do_POST(self):
        # Anti-CSRF: JSON required (forces a preflight) and same origin when Origin is sent
        if not (self.headers.get("Content-Type") or "").startswith("application/json"):
            return self.send(415, {"error": "application/json required"})
        origin = self.headers.get("Origin")
        if origin:
            host_of = lambda v: (v or "").split(",")[0].strip().rsplit(":", 1)[0].strip("[]").lower()
            allowed = {host_of(self.headers.get("Host")), host_of(self.headers.get("X-Forwarded-Host"))}
            if (urllib.parse.urlparse(origin).hostname or "").lower() not in allowed - {""}:
                return self.send(403, {"error": "Origin not allowed"})
        try:
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n) or b"{}") if n else {}
        except Exception:
            return self.send(400, {"error": "Invalid JSON"})
        path = urllib.parse.urlparse(self.path).path
        try:
            if path == "/api/push":
                return self.send(200, api_push(body))
            if path == "/api/verify":
                return self.send(200, api_verify(body.get("session_ids")))
            if path == "/api/plex/import":
                if not CFG["plex"]:
                    return self.send(400, {"error": "Plex import is disabled"})
                return self.send(200, {"imported": plex_import()})
            m = re.fullmatch(r"/api/sessions/(\d+)/(identify|next|ignore|delete)", path)
            if m:
                sid, action = int(m.group(1)), m.group(2)
                if action == "identify":
                    r = api_identify(sid, body)
                    verify_bg([sid])
                    return self.send(200, r)
                if action == "next":
                    return self.send(200, api_next(sid))
                conn = db.connect()
                if action == "ignore":
                    conn.execute("UPDATE sessions SET ignored=? WHERE id=?", (1 if body.get("ignored", True) else 0, sid))
                else:
                    conn.execute("DELETE FROM sessions WHERE id=? AND state='closed'", (sid,))
                return self.send(200, {"ok": True})
            m = re.fullmatch(r"/api/items/(\d+)/delete", path)
            if m:
                db.connect().execute("DELETE FROM items WHERE id=?", (int(m.group(1)),))
                return self.send(200, {"ok": True})
            return self.send(404, {"error": "not found"})
        except ValueError as e:
            return self.send(400, {"error": str(e)})
        except Exception as e:
            return self.send(500, {"error": str(e)})


PAGE = Path(__file__).with_name("tvhistory_dashboard.html").read_text(encoding="utf-8")


def main() -> None:
    db.log_to_file_if_windowless("tvhistory")
    db.connect().close()
    if CFG["plex"]:
        threading.Thread(target=plex_loop, name="plex", daemon=True).start()  # imports and verifies
    else:
        verify_bg()
    srv = ThreadingHTTPServer((CFG["bind"], CFG["port"]), Handler)
    print(f"TV History on http://{CFG['bind']}:{CFG['port']}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
