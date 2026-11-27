<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Insights, type UpNext as Next } from './api'
  import { identify, setHidden } from './actions'
  import { clock } from './clock.svelte'
  import { serviceColor } from './colors'
  import { hrs, minutes, time, titleOf } from './format'
  import Logo from './Logo.svelte'
  import NowPlaying from './NowPlaying.svelte'
  import Today from './Today.svelte'
  import UpNext from './UpNext.svelte'
  import { app, go, sendProblems, toIdentify } from './store.svelte'

  let next = $state<Next[]>([])
  let month = $state<Insights>()

  // The box over the LG over Plex, as the Discord status does: newest open
  // session that is identified first, then any.
  const playing = $derived(app.now.find(s => s.items.length) ?? app.now[0])
  const todays = $derived.by(() => {
    const start = new Date(clock.now); start.setHours(0, 0, 0, 0)
    return app.history.filter(s => s.hidden !== 'ignored' && Date.parse(s.started_at) >= start.getTime())
  })
  const needs = $derived([...toIdentify(app.history), ...sendProblems(app.history)].slice(0, 4))
  const top = $derived(month ? Math.max(...month.services.map(s => s.hours), 1) : 1)

  onMount(() => {
    const load = async () => {
      try { [next, month] = await Promise.all([api.upNext(), api.insights(30)]) } catch { /* the banner says offline */ }
    }
    load()
    const t = setInterval(load, 5 * 60 * 1000)
    return () => clearInterval(t)
  })
</script>

<NowPlaying session={playing} sinks={app.status?.sinks ?? []} />
<div class="content">
  {#if app.status && !app.status.devices.some(d => d.kind !== 'plex')}
    <a class="setup" href="#/setup">
      <b>Connect your first TV</b>
      <span>Couchlog finds the TVs and boxes on your network; you accept on the TV and it starts recording.</span>
    </a>
  {/if}
  <UpNext items={next} />
  <div class="cols">
    <div class="main"><Today sessions={todays} /></div>
    <aside>
      {#if needs.length}
        <section class="needs" aria-labelledby="needs-h">
          <h2 id="needs-h">Needs a look</h2>
          {#each needs as s (s.id)}
            <div class="q">
              <div class="qt">{titleOf(s)}</div>
              <div class="why">
                {#if !s.items.length}
                  {s.app.name} · {time(s.started_at)} · {minutes(s.played_secs)} · not identified
                {:else}
                  Could not be sent: {Object.entries(s.items[0].deliveries).filter(([, d]) => d.status === 'error' || d.status === 'not_found').map(([k, d]) => `${k} ${d.status === 'not_found' ? 'does not list it yet' : 'failed'}`).join(', ')}
                {/if}
              </div>
              <div class="btns">
                <button class="yes" onclick={() => identify(s)}>{s.items.length ? 'Change' : 'Identify'}</button>
                <button class="no" onclick={() => setHidden(s, true)}>Hide</button>
              </div>
            </div>
          {/each}
        </section>
      {/if}
      {#if month}
        <section class="card" aria-labelledby="month-h">
          <div class="ch">
            <h2 id="month-h">Last 30 days</h2>
            <a href="#/insights" onclick={(e) => { e.preventDefault(); go('insights') }}>Insights →</a>
          </div>
          <div class="big">{hrs(month.hours)}</div>
          {#each month.services.filter(s => s.hours > 0).slice(0, 5) as s (s.key)}
            <div class="svc">
              <Logo slug={s.key} size={24} fallback={s.name} />
              <div class="sb">
                <div class="sl"><span>{s.name}</span><span class="m">{hrs(s.hours)}</span></div>
                <div class="track"><div style:width="{(s.hours / top) * 100}%" style:background={serviceColor(s.key)}></div></div>
              </div>
            </div>
          {/each}
        </section>
      {/if}
    </aside>
  </div>
</div>

<style>
  .content { padding: 36px 40px 56px; max-width: 1200px; width: 100%; margin: 0 auto; box-sizing: border-box; display: flex; flex-direction: column; gap: 36px; }
  .setup { display: flex; flex-direction: column; gap: 4px; padding: 20px 22px; border-radius: 16px; background: #1b1810;
    border: 1px solid #3a3018; text-decoration: none; color: #e6dcc2; font-size: 14px; }
  .setup b { color: var(--accent); font-size: 18px; font-family: var(--display); }
  .cols { display: flex; flex-wrap: wrap; gap: 28px; }
  .main { flex: 999 1 460px; min-width: 0; }
  aside { flex: 1 1 300px; min-width: 0; display: flex; flex-direction: column; gap: 24px; }
  h2 { margin: 0; font-size: 18px; }
  .needs { padding: 20px; border-radius: 18px; background: #1a160d; border: 1px solid #3a3018; display: flex; flex-direction: column; gap: 12px; }
  .needs h2 { color: var(--accent); }
  .q { display: flex; flex-direction: column; gap: 6px; padding-top: 10px; border-top: 1px solid #2e2715; }
  .qt { font-size: 14px; font-weight: 600; }
  .why { font-size: 13px; color: #b9ae93; }
  .btns { display: flex; gap: 8px; margin-top: 2px; }
  .btns button { min-height: 36px; padding: 0 12px; border-radius: 9px; font-size: 13px; }
  .yes { border: 0; background: var(--accent); color: #0e0f12; font-weight: 700; }
  .no { border: 1px solid #3a3018; background: transparent; color: #e6dcc2; font-weight: 600; }
  .card { padding: 20px; border-radius: 18px; background: var(--panel); border: 1px solid var(--panel-line); display: flex; flex-direction: column; gap: 14px; }
  .ch { display: flex; align-items: baseline; justify-content: space-between; }
  .ch a { font-size: 13px; font-weight: 600; text-decoration: none; }
  .big { font-family: var(--display); font-size: 44px; font-weight: 700; letter-spacing: -0.02em; }
  .svc { display: flex; align-items: center; gap: 10px; }
  .sb { flex: 1; display: flex; flex-direction: column; gap: 5px; }
  .sl { display: flex; justify-content: space-between; font-size: 13px; color: var(--text-2); }
  .m { color: var(--muted); }
  .track { height: 5px; border-radius: 3px; background: #23262e; }
  .track div { height: 100%; border-radius: 3px; }
  @media (max-width: 640px) { .content { padding: 24px 16px 40px; } }
</style>
