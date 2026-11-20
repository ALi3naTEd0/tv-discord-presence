# Full-history reconciliation (`tvsync_reconcile.py`)

The dashboard only handles **recent** viewings. This CLI compares the **whole** Trakt and Simkl histories, with Plex's own play log as a witness, and produces batches to make them uniform. It reuses the dashboard's credentials and helpers and never refreshes the Trakt token.

```bash
./tvsync_reconcile.py fetch [--refresh]      # download + cache Trakt, Simkl, Plex views (~/.cache/tvsync-reconcile, 6 h)
./tvsync_reconcile.py report [--refresh]     # write reconcile/report-YYYY-MM-DD.md + reconcile/plan.json
./tvsync_reconcile.py batches                # list batches in the plan
./tvsync_reconcile.py apply BATCH            # dry run
./tvsync_reconcile.py apply BATCH --yes [--limit N] [--chunk 100] [--skip "Exact Title"]
./tvsync_reconcile.py delete trakt-history HISTORY_ID [--yes]
./tvsync_reconcile.py delete trakt-playback|simkl-movie|simkl-episode ... [--yes]
```

| Batch | Contents |
|---|---|
| `simkl-movies` | movies only on Trakt |
| `simkl-shows` | episodes only on Trakt |
| `trakt-from-simkl` | what is only on Simkl |
| `trakt-from-plex` | Plex plays Trakt does not have |
| `simkl-from-plex` | Plex plays Simkl does not have (and no other batch covers) |

Rules:

- Read-only unless `apply`/`delete` is run with `--yes`. Review the report and the dry run first.
- Everything is written by external ID (IMDb/TMDB/TVDB), never by title. Items whose IDs disagree between services ("homonyms") are listed for manual review instead of being forced.
- Applied batches leave `reconcile/applied-*.json` and `reconcile/done-*.json`, so re-running `apply` skips what already went through. Deletions are appended to `reconcile/deletions.log`.
- `reconcile/` holds personal watch history and is git-ignored.
- Simkl free accounts are capped at ~500 writes/day and do not store rewatches; big batches need several days or `--limit`.
