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
import unicodedata
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
        "auto_push": bool(h.get("auto_push", True)),            # send identified sessions when they end
        "auto_push_min_ratio": float(h.get("auto_push_min_ratio", 0.8)),  # watched share of the runtime
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


def _norm_title(s: str | None) -> str:
    s = unicodedata.normalize("NFD", (s or "").lower().replace("&", " and "))
    s = "".join(c for c in s if not unicodedata.combining(c))
    return " ".join(re.sub(r"[^\w]+", " ", s).split())


def imdb_poster(kind: str, title: str | None, year: int | None, imdb: str | None) -> str | None:
    """Poster from IMDb for what TMDB has no poster for yet (brand-new films and series, or titles
    TMDB does not list at all). By IMDb id when known; otherwise only a single exact match by
    title, year and kind. It only ever gives a poster, never an ID."""
    q = imdb or title
    if not q:
        return None

    def fetch():
        st, j = http_json("GET", f"https://v3.sg.media-imdb.com/suggestion/x/{urllib.parse.quote(q.lower())}.json")
        return j if st == 200 and isinstance(j, dict) else {}
    try:
        j = cached(("imdbsug", q.lower()), 6 * 3600, fetch)
    except Exception:
        return None
    want_kind = "tv" if kind in ("episode", "tv") else "movie"
    hits = []
    for r in j.get("d", []):
        img = (r.get("i") or {}).get("imageUrl")
        if not img:
            continue
        if imdb:
            if r.get("id") == imdb:
                return imdb_img(img)
        elif (IMDB_KIND.get(r.get("qid")) == want_kind and _norm_title(r.get("l")) == _norm_title(title)
              and (not year or r.get("y") == year)):
            hits.append(img)
    return imdb_img(hits[0]) if len(hits) == 1 else None


POSTER_TRIED: dict = {}   # item id -> last attempt, so titles without any poster are retried every 6 h


def fill_posters() -> int:
    """Items of the last 30 days without a poster: TMDB again (posters arrive days after a
    listing), then IMDb. Used by the dashboard and, through the DB, by the Discord presence."""
    conn = db.connect()
    rows = conn.execute(
        "SELECT i.id, i.kind, i.title, i.year, i.tmdb, i.imdb FROM items i JOIN sessions s ON s.id = i.session_id"
        " WHERE COALESCE(i.poster, '') = '' AND s.started_at > ?", (time.time() - 30 * 86400,)).fetchall()
    fixed = 0
    for it in rows:
        if time.time() - POSTER_TRIED.get(it["id"], 0) < 6 * 3600:
            continue
        POSTER_TRIED[it["id"]] = time.time()
        poster = None
        if it["tmdb"]:
            try:
                d = tmdb(f"/{'tv' if it['kind'] == 'episode' else 'movie'}/{it['tmdb']}")
                poster = IMG + d["poster_path"] if d.get("poster_path") else None
            except Exception:
                pass
        poster = poster or imdb_poster(it["kind"], it["title"], it["year"], it["imdb"])
        if poster:
            conn.execute("UPDATE items SET poster = ? WHERE id = ? AND COALESCE(poster, '') = ''", (poster, it["id"]))
            fixed += 1
    return fixed


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
        # an IMDb id shared by a movie and a series returns the movie even with type=show: filter by type
        hit = next((x for x in j if x.get("type") == t and x.get(t)), None) if st == 200 and isinstance(j, list) else None
        if hit:
            return hit[t]["ids"].get("trakt")
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


def search_all(q: str, prefer: str | None = None) -> dict:
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
    return {"results": collapse_listings(merged, prefer), "errors": errors}


def collapse_listings(recs: list, prefer: str | None = None) -> list:
    """The same work listed twice (TMDB has it as a movie AND a one-episode series, same IMDb id):
    keep one card, the listing that exists on most services (that is where it will be sent),
    then the one with a TVDB id; the other stays reachable as an alternative."""
    def score(r):
        ex = r.get("exists") or {}
        # prefer="tv": the device reported an episode (HBO "Duty Calls" of "The Mad Pooper"), so the
        # series listing is the right one even if a service only has the movie listing (yet)
        return (r["kind"] == prefer, bool(ex.get("trakt")) + bool(ex.get("simkl")), bool(r["ids"].get("tvdb")), "imdb" in r["src"])
    out: list[dict] = []
    for r in recs:
        imdb = r["ids"].get("imdb")
        twin = next((o for o in out if imdb and o["ids"].get("imdb") == imdb and o["kind"] != r["kind"]), None)
        if not twin:
            out.append(r)
            continue
        best, other = (r, twin) if score(r) > score(twin) else (twin, r)
        best.setdefault("alts", []).append({k: other[k] for k in ("kind", "title", "year", "label", "ids", "exists", "poster")})
        if best is r:
            out[out.index(twin)] = r
    return out


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


PLEX_CODECS = {"hevc": "HEVC", "h264": "H.264", "av1": "AV1", "vp9": "VP9", "mpeg2video": "MPEG-2", "vc1": "VC-1"}


def box_playing_format(m: dict) -> str | None:
    """Dynamic range the Android TV box is really decoding for this Plex session (written by tvpresence
    from the box's video driver), or None when Plex plays elsewhere or nothing was reported."""
    try:
        host = (tomllib.loads((db.app_dir("config") / "config.toml").read_text()).get("androidtv") or {}).get("host")
        if not host or (m.get("Player") or {}).get("address") != host:
            return None
        bv = json.loads((db.DATA_DIR / "box_video.json").read_text())
        started = time.time() - (m.get("viewOffset") or 0) / 1000 - 120
        return bv["fmt"] if bv.get("app") == "com.plexapp.android" and bv.get("ts", 0) >= started else None
    except Exception:
        return None


def plex_video(m: dict) -> dict:
    """Format of the file Plex is serving: resolution/codec (same keys as the Android TV capture), HDR, audio, direct play vs transcode."""
    media = next((x for x in m.get("Media") or [] if x.get("selected")), None) or (m.get("Media") or [{}])[0]
    part = (media.get("Part") or [{}])[0]
    streams = part.get("Stream") or []
    v = next((x for x in streams if x.get("streamType") == 1), {})
    a = next((x for x in streams if x.get("streamType") == 2 and x.get("selected")), None) or next((x for x in streams if x.get("streamType") == 2), {})
    title = v.get("displayTitle") or ""
    hdr = next((t for t in ("Dolby Vision", "HDR10+", "HDR10", "HLG") if t in title), None)
    if not hdr:
        hdr = "Dolby Vision" if v.get("DOVIPresent") else {"smpte2084": "HDR10", "arib-std-b67": "HLG"}.get(v.get("colorTrc"))
    ts = m.get("TranscodeSession") or {}
    decision = ("transcode" if ts.get("videoDecision") == "transcode" else
                "direct play" if part.get("decision") == "directplay" or not ts else "direct stream")
    codec = media.get("videoCodec") or v.get("codec") or ""
    file_hdr = hdr or ("SDR" if media.get("width") else None)
    live = box_playing_format(m)
    return {"video_width": media.get("width") or v.get("width"), "video_height": media.get("height") or v.get("height"),
            "video_codec": PLEX_CODECS.get(codec, codec.upper()),
            # what really plays (the box's video driver) when Plex plays on the box; else the file's format
            "video_hdr": live or file_hdr, "video_hdr_live": live, "video_hdr_file": file_hdr, "video_decision": decision,
            "video_bitrate": media.get("bitrate"),
            "audio": re.sub(r"^.*\((.*)\)$", r"\1", a.get("displayTitle") or "") or None}


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
                    ids = guid_ids(md) or (plex_resolve(md.get("guid"))[0] if md.get("guid") else {})
                    if not ep:
                        ids.pop("tvdb", None)
                    if ids.get("tmdb"):
                        d = tmdb(f"/{'tv' if ep else 'movie'}/{ids['tmdb']}")
                        poster = IMG + d["poster_path"] if d.get("poster_path") else None
                except Exception:
                    pass
            if not poster:   # not on TMDB (yet), or no poster there: IMDb's
                poster = imdb_poster("episode" if ep else "movie", m.get("grandparentTitle") if ep else m.get("title"),
                                     None if ep else m.get("year"), ids.get("imdb"))
            pl = m.get("Player") or {}
            out.append({**plex_video(m),
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
                "match": match_badges("episode" if ep else "movie", ids),
                "trakt_watching": trakt_watching(),
            })
        return out
    return cached(("plexnow",), 3, fetch)


