<script lang="ts">
  import { untrack } from 'svelte'
  import { api, type Episode, type Result, type Session, type Show } from './api'
  import { current, minutes, sinkName, time } from './format'
  import { app, loadHistory, loadNow, replaceSession, toast } from './store.svelte'
  import { setHidden } from './actions'

  let { session }: { session: Session } = $props()

  // the dialog is recreated for each session ({#key} in App), so its starting values are read once
  const cur = untrack(() => current(session))
  const hint = untrack(() => session.hint)
  // "S03:E02 - Kim" (Tubi): the episode, but not the show
  const ref = untrack(() => /S(\d{1,2})\s*[:.]?\s*E(\d{1,3})/i.exec(hint?.title ?? ''))
  // the name after the number, "Kim": apps number episodes their own way, the name is surer
  const refName = untrack(() => (ref ? (hint?.title ?? '').slice(ref.index + ref[0].length).replace(/^[\s:.\-–—]+/, '').trim() : ''))
  const norm = (x?: string) => (x ?? '').toLowerCase().normalize('NFD').replace(/[\u0300-\u036f]/g, '').replace(/[^a-z0-9]+/g, ' ').trim()
  let byName = $state('')
  // the show last watched in this app: Tubi and the like are usually the same show
  const recent = untrack(() => app.history.find(x => x.id !== session.id && x.app.id === session.app.id
    && x.items.some(i => i.kind === 'episode'))?.items.at(-1))
  // HBO and Android TV publish the episode name and then the show: search the show.
  let q = $state(cur?.title ?? hint?.subtitle ?? (ref ? recent?.title ?? '' : hint?.title) ?? '')
  let results = $state<Result[]>([])
  let searching = $state(false)
  let searchError = $state('')
  let picked = $state<Result>()
  let show = $state<Show>()
  let season = $state(cur?.season ?? hint?.season ?? (ref ? Number(ref[1]) : 1))
  let episodes = $state<Episode[]>([])
  let chosen = $state<number[]>(cur?.kind === 'episode' && cur.episode ? [cur.episode]
    : hint?.episode ? [hint.episode] : ref ? [Number(ref[2])] : [])
  let remember = $state(!!(hint?.title || hint?.subtitle))
  let saving = $state(false)
  let dialog: HTMLDialogElement

  let timer: ReturnType<typeof setTimeout>
  let seq = 0
  async function search(text: string) {
    const my = ++seq
    if (text.trim().length < 2) { results = []; return }
    searching = true; searchError = ''
    try {
      const r = await api.search(text.trim())
      if (my === seq) results = r
    } catch (e) {
      if (my === seq) searchError = (e as Error).message
    } finally {
      if (my === seq) searching = false
    }
  }
  $effect(() => { const text = q; clearTimeout(timer); timer = setTimeout(() => search(text), 300) })

  // preselect what it is now, or the only result
  $effect(() => {
    if (picked || !results.length) return
    const want = cur?.ids.tmdb ?? (ref ? recent?.ids.tmdb : undefined)
    const same = want ? results.find(r => r.tmdb === want) : undefined
    if (same) pick(same)
  })

  async function pick(r: Result) {
    picked = r; show = undefined; episodes = []
    if (r.kind !== 'tv') return
    try {
      show = await api.show(r.tmdb)
      if (!show.seasons.some(s => s.number === season)) season = show.seasons.find(s => s.number > 0)?.number ?? show.seasons[0]?.number ?? 1
      await loadSeason()
    } catch (e) { toast((e as Error).message) }
  }
  async function loadSeason() {
    if (!picked) return
    try { episodes = await api.season(picked.tmdb, season) } catch { episodes = [] }
    byName = ''
    if (refName && !cur) {
      const hits = episodes.filter(e => norm(e.name) === norm(refName))
      if (hits.length === 1 && !(chosen.length === 1 && chosen[0] === hits[0].number)) {
        byName = `${session.app.name} numbers it E${ref?.[2] ? Number(ref[2]) : '?'}, but “${refName}” is E${hits[0].number} on TMDB.`
        chosen = [hits[0].number]
      }
    }
  }
  function setSeason(n: number) { season = n; chosen = []; loadSeason() }
  const suggested = $derived(ref && recent && picked?.tmdb === recent.ids.tmdb ? `The show you watched last on ${session.app.name}` : '')
  function toggle(n: number, e: MouseEvent) {
    // a click picks one episode; Ctrl/Shift-click adds more (watched several in one go)
    if (e.ctrlKey || e.metaKey || e.shiftKey) chosen = chosen.includes(n) ? chosen.filter(x => x !== n) : [...chosen, n].sort((a, b) => a - b)
    else chosen = [n]
  }

  const canSave = $derived(!!picked && !saving && (picked.kind === 'movie' || chosen.length > 0))
  const saveLabel = $derived(!picked ? 'Save' : picked.kind === 'movie' ? 'Save · movie'
    : chosen.length ? `Save · S${season} E${chosen.join(', E')}` : 'Pick the episode')
  const matchText = $derived(picked?.kind === 'tv' && hint?.subtitle ? hint.subtitle : hint?.title)

  async function save() {
    if (!picked || !canSave) return
    saving = true
    try {
      const s = await api.identify(session.id, picked.kind === 'movie'
        ? { kind: 'movie', tmdb: picked.tmdb, remember }
        : { kind: 'tv', tmdb: picked.tmdb, season, episodes: chosen, remember })
      replaceSession(s)
      toast(`Saved: ${picked.title}${picked.kind === 'tv' ? ` S${season} E${chosen.join(', E')}` : ''}`)
      close()
      loadNow(); loadHistory()
    } catch (e) {
      toast(`Not saved: ${(e as Error).message}`)
    } finally {
      saving = false
    }
  }
  async function ignore() { await setHidden(session, true); close() }
  function close() { app.identifying = undefined }

  $effect(() => { dialog.showModal() })
