<script lang="ts">
  import { onMount } from 'svelte'
  import type { Session } from './api'
  import { identify, setHidden } from './actions'
  import { clock } from './clock.svelte'
  import { current, dayLabel, deliveryWord, episodeLine, minutes, sinkName, time, titleOf } from './format'
  import { slugFor } from './logos'
  import Logo from './Logo.svelte'
  import { app, toIdentify } from './store.svelte'

  type Tab = 'all' | 'identify' | 'unsent' | 'sent' | 'hidden'
  let tab = $state<Tab>((sessionStorage.getItem('history-tab') as Tab) || 'all')
  let q = $state('')
  let menu = $state<number>()

  onMount(() => {
    sessionStorage.removeItem('history-tab')
    const pick = () => { tab = (sessionStorage.getItem('history-tab') as Tab) || tab; sessionStorage.removeItem('history-tab') }
    const closeMenu = () => (menu = undefined)
    addEventListener('history-tab', pick)
    addEventListener('click', closeMenu)
    return () => { removeEventListener('history-tab', pick); removeEventListener('click', closeMenu) }
  })

  const sent = (s: Session) => s.items.length > 0 && s.items.every(it => {
    const d = Object.values(it.deliveries)
    return d.length > 0 && d.every(x => x.status === 'added' || x.status === 'skipped')
  })
  const groups: Record<Tab, (s: Session) => boolean> = {
    all: s => !s.hidden,
    identify: s => toIdentify([s]).length > 0,
    unsent: s => !s.hidden && !s.open && s.items.length > 0 && !sent(s),
    sent: s => !s.hidden && sent(s),
    hidden: s => !!s.hidden,
  }
  const labels: [Tab, string][] = [['all', 'All'], ['identify', 'To identify'], ['unsent', 'Not sent'], ['sent', 'Sent'], ['hidden', 'Hidden']]
  const counts = $derived(Object.fromEntries(labels.map(([t]) => [t, app.history.filter(groups[t]).length])) as Record<Tab, number>)

  const shown = $derived.by(() => {
    const needle = q.trim().toLowerCase()
    return app.history.filter(groups[tab]).filter(s => !needle ||
      [titleOf(s), s.app.name, s.hint?.title, s.hint?.subtitle, ...s.items.map(i => i.episode_title)]
        .some(x => x?.toLowerCase().includes(needle)))
  })
  const days = $derived.by(() => {
    const out: { label: string; rows: Session[] }[] = []
    for (const s of shown) {
      const label = dayLabel(Date.parse(s.started_at), clock.now)
      if (out.at(-1)?.label !== label) out.push({ label, rows: [] })
      out.at(-1)!.rows.push(s)
    }
    return out
  })
  const devices = $derived(app.status?.devices.map(d => d.name).join(' · ') ?? '')

  function note(s: Session): { text: string; tone: 'info' | 'warn' | 'muted' } | undefined {
    if (s.hidden === 'short') return { text: 'Hidden: shorter than this app counts', tone: 'muted' }
    if (s.hidden === 'ignored') return { text: 'Hidden by you', tone: 'muted' }
    if (!s.items.length && s.hint?.title) return { text: `The app said “${s.hint.title}”${s.hint.subtitle ? ` · ${s.hint.subtitle}` : ''}`, tone: 'warn' }
    if (!s.items.length) return { text: 'The app did not say what it was', tone: 'warn' }
    // why it was not sent, once per distinct reason ("Watched 46%…" is the same for Trakt and Simkl)
    const why = new Map<string, string[]>()
    for (const [k, d] of s.items.flatMap(it => Object.entries(it.deliveries))) {
      if (!d.error || d.status === 'added' || d.status === 'skipped') continue
      why.set(d.error, [...(why.get(d.error) ?? []), sinkName(k)])
    }
    if (why.size === 1) return { text: [...why.keys()][0], tone: 'muted' }
    if (why.size > 1) return { text: [...why].map(([e, ks]) => `${ks.join(', ')}: ${e}`).join(' · '), tone: 'muted' }
    if (s.items.length > 1) return { text: `${s.items.length} episodes in one sitting`, tone: 'info' }
  }
  function chips(s: Session) {
    const q = s.quality
    if (!q) return []
    return [q.resolution && `${q.resolution}${q.uhd ? ' UHD' : ''}`, q.hdr, q.codec].filter(Boolean) as string[]
  }
  function delivery(s: Session) {
    const sinks = app.status?.sinks ?? []
    return sinks.map(k => {
      const st = s.items.map(it => it.deliveries[k]?.status).filter(Boolean)
      const worst = st.find(x => x !== 'added' && x !== 'skipped') ?? st[0]
      return { k, word: s.open ? '…' : worst ? deliveryWord(worst) : '—', ok: !!worst && (worst === 'added' || worst === 'skipped'), bad: worst === 'error' || worst === 'not_found' }
    })
  }