def match_badges(kind: str, ids: dict) -> dict:
    """Does the title exist on Trakt / Simkl (so a scrobble can land)? True / False / None (unknown). Cached 1 h."""
    if not ids:
        return {"trakt": None, "simkl": None}
    rec = {"kind": "movie" if kind == "movie" else "tv", "ids": ids}
    out = {}
    for svc, fn in (("trakt", exists_trakt), ("simkl", exists_simkl)):
        try:
            v = cached(("exists", svc, rec["kind"], json.dumps(ids, sort_keys=True)), 3600, lambda f=fn: f(rec))
        except Exception:
            v = None
        out[svc] = None if v is None else bool(v)
    return out


def trakt_watching() -> dict | None:
    """What Trakt shows as being watched right now (i.e. trakt-scrobbler is scrobbling). Cached 30 s."""
    def fetch():
        st, j = http_json("GET", "https://api.trakt.tv/users/me/watching", trakt_headers(True))
        if st != 200 or not isinstance(j, dict):
            return None  # 204 = nothing
        m = j.get("movie") or j.get("show") or {}
        e = j.get("episode") or {}
        return {"title": m.get("title"), "season": e.get("season"), "episode": e.get("number"), "ids": m.get("ids", {})}
    try:
        return cached(("trakt_watching",), 30, fetch)
    except Exception:
        return None


SHORT_KEEP_DAYS = 7


def tidy_short_sessions() -> None:
    """Short YouTube sessions (auto_hidden): purge the unidentified ones after SHORT_KEEP_DAYS.
    No automatic matching: YouTube titles are too unreliable, the user picks the match."""
    conn = db.connect()
    conn.execute("DELETE FROM sessions WHERE auto_hidden=1 AND ended_at < ? AND id NOT IN (SELECT session_id FROM items)",
                 (time.time() - SHORT_KEEP_DAYS * 86400,))


def tvmaze_show_id(imdb: str | None, tvdb: int | None) -> int | None:
    """TVmaze (free, no key, very current for new releases) by TheTVDB or IMDb id."""
    def fetch():
        for key, val in (("thetvdb", tvdb), ("imdb", imdb)):
            if not val:
                continue
            st, j = http_json("GET", f"https://api.tvmaze.com/lookup/shows?{key}={val}", timeout=10)
            if st == 200 and isinstance(j, dict) and j.get("id"):
                return j["id"]
        return None
    return cached(("tvmaze_id", imdb, tvdb), 6 * 3600, fetch)


def tvmaze_episodes(show_id: int) -> list:
    def fetch():
        st, j = http_json("GET", f"https://api.tvmaze.com/shows/{show_id}/episodes", timeout=15)
        return j if st == 200 and isinstance(j, list) else []
    return cached(("tvmaze_eps", show_id), 3600, fetch)


def show_episodes(tmdb_id: int, imdb: str | None = None, tvdb: int | None = None) -> dict:
    """Every season and episode of a show, merged from TMDB and TVmaze (union by season+number),
    so episodes TMDB has not added yet still show up. Specials last."""
    show = tmdb(f"/tv/{tmdb_id}")
    if not (imdb and tvdb):
        try:
            ext = tmdb(f"/tv/{tmdb_id}/external_ids")
            imdb, tvdb = imdb or ext.get("imdb_id"), tvdb or ext.get("tvdb_id")
        except Exception:
            pass
    seasons = {s["season_number"]: {"number": s["season_number"], "name": s.get("name"), "episodes": {}}
               for s in show.get("seasons", [])}

    def tmdb_season(n):
        try:
            return n, tmdb(f"/tv/{tmdb_id}/season/{n}").get("episodes", [])
        except Exception:
            return n, []
    for n, eps in POOL.map(tmdb_season, list(seasons)[:40]):
        for e in eps:
            seasons[n]["episodes"][e["episode_number"]] = {
                "number": e["episode_number"], "name": e.get("name"), "runtime": e.get("runtime"),
                "air_date": e.get("air_date"), "src": ["tmdb"]}
    tvm = None
    try:
        tvm = tvmaze_show_id(imdb, tvdb)
        for e in tvmaze_episodes(tvm) if tvm else []:
            if e.get("season") is None or e.get("number") is None:
                continue
            sz = seasons.setdefault(e["season"], {"number": e["season"], "name": f"Season {e['season']}", "episodes": {}})
            cur = sz["episodes"].get(e["number"])
            if cur:
                cur["src"].append("tvmaze")
                cur["runtime"] = cur["runtime"] or e.get("runtime")
                cur["name"] = cur["name"] or e.get("name")
            else:
                sz["episodes"][e["number"]] = {"number": e["number"], "name": e.get("name"), "runtime": e.get("runtime"),
                                               "air_date": e.get("airdate"), "src": ["tvmaze"]}
    except Exception:
        pass
    out = []
    for n in sorted(seasons, key=lambda k: (k == 0, k)):
        eps = [seasons[n]["episodes"][k] for k in sorted(seasons[n]["episodes"])]
        if eps:
            out.append({"number": n, "name": seasons[n]["name"] or f"Season {n}", "episodes": eps})
    return {"seasons": out, "tvmaze": tvm}


def item_runtime_info(it) -> tuple[int | None, bool]:
    """(runtime in minutes according to TMDB, estimated?). Estimated = borrowed from the movie
    listing of a one-episode series: often a placeholder (The Mad Pooper S1E1: a 120-min movie
    listing for a docuseries episode), so it neither draws an end time nor gates auto-send."""
    if not it["tmdb"]:
        return None, False
    try:
        if it["kind"] == "movie":
            return tmdb(f"/movie/{it['tmdb']}").get("runtime") or None, False
        eps = tmdb(f"/tv/{it['tmdb']}/season/{it['season']}").get("episodes", [])
        rt = next((e.get("runtime") for e in eps if e.get("episode_number") == it["episode"]), None)
        if rt:
            return rt, False
        try:   # TVmaze often has new episodes (and their length) before TMDB
            tvm = tvmaze_show_id(it["imdb"], it["tvdb"])
            rt = next((e.get("runtime") for e in (tvmaze_episodes(tvm) if tvm else [])
                       if e.get("season") == it["season"] and e.get("number") == it["episode"]), None)
            if rt:
                return rt, False
        except Exception:
            pass
        show = tmdb(f"/tv/{it['tmdb']}")   # fall back to the show's typical episode length
        rt = (show.get("episode_run_time") or [None])[0] or (show.get("last_episode_to_air") or {}).get("runtime")
        if rt:
            return rt, False
        if show.get("number_of_episodes") == 1 and it["imdb"]:
            # a one-episode "series" that TMDB also lists as a movie (same IMDb id): the movie's runtime
            mv = tmdb(f"/find/{it['imdb']}", external_source="imdb_id").get("movie_results") or []
            rt = tmdb(f"/movie/{mv[0]['id']}").get("runtime") if mv else None
            return rt or None, bool(rt)
        return None, False
    except Exception:
        return None, False


