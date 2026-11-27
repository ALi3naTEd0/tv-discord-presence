<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type ServiceRow, type Services } from './api'
  import { hrs, money } from './format'
  import Logo from './Logo.svelte'
  import { toast } from './store.svelte'

  const CURRENCIES = [['MXN', 'Mexican peso'], ['USD', 'US dollar'], ['EUR', 'Euro'], ['COP', 'Colombian peso'],
    ['ARS', 'Argentine peso'], ['CLP', 'Chilean peso'], ['BRL', 'Brazilian real'], ['GBP', 'Pound sterling'], ['CAD', 'Canadian dollar']]

  let data = $state<Services>()
  let drafts = $state<Record<string, { price: string; currency: string; period: string; open?: boolean }>>({})
  let adding = $state('')

  onMount(load)
  async function load() {
    try { apply(await api.services()) } catch (e) { toast((e as Error).message) }
  }
  function apply(d: Services) {
    data = d
    for (const s of d.services) {
      drafts[s.key] = { price: s.price ? String(s.price.amount) : drafts[s.key]?.price ?? '', currency: s.price?.currency ?? d.currency,
        period: s.price?.period ?? 'month', open: drafts[s.key]?.open }
    }
  }
  async function save(s: ServiceRow) {
    const d = drafts[s.key]
    const price = d.price.trim() === '' ? 0 : Number(d.price.replace(',', '.'))
    if (!Number.isFinite(price) || price < 0) { toast('Write the price as a number, like 299 or 9.99'); return }
    if ((s.price?.amount ?? 0) === price && (s.price?.currency ?? d.currency) === d.currency && (s.price?.period ?? 'month') === d.period) return
    try {
      apply(await api.setPrice(s.key, price, d.currency, d.period))
      toast(price ? `Saved: ${s.name} ${money(price, d.currency)} a ${d.period}` : `${s.name}: price removed`)
    } catch (e) { toast(`Not saved: ${(e as Error).message}`) }
  }
  async function setCurrency(c: string) {
    if (!data) return
    try { await api.setCurrency(c); data.currency = c; for (const k in drafts) if (!data.services.find(s => s.key === k)?.price) drafts[k].currency = c }
    catch (e) { toast((e as Error).message) }
  }
  function add() {
    const name = adding.trim()
    if (!name || !data) return
    const key = name.toLowerCase().replace(/[^a-z0-9]+/g, '')
    if (!key || data.services.some(s => s.key === key)) { adding = ''; return }
    data.services = [...data.services, { key, name, hours: 0, devices: [] }]
    drafts[key] = { price: '', currency: data.currency, period: 'month', open: true }
    adding = ''
  }

  const paid = $derived(data?.services.filter(s => s.price) ?? [])
  const totals = $derived.by(() => {
    const by: Record<string, number> = {}
    for (const s of paid) by[s.price!.currency] = (by[s.price!.currency] ?? 0) + s.price!.monthly
    return Object.entries(by)
  })
  const avg = $derived.by(() => {
    if (totals.length !== 1) return undefined
    const h = paid.reduce((n, s) => n + s.hours, 0)
    return h > 0 ? money((totals[0][1] * 30 / 30.4) / h, totals[0][0]) : undefined
  })
  const barely = $derived([...paid].sort((a, b) => a.hours - b.hours)[0])
  const freeLabel = { library: 'Your library', free: 'Free', ads: 'Free with ads' } as Record<string, string>
</script>

