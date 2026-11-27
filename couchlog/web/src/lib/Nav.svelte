<script lang="ts">
  import { app, go, toIdentify, type Route } from './store.svelte'
  import { sinkName } from './format'
  import Logo from './Logo.svelte'

  const count = $derived(toIdentify(app.history).length)
  const STATE: Record<string, string> = { playing: 'Playing', paused: 'Paused', idle: 'On', off: 'Off', waiting: 'Not connected' }
  function deviceState(id: number) {
    const d = app.status?.devices.find(x => x.id === id)
    if (d?.state) return STATE[d.state.state] ?? ''
    return app.now.some(s => s.device.id === id) ? 'Playing' : ''
  }
  const dotClass = (id: number) => app.status?.devices.find(x => x.id === id)?.state?.state ?? (app.now.some(s => s.device.id === id) ? 'playing' : '')
  const links: { r: Route; label: string; icon: string }[] = [
    { r: 'home', label: 'Home', icon: 'M3 11l9-7 9 7v9a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z' },
    { r: 'history', label: 'History', icon: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 7v5l3 2' },
    { r: 'insights', label: 'Insights', icon: 'M4 20V10M10 20V4M16 20v-7M22 20H2' },
    { r: 'services', label: 'Services', icon: 'M2 7a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2zM2 10h20' },
  ]
  function toIdentifyTab() { sessionStorage.setItem('history-tab', 'identify'); go('history'); dispatchEvent(new Event('history-tab')) }
</script>

<nav aria-label="Main">
  <a class="brand" href="#/" aria-label="Couchlog home">
    <img src="/favicon.svg" alt="" width="34" height="34" />
    <span>Couchlog</span>
  </a>
  <div class="links">
    {#each links as l (l.r)}
      <a class="item" class:active={app.route === l.r} href={l.r === 'home' ? '#/' : `#/${l.r}`}
        aria-current={app.route === l.r ? 'page' : undefined}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d={l.icon} /></svg>
        <span class="lbl">{l.label}</span>
      </a>
    {/each}
    {#if count}
      <button class="item" onclick={toIdentifyTab}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/></svg>
        <span class="lbl grow">To identify</span>
        <span class="count">{count}</span>
      </button>
    {/if}
  </div>

  {#if app.status}
    <div class="side">
      <div class="label">Devices</div>
      {#each app.status.devices as d (d.id)}
        <div class="dev">
          <span class="dot {dotClass(d.id)}"></span>
          <span class="grow">{d.name}</span>
          <span class="state">{deviceState(d.id)}</span>
        </div>
      {/each}
      <a class="add" href="#/setup">+ Add a TV or box</a>
      {#if app.status.sinks.length || app.status.discord !== 'off'}
        <div class="label">Sends to</div>
        {#each app.status.sinks as k (k)}
          <div class="dev">
            <Logo slug={k} size={24} fallback={k} />
            <span class="grow">{sinkName(k)}</span>
            {#if app.status.read_only}<span class="state" title="Checks only while the old dashboard sends">check only</span>{/if}
          </div>
        {/each}
        <div class="dev" title={app.status.discord === 'dry-run' ? 'Worked out but not shown: the old dashboard shows your Discord status for now' : app.status.discord === 'on' ? 'What you watch shows as your Discord status' : 'Discord status is off'}>
          <Logo slug="discord" size={24} fallback="Discord" />
          <span class="grow">Discord</span>
          <span class="state">{app.status.discord === 'on' ? 'status' : app.status.discord === 'dry-run' ? 'preview only' : 'off'}</span>
        </div>
      {/if}
      <a class="ver" href="#/about" class:on={app.route === 'about'}>About Couchlog · {app.status.version}</a>
    </div>
  {/if}
</nav>

<style>
  nav { flex: 1 1 220px; max-width: 250px; padding: 28px 18px; display: flex; flex-direction: column; gap: 6px;
    border-right: 1px solid #1e2027; position: sticky; top: 0; height: 100vh; overflow-y: auto; }
  .brand { display: flex; align-items: center; gap: 10px; padding: 0 10px 24px; text-decoration: none; color: var(--text); }
  .brand span { font-family: var(--display); font-weight: 700; font-size: 20px; letter-spacing: -0.02em; }
  .links { display: flex; flex-direction: column; gap: 6px; }
  .item { display: flex; align-items: center; gap: 12px; min-height: 44px; padding: 0 12px; border-radius: 10px;
    color: #b4b9c2; text-decoration: none; font-weight: 500; font-size: 15px; background: none; border: 0; text-align: left; }
  .item:hover { color: var(--text); background: #15171c; }
  .item.active { background: var(--nav-active); color: var(--text); font-weight: 600; }
  .count { min-width: 22px; height: 22px; padding: 0 7px; border-radius: 11px; background: var(--accent); color: #0e0f12;
    font-size: 12px; font-weight: 700; display: flex; align-items: center; justify-content: center; }
  .side { display: flex; flex-direction: column; flex: 1; }
  .label { margin-top: 24px; padding: 0 12px 4px; font-size: 12px; font-weight: 600; letter-spacing: 0.08em;
    text-transform: uppercase; color: var(--muted); }
  .dev { display: flex; align-items: center; gap: 10px; min-height: 38px; padding: 0 12px; font-size: 14px; color: var(--text-2); }
  .grow { flex: 1; }
  .dot { width: 8px; height: 8px; border-radius: 4px; background: #4b505a; }
  .dot.playing, .dot.paused { background: var(--ok); }
  .dot.idle { background: #6ea8fe; }
  .dot.waiting { background: var(--accent); }
  .add { padding: 6px 12px; font-size: 13px; color: var(--muted); text-decoration: none; }
  .add:hover { color: var(--accent); }
  .state { font-size: 12px; color: var(--muted); }
  .ver { margin-top: auto; padding: 24px 12px 0; font-size: 11px; color: #5b616c; text-decoration: none; }
  .ver:hover, .ver.on { color: var(--text-2); }
  @media (max-width: 760px) {
    nav { max-width: none; flex-basis: 100%; height: auto; position: sticky; z-index: 5; background: var(--bg);
      flex-direction: row; align-items: center; padding: 10px 12px; border-right: 0; border-bottom: 1px solid #1e2027; gap: 4px; }
    .brand { padding: 0 6px 0 0; }
    .brand span, .lbl, .side { display: none; }
    .links { flex-direction: row; gap: 2px; flex: 1; justify-content: flex-end; }
    .item { padding: 0 11px; }
  }
</style>