def item_runtime(it) -> int | None:
    return item_runtime_info(it)[0]


def fill_runtimes(sid: int) -> None:
    conn = db.connect()
    for it in conn.execute("SELECT * FROM items WHERE session_id=? AND runtime IS NULL", (sid,)).fetchall():
        rt, est = item_runtime_info(it)
        if rt:
            conn.execute("UPDATE items SET runtime=?, runtime_est=? WHERE id=?", (rt, int(est), it["id"]))


def expected_seconds(items) -> float | None:
    """Total runtime of a session's items according to TMDB (None if any is unknown)."""
    total = 0.0
    for it in items:
        rt = (it["runtime"] if "runtime" in it.keys() else None) or item_runtime(it)
        if not rt:
            return None
        total += rt * 60
    return total


REWATCH_WINDOW = 24 * 3600   # the same title again this soon: probably not a second viewing
PARTS_WINDOW = 24 * 3600     # an episode picked up again within this is still the same viewing


def _same_title_parts(conn, sess, it):
    """Earlier sessions (same app, within PARTS_WINDOW) holding the same single title, never sent."""
    return conn.execute(
        "SELECT s.*, i.id AS item_id FROM sessions s JOIN items i ON i.session_id = s.id WHERE s.id <> ? AND s.app_id = ?"
        " AND s.ignored = 0 AND s.started_at < ? AND s.started_at > ? AND i.kind = ?"
        " AND COALESCE(i.season, -1) = COALESCE(?, -1) AND COALESCE(i.episode, -1) = COALESCE(?, -1)"
        " AND ((i.tmdb IS NOT NULL AND i.tmdb = ?) OR (COALESCE(i.imdb, '') <> '' AND i.imdb = ?))"
        " AND i.trakt_status NOT IN ('added', 'skipped') AND (SELECT COUNT(*) FROM items WHERE session_id = s.id) = 1",
        (sess["id"], sess["app_id"], sess["started_at"], sess["started_at"] - PARTS_WINDOW, it["kind"],
         it["season"], it["episode"], it["tmdb"], it["imdb"] or "")).fetchall()


def furthest_before(conn, sess, it) -> float:
    """Furthest real position reached in the earlier, unsent sessions of the same title."""
    return max((p["position"] or 0 for p in _same_title_parts(conn, sess, it)), default=0)


def absorb_parts(conn, sid: int) -> None:
    """Once a session was sent, its earlier partial sessions of the same title are part of that
    viewing: marked so, never sent on their own."""
    sess = conn.execute("SELECT * FROM sessions WHERE id=?", (sid,)).fetchone()
    items = conn.execute("SELECT * FROM items WHERE session_id=?", (sid,)).fetchall()
    if not sess or len(items) != 1 or items[0]["trakt_status"] not in ("added",) and items[0]["simkl_status"] not in ("added",):
        return
    note = "Part of the viewing sent with the session at " + datetime.fromtimestamp(sess["started_at"]).strftime("%H:%M")
    for p in _same_title_parts(conn, sess, items[0]):
        conn.execute("UPDATE items SET trakt_status = CASE WHEN trakt_status IN ('pending','ready','error') THEN 'skipped' ELSE trakt_status END,"
                     " simkl_status = CASE WHEN simkl_status IN ('pending','ready','error') THEN 'skipped' ELSE simkl_status END,"
                     " trakt_error = ?, simkl_error = ? WHERE id = ?", (note, note, p["item_id"]))


def seen_shortly_before(conn, sess, items):
    """The earlier session (ended within REWATCH_WINDOW before this one started) whose viewing of one
    of these titles was already sent: same movie, or same show and episode, by external ID. A start
    that was never sent (a minute, then you came back) does not count."""
    for it in items:
        if not (it["tmdb"] or it["imdb"]):
            continue
        row = conn.execute(
            "SELECT s.* FROM items i JOIN sessions s ON s.id = i.session_id WHERE s.id <> ? AND s.ignored = 0"
            " AND (i.trakt_status = 'added' OR i.simkl_status = 'added')"   # a viewing that counted, not a 1-min start
            " AND i.kind = ? AND COALESCE(i.season, -1) = COALESCE(?, -1) AND COALESCE(i.episode, -1) = COALESCE(?, -1)"
            " AND ((i.tmdb IS NOT NULL AND i.tmdb = ?) OR (COALESCE(i.imdb, '') <> '' AND i.imdb = ?))"
            " AND s.ended_at <= ? AND s.ended_at >= ? ORDER BY s.ended_at DESC LIMIT 1",
            (sess["id"], it["kind"], it["season"], it["episode"], it["tmdb"], it["imdb"] or "",
             sess["started_at"] + 60, sess["started_at"] - REWATCH_WINDOW)).fetchone()
        if row:
            return row
    return None


