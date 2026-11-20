#!/usr/bin/env python3
"""Reconcile the FULL watch history between Trakt and Simkl (with Plex as a witness).

  tvsync_reconcile.py fetch [--refresh]        download and cache Trakt, Simkl and Plex views
  tvsync_reconcile.py report [--refresh]       write reconcile/report-DATE.md + reconcile/plan.json
  tvsync_reconcile.py batches                  list the plan's batches
  tvsync_reconcile.py apply BATCH [--yes]      apply a batch (dry run without --yes)
  tvsync_reconcile.py delete trakt HISTORY_ID [--yes]
  tvsync_reconcile.py delete simkl-episode|simkl-movie ... [--yes]

Read-only except `apply`/`delete` with --yes. Everything is written by external ID
(imdb/tmdb/tvdb), never by title. Reuses credentials and helpers from
tvhistory_dashboard.py; never renews the Trakt token (trakt-scrobbler does).
"""
from __future__ import annotations

import argparse
import json
import re
import sqlite3
import sys
import time
import unicodedata
import urllib.parse
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import tvhistory_dashboard as d  # noqa: E402

CACHE = Path.home() / ".cache/tvsync-reconcile"
OUT = Path(__file__).resolve().parent / "reconcile"
MAX_AGE = 6 * 3600


