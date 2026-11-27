<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type FoundDevice, type NetflixStatus } from './api'
  import { sinkName } from './format'
  import Logo from './Logo.svelte'
  import { app, go, loadStatus, toast } from './store.svelte'

  const KIND: Record<string, string> = { webos: 'LG webOS TV', androidtv: 'Android TV / Google TV', plex: 'Plex server' }

  let found = $state<FoundDevice[]>()
  let scanning = $state(false)
  let names = $state<Record<string, string>>({})
  let adding = $state<string>()
  let manual = $state({ kind: 'webos', host: '', name: '' })
  let showManual = $state(false)
  let netflix = $state<NetflixStatus>()
  let autoScanned = false
  // a first run: look for TVs right away (once the status says there are none)
  $effect(() => { if (app.status && !autoScanned && !tvs.length) { autoScanned = true; scan() } })

  const tvs = $derived(app.status?.devices.filter(d => d.kind !== 'plex') ?? [])
  const waiting = $derived(tvs.filter(d => d.state?.state === 'waiting'))

  async function scan() {
    scanning = true
    try {
      found = await api.scan()
      for (const f of found) names[f.host] ??= f.name
    } catch (e) { toast(`Could not look: ${(e as Error).message}`) }
    finally { scanning = false }
  }
  async function add(kind: string, host: string, name: string) {
    adding = host
    try {
      await api.addDevice(kind, host, name)
      toast(`Added ${name || KIND[kind]}. ${kind === 'webos' ? 'Accept the prompt on the TV.' : 'Allow the connection on the TV.'}`)
      await loadStatus()
      if (found) found = found.map(f => (f.host === host ? { ...f, added_as: name } : f))
      showManual = false
      manual = { kind: 'webos', host: '', name: '' }
    } catch (e) { toast((e as Error).message) }
    finally { adding = undefined }
  }

  let nfTimer: ReturnType<typeof setTimeout>
  async function loadNetflix() {
    try { netflix = await api.netflix() } catch { /* offline banner */ }
    clearTimeout(nfTimer)
    if (netflix?.connecting) nfTimer = setTimeout(loadNetflix, 2000)
  }
  async function connect() {
    try { netflix = await api.connectNetflix(); loadNetflix() } catch (e) { toast((e as Error).message) }
  }

  onMount(() => {
    loadNetflix()
    const t = setInterval(loadStatus, 3000) // see a new TV connect as soon as you accept it
    return () => { clearInterval(t); clearTimeout(nfTimer) }
  })
</script>