def auto_push() -> None:
    """Send identified sessions once they end, like a scrobble would: TV/Android TV sessions only if
    at least auto_push_min_ratio of the runtime was played; Plex views always (Plex already marked
    them watched). Already-present plays are detected first (verify), so nothing is duplicated."""
    if not CFG["auto_push"]:
        return
    conn = db.connect()
    rows = conn.execute(
        "SELECT * FROM sessions WHERE state='closed' AND ignored=0 AND auto_hidden=0 AND ended_at < ?"
        " AND id IN (SELECT session_id FROM items WHERE trakt_status IN ('pending','ready','error')"
        "   OR simkl_status IN ('pending','ready','error')"
        "   OR ((trakt_status='not_found' OR simkl_status='not_found') AND COALESCE(pushed_at,0) < ?))",
        (time.time() - 15, time.time() - 86400)).fetchall()
    ready = []
    for sess in rows:
        items = conn.execute("SELECT * FROM items WHERE session_id=?", (sess["id"],)).fetchall()
        if any(it["auto_how"] == "progress" for it in items):
            continue   # a guessed episode waits for you to confirm it
        prev = seen_shortly_before(conn, sess, items)
        if prev:   # watched again within a day: you confirm it with Send if it was a second viewing
            when = datetime.fromtimestamp(prev["ended_at"]).strftime("%b %d %H:%M")
            note = f"You already watched this ({when}): not sent automatically. Press Send if it was a second viewing"
            conn.execute("UPDATE items SET trakt_error=CASE WHEN trakt_status IN ('pending','ready') THEN ? ELSE trakt_error END,"
                         " simkl_error=CASE WHEN simkl_status IN ('pending','ready') THEN ? ELSE simkl_error END"
                         " WHERE session_id=?", (note, note, sess["id"]))
            continue
        if sess["source"] != "plex":
            if any(it["runtime_est"] for it in items):
                note = "Runtime is only an estimate from TMDB: not sent automatically. Send it if you finished it"
                conn.execute("UPDATE items SET trakt_error=CASE WHEN trakt_status IN ('pending','ready') THEN ? ELSE trakt_error END,"
                             " simkl_error=CASE WHEN simkl_status IN ('pending','ready') THEN ? ELSE simkl_error END"
                             " WHERE session_id=?", (note, note, sess["id"]))
                continue
            exp = expected_seconds(items)
            # Android TV knows the real position: a session that started mid-way still counts what was watched.
            # With one title the furthest point is the measure: played_seconds adds up every attempt, so
            # restarting the episode after coming back (same title within 6 h resumes the session) or
            # rewinding counted the same minutes twice.
            if sess["source"] == "androidtv" and sess["position"] and len(items) == 1:
                # one episode picked up over several sessions is one viewing: the furthest point reached
                watched = max(sess["position"], furthest_before(conn, sess, items[0]))
            else:
                watched = max(sess["played_seconds"], sess["position"] or 0)
            ratio = watched / exp if exp else None
            if ratio is None or ratio < CFG["auto_push_min_ratio"]:
                note = (f"Watched {ratio:.0%} of the runtime (< {CFG['auto_push_min_ratio']:.0%}): not sent automatically"
                        if ratio is not None else "Runtime unknown: not sent automatically")
                conn.execute("UPDATE items SET trakt_error=CASE WHEN trakt_status IN ('pending','ready') THEN ? ELSE trakt_error END,"
                             " simkl_error=CASE WHEN simkl_status IN ('pending','ready') THEN ? ELSE simkl_error END"
                             " WHERE session_id=?", (note, note, sess["id"]))
                continue
        ready.append(sess["id"])
    if not ready:
        return
    api_verify(ready)              # marks what is already in your history as added
    r = api_push({"session_ids": ready})
    for sid in ready:              # the earlier partial sessions of what was just sent are part of it
        absorb_parts(conn, sid)
    sent = [x for x in r.get("results", []) if x.get("status") == "added"]
    if sent:
        print(f"Auto-sent: {', '.join(sorted({x['label'] + ' → ' + x['target'] for x in sent}))}", flush=True)


def auto_loop() -> None:
    while True:
        try:
            auto_identify()
        except Exception as e:
            print(f"Auto-identify: {e}", flush=True)
        try:
            netflix_identify()
        except Exception as e:
            print(f"Netflix: {e}", flush=True)
        try:
            n = fill_posters()
            if n:
                print(f"Posters: {n} filled", flush=True)
        except Exception as e:
            print(f"Posters: {e}", flush=True)
        # sends within half a minute of a session ending; meanwhile, a session that just
        # published a title is identified within seconds (before you reach for Identify)
        for _ in range(10):
            time.sleep(3)
            try:
                if fresh_hints():
                    auto_identify()
            except Exception as e:
                print(f"Auto-identify: {e}", flush=True)
        try:
            auto_push()
        except Exception as e:
            print(f"Auto-send: {e}", flush=True)


def plex_loop() -> None:
    first = True
    while True:
        try:
            tidy_short_sessions()
        except Exception as e:
            print(f"Short sessions: {e}", flush=True)
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
    db.connect().execute("UPDATE sessions SET auto_hidden=0 WHERE id=?", (sid,))
    rows = []
    if kind == "movie":
        rows.append(("movie", title, year, None, None, None, ids["tmdb"], None, ids["imdb"], poster))
    else:
        season = int(b["season"])
        eps = sorted({int(e) for e in b.get("episodes") or []})
        if not eps:
            raise ValueError("Pick at least one episode")
        if ids["tmdb"]:
            try:   # names from TMDB + TVmaze, so episodes TMDB lacks still get their title
                sz = next((z for z in show_episodes(int(ids["tmdb"]), ids["imdb"], ids["tvdb"])["seasons"]
                           if z["number"] == season), {"episodes": []})
                ep_titles = {e["number"]: e.get("name") for e in sz["episodes"]}
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
    fill_runtimes(sid)   # lets Discord show a progress bar and auto-send check the watched share
    return {"ok": True, "items": len(rows)}


# ------------------------------------------------------------------- Netflix
# Netflix tells the TV nothing (no title in its media session), but your account keeps your
# viewing activity. The dashboard reads it with the Netflix session your browser on this computer
# already has: the netflix.com cookies from the browser's own cookie store (like yt-dlp's
# --cookies-from-browser), read-only, never stored or sent anywhere but Netflix. The browser renews
# them, so nothing has to be done. The activity comes from Netflix's internal web API, the way
# Universal Trakt Scrobbler does it (MIT, github.com/trakt-tools/universal-trakt-scrobbler); it is
# not public and can move.
NETFLIX = "https://www.netflix.com"


def firefox_cookie_dbs() -> list[Path]:
    """cookies.sqlite of every Zen / Firefox / LibreWolf / Floorp profile on this computer."""
    home = Path.home()
    xdg = Path(os.environ.get("XDG_CONFIG_HOME") or home / ".config")
    roots = [xdg / "zen", xdg / "mozilla/firefox", home / ".zen", home / ".mozilla/firefox", home / ".librewolf", home / ".floorp",
             home / ".var/app/app.zen_browser.zen/.zen", home / ".var/app/org.mozilla.firefox/.mozilla/firefox",
             home / "snap/firefox/common/.mozilla/firefox",
             home / "Library/Application Support/zen/Profiles", home / "Library/Application Support/Firefox/Profiles"]
    appdata = os.environ.get("APPDATA")
    if appdata:
        roots += [Path(appdata) / "zen/Profiles", Path(appdata) / "Mozilla/Firefox/Profiles"]
    return [p for r in roots if r.is_dir() for p in r.glob("*/cookies.sqlite")]


COUCHLOG_NETFLIX = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config") / "couchlog" / "netflix-session.json"


def netflix_cookies() -> tuple[dict, str]:
    """(netflix.com cookies, where they came from): the session Couchlog saved with "Connect
    Netflix" first, else the browser profile that used Netflix last (a snapshot of its cookie
    store, which the browser keeps locked)."""
    try:
        saved = json.loads(COUCHLOG_NETFLIX.read_text())
        if saved.get("cookies", {}).get("NetflixId"):
            return saved["cookies"], f"couchlog ({saved.get('how', 'connect')})"
    except (OSError, ValueError):
        pass
    import shutil
    import tempfile
    best, best_at, src = None, 0, ""
    for path in firefox_cookie_dbs():
        with tempfile.TemporaryDirectory() as tmp:
            dst = Path(tmp) / "cookies.sqlite"
            try:
                shutil.copy2(path, dst)
                for ext in ("-wal", "-shm"):
                    if Path(f"{path}{ext}").exists():
                        shutil.copy2(f"{path}{ext}", f"{dst}{ext}")
                c = sqlite3.connect(dst)
                rows = c.execute("SELECT name, value, lastAccessed FROM moz_cookies WHERE host LIKE '%netflix.com'"
                                 " AND (expiry = 0 OR expiry > ?)", (int(time.time()),)).fetchall()
                c.close()
            except (OSError, sqlite3.Error):
                continue
        cookies = {n: v for n, v, _ in rows}
        at = max((a for n, _, a in rows if n == "NetflixId"), default=0)
        if "NetflixId" in cookies and at > best_at:
            best, best_at, src = cookies, at, str(path.parent)
    if not best:
        raise RuntimeError("Not signed in to Netflix in Zen/Firefox on this computer")
    return best, src


