<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Session, type Status } from './lib/api'
  import { syncClock, clock } from './lib/clock.svelte'
  import { sinkName } from './lib/format'
  import Logo from './lib/Logo.svelte'
  import NowPlaying from './lib/NowPlaying.svelte'
  import Today from './lib/Today.svelte'

  let status = $state<Status>()
  let now = $state<Session[]>([])
  let today = $state<Session[]>([])
  let offline = $state(false)

  // The box over the LG over Plex, as the Discord status does: newest open
  // session that is identified first, then any.
  const playing = $derived(now.find(s => s.items.length) ?? now[0])
  const todays = $derived.by(() => {
    const start = new Date(clock.now); start.setHours(0, 0, 0, 0)
    return today.filter(s => s.hidden !== 'ignored' && Date.parse(s.started_at) >= start.getTime())
  })
  const deviceState = (id: number) => (now.some(s => s.device.id === id) ? 'Playing' : '')

  async function poll(fn: () => Promise<void>) {
    try { await fn(); offline = false } catch { offline = true }
  }
  const loadNow = () => poll(async () => { const r = await api.now(); syncClock(r.server_time); now = r.sessions })
  const loadToday = () => poll(async () => { today = (await api.history(1)).sessions })
  const loadStatus = () => poll(async () => { status = await api.status() })

  onMount(() => {
    loadStatus(); loadNow(); loadToday()
    const timers = [setInterval(loadNow, 5000), setInterval(loadToday, 30000), setInterval(loadStatus, 60000)]
    return () => timers.forEach(clearInterval)
  })
</script>

<div class="shell">
  <nav aria-label="Main">
    <div class="brand">
      <img src="/favicon.svg" alt="" width="34" height="34" />
      <span>Couchlog</span>
    </div>
    <a class="item active" href="/" aria-current="page">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 11l9-7 9 7v9a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z"/></svg>
      Home
    </a>

    {#if status}
      <div class="label">Devices</div>
      {#each status.devices as d (d.id)}
        <div class="dev">
          <span class="dot" class:on={deviceState(d.id)}></span>
          <span class="grow">{d.name}</span>
          <span class="state">{deviceState(d.id)}</span>
        </div>
      {/each}

      {#if status.sinks.length}
        <div class="label">Sends to</div>
        {#each status.sinks as k (k)}
          <div class="dev">
            <Logo slug={k} size={24} fallback={k} />
            <span class="grow">{sinkName(k)}</span>
            {#if status.read_only}<span class="state" title="Checks only while the old dashboard sends">check only</span>{/if}
          </div>
        {/each}
      {/if}
      <div class="ver">{status.version}</div>
    {/if}
  </nav>

  <main>
    {#if offline}<div class="offline" role="status">Can't reach Couchlog. Retrying…</div>{/if}
    <NowPlaying session={playing} sinks={status?.sinks ?? []} />
    <div class="content">
      <Today sessions={todays} />
    </div>
  </main>
</div>

<style>
  .shell { min-height: 100vh; display: flex; flex-wrap: wrap; }
  nav { flex: 1 1 220px; max-width: 250px; padding: 28px 18px; display: flex; flex-direction: column; gap: 6px;
    border-right: 1px solid #1e2027; }
  .brand { display: flex; align-items: center; gap: 10px; padding: 0 10px 24px; }
  .brand span { font-family: var(--display); font-weight: 700; font-size: 20px; letter-spacing: -0.02em; }
  .item { display: flex; align-items: center; gap: 12px; min-height: 44px; padding: 0 12px; border-radius: 10px;
    color: #b4b9c2; text-decoration: none; font-weight: 500; font-size: 15px; }
  .item.active { background: var(--nav-active); color: var(--text); font-weight: 600; }
  .label { margin-top: 24px; padding: 0 12px 4px; font-size: 12px; font-weight: 600; letter-spacing: 0.08em;
    text-transform: uppercase; color: var(--muted); }
  .dev { display: flex; align-items: center; gap: 10px; min-height: 38px; padding: 0 12px; font-size: 14px; color: var(--text-2); }
  .grow { flex: 1; }
  .dot { width: 8px; height: 8px; border-radius: 4px; background: #4b505a; }
  .dot.on { background: var(--ok); }
  .state { font-size: 12px; color: var(--muted); }
  .ver { margin-top: auto; padding: 24px 12px 0; font-size: 11px; color: #4b505a; }
  main { flex: 999 1 560px; min-width: 0; display: flex; flex-direction: column; }
  .content { padding: 36px 40px 56px; max-width: 1200px; }
  .offline { padding: 10px 40px; background: #3a1d1f; color: #ffb4b8; font-size: 14px; }
  @media (max-width: 640px) {
    nav { max-width: none; flex-basis: 100%; flex-direction: row; flex-wrap: wrap; align-items: center;
      padding: 12px 16px; border-right: 0; border-bottom: 1px solid #1e2027; }
    .brand { padding: 0 8px 0 0; }
    .label, .dev, .ver { display: none; }
    .content { padding: 24px 16px 40px; }
    .offline { padding: 10px 16px; }
  }
</style>
