<script lang="ts">
  import { onMount } from 'svelte'
  import History from './lib/History.svelte'
  import Home from './lib/Home.svelte'
  import IdentifyDialog from './lib/IdentifyDialog.svelte'
  import Insights from './lib/Insights.svelte'
  import Nav from './lib/Nav.svelte'
  import Services from './lib/Services.svelte'
  import About from './lib/About.svelte'
  import { app, loadHistory, loadNow, loadStatus } from './lib/store.svelte'

  onMount(() => {
    loadStatus(); loadNow(); loadHistory()
    const timers = [setInterval(loadNow, 5000), setInterval(loadHistory, 30000), setInterval(loadStatus, 60000)]
    return () => timers.forEach(clearInterval)
  })
  const titles = { home: 'Couchlog', history: 'History · Couchlog', insights: 'Insights · Couchlog', services: 'Your services · Couchlog', about: 'About · Couchlog' }
  $effect(() => { document.title = titles[app.route] })
</script>

<div class="shell">
  <Nav />
  <main>
    {#if app.offline}<div class="offline" role="status">Can't reach Couchlog. Retrying…</div>{/if}
    {#if app.route === 'history'}<History />
    {:else if app.route === 'insights'}<Insights />
    {:else if app.route === 'services'}<Services />
    {:else if app.route === 'about'}<About />
    {:else}<Home />{/if}
  </main>
</div>

{#if app.identifying}
  {#key app.identifying.id}<IdentifyDialog session={app.identifying} />{/key}
{/if}
{#if app.toast}<div class="toast" role="status">{app.toast}</div>{/if}

<style>
  .shell { min-height: 100vh; display: flex; flex-wrap: wrap; align-items: flex-start; }
  main { flex: 999 1 560px; min-width: 0; display: flex; flex-direction: column; }
  .offline { padding: 10px 40px; background: #3a1d1f; color: #ffb4b8; font-size: 14px; }
  .toast { position: fixed; left: 50%; bottom: 24px; transform: translateX(-50%); z-index: 50; max-width: calc(100vw - 32px);
    padding: 12px 18px; border-radius: 12px; background: #f2f1ee; color: #0b0c0f; font-size: 14px; font-weight: 600;
    box-shadow: 0 16px 40px rgba(0, 0, 0, 0.5); animation: up 0.18s ease-out; }
  @keyframes up { from { transform: translate(-50%, 8px); opacity: 0; } }
  @media (max-width: 640px) { .offline { padding: 10px 16px; } }
</style>