def netflix_http(url: str, data: bytes | None = None, headers: dict | None = None) -> str:
    cookies = cached(("netflix_cookies",), 600, netflix_cookies)[0]
    h = {"Cookie": "; ".join(f"{k}={v}" for k, v in cookies.items()),
         "User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0",
         "Accept": "*/*", **(headers or {})}
    if data is not None:
        h.setdefault("Content-Type", "application/x-www-form-urlencoded")
    req = urllib.request.Request(url, data=data, headers=h, method="POST" if data is not None else "GET")
    import http.client
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req, timeout=25) as r:
                return r.read().decode("utf-8", "replace")
        except http.client.IncompleteRead as e:   # Netflix sometimes cuts a long page short: retry
            if attempt == 2:
                return e.partial.decode("utf-8", "replace")


def _js_unescape(v: str) -> str:
    v = re.sub(r"\\x([0-9A-Fa-f]{2})", lambda m: chr(int(m.group(1), 16)), v)
    v = re.sub(r"\\u([0-9A-Fa-f]{4})", lambda m: chr(int(m.group(1), 16)), v)
    return v.replace("\\/", "/")


def netflix_profile() -> dict:
    """authURL, profile guid and name, from the viewing-activity page of the saved session."""
    def fetch():
        html = netflix_http(f"{NETFLIX}/settings/viewed/")
        grab = lambda rx: (m.group(1) if (m := re.search(rx, html)) else None)
        # Netflix serves two variants of this page: the profile is under userInfo or auiUserContext
        prof = {"auth_url": grab(r'"authURL":"(.*?)"'),
                "guid": grab(r'"(?:userInfo|auiUserContext)":\{"data":\{[^}]*?"userGuid":"(.*?)"'),
                "name": grab(r'"(?:userInfo|auiUserContext)":\{"data":\{"name":"(.*?)"'),
                "build": grab(r'"BUILD_IDENTIFIER":"(.*?)"')}
        prof = {k: _js_unescape(v) if v else v for k, v in prof.items()}
        if not prof["auth_url"] or not prof["guid"]:
            raise RuntimeError("Netflix session expired or no profile selected: open netflix.com in the browser")
        return prof
    return cached(("netflix_profile",), 3600, fetch)


def netflix_activity(page: int = 0, size: int = 50) -> list:
    """This profile's viewing activity, newest first: movieID, series, seriesTitle, title,
    episodeTitle, date (ms), bookmark and duration (s)."""
    prof = netflix_profile()
    path = urllib.parse.quote(json.dumps(["aui", "viewingActivity", page, size], separators=(",", ":")))
    url = f"{NETFLIX}/api/aui/pathEvaluator/web/%5E2.0.0?method=call&callPath={path}&falcor_server=0.1.0"
    body = "param=" + urllib.parse.quote(json.dumps({"guid": prof["guid"]}))
    text = netflix_http(url, body.encode(), {"x-netflix.request.routing":
                        '{"path":"/nq/aui/endpoint/%5E1.0.0-web/pathEvaluator","control_tag":"auinqweb"}'})
    return ((json.loads(text).get("jsonGraph") or {}).get("aui") or {}).get("viewingActivity", {}) \
        .get("value", {}).get("viewedItems") or []


def netflix_metadata(movie_id: int) -> dict | None:
    """A show (all seasons and episodes) or a movie, with English titles and the year."""
    def fetch():
        prof = netflix_profile()
        bases = [f"{NETFLIX}/nq/website/memberapi/release"]
        if prof.get("build"):
            bases += [f"{NETFLIX}/nq/website/memberapi/{prof['build']}", f"{NETFLIX}/api/shakti/{prof['build']}"]
        bases.append(f"{NETFLIX}/api/shakti/mre")
        for b in bases:
            for extra in ("", "&authURL=" + urllib.parse.quote(prof["auth_url"])):
                try:
                    j = json.loads(netflix_http(f"{b}/metadata?languages=en-US&movieid={movie_id}{extra}"))
                    if j.get("video"):
                        return j["video"]
                except Exception:
                    continue
        return None
    return cached(("netflix_md", movie_id), 24 * 3600, fetch)


NETFLIX_APPS = ("com.netflix.ninja", "netflix")
NETFLIX_WINDOW = 300   # s: Netflix's date is when the title started; the TV session starts within this


def netflix_resolve(v: dict):
    """(kind, tmdb id, season, episode) for a viewing-activity entry, via Netflix's metadata
    (English titles, real season/episode numbers) and an exact TMDB match; None if unsure."""
    if v.get("series"):
        md = netflix_metadata(v["series"])
        if not md or md.get("type") != "show":
            return None
        show = tmdb_unique(md.get("title") or v.get("seriesTitle"), "tv")
        if not show:
            return None
        for s in md.get("seasons", []):
            for e in s.get("episodes", []):
                if e.get("id") == v["movieID"]:
                    if not md.get("hiddenEpisodeNumbers"):
                        return "tv", show["id"], s["seq"], e["seq"]
                    se = episode_by_name(show["id"], e.get("title") or v.get("episodeTitle") or "")
                    return ("tv", show["id"], se[0], se[1]) if isinstance(se, tuple) else None
        return None
    md = netflix_metadata(v["movieID"]) or {}
    mv = tmdb_unique(md.get("title") or v.get("title"), "movie", md.get("year"))
    return ("movie", mv["id"], None, None) if mv else None


def netflix_matches(conn, activity: list) -> list:
    """[(session row, activity entry)] for unidentified Netflix sessions whose start matches when
    Netflix says that title started (Netflix keeps only the latest viewing of each title)."""
    used, out = set(), []
    rows = conn.execute(
        "SELECT * FROM sessions WHERE app_id IN (?, ?) AND ignored = 0 AND started_at > ?"
        " AND id NOT IN (SELECT session_id FROM items) ORDER BY started_at",
        (*NETFLIX_APPS, time.time() - 7 * 86400)).fetchall()
    for v in activity:
        when = v.get("date", 0) / 1000
        best = min((s for s in rows if s["id"] not in used and abs(s["started_at"] - when) <= NETFLIX_WINDOW),
                   key=lambda s: abs(s["started_at"] - when), default=None)
        if best:
            used.add(best["id"])
            out.append((best, v))
    return out


NETFLIX_LAST = {"at": 0.0}
NETFLIX_SPLIT_DONE: set = set()   # (session id, entry date) already split


