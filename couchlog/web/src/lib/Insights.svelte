<script lang="ts">
  import { api, type Insights } from './api'
  import { serviceColor } from './colors'
  import { hrs, money, shortDate } from './format'
  import Logo from './Logo.svelte'
  import { go } from './store.svelte'

  let days = $state(30)
  let data = $state<Insights>()
  let error = $state('')

  $effect(() => {
    const d = days
    error = ''
    api.insights(d).then(r => { if (d === days) data = r }).catch(e => (error = (e as Error).message))
  })

  const periods: [number, string][] = [[7, 'Week'], [30, '30 days'], [365, 'Year']]
  const top = $derived(data ? Math.max(...data.services.map(s => s.hours), 0.1) : 1)
  const topWeek = $derived(data ? Math.max(...data.weeks.map(w => w.hours), 0.1) : 1)
  const mixTotal = $derived(data ? data.movies.hours + data.episodes.hours : 0)
  const paid = $derived(data?.services.filter(s => s.price) ?? [])
  const unpriced = $derived(data?.services.filter(s => !s.price && !s.free && s.hours > 0) ?? [])
  const barely = $derived([...paid].sort((a, b) => a.hours - b.hours)[0])
  const period = $derived(days === 7 ? '7 days' : days === 30 ? '30 days' : 'a year')
  const partialFirst = $derived(!!data?.first_seen && !!data.weeks.length && Date.parse(data.first_seen) > Date.parse(data.weeks[0].start))
  const freeLabel = { library: 'your own library', free: 'free', ads: 'free with ads' } as Record<string, string>
</script>