<div class="page">
  <header>
    <h1>Set up Couchlog</h1>
    <p>Three steps, each optional. Couchlog starts recording as soon as one TV is connected.</p>
  </header>

  <section class="step">
    <div class="num">1</div>
    <div class="body">
      <h2>Your TVs and boxes</h2>
      <p class="tip">Couchlog listens to LG webOS TVs and Android TV / Google TV boxes on your network. Nothing to install on them.</p>

      {#if tvs.length}
        <ul class="devs">
          {#each tvs as d (d.id)}
            <li>
              <span class="dot {d.state?.state ?? ''}"></span>
              <div class="dn"><b>{d.name}</b><span>{KIND[d.kind] ?? d.kind}</span></div>
              <span class="ds">
                {#if !d.state}—
                {:else if d.state.state === 'waiting'}Waiting for you to accept on the TV
                {:else if d.state.state === 'off'}Off or unreachable
                {:else if d.state.state === 'playing' || d.state.state === 'paused'}Connected · {d.state.app ?? d.state.state}
                {:else}Connected{/if}
              </span>
            </li>
          {/each}
        </ul>
      {/if}

      {#if waiting.length}
        <div class="note">
          {#each waiting as d (d.id)}
            <p><b>{d.name}:</b>
              {#if d.kind === 'webos'}a message appears on the TV asking to allow Couchlog. Choose <b>Allow</b> with the remote.
              {:else}the TV asks <b>“Allow network debugging?”</b>. Tick <b>Always allow from this computer</b> and choose <b>Allow</b>.{/if}
            </p>
          {/each}
        </div>
      {/if}

      <div class="row">
        <button class="primary" onclick={scan} disabled={scanning}>{scanning ? 'Looking on your network…' : found ? 'Look again' : 'Look for TVs'}</button>
        <button class="ghost" onclick={() => (showManual = !showManual)}>Add by IP address</button>
      </div>

      {#if found}
        {#if found.length}
          <ul class="found">
            {#each found as f (f.host)}
              <li>
                <div class="fl">
                  <Logo slug={undefined} size={34} fallback={f.kind === 'webos' ? 'L' : 'A'} />
                  <div class="dn"><b>{f.name}</b><span>{KIND[f.kind]} · {f.host}{f.model ? ` · ${f.model}` : ''}</span></div>
                </div>
                {#if f.added_as}
                  <span class="added">✓ Added as {f.added_as}</span>
                {:else if f.kind === 'androidtv' && !f.adb}
                  <span class="warn">Network debugging is off on this TV: see below</span>
                {:else}
                  <div class="fa">
                    <input aria-label="Name for {f.host}" bind:value={names[f.host]} />
                    <button class="primary" disabled={adding === f.host} onclick={() => add(f.kind, f.host, names[f.host])}>{adding === f.host ? 'Adding…' : 'Add'}</button>
                  </div>
                {/if}
              </li>
            {/each}
          </ul>
        {:else}
          <p class="tip">No TVs answered. Make sure they are on and on the same network as this computer, or add one by its IP address.</p>
        {/if}
        {#if found.some(f => f.kind === 'androidtv' && !f.adb)}
          <div class="note">
            <p><b>Turning on network debugging on Android TV / Google TV:</b> Settings → System → About → select <b>Android TV OS build</b> seven times.
              Then Settings → System → Developer options → turn on <b>USB debugging</b> (and <b>Network debugging</b> if the TV lists it). Look again afterwards.</p>
          </div>
        {/if}
      {/if}

      {#if showManual}
        <form class="manual" onsubmit={(e) => { e.preventDefault(); add(manual.kind, manual.host, manual.name) }}>
          <select bind:value={manual.kind} aria-label="Kind of device">
            <option value="webos">LG webOS TV</option>
            <option value="androidtv">Android TV / Google TV</option>
          </select>
          <input bind:value={manual.host} placeholder="IP address, like 192.168.1.20" aria-label="IP address" required />
          <input bind:value={manual.name} placeholder="Name (Living room TV)" aria-label="Name" />
          <button class="primary" type="submit" disabled={!manual.host.trim() || !!adding}>Add</button>
        </form>
      {/if}
    </div>
  </section>

  <section class="step">
    <div class="num">2</div>
    <div class="body">
      <h2><Logo slug="netflix" size={26} /> Netflix</h2>
      <p class="tip">Netflix on a TV does not say what is playing. Couchlog reads your account's viewing activity instead, with a window it opens to sign in. Your password never reaches Couchlog.</p>
      {#if netflix}
        {#if netflix.profile && !netflix.connecting}
          <p class="ok">✓ Connected as <b>{netflix.profile}</b></p>
        {:else if netflix.connecting}
          <p class="tip">{netflix.message || 'Opening Netflix…'} Sign in in the window that opened; it closes by itself.</p>
        {:else if netflix.error}
          <p class="warn">{netflix.error}</p>
        {/if}
        {#if !netflix.enabled}
          <p class="tip">Netflix is off in this copy's settings (<code>netflix = true</code> turns it on).</p>
        {/if}
        <div class="row">
          <button class="primary" onclick={connect} disabled={netflix.connecting}>{netflix.profile ? 'Connect again' : 'Connect Netflix'}</button>
        </div>
      {/if}
    </div>
  </section>

  <section class="step">
    <div class="num">3</div>
    <div class="body">
      <h2>What you pay</h2>
      <p class="tip">Add each subscription's price to see what every hour you watch really costs. Services show up by themselves as Couchlog sees them.</p>
      <div class="row"><button class="ghost" onclick={() => go('services')}>Set prices →</button></div>
    </div>
  </section>

  <section class="step">
    <div class="num">4</div>
    <div class="body">
      <h2>Where it sends</h2>
      {#if app.status?.sinks.length}
        <p class="tip">Identified titles go to {app.status.sinks.map(sinkName).join(' and ')} by ID{app.status.read_only ? ' (checking only for now)' : ''}.</p>
      {:else}
        <p class="tip">Couchlog keeps your history by itself. Sign-in to Trakt and Simkl from here is coming; for now they are set in the settings file.</p>
      {/if}
    </div>
  </section>
</div>

<style>
  .page { padding: 32px 40px 56px; max-width: 900px; width: 100%; margin: 0 auto; box-sizing: border-box; display: flex; flex-direction: column; gap: 18px; }
  h1 { margin: 0; font-size: 34px; }
  header p { margin: 6px 0 0; color: var(--muted); font-size: 14px; }
  .step { display: flex; gap: 18px; padding: 22px 24px; border-radius: 18px; background: #16181d; border: 1px solid #23262e; }
  .num { flex: 0 0 34px; height: 34px; border-radius: 17px; background: var(--accent-soft); color: var(--accent);
    font-family: var(--display); font-weight: 700; display: flex; align-items: center; justify-content: center; }
  .body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 12px; }
  h2 { margin: 4px 0 0; font-size: 20px; display: flex; align-items: center; gap: 10px; }
  .tip { margin: 0; color: var(--text-2); font-size: 14px; line-height: 1.5; }
  .ok { margin: 0; color: var(--ok); font-size: 15px; }
  .warn { margin: 0; color: var(--accent); font-size: 14px; }
  .note { padding: 14px 16px; border-radius: 12px; background: #1b1810; border: 1px solid #3a3018; font-size: 14px; color: #e6dcc2; line-height: 1.5; }
  .note p { margin: 0; } .note p + p { margin-top: 8px; }
  .row { display: flex; flex-wrap: wrap; gap: 10px; }
  .primary { min-height: 42px; padding: 0 18px; border-radius: 11px; border: 0; background: var(--accent); color: #0e0f12; font-weight: 700; font-size: 14px; }
  .primary:disabled { opacity: 0.55; cursor: default; }
  .ghost { min-height: 42px; padding: 0 16px; border-radius: 11px; border: 1px solid #2e323b; background: none; color: var(--text-2); font-weight: 600; font-size: 14px; }
  ul { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
  .devs li, .found li { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; padding: 12px 14px; border-radius: 12px; background: #0e0f12; border: 1px solid #23262e; }
  .dot { width: 10px; height: 10px; border-radius: 5px; background: #4b505a; flex: 0 0 auto; }
  .dot.playing, .dot.paused { background: var(--ok); } .dot.idle { background: #6ea8fe; } .dot.waiting { background: var(--accent); }
  .dn { flex: 1; min-width: 160px; display: flex; flex-direction: column; }
  .dn span { font-size: 12px; color: var(--muted); }
  .ds { font-size: 13px; color: var(--text-2); }
  .fl { display: flex; align-items: center; gap: 12px; flex: 1; min-width: 220px; }
  .fa { display: flex; gap: 8px; }
  .added { font-size: 13px; color: var(--sent-fg); }
  input, select { min-height: 42px; padding: 0 12px; border-radius: 10px; background: #0e0f12; border: 1px solid #2e323b; color: var(--text); font: inherit; font-size: 14px; min-width: 0; }
  .fa input { width: 180px; }
  .manual { display: flex; flex-wrap: wrap; gap: 8px; }
  .manual input { flex: 1 1 180px; }
  code { font-size: 13px; color: var(--text); }
  @media (max-width: 640px) { .page { padding: 24px 16px 40px; } .step { padding: 18px 16px; gap: 12px; } }
</style>