def netflix_split_open(conn, newest) -> int:
    """Netflix moved on to another title inside the session that is playing (autoplay the box
    did not catch): a new entry started 5+ minutes after the session did. Split the session there,
    onto that title, as the "next episode" button would."""
    if not newest:
        return 0
    nd = newest["date"] / 1000
    s = conn.execute("SELECT * FROM sessions WHERE app_id IN (?, ?) AND state = 'open' AND ignored = 0"
                     " AND started_at < ? ORDER BY id DESC LIMIT 1", (*NETFLIX_APPS, nd - NETFLIX_WINDOW)).fetchone()
    if not s or (s["id"], newest["date"]) in NETFLIX_SPLIT_DONE:
        return 0
    NETFLIX_SPLIT_DONE.add((s["id"], newest["date"]))
    hit = netflix_resolve(newest)
    if not hit:
        return 0
    kind, tid, season, episode = hit
    cur = db.current_item(conn, s["id"])
    if cur and cur["tmdb"] == tid and (kind == "movie" or (cur["season"], cur["episode"]) == (season, episode)):
        return 0   # the session already is that title
    if cur and cur["kind"] == "episode":
        nxt = api_next(s["id"], auto=True)["session"]   # keeps the format, marks it as an automatic split
    else:
        return 0   # a movie or an unidentified session: left to the usual rules
    body = {"kind": kind, "tmdb": tid}
    if kind == "tv":
        body.update(season=season, episodes=[episode])
    api_identify(nxt, body)
    conn.execute("UPDATE items SET auto_how='netflix' WHERE session_id=?", (nxt,))
    print(f"Netflix: session {s['id']} moved on to {newest.get('title')} (session {nxt})", flush=True)
    return 1


def netflix_identify(force: bool = False) -> int:
    """Identify Netflix sessions from your account's viewing activity (every 2 min, only while
    there are unidentified Netflix sessions from the last 7 days)."""
    if not force and time.time() - NETFLIX_LAST["at"] < 55:   # about once a minute, only while some wait
        return 0
    conn = db.connect()
    pending = conn.execute("SELECT COUNT(*) FROM sessions WHERE app_id IN (?, ?) AND ignored = 0 AND started_at > ?"
                           " AND id NOT IN (SELECT session_id FROM items)",
                           (*NETFLIX_APPS, time.time() - 7 * 86400)).fetchone()[0]
    if not pending:
        return 0
    try:
        activity = netflix_activity(0, 50)
    except Exception as e:   # a timeout or a hiccup: try again on the next round (2 min)
        print(f"Netflix: {e}", flush=True)
        return 0
    NETFLIX_LAST["at"] = time.time()
    pairs = netflix_matches(conn, activity)
    # Netflix keeps one entry per title, with when you started it: picking it up again (or restarting
    # it) adds no new entry. A session with no entry of its own, 6+ minutes in, while the newest entry
    # is from before it (within 6 h), is that same title: a new title would have its own entry by now.
    matched = {s["id"] for s, _ in pairs}
    newest = max(activity, key=lambda v: v.get("date", 0), default=None)
    if newest:
        nd = newest["date"] / 1000
        for s in conn.execute(
                "SELECT * FROM sessions WHERE app_id IN (?, ?) AND ignored = 0 AND started_at > ? AND started_at < ?"
                " AND id NOT IN (SELECT session_id FROM items) ORDER BY started_at",
                (*NETFLIX_APPS, nd + NETFLIX_WINDOW, time.time() - 360)).fetchall():
            if s["id"] not in matched and s["started_at"] - nd <= 6 * 3600:
                pairs.append((s, newest))
    done = netflix_split_open(conn, newest)
    for s, v in pairs:
        hit = netflix_resolve(v)
        if not hit:
            continue
        kind, tid, season, episode = hit
        body = {"kind": kind, "tmdb": tid}
        if kind == "tv":
            body.update(season=season, episodes=[episode])
        api_identify(s["id"], body)
        conn.execute("UPDATE items SET auto_how='netflix' WHERE session_id=?", (s["id"],))
        print(f"Netflix: session {s['id']} is {v.get('title')}", flush=True)
        done += 1
    return done


NETFLIX_CONNECT = {"proc": None, "lines": [], "error": None}


def couchlog_bin() -> str | None:
    import shutil
    return shutil.which("couchlog") or next((str(p) for p in [Path.home() / ".local/bin/couchlog"] if p.exists()), None)


def netflix_connect() -> dict:
    """Run `couchlog connect netflix`: Couchlog opens its own browser window on this computer's
    screen; when you sign in, it saves the session (couchlog keeps renewing it)."""
    import subprocess
    p = NETFLIX_CONNECT["proc"]
    if p and p.poll() is None:
        return {"ok": True, "running": True}
    exe = couchlog_bin()
    if not exe:
        raise ValueError("couchlog is not installed on this computer")
    env = dict(os.environ)
    try:   # a service may have started before the graphical session: borrow its display
        for line in subprocess.run(["systemctl", "--user", "show-environment"], capture_output=True, text=True,
                                   timeout=5).stdout.splitlines():
            k, _, v = line.partition("=")
            if k in ("DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS") and not env.get(k):
                env[k] = v
    except Exception:
        pass
    proc = subprocess.Popen([exe, "connect", "netflix"], env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True)
    NETFLIX_CONNECT.update(proc=proc, lines=[], error=None)

    def follow():
        for line in proc.stdout:
            NETFLIX_CONNECT["lines"].append(line.strip())
        if proc.wait() != 0:
            NETFLIX_CONNECT["error"] = (NETFLIX_CONNECT["lines"] or ["couchlog connect failed"])[-1]
        _cache.pop(("netflix_profile",), None)
        _cache.pop(("netflix_cookies",), None)
    threading.Thread(target=follow, name="netflix-connect", daemon=True).start()
    return {"ok": True, "running": True}


def netflix_status() -> dict:
    p = NETFLIX_CONNECT["proc"]
    if p and p.poll() is None:
        return {"connected": False, "connecting": True,
                "reason": (NETFLIX_CONNECT["lines"] or ["Opening the sign-in window…"])[-1]}
    if NETFLIX_CONNECT["error"]:
        err, NETFLIX_CONNECT["error"] = NETFLIX_CONNECT["error"], None
        return {"connected": False, "reason": err}
    try:
        _, src = netflix_cookies()
    except Exception as e:
        return {"connected": False, "reason": str(e)}
    try:
        prof = netflix_profile()
        return {"connected": True, "profile": prof.get("name"), "browser_profile": src}
    except Exception as e:
        return {"connected": False, "reason": str(e)[:200], "browser_profile": src}


# ------------------------------------------------------- automatic identification
# The same rules as Couchlog's identify.Auto (Go): only what is certain is identified without
# asking, because a wrong guess reaches Trakt. What each app publishes on Android TV:
#   HBO Max:  title = episode name, subtitle = show          ("The Rise of Top G" / "Andrew Tate: Toxic Empire")
#   Tubi:     "S02:E21 - Lauren", no show name                (the show you were watching in Tubi)
#   YouTube:  video names; only its own film store counts     (subtitle "Películas y series de YouTube")
#   Netflix:  nothing
EP_REF = re.compile(r"(?i)(?:\b[st](\d{1,2})\s*[:.\-]?\s*e(?:p|pisode|pisodio)?\.?\s*(\d{1,3})\b|"
                    r"\b(?:season|temporada)\s*(\d{1,2})\s*[:,.\-]?\s*(?:episode|episodio|capítulo|cap\.?|ep\.?)\s*(\d{1,3})\b|"
                    r"\b(\d{1,2})x(\d{2,3})\b)")
EP_PLACEHOLDER = re.compile(r"(?i)^\s*(?:episode|episodio|capítulo|chapter|ep\.?)\s*\d+\s*$|^\s*(?:tba|tbd)?\s*$")
YT_MOVIE_CHANNELS = {"películas y series de youtube", "youtube movies", "youtube movies & tv",
                     "youtube movies and tv", "filmes e séries do youtube"}
NO_GUESS_APPS = {"com.google.android.youtube.tv", "youtube.leanback.v4"}
AUTO_TRIED: dict = {}   # (session id, hint) -> last attempt