<div class="page">
  <header>
    <div>
      <h1>Your services</h1>
      <div class="sub">What you pay, so Couchlog can tell you what each hour really costs. It stays on this computer.</div>
    </div>
    {#if data}
      <label class="cur">New prices in
        <select value={data.currency} onchange={(e) => setCurrency(e.currentTarget.value)} aria-label="Currency for new prices">
          {#each CURRENCIES as [c, n] (c)}<option value={c}>{c} · {n}</option>{/each}
        </select>
      </label>
    {/if}
  </header>

  {#if data}
    <section class="sum" aria-label="Summary">
      <div class="k">
        <div class="kl">You pay a month</div>
        <div class="kv">{totals.length ? totals.map(([c, v]) => money(v, c)).join(' + ') : '—'}</div>
        <div class="ks">{paid.length} paid service{paid.length === 1 ? '' : 's'}</div>
      </div>
      <div class="k">
        <div class="kl">Per hour watched</div>
        <div class="kv">{avg ?? '—'}</div>
        <div class="ks">{totals.length > 1 ? 'prices in different currencies' : 'on paid services, last 30 days'}</div>
      </div>
      <div class="k warn">
        <div class="kl">Least used</div>
        <div class="kv gold">{barely?.name ?? '—'}</div>
        <div class="ks">{barely ? `${hrs(barely.hours)} in 30 days` : 'set your prices to see it'}</div>
      </div>
    </section>

    <section class="table" aria-labelledby="subs-h">
      <div class="th">
        <span id="subs-h">Service</span><span>Price</span><span>Currency</span><span>Every</span><span>Watched</span><span>Per hour</span>
      </div>
      {#each data.services as s (s.key)}
        {@const d = drafts[s.key]}
        <div class="tr">
          <div class="svc">
            <Logo slug={s.key} size={40} fallback={s.name} />
            <div><div class="sn">{s.name}</div><div class="sd">{s.devices.length ? `On ${s.devices.join(', ')}` : 'Added by you'}</div></div>
          </div>
          {#if s.free && !s.price && !d?.open}
            <span class="free">{freeLabel[s.free]}{#if s.free !== 'library'} · <button class="link" onclick={() => (d.open = true)}>I pay for it</button>{/if}</span>
            <span></span><span></span>
          {:else if d}
            <input aria-label="{s.name} price" placeholder="0.00" inputmode="decimal" bind:value={d.price} onchange={() => save(s)} />
            <select aria-label="{s.name} currency" bind:value={d.currency} onchange={() => d.price && save(s)}>
              {#each CURRENCIES as [c] (c)}<option value={c}>{c}</option>{/each}
            </select>
            <select aria-label="{s.name} billing" bind:value={d.period} onchange={() => d.price && save(s)}>
              <option value="month">Month</option><option value="year">Year</option>
            </select>
          {/if}
          <span class="h">{hrs(s.hours)}</span>
          <span class="per" class:muted={s.per_hour === undefined}>
            {#if s.per_hour !== undefined && s.price}{money(s.per_hour, s.price.currency)}{:else if s.price}not watched{:else if s.free && !d?.open}—{:else}set a price{/if}
          </span>
        </div>
      {/each}
      <div class="foot">
        <form onsubmit={(e) => { e.preventDefault(); add() }}>
          <input bind:value={adding} placeholder="Another service you pay for" aria-label="Service name" />
          <button type="submit" disabled={!adding.trim()}>+ Add</button>
        </form>
        <span>Services show up here by themselves when Couchlog sees them on your TV.</span>
      </div>
    </section>
  {/if}
</div>

<style>
  .page { padding: 32px 40px 56px; max-width: 1200px; width: 100%; margin: 0 auto; box-sizing: border-box; display: flex; flex-direction: column; gap: 24px; }
  header { display: flex; flex-wrap: wrap; align-items: flex-end; justify-content: space-between; gap: 16px; }
  h1 { margin: 0; font-size: 34px; }
  .sub { margin-top: 6px; font-size: 14px; color: var(--muted); }
  .cur { display: flex; align-items: center; gap: 10px; font-size: 14px; color: var(--text-2); }
  select, input { min-height: 44px; padding: 0 12px; border-radius: 10px; background: #0e0f12; border: 1px solid #2e323b;
    color: var(--text); font: inherit; font-size: 14px; width: 100%; min-width: 0; }
  .cur select { width: auto; background: #16181d; }
  .sum { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 16px; }
  .k { padding: 20px; border-radius: 16px; background: #16181d; border: 1px solid #23262e; }
  .k.warn { background: #1b1810; border-color: #3a3018; }
  .kl { font-size: 13px; color: var(--muted); }
  .kv { margin-top: 6px; font-family: var(--display); font-size: 30px; font-weight: 700; }
  .kv.gold { color: var(--accent); }
  .ks { margin-top: 4px; font-size: 13px; color: var(--text-2); }
  .table { container-type: inline-size; border-radius: 18px; background: #16181d; border: 1px solid #23262e; overflow: hidden; }
  .th, .tr { display: grid; grid-template-columns: minmax(0, 1.5fr) 130px 100px 110px 80px minmax(0, 0.8fr); gap: 14px; align-items: center; padding: 14px 20px; }
  .th { font-size: 12px; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; color: var(--muted); border-bottom: 1px solid #23262e; }
  .tr { border-bottom: 1px solid #1f2229; }
  .svc { display: flex; align-items: center; gap: 12px; min-width: 0; }
  .sn { font-weight: 600; font-size: 15px; }
  .sd { font-size: 12px; color: var(--muted); }
  .free { grid-column: span 1; font-size: 14px; color: var(--muted); white-space: nowrap; }
  .link { background: none; border: 0; padding: 0; color: var(--accent); font-size: 13px; }
  .h { font-size: 14px; color: var(--text-2); }
  .per { font-size: 15px; font-weight: 600; }
  .per.muted { color: var(--muted); font-weight: 500; font-size: 14px; }
  .foot { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; padding: 16px 20px; }
  .foot form { display: flex; gap: 8px; flex: 0 1 420px; }
  .foot button { min-height: 44px; padding: 0 16px; border-radius: 12px; border: 1px dashed #3a3f4a; background: transparent; color: var(--text-2);
    font-weight: 600; font-size: 14px; white-space: nowrap; }
  .foot span { font-size: 13px; color: var(--muted); }
  @media (max-width: 860px) {
    .page { padding: 24px 16px 40px; }
    .sum { grid-template-columns: 1fr; }
  }
  @container (max-width: 760px) {
    .th { display: none; }
    .tr { grid-template-columns: 1fr 1fr 1fr; }
    .svc { grid-column: 1 / -1; }
    .h::before { content: 'Watched '; color: var(--muted); font-size: 12px; }
  }
</style>