</script>

<div class="page">
  <header>
    <div>
      <h1>History</h1>
      <div class="sub">Everything watched on every TV, box and Plex. {app.history.filter(s => !s.hidden).length} sessions in 30 days.</div>
    </div>
    <label class="search">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/></svg>
      <input bind:value={q} placeholder="Search your history…" aria-label="Search your history" />
    </label>
  </header>

  <div class="tabs" role="group" aria-label="Show">
    {#each labels as [t, label] (t)}
      <button aria-pressed={tab === t} class:on={tab === t} onclick={() => (tab = t)}>{label} <span>{counts[t]}</span></button>
    {/each}
    <span class="grow"></span>
    <span class="devs">{devices}</span>
  </div>

  {#each days as day (day.label)}
    <section>
      <h2>{day.label}</h2>
      {#each day.rows as s (s.id)}
        {@const it = current(s)}
        {@const n = note(s)}
        <article class:dim={!!s.hidden}>
          <div class="thumb">{#if it?.poster}<img src={it.poster} alt="" loading="lazy" />{:else}<Logo slug={slugFor(s.app.id, s.app.name)} size={30} fallback={s.app.name} />{/if}</div>
          <div class="text">
            <div class="title">{titleOf(s)}</div>
            <div class="line">
              {#if it}{s.items.length > 1 ? s.items.map(i => `E${i.episode}`).join(' · ') : episodeLine(it) || 'Movie'}{:else}Not identified{/if}
            </div>
            <div class="meta">
              <Logo slug={slugFor(s.app.id, s.app.name)} size={16} fallback={s.app.name} />
              {s.app.name} on {s.device.name} · {time(s.started_at)} · {s.open ? 'playing' : minutes(s.played_secs)}
            </div>
            {#if n}<div class="note {n.tone}">{n.text}</div>{/if}
          </div>
          <div class="right">
            <div class="fmt">{#each chips(s) as c (c)}<span>{c}</span>{/each}</div>
            {#if s.items.length}
              <div class="del">
                {#each delivery(s) as d (d.k)}
                  <span class:ok={d.ok} class:bad={d.bad}>{sinkName(d.k)} {d.word}</span>
                {/each}
              </div>
            {/if}
          </div>
          <div class="more">
            <button aria-label="More actions" aria-expanded={menu === s.id}
              onclick={(e) => { e.stopPropagation(); menu = menu === s.id ? undefined : s.id }}>
              <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.8"/><circle cx="12" cy="12" r="1.8"/><circle cx="19" cy="12" r="1.8"/></svg>
            </button>
            {#if menu === s.id}
              <div class="menu" role="menu">
                {#if app.status?.identify}<button role="menuitem" onclick={() => identify(s)}>{it ? 'Change what it was' : 'Identify'}</button>{/if}
                {#if it}{#each it.links as l (l.site)}<a role="menuitem" href={l.url} target="_blank" rel="noopener noreferrer">Open on {l.name}</a>{/each}{/if}
                {#if s.hidden}
                  <button role="menuitem" onclick={() => setHidden(s, false)}>Show in history</button>
                {:else}
                  <button role="menuitem" onclick={() => setHidden(s, true)}>Hide</button>
                {/if}
              </div>
            {/if}
          </div>
        </article>
      {/each}
    </section>
  {:else}
    <p class="empty">{q ? 'Nothing matches.' : 'Nothing here.'}</p>
  {/each}
</div>

<style>
  .page { padding: 32px 40px 56px; max-width: 1200px; width: 100%; margin: 0 auto; box-sizing: border-box; display: flex; flex-direction: column; gap: 20px; }
  header { display: flex; flex-wrap: wrap; align-items: flex-end; justify-content: space-between; gap: 16px; }
  h1 { margin: 0; font-size: 34px; }
  .sub { margin-top: 6px; font-size: 14px; color: var(--muted); }
  .search { display: flex; align-items: center; gap: 10px; min-height: 44px; padding: 0 14px; border-radius: 12px; background: var(--panel);
    border: 1px solid #2e323b; color: var(--muted); flex: 0 1 320px; }
  .search input { flex: 1; min-width: 0; background: none; border: 0; outline: none; color: var(--text); font: inherit; font-size: 14px; }
  .tabs { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; }
  .tabs button { min-height: 40px; padding: 0 16px; border-radius: 999px; border: 1px solid #2e323b; background: transparent;
    color: var(--text-2); font-size: 14px; font-weight: 600; }
  .tabs button span { opacity: 0.65; }
  .tabs button.on { background: var(--text); color: #0e0f12; border-color: var(--text); }
  .grow { flex: 1; }
  .devs { font-size: 13px; color: var(--muted); }
  section { display: flex; flex-direction: column; gap: 10px; }
  h2 { margin: 10px 0 0; font-family: var(--font); font-size: 13px; font-weight: 700; letter-spacing: 0.08em; text-transform: uppercase; color: var(--muted); }
  article { display: flex; flex-wrap: wrap; align-items: center; gap: 18px; padding: 14px 18px; border-radius: 16px; background: #16181d; border: 1px solid #23262e; }
  article.dim { opacity: 0.6; }
  .thumb { flex: 0 0 48px; height: 72px; border-radius: 8px; overflow: hidden; background: #22252c; display: flex; align-items: center; justify-content: center; }
  .thumb img { width: 100%; height: 100%; object-fit: cover; }
  .text { flex: 1 1 240px; min-width: 0; display: flex; flex-direction: column; gap: 3px; }
  .title { font-weight: 600; font-size: 16px; }
  .line { font-size: 14px; color: var(--text-2); }
  .meta { display: flex; align-items: center; gap: 7px; font-size: 13px; color: var(--muted); flex-wrap: wrap; }
  .note { margin-top: 3px; font-size: 13px; }
  .note.info { color: var(--sent-fg); }
  .note.warn { color: var(--accent); }
  .note.muted { color: #9a937f; }
  .right { display: flex; flex-direction: column; align-items: flex-end; gap: 8px; }
  .fmt { display: flex; flex-wrap: wrap; gap: 6px; }
  .fmt span { padding: 4px 8px; border-radius: 6px; background: #1e2128; font-size: 12px; font-weight: 600; color: var(--text-2); }
  .del { display: flex; gap: 6px; }
  .del span { padding: 5px 10px; border-radius: 999px; font-size: 12px; font-weight: 600; background: #1e2128; color: var(--muted); white-space: nowrap; }
  .del span.ok { background: #16243a; color: var(--sent-fg); }
  .del span.bad { background: #2a2312; color: var(--accent); }
  .more { position: relative; }
  .more > button { width: 44px; height: 44px; border-radius: 10px; border: 1px solid #2e323b; background: none; color: var(--text-2);
    display: flex; align-items: center; justify-content: center; }
  .menu { position: absolute; right: 0; top: 50px; z-index: 10; min-width: 200px; padding: 6px; border-radius: 12px; background: #1b1d23;
    border: 1px solid #2e323b; box-shadow: 0 16px 40px rgba(0, 0, 0, 0.5); display: flex; flex-direction: column; }
  .menu button, .menu a { min-height: 40px; padding: 0 12px; border-radius: 8px; border: 0; background: none; color: var(--text);
    text-align: left; font-size: 14px; text-decoration: none; display: flex; align-items: center; }
  .menu button:hover, .menu a:hover { background: #262932; }
  .empty { color: var(--muted); }
  @media (max-width: 760px) { .page { padding: 24px 16px 40px; } article { gap: 12px; flex-wrap: wrap; } .right { align-items: flex-start; } }
</style>