<div class="page">
  <header>
    <div>
      <h1>Insights</h1>
      <div class="sub">What you actually watch, and whether each service is worth it.</div>
    </div>
    <div class="seg" role="group" aria-label="Period">
      {#each periods as [d, label] (d)}
        <button aria-pressed={days === d} class:on={days === d} onclick={() => (days = d)}>{label}</button>
      {/each}
    </div>
  </header>

  {#if error}<p class="err">{error}</p>{/if}
  {#if data}
    <section class="totals" aria-label="Totals">
      <div class="k"><div class="kl">Watched</div><div class="kv">{hrs(data.hours)}</div><div class="ks">{data.sessions} sessions</div></div>
      <div class="k"><div class="kl">Movies</div><div class="kv">{data.movies.n}</div><div class="ks">{hrs(data.movies.hours)}</div></div>
      <div class="k"><div class="kl">Episodes</div><div class="kv">{data.episodes.n}</div><div class="ks">{hrs(data.episodes.hours)}</div></div>
      <div class="k"><div class="kl">Sent everywhere</div><div class="kv">{data.sent}</div><div class="ks">by ID, nothing twice</div></div>
    </section>

    <div class="row">
      <section class="card wide" aria-labelledby="svc-h">
        <div class="ch"><h2 id="svc-h">Your services</h2><span>Hours watched · cost per hour</span></div>
        {#each data.services as s (s.key)}
          <div class="svc">
            <div class="sn"><Logo slug={s.key} size={26} fallback={s.name} />{s.name}</div>
            <div class="track"><div style:width="{(s.hours / top) * 100}%" style:background={serviceColor(s.key)}></div></div>
            <div class="sh">{hrs(s.hours)}</div>
            <div class="sc" class:hot={s === barely && paid.length > 1}>
              {#if s.per_hour !== undefined && s.price}{money(s.per_hour, s.price.currency)} / h
              {:else if s.price}not watched
              {:else if s.free}{freeLabel[s.free]}
              {:else}<button class="link" onclick={() => go('services')}>set a price</button>{/if}
            </div>
          </div>
        {/each}
        {#if barely?.price && barely.per_hour !== undefined}
          <div class="callout">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="var(--accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 9v4M12 17h.01"/><path d="M10.3 3.9L1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/></svg>
            <div>{barely.name}: {hrs(barely.hours)} in {period}. At {money(barely.price.monthly, barely.price.currency)} a month, each hour cost you <b>{money(barely.per_hour, barely.price.currency)}</b>.</div>
          </div>
        {:else if unpriced.length}
          <div class="callout">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="var(--accent)" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>
            <div>Add what you pay for {unpriced.map(s => s.name).join(' and ')} to see what each hour really costs.</div>
            <button onclick={() => go('services')}>Set prices</button>
          </div>
        {/if}
      </section>

      <section class="card" aria-labelledby="wk-h">
        <h2 id="wk-h">Week by week</h2>
        <div class="weeks">
          {#each data.weeks.slice(-8) as w, i (w.start)}
            <div class="wk">
              <div class="wh">{hrs(w.hours)}</div>
              <div class="wb" style:height="{Math.max(3, (w.hours / topWeek) * 100)}%" class:last={i === data.weeks.slice(-8).length - 1}></div>
              <div class="wl">{shortDate(w.start)}</div>
            </div>
          {/each}
        </div>
        {#if partialFirst}<div class="tip">History starts {shortDate(data.first_seen!)}, so the first week is partial.</div>{/if}
      </section>
    </div>

    <div class="row">
      <section class="card" aria-labelledby="mix-h">
        <h2 id="mix-h">Movies or series</h2>
        {#if mixTotal > 0}
          <div class="mix">
            <div style:width="{(data.movies.hours / mixTotal) * 100}%" class="mv"></div>
            <div style:width="{(data.episodes.hours / mixTotal) * 100}%" class="ep"></div>
          </div>
          <div class="mixl">
            <span><b class="mvt">Movies</b> · {data.movies.n} · {hrs(data.movies.hours)}</span>
            <span><b class="ept">Episodes</b> · {data.episodes.n} · {hrs(data.episodes.hours)}</span>
          </div>
        {:else}<div class="tip">Nothing identified in this period.</div>{/if}
      </section>

      <section class="card wide" aria-labelledby="top-h">
        <h2 id="top-h">Series you lived in</h2>
        {#each data.top_shows as t, i (t.tmdb)}
          <div class="show">
            <span class="rank">{i + 1}</span>
            <div class="sp">{#if t.poster}<img src={t.poster} alt="" loading="lazy" />{/if}</div>
            <div class="st"><div class="stt">{t.title}</div><div class="ste">{t.episodes} episode{t.episodes === 1 ? '' : 's'}</div></div>
            <div class="sh">{hrs(t.hours)}</div>
          </div>
        {:else}<div class="tip">No series in this period.</div>{/each}
      </section>
    </div>
  {/if}
</div>

<style>
  .page { padding: 32px 40px 56px; max-width: 1180px; display: flex; flex-direction: column; gap: 24px; }
  header { display: flex; flex-wrap: wrap; align-items: flex-end; justify-content: space-between; gap: 16px; }
  h1 { margin: 0; font-size: 34px; }
  .sub { margin-top: 6px; font-size: 14px; color: var(--muted); }
  .seg { display: flex; gap: 6px; padding: 4px; border-radius: 12px; background: #16181d; border: 1px solid #23262e; }
  .seg button { min-height: 40px; padding: 0 14px; border-radius: 9px; border: 0; background: none; color: var(--text-2); font-size: 14px; font-weight: 600; }
  .seg button.on { background: var(--text); color: #0e0f12; }
  .err { color: #ff9aa0; }
  .totals { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 16px; }
  .k { padding: 20px; border-radius: 16px; background: #16181d; border: 1px solid #23262e; display: flex; flex-direction: column; gap: 6px; }
  .kl { font-size: 13px; color: var(--muted); }
  .kv { font-family: var(--display); font-size: 34px; font-weight: 700; letter-spacing: -0.02em; }
  .ks { font-size: 13px; color: var(--text-2); }
  .row { display: flex; flex-wrap: wrap; gap: 24px; }
  .card { flex: 1 1 300px; min-width: 0; padding: 24px; border-radius: 18px; background: #16181d; border: 1px solid #23262e; display: flex; flex-direction: column; gap: 16px; }
  .card.wide { flex: 999 1 460px; }
  h2 { margin: 0; font-size: 20px; }
  .ch { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
  .ch span { font-size: 13px; color: var(--muted); }
  .svc { display: grid; grid-template-columns: 150px minmax(0, 1fr) 70px 130px; align-items: center; gap: 14px; }
  .sn { display: flex; align-items: center; gap: 10px; font-weight: 600; font-size: 15px; min-width: 0; }
  .track { height: 10px; border-radius: 5px; background: #23262e; }
  .track div { height: 100%; border-radius: 5px; }
  .sh { font-size: 14px; color: var(--text-2); text-align: right; }
  .sc { font-size: 14px; color: var(--muted); text-align: right; }
  .sc.hot { color: var(--accent); font-weight: 600; }
  .link { background: none; border: 0; color: var(--accent); padding: 0; font-size: 14px; }
  .callout { padding: 16px; border-radius: 12px; background: #1b1810; border: 1px solid #3a3018; display: flex; flex-wrap: wrap; align-items: center; gap: 12px; }
  .callout div { flex: 1 1 240px; font-size: 14px; color: #e6dcc2; }
  .callout b { color: var(--accent); }
  .callout button { min-height: 40px; padding: 0 14px; border-radius: 10px; border: 0; background: var(--accent); color: #0e0f12; font-size: 13px; font-weight: 700; }
  .weeks { display: flex; align-items: flex-end; gap: 12px; height: 190px; padding-top: 10px; }
  .wk { flex: 1; display: flex; flex-direction: column; align-items: center; gap: 8px; height: 100%; justify-content: flex-end; min-width: 0; }
  .wh { font-size: 12px; font-weight: 600; color: var(--text-2); white-space: nowrap; }
  .wb { width: 100%; border-radius: 8px 8px 3px 3px; background: #3a3f4a; }
  .wb.last { background: var(--accent); }
  .wl { font-size: 11px; color: var(--muted); white-space: nowrap; }
  .tip { font-size: 13px; color: var(--muted); }
  .mix { display: flex; height: 14px; border-radius: 7px; overflow: hidden; gap: 3px; }
  .mv { background: var(--accent); } .ep { background: #6ea8fe; }
  .mixl { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 8px; font-size: 14px; }
  .mvt { color: var(--accent); } .ept { color: var(--sent-fg); }
  .show { display: flex; align-items: center; gap: 14px; }
  .rank { width: 22px; font-family: var(--display); font-weight: 700; color: var(--muted); }
  .sp { flex: 0 0 36px; height: 54px; border-radius: 6px; overflow: hidden; background: #2a2d35; }
  .sp img { width: 100%; height: 100%; object-fit: cover; }
  .st { flex: 1; min-width: 0; }
  .stt { font-weight: 600; font-size: 15px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .ste { font-size: 13px; color: var(--muted); }
  @media (max-width: 760px) {
    .page { padding: 24px 16px 40px; }
    .totals { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .svc { grid-template-columns: minmax(0, 1fr) 60px 100px; }
    .svc .track { display: none; }
  }
</style>