def parse_episode_ref(s: str | None):
    """(season, episode, rest of the text) from "S02:E21 - Lauren", "Season 1: Episode 2", "1x02"…, or None."""
    m = EP_REF.search(s or "")
    if not m:
        return None
    g = [int(x) if x else None for x in m.groups()]
    season, episode = next((g[i], g[i + 1]) for i in (0, 2, 4) if g[i] is not None)
    rest = (s[:m.start()] + " " + s[m.end():]).strip(" -–—:|·,.")
    return season, episode, " ".join(rest.split())


def tmdb_unique(name: str, kind: str, year: int | None = None) -> dict | None:
    """The only TMDB movie/tv whose name or original name is exactly `name` (and year, when given
    and there are several); None if none or still several."""
    if not name:
        return None
    want, hits = _norm_title(name), []
    for r in tmdb("/search/multi", query=name, include_adult="false").get("results", []):
        if r.get("media_type") != kind:
            continue
        names = {_norm_title(r.get(k)) for k in ("title", "name", "original_title", "original_name")}
        if want in names:
            hits.append(r)
    if len(hits) > 1 and year:
        hits = [r for r in hits if year_of(r.get("release_date") or r.get("first_air_date")) == year]
    return hits[0] if len(hits) == 1 else None


def episode_by_name(show_id: int, name: str):
    """(season, episode) whose name is `name` (TMDB + TVmaze), "placeholders" when the show lists
    no names yet ("Episode 1", "TBA": a brand-new series), else None."""
    want, found, placeholders = _norm_title(name), [], True
    for sz in show_episodes(show_id)["seasons"]:
        for e in sz["episodes"]:
            if not EP_PLACEHOLDER.match(e.get("name") or ""):
                placeholders = False
            if want and _norm_title(e.get("name")) == want:
                found.append((sz["number"], e["number"]))
    if len(found) == 1:
        return found[0]
    return "placeholders" if not found and placeholders else None


def next_after_last(conn, show_id: int):
    """The episode after the furthest one of this show in your history (S1E1 if none)."""
    last = conn.execute("SELECT season, episode FROM items WHERE tmdb=? AND kind='episode'"
                        " ORDER BY season DESC, episode DESC LIMIT 1", (show_id,)).fetchone()
    if not last:
        return 1, 1
    counts = {s["season_number"]: s.get("episode_count") or 0 for s in tmdb(f"/tv/{show_id}").get("seasons", [])}
    s, e = last["season"], last["episode"] + 1
    if e > counts.get(s, e):
        nxt = [n for n in sorted(counts) if n > s and counts[n] > 0]
        if nxt:
            s, e = nxt[0], 1
        # else: a new series TMDB is still filling in (1 of 4 listed): the next number anyway
    return s, e


def auto_identify_session(conn, sess):
    """(kind, tmdb id, season, episode, how) for a session the rules are sure about, else None."""
    title, sub = (sess["hint_title"] or "").strip(), (sess["hint_extra"] or "").strip()
    if not title:
        return None
    if sess["app_id"] in NO_GUESS_APPS:
        if sub.lower() not in YT_MOVIE_CHANNELS:
            return None
        sub = ""   # YouTube's film store: the title is a film
    season, episode = sess["hint_season"], sess["hint_episode"]
    name, ep_name = title, ""
    if not episode:
        ref = parse_episode_ref(title)
        if ref:
            season, episode, ep_name = ref
            name = ""
        elif parse_episode_ref(sub):
            season, episode, sub = parse_episode_ref(sub)
            ep_name, name = title, ""
    if episode:
        for n in (name, sub):
            show = tmdb_unique(n, "tv")
            if show:
                return "tv", show["id"], season, episode, "match"
        if ep_name:   # no show name (Tubi): the show you were watching in this app, if the name matches
            prev = conn.execute(
                "SELECT i.tmdb FROM items i JOIN sessions s ON s.id=i.session_id WHERE s.app_id=? AND i.kind='episode'"
                " AND i.tmdb IS NOT NULL AND s.started_at > ? AND s.id <> ? ORDER BY s.started_at DESC LIMIT 1",
                (sess["app_id"], time.time() - 30 * 86400, sess["id"])).fetchone()
            if prev:
                sz = next((z for z in show_episodes(prev["tmdb"])["seasons"] if z["number"] == season), None)
                ep = next((e for e in (sz or {}).get("episodes", []) if e["number"] == episode), None)
                if ep and _norm_title(ep.get("name")) == _norm_title(ep_name):
                    return "tv", prev["tmdb"], season, episode, "continuation"
        return None
    mv = tmdb_unique(title, "movie")
    show = tmdb_unique(sub, "tv") if sub else None
    if mv and not show:
        return "movie", mv["id"], None, None, "match"
    if show:   # "episode name / show"
        se = episode_by_name(show["id"], title)
        if se == "placeholders":
            nxt = next_after_last(conn, show["id"])
            return ("tv", show["id"], nxt[0], nxt[1], "progress") if nxt else None
        if se:
            return "tv", show["id"], se[0], se[1], "match"
    return None


GUESS_NOTE = ("Episode guessed from your progress (the series has no episode names yet): "
              "confirm it with Identify to send it")


def fresh_hints() -> bool:
    """An open session with a title the rules have not tried yet (cheap: one query)."""
    conn = db.connect()
    rows = conn.execute(
        "SELECT id, hint_title, hint_extra FROM sessions WHERE state='open' AND source <> 'plex' AND ignored = 0"
        " AND COALESCE(hint_title, '') <> '' AND id NOT IN (SELECT session_id FROM items)").fetchall()
    conn.close()
    return any((r["id"], r["hint_title"], r["hint_extra"]) not in AUTO_TRIED for r in rows)


def auto_identify() -> int:
    """Identify recent sessions without items from what the device published (see the rules above)."""
    conn = db.connect()
    rows = conn.execute(
        "SELECT * FROM sessions WHERE source <> 'plex' AND ignored = 0 AND COALESCE(hint_title, '') <> ''"
        " AND started_at > ? AND id NOT IN (SELECT session_id FROM items)", (time.time() - 3 * 86400,)).fetchall()
    done = 0
    for s in rows:
        key = (s["id"], s["hint_title"], s["hint_extra"])
        if time.time() - AUTO_TRIED.get(key, 0) < 3600:
            continue
        AUTO_TRIED[key] = time.time()
        try:
            hit = auto_identify_session(conn, s)
        except Exception as e:
            print(f"Auto-identify {s['id']}: {e}", flush=True)
            continue
        if not hit:
            continue
        kind, tid, season, episode, how = hit
        body = {"kind": kind, "tmdb": tid}
        if kind == "tv":
            body.update(season=season, episodes=[episode])
        api_identify(s["id"], body)
        conn.execute("UPDATE items SET auto_how=? WHERE session_id=?", (how, s["id"]))
        if how == "progress":   # never sent until you confirm it
            conn.execute("UPDATE items SET trakt_error=?, simkl_error=? WHERE session_id=?",
                         (GUESS_NOTE, GUESS_NOTE, s["id"]))
        it = db.current_item(conn, s["id"])
        print(f"Auto-identified session {s['id']} ({how}): {it['title']}"
              + (f" S{it['season']}E{it['episode']}" if it["kind"] == "episode" else ""), flush=True)
        done += 1
    return done