</script>

<dialog bind:this={dialog} aria-labelledby="dlg-h" onclose={close} onclick={(e) => e.target === dialog && close()}>
  <div class="box">
    <header>
      <div class="hl">
        <div class="meta">{session.app.name} on {session.device.name} · {time(session.started_at)} · {minutes(session.played_secs)}</div>
        <h1 id="dlg-h">{cur ? 'Change what this was' : 'What was this?'}</h1>
        {#if hint?.title}
          <div class="said">The app said: <b>“{hint.title}”</b>{#if hint.subtitle} · <b>{hint.subtitle}</b>{/if}</div>
        {/if}
      </div>
      <button class="x" aria-label="Close" onclick={close}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>
      </button>
    </header>

    <div class="search">
      <label>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="var(--accent)" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/></svg>
        <!-- svelte-ignore a11y_autofocus -->
        <input bind:value={q} aria-label="Title, IMDb id or link" placeholder="Title, IMDb id or a TMDB / IMDb link" autofocus />
        {#if searching}<span class="hintx">Searching…</span>{/if}
      </label>
    </div>

    <div class="cols">
      <div class="results" role="listbox" aria-label="Results">
        {#if searchError}<p class="err">{searchError}</p>{/if}
        {#if !searching && q.trim().length >= 2 && !results.length && !searchError}<p class="none">Nothing found. Try the original title, or paste an IMDb link.</p>{/if}
        {#each results.slice(0, 8) as r (r.kind + r.tmdb)}
          <button role="option" aria-selected={picked === r} class:on={picked === r} onclick={() => pick(r)}>
            <div class="th">{#if r.poster}<img src={r.poster} alt="" loading="lazy" />{/if}</div>
            <div class="rt">
              <div class="rn">{r.title}</div>
              <div class="rk">{r.kind === 'tv' ? 'Series' : 'Movie'}{r.year ? ` · ${r.year}` : ''}{r.original_title && r.original_title !== r.title ? ` · ${r.original_title}` : ''}</div>
              {#if r.overview}<div class="ro">{r.overview}</div>{/if}
            </div>
          </button>
        {/each}
      </div>

      <div class="pane">
        {#if picked?.kind === 'tv'}
          <div class="ptitle">Which episode</div>
          {#if suggested}<div class="tip">{suggested}; the app said season {Number(ref?.[1])}, episode {Number(ref?.[2])}.</div>{/if}
          {#if byName}<div class="tip gold">{byName}</div>{/if}
          {#if show}
            {#if show.seasons.length > 6}
              <select class="season-select" aria-label="Season" value={season} onchange={(e) => setSeason(Number(e.currentTarget.value))}>
                {#each show.seasons as s (s.number)}
                  <option value={s.number}>{s.number === 0 ? 'Specials' : `Season ${s.number}`} · {s.episodes} episodes</option>
                {/each}
              </select>
            {:else}
              <div class="seasons" role="group" aria-label="Season">
                {#each show.seasons as s (s.number)}
                  <button aria-pressed={season === s.number} class:on={season === s.number} onclick={() => setSeason(s.number)}>
                    {s.number === 0 ? 'Specials' : `Season ${s.number}`}
                  </button>
                {/each}
              </div>
            {/if}
            <div class="eps">
              {#each episodes as e (e.number)}
                <button aria-pressed={chosen.includes(e.number)} class:on={chosen.includes(e.number)} class:seen={e.watched}
                  title={e.name} onclick={(ev) => toggle(e.number, ev)}>
                  <span class="en">E{e.number}</span>
                  <span class="enm">{e.watched ? '✓ ' : ''}{e.name || 'no name yet'}</span>
                </button>
              {/each}
            </div>
            <div class="tip">Ctrl-click to add more episodes watched in one go. ✓ = already in your history.</div>
          {:else}
            <div class="tip">Loading the seasons…</div>
          {/if}
        {:else if picked}
          <div class="ptitle">Movie</div>
          <div class="movie">{picked.title}{picked.year ? ` (${picked.year})` : ''}</div>
          {#if picked.overview}<p class="ov">{picked.overview}</p>{/if}
        {:else if ref && !q.trim()}
          <div class="tip">{session.app.name} said season {Number(ref[1])}, episode {Number(ref[2])}, but not which show. Search the show: that episode will be picked for you.</div>
        {:else}
          <div class="tip">Pick a result on the left.</div>
        {/if}
        {#if picked && matchText}
          <label class="remember">
            <input type="checkbox" bind:checked={remember} />
            <span><b>Remember for next time</b><br />When {session.app.name} says “{matchText}”, it is this {picked.kind === 'tv' ? 'series' : 'movie'}.</span>
          </label>
        {/if}
      </div>
    </div>

    <footer>
      <div class="fn">
        {#if app.status?.sinks.length}Goes to {app.status.sinks.map(sinkName).join(' and ')} by ID. Correcting it later takes the wrong one back out.{/if}
      </div>
      <div class="fb">
        {#if !session.hidden}<button class="ghost" onclick={ignore}>Not a show · Hide</button>{/if}
        <button class="save" disabled={!canSave} onclick={save}>{saving ? 'Saving…' : saveLabel}</button>
      </div>
    </footer>
  </div>
</dialog>

<style>
  dialog { padding: 0; border: 0; background: transparent; color: var(--text); width: min(980px, calc(100vw - 32px));
    max-height: calc(100vh - 48px); }
  dialog::backdrop { background: rgba(4, 5, 7, 0.75); backdrop-filter: blur(4px); }
  .box { border-radius: 22px; background: var(--panel); border: 1px solid #2a2d35; box-shadow: 0 30px 80px rgba(0, 0, 0, 0.6);
    display: flex; flex-direction: column; max-height: calc(100vh - 48px); }
  header { display: flex; gap: 16px; padding: 22px 26px; border-bottom: 1px solid #23262e; }
  .hl { flex: 1; min-width: 0; }
  .meta { font-size: 13px; color: var(--muted); }
  h1 { margin: 4px 0 0; font-size: 24px; }
  .said { margin-top: 4px; font-size: 14px; color: var(--text-2); }
  .said b { color: var(--text); font-weight: 500; }
  .x { width: 44px; height: 44px; border-radius: 12px; border: 1px solid #2e323b; background: none; color: var(--text-2);
    display: flex; align-items: center; justify-content: center; flex: 0 0 auto; }
  .search { padding: 20px 26px 0; }
  .search label { display: flex; align-items: center; gap: 12px; min-height: 52px; padding: 0 16px; border-radius: 14px;
    background: #0e0f12; border: 1px solid var(--accent); }
  .search input { flex: 1; min-width: 0; background: transparent; border: 0; outline: none; color: var(--text); font: inherit; font-size: 16px; }
  .hintx { font-size: 12px; color: var(--muted); }
  .cols { display: flex; flex-wrap: wrap; gap: 24px; padding: 20px 26px; overflow-y: auto; }
  .results { flex: 1 1 360px; min-width: 0; display: flex; flex-direction: column; gap: 10px; }
  .results button { display: flex; align-items: flex-start; gap: 14px; padding: 12px; border-radius: 14px; border: 1px solid #23262e;
    background: #16181d; color: inherit; text-align: left; }
  .results button.on { background: #1b1810; border-color: var(--accent); }
  .th { flex: 0 0 46px; height: 69px; border-radius: 8px; overflow: hidden; background: #2a2d35; }
  .th img { width: 100%; height: 100%; object-fit: cover; }
  .rt { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 3px; }
  .rn { font-weight: 600; font-size: 15px; }
  .rk { font-size: 13px; color: var(--muted); }
  .ro { font-size: 12px; color: #7d838d; display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; }
  .err { color: #ff9aa0; font-size: 14px; }
  .none { color: var(--muted); font-size: 14px; }
  .pane { flex: 1 1 360px; min-width: 0; display: flex; flex-direction: column; gap: 14px; padding: 18px; border-radius: 16px;
    background: #0e0f12; border: 1px solid #23262e; align-self: flex-start; }
  .ptitle { font-size: 13px; font-weight: 700; letter-spacing: 0.08em; text-transform: uppercase; color: var(--muted); }
  .seasons { display: flex; flex-wrap: wrap; gap: 8px; }
  .seasons button { min-height: 38px; padding: 0 14px; border-radius: 10px; border: 1px solid #2a2d35; background: #16181d;
    color: var(--text); font-size: 14px; font-weight: 600; }
  .seasons button.on { background: var(--text); color: #0e0f12; border-color: var(--text); }
  .season-select { min-height: 42px; padding: 0 12px; border-radius: 10px; background: #16181d; border: 1px solid #2a2d35;
    color: var(--text); font: inherit; font-size: 14px; font-weight: 600; }
  .eps { display: grid; grid-template-columns: repeat(auto-fill, minmax(96px, 1fr)); gap: 8px; max-height: 300px; overflow-y: auto; }
  .eps button { min-height: 64px; padding: 6px; border-radius: 12px; border: 1px solid #2a2d35; background: #16181d; color: var(--text);
    display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 2px; }
  .eps button.seen { border-color: #23395a; }
  .eps button.on { background: var(--accent); color: #0e0f12; border-color: var(--accent); }
  .en { font-family: var(--display); font-size: 18px; font-weight: 700; }
  .enm { font-size: 11px; opacity: 0.8; max-width: 100%; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .tip { font-size: 13px; color: var(--muted); }
  .tip.gold { color: var(--accent); }
  .movie { font-size: 18px; font-weight: 600; }
  .ov { margin: 0; font-size: 13px; color: var(--text-2); }
  .remember { display: flex; gap: 12px; padding: 12px; border-radius: 12px; background: #16181d; cursor: pointer; font-size: 13px; color: var(--muted); }
  .remember b { color: var(--text); font-size: 14px; }
  .remember input { width: 20px; height: 20px; margin: 2px 0 0; accent-color: var(--accent); flex: 0 0 auto; }
  footer { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; padding: 18px 26px;
    border-top: 1px solid #23262e; }
  .fn { font-size: 13px; color: var(--muted); flex: 1 1 280px; }
  .fb { display: flex; gap: 10px; }
  .ghost { min-height: 44px; padding: 0 18px; border-radius: 12px; border: 1px solid #2e323b; background: none; color: var(--text-2);
    font-weight: 600; font-size: 14px; }
  .save { min-height: 44px; padding: 0 20px; border-radius: 12px; border: 0; background: var(--accent); color: #0e0f12;
    font-weight: 700; font-size: 14px; }
  .save:disabled { opacity: 0.45; cursor: default; }
  @media (max-width: 640px) {
    header, .search, .cols, footer { padding-left: 16px; padding-right: 16px; }
  }
</style>