# ------------------------------------------------------------------ utilities
def ts_of(s: str | None) -> float | None:
    if not s:
        return None
    try:
        return datetime.strptime(s[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
    except ValueError:
        return None


def day(ts: float | None) -> str:
    return datetime.fromtimestamp(ts).strftime("%Y-%m-%d %H:%M") if ts else "?"


def norm(t: str | None) -> str:
    t = unicodedata.normalize("NFKD", t or "").encode("ascii", "ignore").decode().lower()
    t = t.replace("&", " and ").replace("'", "").replace("’", "")
    t = re.sub(r"\([^)]*\)", " ", t)
    t = re.sub(r"[^a-z0-9]+", " ", t).strip()
    return re.sub(r"^(the|a|an) ", "", t)


def sid(v) -> str:
    return "" if v in (None, "", 0) else str(v)


def disk_cache(name: str, refresh: bool, fn):
    CACHE.mkdir(parents=True, exist_ok=True)
    f = CACHE / f"{name}.json"
    if not refresh and f.exists() and time.time() - f.stat().st_mtime < MAX_AGE:
        return json.loads(f.read_text())
    val = fn()
    f.write_text(json.dumps(val))
    return val


# --------------------------------------------------------------------- bajadas
def fetch_trakt(kind: str) -> list:
    out, page = [], 1
    while True:
        q = urllib.parse.urlencode({"page": page, "limit": 100})
        st, j = d.http_json("GET", f"https://api.trakt.tv/sync/history/{kind}?{q}", d.trakt_headers(True), timeout=60)
        if st != 200 or not isinstance(j, list):
            raise RuntimeError(f"Trakt {st} on {kind} history page {page}")
        out += j
        print(f"  trakt {kind}: page {page} ({len(out)})", file=sys.stderr)
        if len(j) < 100:
            return out
        page += 1
        time.sleep(0.4)


def fetch_simkl() -> dict:
    cid, tok = d.simkl_creds()
    out = {}
    for typ in ("movies", "shows", "anime"):
        url = (f"https://api.simkl.com/sync/all-items/{typ}?extended=full&episode_watched_at=yes"
               f"&client_id={cid}&app-name=tvsync&app-version=1.0")
        st, j = d.http_json("GET", url, {"Authorization": f"Bearer {tok}"}, timeout=120)
        if st != 200:
            raise RuntimeError(f"Simkl {st} while reading {typ}")
        out[typ] = ((j or {}).get(typ) or []) if isinstance(j, dict) else []
        print(f"  simkl {typ}: {len(out[typ])}", file=sys.stderr)
    return out


def fetch_plex(refresh: bool) -> list:
    c = sqlite3.connect(Path(d.CFG["plex_db"]).as_uri() + "?mode=ro", uri=True, timeout=10)
    c.row_factory = sqlite3.Row
    rows = [dict(r) for r in c.execute(
        f"SELECT {d.VIEW_COLS} FROM metadata_item_views WHERE account_id=1 AND metadata_type IN (1,4)"
        " ORDER BY viewed_at")]
    c.close()
    gfile = CACHE / "plex_guids.json"
    guids = json.loads(gfile.read_text()) if gfile.exists() and not refresh else {}
    retry = {g for g, v in guids.items() if v.get("error")}  # e.g. a transient 503 from the provider
    need = sorted(({(r["grandparent_guid"] if r["metadata_type"] == 4 else r["guid"]) for r in rows} - set(guids) | retry) - {None})

    def res(g):
        try:
            ids, md = d.plex_resolve(g)
            return g, {"ids": ids, "title": md.get("title"), "year": md.get("year")}
        except Exception as e:  # noqa: BLE001
            return g, {"ids": {}, "error": str(e)}
    with ThreadPoolExecutor(8) as ex:
        for g, v in ex.map(res, need):
            guids[g] = v
    gfile.write_text(json.dumps(guids))
    for r in rows:
        g = guids.get(r["grandparent_guid"] if r["metadata_type"] == 4 else r["guid"]) or {}
        r["ids"] = {k: v for k, v in (g.get("ids") or {}).items() if not (r["metadata_type"] == 1 and k == "tvdb")}
        r["show_title"] = g.get("title")
        r["year"] = g.get("year")
    return rows


def load(refresh: bool) -> dict:
    return {
        "trakt_movies": disk_cache("trakt_movies", refresh, lambda: fetch_trakt("movies")),
        "trakt_episodes": disk_cache("trakt_episodes", refresh, lambda: fetch_trakt("episodes")),
        "simkl": disk_cache("simkl", refresh, fetch_simkl),
        "plex": fetch_plex(refresh),
    }


def trakt_aired(trakt_id: int) -> list:
    """Aired episodes of a show according to Trakt (no specials), cached on disk."""
    f = CACHE / "trakt_aired.json"
    cache = json.loads(f.read_text()) if f.exists() else {}
    k = str(trakt_id)
    if k not in cache:
        st, j = d.http_json("GET", f"https://api.trakt.tv/shows/{trakt_id}/seasons?extended=episodes,full",
                            d.trakt_headers(False), timeout=60)
        if st != 200 or not isinstance(j, list):
            raise RuntimeError(f"Trakt {st} on seasons of {trakt_id}")
        now = time.time()
        cache[k] = [[se["number"], e["number"]] for se in j if se["number"] > 0 for e in se.get("episodes") or []
                    if ts_of(e.get("first_aired")) and ts_of(e.get("first_aired")) <= now]
        f.write_text(json.dumps(cache))
    return [tuple(x) for x in cache[k]]


def fetch_playback() -> list:
    st, j = d.http_json("GET", "https://api.trakt.tv/sync/playback?limit=500", d.trakt_headers(True))
    if st != 200 or not isinstance(j, list):
        raise RuntimeError(f"Trakt {st} on /sync/playback")
    return j


# ---------------------------------------------------------------------- model
# A "title" is a movie or a show on one service:
#   {svc, kind: movie|show, title, year, ids{imdb,tmdb,tvdb,trakt|simkl,slug}, plays/eps}
# Movies keep plays = [(ts, history_id)], shows keep eps = {(s,e): [(ts, history_id)]}.
EXT = ("imdb", "tmdb", "tvdb")


def clean_ids(ids: dict, kind: str) -> dict:
    out = {}
    for k in ("trakt", "simkl", "imdb", "tmdb", "tvdb", "slug"):
        v = ids.get(k)
        if v in (None, "", 0) or (kind == "movie" and k == "tvdb"):
            continue
        out[k] = int(v) if k in ("trakt", "simkl", "tmdb", "tvdb") and str(v).isdigit() else v
    return out


def build_trakt(D) -> dict:
    T = {}
    for h in D["trakt_movies"]:
        m = h["movie"]
        x = T.setdefault(("movie", m["ids"]["trakt"]), {"svc": "trakt", "kind": "movie", "title": m["title"],
                                                         "year": m.get("year"), "ids": clean_ids(m["ids"], "movie"), "plays": []})
        x["plays"].append((ts_of(h["watched_at"]), h["id"], h["action"]))
    for h in D["trakt_episodes"]:
        s, e = h["show"], h["episode"]
        x = T.setdefault(("show", s["ids"]["trakt"]), {"svc": "trakt", "kind": "show", "title": s["title"],
                                                        "year": s.get("year"), "ids": clean_ids(s["ids"], "show"), "eps": defaultdict(list)})
        x["eps"][(e["season"], e["number"])].append((ts_of(h["watched_at"]), h["id"], h["action"]))
    return T


def build_simkl(D) -> dict:
    S = {}
    for x in D["simkl"]["movies"]:
        m = x["movie"]
        if x.get("status") != "completed" and not x.get("last_watched_at"):
            continue
        S[("movie", m["ids"]["simkl"])] = {"svc": "simkl", "kind": "movie", "title": m["title"], "year": m.get("year"),
                                            "ids": clean_ids(m["ids"], "movie"),
                                            "plays": [(ts_of(x.get("last_watched_at")), None, x.get("status"))]}
    for typ in ("shows", "anime"):
        for x in D["simkl"][typ]:
            s = x["show"]
            if typ == "anime" and x.get("anime_type") == "movie":
                S[("movie", s["ids"]["simkl"])] = {"svc": "simkl", "kind": "movie", "title": s["title"], "year": s.get("year"),
                                                    "ids": clean_ids(s["ids"], "movie"), "anime": True,
                                                    "plays": [(ts_of(x.get("last_watched_at")), None, x.get("status"))]}
                continue
            eps = defaultdict(list)
            for se in x.get("seasons") or []:
                for e in se.get("episodes") or []:
                    eps[(se["number"], e["number"])].append((ts_of(e.get("watched_at")), None, None))
            # Simkl does NOT list the episodes of "completed" shows: only the count.
            complete = x.get("status") == "completed" and not eps and (x.get("watched_episodes_count") or 0) > 0
            if not eps and not complete:
                continue
            S[("show", s["ids"]["simkl"])] = {"svc": "simkl", "kind": "show", "title": s["title"], "year": s.get("year"),
                                               "ids": clean_ids(s["ids"], "show"), "eps": eps,
                                               "anime": typ == "anime", "status": x.get("status"), "complete": complete,
                                               "wcount": x.get("watched_episodes_count") or len(eps),
                                               "last": ts_of(x.get("last_watched_at"))}
    return S


def index(items: dict) -> dict:
    ix = defaultdict(set)
    for key, x in items.items():
        for k in EXT:
            if sid(x["ids"].get(k)):
                ix[(x["kind"], k, sid(x["ids"][k]))].add(key)
    return ix


def find(x: dict, ix: dict) -> list:
    """Candidates on the other service sharing an external ID, best match first."""
    hits = defaultdict(int)
    for k in EXT:
        v = sid(x["ids"].get(k))
        if v:
            for key in ix.get((x["kind"], k, v), ()):
                hits[key] += {"imdb": 3, "tvdb": 2, "tmdb": 2}[k]
    return sorted(hits, key=lambda k: -hits[k])


def id_conflicts(a: dict, b: dict) -> list:
    return [f"{k}: {a['ids'][k]} vs {b['ids'][k]}" for k in EXT
            if sid(a["ids"].get(k)) and sid(b["ids"].get(k)) and sid(a["ids"][k]) != sid(b["ids"][k])]


def ep_ranges(eps) -> str:
    by = defaultdict(list)
    for s, e in sorted(set(eps)):
        by[s].append(e)
    parts = []
    for s, es in by.items():
        runs, start, prev = [], es[0], es[0]
        for e in es[1:] + [None]:
            if e is not None and e == prev + 1:
                prev = e
                continue
            runs.append(f"E{start:02d}" + (f"–E{prev:02d}" if prev != start else ""))
            if e is not None:
                start = prev = e
        parts.append(f"S{s:02d} " + ",".join(runs))
    return "; ".join(parts)


def label(x: dict) -> str:
    t = x["title"] or "?"
    return t if re.search(r"\(\d{4}\)$", t) else f"{t} ({x['year'] or '?'})"


def idstr(ids: dict) -> str:
    return " ".join(f"{k}:{ids[k]}" for k in ("trakt", "simkl", "imdb", "tmdb", "tvdb") if ids.get(k))


# ------------------------------------------------------------------- analysis
def plex_witness(D, T: dict, S: dict) -> dict:
    """Match each Plex view against what Trakt and Simkl recorded at that time."""
    ev = []  # (ts, svc, key, (s,e)|None, history_id)
    for svc, items in (("trakt", T), ("simkl", S)):
        for key, x in items.items():
            if x["kind"] == "movie":
                for ts, hid, _ in x["plays"]:
                    if ts:
                        ev.append((ts, svc, key, None, hid))
            else:
                for se, plays in x["eps"].items():
                    for ts, hid, _ in plays:
                        if ts:
                            ev.append((ts, svc, key, se, hid))
    ev.sort(key=lambda t: t[0])
    times = [t[0] for t in ev]
    import bisect

    def same(p, x, se):
        if not any(sid(p["ids"].get(k)) and sid(p["ids"].get(k)) == sid(x["ids"].get(k)) for k in EXT):
            return False
        return p["metadata_type"] == 1 or se == (p["parent_index"], p["index"])

    res = {"unresolved": [], "miss": {"trakt": [], "simkl": []}, "wrong": {"trakt": [], "simkl": []}, "ok": 0}
    plex_claimed = set()  # events that belong to some Plex view
    near = {}
    for i, p in enumerate(D["plex"]):
        p["_i"] = i
        if not p["ids"]:
            res["unresolved"].append(p)
            continue
        lo, hi = bisect.bisect_left(times, p["viewed_at"] - 3600), bisect.bisect_right(times, p["viewed_at"] + 3 * 3600)
        near[i] = ev[lo:hi]
        for t in near[i]:
            items = T if t[1] == "trakt" else S
            if same(p, items[t[2]], t[3]):
                plex_claimed.add(t)
    for i, p in enumerate(D["plex"]):
        if not p["ids"]:
            continue
        for svc, items, ix in (("trakt", T, D["_ixT"]), ("simkl", S, D["_ixS"])):
            kind = "movie" if p["metadata_type"] == 1 else "show"
            have = False
            for key in find({"kind": kind, "ids": p["ids"]}, ix):
                x = items[key]
                if kind == "movie" or x.get("complete") or (p["parent_index"], p["index"]) in x["eps"]:
                    have = True
                    break
            # Did it also record something else at that time? Events on that service no Plex view explains.
            # Checked even when the view is present (otherwise fixing the gap would hide the bad scrobble).
            strays = [t for t in near[i] if t[1] == svc and t not in plex_claimed
                      and -1800 <= t[0] - p["viewed_at"] <= 2 * 3600]
            if strays:
                res["wrong"][svc].append((p, strays))
            if not have:
                res["miss"][svc].append(p)
    return res


def analyze(D) -> dict:
    T, S = build_trakt(D), build_simkl(D)
    ixT, ixS = index(T), index(S)
    D["_ixT"], D["_ixS"] = ixT, ixS
    R = {"T": T, "S": S, "pairs": [], "only_T": [], "only_S": [], "ep_T": [], "ep_S": [], "conflicts": [], "multi": []}
    matchedS = set()
    for key, x in T.items():
        c = find(x, ixS)
        if not c:
            R["only_T"].append(key)
            continue
        if len(c) > 1:
            R["multi"].append((key, c))
        y = S[c[0]]
        matchedS.update(c)
        R["pairs"].append((key, c[0]))
        if id_conflicts(x, y):
            R["conflicts"].append((key, c[0], id_conflicts(x, y)))
        if x["kind"] == "show" and not any(S[k].get("complete") for k in c):
            missS = sorted(set(x["eps"]) - set().union(*(S[k]["eps"].keys() for k in c)))
            if missS:
                R["ep_T"].append((key, c[0], missS))
    for key, y in S.items():
        c = find(y, ixT)
        if not c:
            R["only_S"].append(key)
            continue
        if y["kind"] == "show":
            have = set().union(*(T[k]["eps"].keys() for k in c))
            if y.get("complete"):
                # "completed" on Simkl = everything aired; compare against Trakt's list
                if len({se for se in have if se[0] > 0}) >= y["wcount"]:
                    continue
                aired = trakt_aired(T[c[0]]["ids"]["trakt"])
                y["eps"] = {se: [(y["last"], None, None)] for se in aired}
            missT = sorted(set(y["eps"]) - have)
            if missT:
                R["ep_S"].append((key, c[0], missT))
    # same (normalized) title with no shared ID: possible wrong match on one side
    byT = defaultdict(list)
    for key in R["only_T"]:
        byT[norm(T[key]["title"])].append(key)
    R["same_title"] = [(byT[norm(S[k]["title"])], k) for k in R["only_S"] if norm(S[k]["title"]) in byT]
    R["plex"] = plex_witness(D, T, S)
    return R


# ------------------------------------------------------------- sospechas
def strays_strict(R) -> dict:
    """Only events that are almost certainly the same view recorded against something else:
    Trakt = scrobble (trakt-scrobbler) within ±30 min of the Plex view; Simkl = same time (±3 min)."""
    T, S = R["T"], R["S"]
    out = {"trakt": [], "simkl": []}
    for p, st in R["plex"]["wrong"]["trakt"]:
        keep = []
        for t in st:
            x = T[t[2]]
            plays = x["plays"] if x["kind"] == "movie" else x["eps"][t[3]]
            acts = {a for ts, h, a in plays if ts == t[0]}
            if "scrobble" in acts and abs(t[0] - p["viewed_at"]) <= 1800:
                keep.append(t)
        if keep:
            out["trakt"].append((p, keep))
    for p, st in R["plex"]["wrong"]["simkl"]:
        keep = [t for t in st if abs(t[0] - p["viewed_at"]) <= 180]  # Simkl imports from Plex with the exact time
        if keep:
            out["simkl"].append((p, keep))
    return out


def tmdb_ext(kind: str, tmdb_id) -> dict:
    """External IDs according to TMDB (tie-breaker when one side has no imdb)."""
    if not tmdb_id:
        return {}
    f = CACHE / "tmdb_ext.json"
    cache = json.loads(f.read_text()) if f.exists() else {}
    k = f"{kind}:{tmdb_id}"
    if k not in cache:
        try:
            j = d.tmdb(f"/{'movie' if kind == 'movie' else 'tv'}/{tmdb_id}/external_ids")
            cache[k] = {"imdb": j.get("imdb_id"), "tvdb": j.get("tvdb_id")}
        except Exception:  # noqa: BLE001
            cache[k] = {}
        f.write_text(json.dumps(cache))
    return cache[k]


def same_work_as_plex(y: dict, p: dict) -> tuple[bool | None, str]:
    """Are the Simkl item and the Plex view the same work even without shared IDs?"""
    kind = "movie" if p["metadata_type"] == 1 else "show"
    pim = p["ids"].get("imdb")
    yim = y["ids"].get("imdb") or tmdb_ext(y["kind"], y["ids"].get("tmdb")).get("imdb")
    if yim and pim:
        return (yim == pim), f"imdb {yim} vs {pim}"
    if pim and y["ids"].get("tmdb"):
        try:
            j = d.tmdb(f"/find/{pim}", external_source="imdb_id")
            hits = [r["id"] for r in j.get(f"{'movie' if kind == 'movie' else 'tv'}_results") or []]
        except Exception:  # noqa: BLE001
            hits = []
        if hits:
            return (y["ids"]["tmdb"] in hits), f"TMDB maps {pim} to tmdb {hits[0]}, Simkl has tmdb {y['ids']['tmdb']}"
    a, b = norm(y["title"]), norm(p["show_title"] or p["title"])
    if a and b and (a.startswith(b) or b.startswith(a)):
        return None, "same title; TMDB has no imdb link to confirm it"
    return False, "different titles and IDs"


def plex_label(p) -> str:
    if p["metadata_type"] == 4:
        return f"{p['show_title'] or p['grandparent_title']} S{p['parent_index']:02d}E{p['index']:02d}"
    return f"{p['show_title'] or p['title']} ({p['year'] or '?'})"


# ---------------------------------------------------------------- plan
def iso(ts) -> str:
    return d.iso(ts) if ts else None


def last_ts(plays) -> float | None:
    return max((ts for ts, _, _ in plays if ts), default=None)


def build_plan(D, R, held_T: set, held_S: set) -> dict:
    """Add batches by external ID. held_*: (kind, key[, (s,e)]) not propagated because they are suspicious."""
    T, S = R["T"], R["S"]
    plan = {}

    def ids_for(x, order):
        return {k: x["ids"][k] for k in order if x["ids"].get(k)}

    # ---- Trakt -> Simkl
    mv, sh = [], {}
    for key in R["only_T"]:
        x = T[key]
        if ("item", key) in held_T:
            continue
        if x["kind"] == "movie":
            mv.append({"title": x["title"], "year": x["year"], "ids": ids_for(x, ("imdb", "tmdb")),
                       "watched_at": iso(last_ts(x["plays"])), "src": "trakt"})
        else:
            eps = [[s, e, iso(last_ts(pl))] for (s, e), pl in sorted(x["eps"].items()) if ("ep", key, (s, e)) not in held_T]
            if eps:
                sh[key] = {"title": x["title"], "year": x["year"], "ids": ids_for(x, ("imdb", "tvdb", "tmdb")), "eps": eps, "src": "trakt"}
    for key, _skey, miss in R["ep_T"]:
        x = T[key]
        eps = [[s, e, iso(last_ts(x["eps"][(s, e)]))] for s, e in miss if ("ep", key, (s, e)) not in held_T]
        if eps:
            sh[key] = {"title": x["title"], "year": x["year"], "ids": ids_for(x, ("imdb", "tvdb", "tmdb")), "eps": eps, "src": "trakt"}
    plan["simkl-movies"] = {"svc": "simkl", "movies": mv, "shows": []}
    plan["simkl-shows"] = {"svc": "simkl", "movies": [], "shows": list(sh.values())}

    # ---- Simkl -> Trakt
    mv, sh = [], {}
    for key in R["only_S"]:
        y = S[key]
        if ("item", key) in held_S:
            continue
        if y["kind"] == "movie":
            mv.append({"title": y["title"], "year": y["year"], "ids": ids_for(y, ("imdb", "tmdb")),
                       "watched_at": iso(last_ts(y["plays"])) or "released", "src": "simkl"})
        elif y.get("complete"):
            sh[key] = {"title": y["title"], "year": y["year"], "ids": ids_for(y, ("imdb", "tvdb", "tmdb")), "eps": [],
                       "all": True, "watched_at": iso(y["last"]) or "released", "n": y["wcount"], "src": "simkl"}
        else:
            eps = [[s, e, iso(last_ts(pl)) or "released"] for (s, e), pl in sorted(y["eps"].items()) if ("ep", key, (s, e)) not in held_S]
            if eps:
                sh[key] = {"title": y["title"], "year": y["year"], "ids": ids_for(y, ("imdb", "tvdb", "tmdb")), "eps": eps, "src": "simkl"}
    for key, _tkey, miss in R["ep_S"]:
        y = S[key]
        eps = [[s, e, iso(last_ts(y["eps"][(s, e)])) or "released"] for s, e in miss if ("ep", key, (s, e)) not in held_S]
        if eps:
            sh[key] = {"title": y["title"], "year": y["year"], "ids": ids_for(y, ("imdb", "tvdb", "tmdb")), "eps": eps, "src": "simkl"}
    plan["trakt-from-simkl"] = {"svc": "trakt", "movies": mv, "shows": list(sh.values())}

    # ---- Plex -> whatever each service is missing that no other batch covers
    for svc in ("trakt", "simkl"):
        already = set()
        for name, b in plan.items():
            if b["svc"] != svc:
                continue
            for m in b["movies"]:
                already |= {("movie", k, sid(v)) for k, v in m["ids"].items()}
            for s in b["shows"]:
                if s.get("all"):  # whole show: covers any Plex episode
                    already |= {("all", k, sid(v)) for k, v in s["ids"].items()}
                for se in s["eps"]:
                    already |= {("ep", k, sid(v), se[0], se[1]) for k, v in s["ids"].items()}
        mv, sh = {}, {}
        for p in R["plex"]["miss"][svc]:
            ids = dict(p["ids"])
            if p["metadata_type"] == 1:
                if any(("movie", k, sid(v)) in already for k, v in ids.items()):
                    continue
                k0 = sid(ids.get("imdb") or ids.get("tmdb"))
                mv[k0] = {"title": p["show_title"] or p["title"], "year": p["year"],
                          "ids": {k: ids[k] for k in ("imdb", "tmdb") if ids.get(k)}, "watched_at": iso(p["viewed_at"]), "src": "plex"}
            else:
                se = (p["parent_index"], p["index"])
                if any(("ep", k, sid(v), *se) in already or ("all", k, sid(v)) in already for k, v in ids.items()):
                    continue
                k0 = sid(ids.get("tvdb") or ids.get("imdb") or ids.get("tmdb"))
                s = sh.setdefault(k0, {"title": p["show_title"] or p["grandparent_title"], "year": p["year"],
                                       "ids": {k: ids[k] for k in ("imdb", "tvdb", "tmdb") if ids.get(k)}, "eps": [], "src": "plex"})
                if not any(e[:2] == list(se) for e in s["eps"]):
                    s["eps"].append([se[0], se[1], iso(p["viewed_at"])])
        plan[f"{svc}-from-plex"] = {"svc": svc, "movies": list(mv.values()), "shows": list(sh.values())}
    for b in plan.values():
        b["count"] = len(b["movies"]) + sum(s.get("n") or len(s["eps"]) for s in b["shows"])
    return plan


# --------------------------------------------------------------------- report
def write_report(D, R) -> None:
    T, S, P = R["T"], R["S"], R["plex"]
    X = strays_strict(R)
    held_T, held_S = set(), set()
    L = []
    w = L.append

    # --- suspicions (computed first because they decide what is NOT propagated)
    sus_T = []  # (plex view, [(trakt key, s/e, ts, history_id)])
    for p, st in X["trakt"]:
        rows = []
        for t in st:
            x = T[t[2]]
            plays = x["plays"] if x["kind"] == "movie" else x["eps"][t[3]]
            hid = [h for ts, h, a in plays if ts == t[0]]
            rows.append((t[2], t[3], t[0], hid[0] if hid else None))
            # do not propagate to Simkl what only exists because of this scrobble
            if x["kind"] == "movie":
                if len(x["plays"]) == 1:
                    held_T.add(("item", t[2]))
            else:
                if len(plays) == 1:
                    held_T.add(("ep", t[2], t[3]))
        sus_T.append((p, rows))
    sus_S = []
    for p, st in X["simkl"]:
        rows = []
        for t in st:
            y = S[t[2]]
            same_work, why = same_work_as_plex(y, p)
            rows.append((t[2], t[3], t[0], same_work, why))
            # Plex wins: the view is added with Plex's IDs (*-from-plex batch); Simkl's is not propagated
            held_S.add(("item", t[2]) if y["kind"] == "movie" else ("ep", t[2], t[3]))
        sus_S.append((p, rows))
    same_title = []
    for tkeys, skey in R["same_title"]:
        y = S[skey]
        for tk in tkeys:
            x = T[tk]
            ext = {k: v for k, v in tmdb_ext(y["kind"], y["ids"].get("tmdb")).items() if v} if not y["ids"].get("imdb") else {}
            same = bool(ext.get("imdb") and ext["imdb"] == x["ids"].get("imdb"))
            # what does Plex say?
            pv = [p for p in D["plex"] if p["ids"] and norm(p["show_title"] or p["title"] if p["metadata_type"] == 1 else p["show_title"] or p["grandparent_title"]) == norm(x["title"])]
            verdict = ""
            for p in pv:
                if any(sid(p["ids"].get(k)) and sid(p["ids"][k]) == sid(x["ids"].get(k)) for k in EXT):
                    verdict = "Plex agrees with **Trakt** → Simkl is wrong"
                elif any(sid(p["ids"].get(k)) and sid(p["ids"][k]) == sid(y["ids"].get(k)) for k in EXT):
                    verdict = "Plex agrees with **Simkl** → Trakt is wrong"
            if same:
                verdict = "same work (TMDB confirms), Simkl just lacks IDs"
            same_title.append((tk, skey, ext, verdict))
            if not same:  # without a witness hold both; with one, only the wrong side
                if "Trakt is wrong" not in verdict:
                    held_S.add(("item", skey))
                if "Simkl is wrong" not in verdict:
                    held_T.add(("item", tk))

    # homonyms: Trakt-only items whose title matches another work Simkl has (and Trakt has too).
    # Usually different works, but that is what fuzzy matches look like; held for review.
    stitle = defaultdict(list)
    for k, y in S.items():
        stitle[(y["kind"], norm(y["title"]))].append(k)
    homonyms = []
    paired = {tk for tks, _ in R["same_title"] for tk in tks}
    plex_ids = {(k, sid(v)) for pv in D["plex"] for k, v in pv["ids"].items()}  # what Plex confirms is not held
    for k in R["only_T"]:
        x = T[k]
        hit = [h for h in stitle.get((x["kind"], norm(x["title"])), []) if h not in R["only_S"]]
        if hit and k not in paired and not any((kk, sid(x["ids"].get(kk))) in plex_ids for kk in EXT):
            homonyms.append((k, hit))
            held_T.add(("item", k))

    plan = build_plan(D, R, held_T, held_S)
    OUT.mkdir(exist_ok=True)
    stamp = datetime.now().strftime("%Y-%m-%d")

    # ------------------------------------------------------------ cabecera
    nT_m = sum(1 for k in T if k[0] == "movie")
    nS_m = sum(1 for k in S if k[0] == "movie")
    nT_e = sum(len(x["eps"]) for x in T.values() if x["kind"] == "show")
    nS_e = sum(x["wcount"] if x.get("complete") else len(x["eps"]) for x in S.values() if x["kind"] == "show")
    only_T_m = [k for k in R["only_T"] if k[0] == "movie"]
    only_T_s = [k for k in R["only_T"] if k[0] == "show"]
    only_S_m = [k for k in R["only_S"] if k[0] == "movie"]
    only_S_s = [k for k in R["only_S"] if k[0] == "show"]
    w(f"# Trakt ↔ Simkl reconciliation — {stamp}")
    w("")
    w("**Read-only** report: nothing was written to Trakt or Simkl. "
      f"Data downloaded {datetime.fromtimestamp((CACHE / 'trakt_movies.json').stat().st_mtime):%Y-%m-%d %H:%M}. "
      "Compared by external ID (imdb/tmdb/tvdb), never by title; Plex (`metadata_item_views`, account 1, since "
      f"{day(min(p['viewed_at'] for p in D['plex']))[:10]}) is the witness of what you actually watched.")
    w("")
    w("| | Trakt | Simkl |")
    w("|---|---:|---:|")
    w(f"| Movies watched (unique) | {nT_m} | {nS_m} |")
    w(f"| Shows with something watched | {len(T) - nT_m} | {len(S) - nS_m} |")
    w(f"| Episodes watched (unique) | {nT_e} | {nS_e} |")
    w("")
    w("**Summary**")
    w("")
    w(f"- (a) Missing on **Simkl**: {len(only_T_m)} movies, {len(only_T_s)} whole shows "
      f"({sum(len(T[k]['eps']) for k in only_T_s)} episodes) and {sum(len(m) for *_, m in R['ep_T'])} loose episodes in {len(R['ep_T'])} shows already there.")
    w(f"- (b) Missing on **Trakt**: {len(only_S_m)} movies, {len(only_S_s)} shows ({sum(S[k]['wcount'] for k in only_S_s)} episodes) "
      f"and {sum(len(m) for *_, m in R['ep_S'])} loose episodes. Also, {len(P['miss']['trakt'])} Plex views are not on Trakt.")
    w(f"- (c) Suspicions: {len(sus_T)} Plex views trakt-scrobbler recorded against another work, {len(sus_S)} on Simkl, "
      f"{len(same_title)} same titles with different IDs, {len(R['conflicts'])} conflicting IDs within a match, "
      f"{len(homonyms)} held homonyms, "
      f"and any known trakt-scrobbler mismatches (c5).")
    w("- Anything flagged as suspicious is **not** added to batches (a bad match is never propagated to the other service).")
    w("")
    w("**Proposed batches** (nothing is applied without your OK, batch by batch):")
    w("")
    w("| Batch | Target | Adds | Movies | Episodes |")
    w("|---|---|---|---:|---:|")
    desc = {"simkl-movies": "movies only on Trakt", "simkl-shows": "episodes only on Trakt",
            "trakt-from-simkl": "what is only on Simkl", "trakt-from-plex": "Plex views Trakt lacks",
            "simkl-from-plex": "Plex views Simkl lacks (not covered by another batch)"}
    for name, b in plan.items():
        w(f"| `{name}` | {b['svc'].capitalize()} | {desc[name]} | {len(b['movies'])} | {sum(s.get('n') or len(s['eps']) for s in b['shows'])} |")
    w("")

    # ------------------------------------------------------- (c) computed first, but printed last
    # (a)
    w("## (a) On Trakt, missing on Simkl")
    w("")
    w(f"### Movies ({len(only_T_m)})")
    w("")
    w("Sorted by last time Trakt marked it. `⚠` = suspicious, not in the batch.")
    w("")
    w("| Movie | Plays on Trakt | Last | IDs |")
    w("|---|---:|---|---|")
    for k in sorted(only_T_m, key=lambda k: -(last_ts(T[k]["plays"]) or 0)):
        x = T[k]
        flag = " ⚠" if ("item", k) in held_T else ""
        w(f"| {label(x)}{flag} | {len(x['plays'])} | {day(last_ts(x['plays']))[:10]} | {idstr(x['ids'])} |")
    w("")
    w(f"### Shows not on Simkl ({len(only_T_s)})")
    w("")
    w("| Show | Episodes | Which | Last | IDs |")
    w("|---|---:|---|---|---|")
    for k in sorted(only_T_s, key=lambda k: -len(T[k]["eps"])):
        x = T[k]
        held = [se for se in x["eps"] if ("ep", k, se) in held_T]
        flag = f" ⚠ ({len(held)} suspicious)" if held else ""
        last = max((last_ts(pl) or 0) for pl in x["eps"].values())
        w(f"| {label(x)}{flag} | {len(x['eps'])} | {ep_ranges(x['eps'])} | {day(last)[:10]} | {idstr(x['ids'])} |")
    w("")
    w(f"### Loose episodes in shows present on both ({len(R['ep_T'])} shows)")
    w("")
    w("| Show | Missing on Simkl | Which | Simkl has |")
    w("|---|---:|---|---:|")
    for k, sk, miss in sorted(R["ep_T"], key=lambda t: -len(t[2])):
        w(f"| {label(T[k])} | {len(miss)} | {ep_ranges(miss)} | {S[sk]['wcount']} |")
    w("")

    # (b)
    w("## (b) On Simkl, missing on Trakt")
    w("")
    w(f"### Movies ({len(only_S_m)})")
    w("")
    w("| Movie | Last | IDs | Note |")
    w("|---|---|---|---|")
    for k in sorted(only_S_m, key=lambda k: -(last_ts(S[k]["plays"]) or 0)):
        y = S[k]
        note = "⚠ suspicious (see c)" if ("item", k) in held_S else ""
        w(f"| {label(y)} | {day(last_ts(y['plays']))[:10]} | {idstr(y['ids'])} | {note} |")
    w("")
    w(f"### Shows ({len(only_S_s)}) and loose episodes ({len(R['ep_S'])} shows)")
    w("")
    w("| Show | Missing on Trakt | Which | IDs |")
    w("|---|---:|---|---|")
    for k in only_S_s:
        y = S[k]
        w(f"| {label(y)}{' (anime)' if y.get('anime') else ''} | {y['wcount']} | {'complete (Simkl does not list episodes)' if y.get('complete') else ep_ranges(y['eps'])} | {idstr(y['ids'])} |")
    for k, tk, miss in R["ep_S"]:
        note = " — complete on Simkl; using Trakt's list" if S[k].get("complete") else ""
        w(f"| {label(S[k])} (Trakt already has {len(T[tk]['eps'])}){note} | {len(miss)} | {ep_ranges(miss)} | {idstr(S[k]['ids'])} |")
    w("")
    for svc in ("trakt", "simkl"):
        miss = P["miss"][svc]
        grp = defaultdict(list)
        for p in miss:
            grp[(p["metadata_type"], p["show_title"] or (p["grandparent_title"] if p["metadata_type"] == 4 else p["title"]))].append(p)
        w(f"### Plex views not on {svc.capitalize()} ({len(miss)} views, {len(grp)} titles)")
        w("")
        if svc == "simkl":
            w("Most are also missing via (a); the `simkl-from-plex` batch only carries those not coming from Trakt.")
            w("")
        w("| Title | Views | Which | Dates | IDs (from Plex) |")
        w("|---|---:|---|---|---|")
        for (mt, title), ps in sorted(grp.items(), key=lambda kv: kv[1][0]["viewed_at"]):
            eps = [(p["parent_index"], p["index"]) for p in ps] if mt == 4 else []
            dates = sorted(day(p["viewed_at"])[:10] for p in ps)
            dr = dates[0] if dates[0] == dates[-1] else f"{dates[0]} → {dates[-1]}"
            w(f"| {label({'title': title, 'year': ps[0]['year']})} | {len(set((p['parent_index'], p['index']) for p in ps)) if mt == 4 else len(ps)} | {ep_ranges(eps) if eps else 'movie'} | {dr} | {idstr(ps[0]['ids'])} |")
        w("")

    # (c)
    w("## (c) Suspected wrong matches")
    w("")
    w("### c1. Plex views Trakt recorded against another work")
    w("")
    w("A trakt-scrobbler scrobble within ±30 min of a Plex view Trakt does not have, pointing at something else. "
      "Plex IDs are Plex's own match (the reference). Fix = add the Plex item (in `trakt-from-plex`) "
      "and **delete the wrong history id** (confirmed one by one).")
    w("")
    w("| Watched on Plex | Plex time | Trakt recorded | Δ | history id | Does the wrong work have other legit plays? |")
    w("|---|---|---|---:|---|---|")
    for p, rows in sus_T:
        for tk, se, ts, hid in rows:
            x = T[tk]
            nplays = len(x["plays"]) if x["kind"] == "movie" else sum(len(v) for v in x["eps"].values())
            other = f"yes ({nplays - 1} more)" if nplays > 1 else "no, only this one"
            what = label(x) + (f" S{se[0]:02d}E{se[1]:02d}" if se else "") + f" · {idstr(x['ids'])}"
            w(f"| {plex_label(p)} · {idstr(p['ids'])} | {day(p['viewed_at'])} | {what} | {(ts - p['viewed_at']) / 60:+.0f} min | `{hid}` | {other} |")
    w("")
    w("### c2. Plex views Simkl recorded against another work")
    w("")
    w("Simkl imports from Plex with the exact same time; if the view does not match by ID, Simkl matched it by title.")
    w("")
    w("| Watched on Plex | Simkl recorded | Verdict |")
    w("|---|---|---|")
    for p, rows in sus_S:
        for sk, se, ts, same_work, why in rows:
            y = S[sk]
            if same_work is True:
                v = f"same work ({why}); only the IDs differ — nothing to delete"
            elif same_work is None:
                v = f"probably the same work ({why}); no deletion proposed"
            else:
                v = f"**different work** ({why}) → delete on Simkl and add the Plex one"
            w(f"| {plex_label(p)} · {idstr(p['ids'])} | {label(y)} · {idstr(y['ids'])} | {v} |")
    w("")
    w("### c3. Same title, different IDs (no shared ID)")
    w("")
    w("| Trakt | Simkl | Verdict |")
    w("|---|---|---|")
    for tk, sk, ext, verdict in same_title:
        w(f"| {label(T[tk])} · {idstr(T[tk]['ids'])} | {label(S[sk])} · {idstr(S[sk]['ids'])} | {verdict or 'no Plex witness: review by hand'} |")
    w("")
    w("### c4. Matched but with a differing ID")
    w("")
    w("| Trakt | Simkl | Difference |")
    w("|---|---|---|")
    for tk, sk, c in R["conflicts"]:
        w(f"| {label(T[tk])} · {idstr(T[tk]['ids'])} | {label(S[sk])} · {idstr(S[sk]['ids'])} | {'; '.join(c)} |")
    w("")
    w("### c5b. Homonyms (held for review)")
    w("")
    w("Only on Trakt, and the title matches **another** work both services already have. Usually two different "
      "movies you watched, but a fuzzy match looks the same. Not batched until you confirm.")
    w("")
    w("| Only on Trakt | How it got there (plays) | The other work (on both) |")
    w("|---|---|---|")
    for k, hit in homonyms:
        x = T[k]
        how = ", ".join(f"{a} {day(ts)[:10]}" for ts, _, a in sorted(x["plays"] if x["kind"] == "movie" else
                                                                     [p for v in x["eps"].values() for p in v], key=lambda t: t[0] or 0))
        w(f"| {label(x)} · {idstr(x['ids'])} | {how} | {'; '.join(label(S[h]) + ' · ' + idstr(S[h]['ids']) for h in hit)} |")
    w("")
    # Optional, personal: reconcile/known-mismatches.json (git-ignored), e.g.
    # [{"trakt": 283137, "real": "Real Title (2026)", "wrong": "What Trakt matched", "cache_key": "Real Title2026"}]
    km_file = OUT / "known-mismatches.json"
    km = json.loads(km_file.read_text()) if km_file.exists() else []
    known = {m["trakt"]: m for m in km}
    if known:
        w("### c5. Known trakt-scrobbler mismatches")
        w("")
        w("Scrobbles that trakt-scrobbler pointed at the wrong title. Below 80 % they never reach **history**, "
          "but they leave **playback progress** (`/sync/playback`) against the wrong item:")
        w("")
        w("| What you watched | Trakt pointed it at | In history? | Progress on Trakt (playback id) |")
        w("|---|---|---|---|")
        for tid, m in known.items():
            inh = any(k == ("show", tid) or k == ("movie", tid) for k in T)
            pb = [pp for pp in D["playback"] if ((pp.get("show") or pp.get("movie")) or {}).get("ids", {}).get("trakt") == tid]
            wrong = (pb[0].get("show") or pb[0].get("movie"))["title"] if pb else m.get("wrong", "?")
            pbs = ", ".join(f"`{pp['id']}` ({pp['progress']:.1f} %, {pp['paused_at'][:10]})" for pp in pb) or "no"
            w(f"| {m['real']} | {wrong} (trakt {tid}) | {'yes' if inh else 'no'} | {pbs} |")
        keys = [f"`{m['cache_key']}`→{tid}" for tid, m in known.items() if m.get("cache_key")]
        if keys:
            w("")
            w("trakt-scrobbler's `trakt_cache.json` still maps " + ", ".join(keys) +
              "; while those keys remain, the next scrobble repeats the error. Fix them only with the user's OK.")
        w("")
    w("### c6. Plex views without external IDs (cannot be reconciled automatically)")
    w("")
    w("| View | Date | GUID | What Trakt/Simkl have at that time |")
    w("|---|---|---|---|")
    for p in P["unresolved"]:
        near = []
        for svc, items in (("Trakt", T), ("Simkl", S)):
            for key, x in items.items():
                pls = x["plays"] if x["kind"] == "movie" else [t for v in x["eps"].values() for t in v]
                if any(ts and -1800 <= ts - p["viewed_at"] <= 1800 for ts, _, _ in pls):
                    near.append(f"{svc}: {label(x)}")
        t = (p["grandparent_title"] + " · " if p["grandparent_title"] else "") + (p["title"] or "")
        w(f"| {t} | {day(p['viewed_at'])} | `{p['grandparent_guid'] or p['guid']}` | {'; '.join(near) or '—'} |")
    w("")
    w("---")
    w("Regenerate: `python3 tvsync_reconcile.py report --refresh` · list batches: `python3 tvsync_reconcile.py batches`")

    rep = OUT / f"report-{stamp}.md"
    rep.write_text("\n".join(L) + "\n")
    held = {"trakt": sorted(map(str, held_T)), "simkl": sorted(map(str, held_S))}
    deletions = [{"svc": "trakt", "history_id": hid, "what": label(T[tk]) + (f" S{se[0]:02d}E{se[1]:02d}" if se else ""),
                  "plex": plex_label(p)} for p, rows in sus_T for tk, se, ts, hid in rows]
    deletions += [{"svc": "simkl", "kind": S[sk]["kind"], "ids": S[sk]["ids"], "ep": se, "what": label(S[sk]),
                   "plex": plex_label(p)} for p, rows in sus_S for sk, se, ts, same, why in rows if same is False]
    deletions += [{"svc": "trakt-playback", "playback_id": pp["id"],
                   "what": ((pp.get("show") or pp.get("movie")) or {}).get("title")}
                  for pp in D["playback"] if ((pp.get("show") or pp.get("movie")) or {}).get("ids", {}).get("trakt") in known]
    (OUT / "plan.json").write_text(json.dumps({"generated": stamp, "batches": plan, "held": held,
                                               "deletion_candidates": deletions}, indent=1, ensure_ascii=False))
    print(rep)
    print({k: v["count"] for k, v in plan.items()})


# ---------------------------------------------------------------------- write
def item_key(it: dict, kind: str, ep=None) -> str:
    ids = it["ids"]
    base = f"{kind}:" + ",".join(f"{k}={ids[k]}" for k in sorted(ids))
    return base + (f":S{ep[0]}E{ep[1]}" if ep else "")


def units(batch: dict) -> list:
    """Flatten a batch into units (movie, episode or whole show) with their key."""
    out = []
    for m in batch["movies"]:
        out.append((item_key(m, "movie"), "movie", m, None))
    for sh in batch["shows"]:
        if sh.get("all"):
            out.append((item_key(sh, "show") + ":all", "show-all", sh, None))
        for s, e, wa in sh["eps"]:
            out.append((item_key(sh, "show", (s, e)), "ep", sh, (s, e, wa)))
    return out


def body_for(svc: str, chunk: list) -> dict:
    movies, shows = [], {}
    for _k, kind, it, ep in chunk:
        meta = {"ids": it["ids"]}
        if svc == "simkl":
            meta = {"title": it["title"], **({"year": it["year"]} if it.get("year") else {}), "ids": it["ids"]}
        if kind == "movie":
            movies.append({**meta, "watched_at": it["watched_at"] or "released"} if (svc == "trakt" or it["watched_at"])
                          else meta)
        elif kind == "show-all":
            shows[json.dumps(it["ids"], sort_keys=True)] = {**meta, "watched_at": it["watched_at"]}
        else:
            sh = shows.setdefault(json.dumps(it["ids"], sort_keys=True), {**meta, "seasons": []})
            se = next((x for x in sh["seasons"] if x["number"] == ep[0]), None)
            if se is None:
                se = {"number": ep[0], "episodes": []}
                sh["seasons"].append(se)
            se["episodes"].append({"number": ep[1], **({"watched_at": ep[2]} if ep[2] else {})})
    body = {}
    if movies:
        body["movies"] = movies
    if shows:
        body["shows"] = list(shows.values())
    return body


def post(svc: str, path: str, body: dict):
    if svc == "trakt":
        return d.http_json("POST", f"https://api.trakt.tv{path}", d.trakt_headers(True), body, timeout=120)
    cid, tok = d.simkl_creds()
    return d.http_json("POST", f"https://api.simkl.com{path}?client_id={cid}&app-name=tvsync&app-version=1.0",
                       {"Authorization": f"Bearer {tok}"}, body, timeout=120)


def cmd_batches() -> None:
    plan = json.loads((OUT / "plan.json").read_text())
    print(f"plan from {plan['generated']}")
    for name, b in plan["batches"].items():
        done = OUT / f"done-{name}.json"
        nd = len(json.loads(done.read_text())) if done.exists() else 0
        print(f"  {name:18} -> {b['svc']:5}  {len(b['movies']):4} movies, "
              f"{sum(s.get('n') or len(s['eps']) for s in b['shows']):5} episodes  (already applied: {nd})")
    print(f"  deletion candidates: {len(plan['deletion_candidates'])} (one by one, with `delete`)")


def cmd_apply(name: str, yes: bool, limit: int | None, chunk_size: int, skip: list | None = None) -> None:
    plan = json.loads((OUT / "plan.json").read_text())
    b = plan["batches"][name]
    svc = b["svc"]
    donef = OUT / f"done-{name}.json"
    done = set(json.loads(donef.read_text())) if donef.exists() else set()
    skip_titles = {t.lower() for t in (skip or [])}
    todo = [u for u in units(b) if u[0] not in done and u[2]["title"].lower() not in skip_titles]
    if limit:
        todo = todo[:limit]
    print(f"{name}: {len(todo)} pending units to {svc} (already done: {len(done)})")
    for u in todo[:15]:
        it, ep = u[2], u[3]
        print(f"   {it['title']} ({it.get('year')}) {('S%02dE%02d' % ep[:2]) if ep else u[1]}  {idstr(it['ids'])}")
    if len(todo) > 15:
        print(f"   … and {len(todo) - 15} more")
    if not yes:
        print("DRY RUN: nothing was written. Add --yes to apply.")
        return
    log = []
    for i in range(0, len(todo), chunk_size):
        chunk = todo[i:i + chunk_size]
        body = body_for(svc, chunk)
        st, j = post(svc, "/sync/history", body)
        log.append({"status": st, "sent": len(chunk), "resp": j})
        if st not in (200, 201) or not isinstance(j, dict):
            print(f"  ✗ {svc} {st}: {str(j)[:300]} — stopping; progress so far is saved")
            break
        nf = j.get("not_found") or {}
        miss = set()  # (ids, None) = whole work not found; (ids, (s, e)) = only that episode
        for k in ("movies", "shows", "seasons", "episodes"):
            for x in nf.get(k) or []:
                key = json.dumps(x.get("ids"), sort_keys=True)
                eps = [(se["number"], e["number"]) for se in x.get("seasons") or [] for e in se.get("episodes") or []]
                if eps:
                    miss |= {(key, ep) for ep in eps}
                else:
                    miss.add((key, None))
        # Simkl puts what it considers unreleased into "plantowatch": that is not a play
        for stt in (j.get("added") or {}).get("statuses") or []:
            if (stt.get("response") or {}).get("status") == "plantowatch":
                miss.add((json.dumps((stt.get("request") or {}).get("ids"), sort_keys=True), None))
        for u in chunk:
            key = json.dumps(u[2]["ids"], sort_keys=True)
            if (key, None) not in miss and not (u[3] and (key, tuple(u[3][:2])) in miss):
                done.add(u[0])
        donef.write_text(json.dumps(sorted(done)))
        print(f"  ✓ {i + len(chunk)}/{len(todo)}  added={j.get('added')}  not_found={sum(len(v or []) for v in nf.values())}")
        time.sleep(1.5)
    (OUT / f"applied-{name}-{int(time.time())}.json").write_text(json.dumps(log, indent=1, ensure_ascii=False))


def cmd_delete(what: str, args: list, yes: bool) -> None:
    if what == "trakt-history":
        desc, call = f"Trakt: remove play history id {args[0]}", ("trakt", "/sync/history/remove", {"ids": [int(args[0])]})
    elif what == "trakt-playback":
        desc, call = f"Trakt: remove playback progress {args[0]}", ("trakt-del", f"/sync/playback/{int(args[0])}", None)
    elif what == "simkl-movie":
        desc, call = f"Simkl: remove movie simkl {args[0]}", ("simkl", "/sync/history/remove", {"movies": [{"ids": {"simkl": int(args[0])}}]})
    elif what == "simkl-episode":
        sid_, s, e = map(int, args[:3])
        desc, call = (f"Simkl: remove S{s:02d}E{e:02d} from show simkl {sid_}",
                      ("simkl", "/sync/history/remove", {"shows": [{"ids": {"simkl": sid_}, "seasons": [{"number": s, "episodes": [{"number": e}]}]}]}))
    else:
        raise SystemExit("unknown type")
    print(desc)
    if not yes:
        print("DRY RUN: nothing was deleted. Add --yes to apply.")
        return
    if call[0] == "trakt-del":
        st, j = d.http_json("DELETE", f"https://api.trakt.tv{call[1]}", d.trakt_headers(True))
    else:
        st, j = post(call[0], call[1], call[2])
    print(st, j)
    with (OUT / "deletions.log").open("a") as f:
        f.write(json.dumps({"at": time.time(), "what": desc, "status": st, "resp": j}, ensure_ascii=False) + "\n")


# --------------------------------------------------------------------- main
def main() -> None:
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("fetch")
    p.add_argument("--refresh", action="store_true")
    p = sub.add_parser("report")
    p.add_argument("--refresh", action="store_true")
    sub.add_parser("batches")
    p = sub.add_parser("apply")
    p.add_argument("batch")
    p.add_argument("--yes", action="store_true")
    p.add_argument("--limit", type=int)
    p.add_argument("--chunk", type=int, default=100)
    p.add_argument("--skip", action="append", help="exact title to skip (repeatable)")
    p = sub.add_parser("delete")
    p.add_argument("what", choices=["trakt-history", "trakt-playback", "simkl-movie", "simkl-episode"])
    p.add_argument("args", nargs="+")
    p.add_argument("--yes", action="store_true")
    a = ap.parse_args()
    if a.cmd == "batches":
        cmd_batches()
    elif a.cmd == "apply":
        cmd_apply(a.batch, a.yes, a.limit, a.chunk, a.skip)
    elif a.cmd == "delete":
        cmd_delete(a.what, a.args, a.yes)
    elif a.cmd == "fetch":
        data = load(a.refresh)
        print({k: len(v) for k, v in data.items()})
    elif a.cmd == "report":
        D = load(a.refresh)
        D["playback"] = disk_cache("playback", a.refresh, fetch_playback)
        R = analyze(D)
        write_report(D, R)


if __name__ == "__main__":
    main()