def api_next(sid: int, auto: bool = False) -> dict:
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
            "INSERT INTO sessions(app_id, app_name, app_logo, started_at, last_seen_at, last_play_at, source, auto_next_from)"
            " VALUES (?,?,?,?,?,?,?,?)", (sess["app_id"], sess["app_name"], sess["app_logo"], now, now, now, sess["source"],
                                          sid if auto else None))
        # same player, same stream: the next episode starts with the format already reported (the box only
        # reports it when a decoder starts or stops)
        conn.execute("UPDATE sessions SET video_codec=?, video_width=?, video_height=?, video_hdr=?, video_hdr_live=?,"
                     " video_hdr_since=? WHERE id=?", (sess["video_codec"], sess["video_width"], sess["video_height"],
                                                       sess["video_hdr_live"], sess["video_hdr_live"],
                                                       now if sess["video_hdr_live"] else None, cur.lastrowid))
        conn.execute(
            "INSERT INTO items(session_id, kind, title, year, season, episode, episode_title, tmdb, tvdb, imdb, poster)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (cur.lastrowid, "episode", it["title"], it["year"], s, e, ep_title,
             it["tmdb"], it["tvdb"], it["imdb"], it["poster"]))
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    fill_runtimes(cur.lastrowid)
    return {"ok": True, "season": s, "episode": e, "session": cur.lastrowid}


def api_undo_next(sid: int) -> dict:
    """The automatic "next episode" was wrong because it was the same episode (you went back to the start):
    put the time back into the previous session and drop this one. For a different episode or show, use Change."""
    conn = db.connect()
    sess = conn.execute("SELECT * FROM sessions WHERE id=?", (sid,)).fetchone()
    if not sess or not sess["auto_next_from"]:
        raise ValueError("This session was not split automatically")
    prev = conn.execute("SELECT * FROM sessions WHERE id=?", (sess["auto_next_from"],)).fetchone()
    if not prev:
        raise ValueError("The previous session no longer exists")
    conn.execute("BEGIN IMMEDIATE")
    try:
        conn.execute("UPDATE sessions SET state=?, ended_at=?, last_seen_at=MAX(last_seen_at, ?), last_play_at=MAX(last_play_at, ?),"
                     " played_seconds=played_seconds+?, live_position=? WHERE id=?",
                     (sess["state"], sess["ended_at"], sess["last_seen_at"], sess["last_play_at"],
                      sess["played_seconds"], sess["live_position"], prev["id"]))
        conn.execute("DELETE FROM sessions WHERE id=?", (sid,))   # its items go with it (ON DELETE CASCADE)
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise
    return {"ok": True, "session": prev["id"]}


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
    nows = [s for s in sessions if s["state"] == "open"]   # one per device (LG TV, Android TV)
    for s in nows:
        if s["items"]:
            it = s["items"][-1]
            it["match"] = match_badges(it["kind"], {k: it[k] for k in ("imdb", "tmdb", "tvdb") if it[k]})
    now = nows[0] if nows else None
    try:
        pnow = plex_now() if CFG["plex"] else []
    except Exception:
        pnow = []
    return {"now": now, "nows": nows, "auto_push": CFG["auto_push"] and CFG["auto_push_min_ratio"], "sessions": sessions, "pending": pending, "waiting": waiting, "server_time": time.time(),
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
        if not getattr(self, "_head_only", False):
            self.wfile.write(data)

    def do_HEAD(self):   # browsers probe the service worker / icons with HEAD
        self._head_only = True
        try:
            self.do_GET()
        finally:
            self._head_only = False

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        qs = urllib.parse.parse_qs(u.query)
        try:
            if u.path == "/":
                return self.send(200, raw=PAGE.encode(), ctype="text/html; charset=utf-8")
            if u.path in STATIC:
                name, ctype = STATIC[u.path]
                return self.send(200, raw=(STATIC_DIR / name).read_bytes(), ctype=ctype)
            if u.path == "/api/plex_now":   # light endpoint for tvpresence (Discord)
                return self.send(200, {"plex_now": plex_now()})
            if u.path == "/api/netflix/status":
                return self.send(200, netflix_status())
            if u.path == "/api/state":
                return self.send(200, api_state(int(qs.get("days", ["60"])[0])))
            if u.path == "/api/search":
                q = (qs.get("q", [""])[0]).strip()
                prefer = (qs.get("prefer") or [""])[0] or None
                return self.send(200, search_all(q, prefer if prefer in ("tv", "movie") else None)
                                 if len(q) >= 2 else {"results": [], "errors": []})
            m = re.fullmatch(r"/api/tmdb/tv/(\d+)", u.path)
            if m:
                d = tmdb(f"/tv/{m.group(1)}")
                return self.send(200, {"seasons": [
                    {"number": s["season_number"], "name": s.get("name"), "count": s.get("episode_count") or 0}
                    for s in d.get("seasons", []) if (s.get("episode_count") or 0) > 0]})
            m = re.fullmatch(r"/api/show/(\d+)/episodes", u.path)
            if m:
                tv = (qs.get("tvdb") or [""])[0]
                return self.send(200, show_episodes(int(m.group(1)), (qs.get("imdb") or [""])[0] or None,
                                                    int(tv) if tv.isdigit() else None))
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
            if path == "/api/netflix/connect":
                return self.send(200, netflix_connect())
            if path == "/api/push":
                return self.send(200, api_push(body))
            if path == "/api/verify":
                return self.send(200, api_verify(body.get("session_ids")))
            if path == "/api/plex/import":
                if not CFG["plex"]:
                    return self.send(400, {"error": "Plex import is disabled"})
                return self.send(200, {"imported": plex_import()})
            m = re.fullmatch(r"/api/sessions/(\d+)/(identify|next|undo_next|ignore|delete)", path)
            if m:
                sid, action = int(m.group(1)), m.group(2)
                if action == "identify":
                    r = api_identify(sid, body)
                    verify_bg([sid])
                    return self.send(200, r)
                if action == "next":
                    return self.send(200, api_next(sid, bool(body.get("auto"))))
                if action == "undo_next":
                    return self.send(200, api_undo_next(sid))
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
STATIC_DIR = Path(__file__).with_name("static")
# Branding + PWA (Couchlog): icons, manifest and a pass-through service worker.
# The service worker is served from "/" so its scope covers the whole dashboard.
STATIC = {
    "/static/icon.svg": ("icon.svg", "image/svg+xml"),
    "/static/icon-192.png": ("icon-192.png", "image/png"),
    "/static/icon-512.png": ("icon-512.png", "image/png"),
    "/static/apple-touch-icon.png": ("apple-touch-icon.png", "image/png"),
    "/favicon.ico": ("favicon-32.png", "image/png"),
    "/manifest.webmanifest": ("manifest.webmanifest", "application/manifest+json"),
    "/sw.js": ("sw.js", "text/javascript; charset=utf-8"),
}


def main() -> None:
    db.log_to_file_if_windowless("tvhistory")
    db.connect().close()
    if CFG["plex"]:
        threading.Thread(target=plex_loop, name="plex", daemon=True).start()  # imports and verifies
    else:
        verify_bg()
    threading.Thread(target=auto_loop, name="auto", daemon=True).start()   # sends what ended
    srv = ThreadingHTTPServer((CFG["bind"], CFG["port"]), Handler)
    print(f"TV History on http://{CFG['bind']}:{CFG['port']}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
