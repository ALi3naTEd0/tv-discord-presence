<script lang="ts">
  import type { Session } from './api'
  import { clock } from './clock.svelte'
  import { clockTime, current, episodeLine, moment, sinkName, time, titleOf } from './format'
  import { slugFor } from './logos'
  import Logo from './Logo.svelte'
  import QualityChips from './QualityChips.svelte'
  import SiteLinks from './SiteLinks.svelte'

  let { session, sinks }: { session?: Session; sinks: string[] } = $props()

  const it = $derived(session && current(session))
  const slug = $derived(session && slugFor(session.app.id, session.app.name))
  const pos = $derived(session?.live_position_secs ?? session?.position_secs)
  const total = $derived(it?.runtime_min ? it.runtime_min * 60 : undefined)
  const left = $derived(total !== undefined && pos !== undefined ? Math.max(0, total - pos) : undefined)
  const sinksLine = $derived(sinks.map(sinkName).join(' and '))
</script>

<section class="hero" aria-label="Now playing" style:--poster={it?.poster ? `url("${it.poster}")` : 'none'}>
  <div class="veil"></div>
  <header>
    <div class="when">
      {#if session}<span class="live"><span class="dot"></span>NOW PLAYING</span>{/if}
      {moment(clock.now)}
    </div>
  </header>

  {#if session}
    <div class="body">
      <div class="poster">
        {#if it?.poster}
          <img src={it.poster} alt="" />
        {:else}
          <span class="ptitle">{titleOf(session)}</span>
        {/if}
        <span class="plogo"><Logo {slug} size={32} fallback={session.app.name} /></span>
      </div>

      <div class="info">
        <QualityChips q={session.quality} />
        <h1>{titleOf(session)}</h1>
        {#if it}
          {#if episodeLine(it)}<div class="ep">{episodeLine(it)}</div>{/if}
        {:else if session.hint?.subtitle}
          <div class="ep">{session.hint.subtitle}</div>
        {/if}
        <div class="where">
          <Logo {slug} size={22} fallback={session.app.name} />
          {session.app.name} on {session.device.name}
          {#if !it}· <span class="warn">not identified yet</span>{/if}
        </div>

        {#if session.progress !== undefined && pos !== undefined && total}
          <div class="progress">
            <div class="bar"><span style:width="{(session.progress * 100).toFixed(1)}%"></span></div>
            <div class="meta">
              <span>{clockTime(pos)} / {it?.runtime_estimated ? '~' : ''}{clockTime(total)}</span>
              <span>
                {#if session.ends_at}Ends at <strong>{time(session.ends_at)}</strong> · {/if}
                {Math.round((left ?? 0) / 60)} min left
              </span>
            </div>
          </div>
        {/if}
        {#if it && sinksLine}<div class="dest">Goes to {sinksLine} when it ends</div>{/if}

        {#if it}<SiteLinks links={it.links} />{/if}
      </div>
    </div>
  {:else}
    <div class="idle">
      <h1>Nothing playing</h1>
      <p>When the TV, the box or Plex plays something, it shows up here.</p>
    </div>
  {/if}
</section>

<style>
  .hero { position: relative; overflow: hidden; background: #241a2c; border-bottom: 1px solid #2e2236;
    padding: 28px 40px 40px; display: flex; flex-direction: column; gap: 28px; isolation: isolate; }
  .hero::before { content: ''; position: absolute; inset: -40px; z-index: -2; background: var(--poster) center / cover;
    filter: blur(48px) saturate(1.3); opacity: 0.55; transform: scale(1.1); }
  .veil { position: absolute; inset: 0; z-index: -1;
    background: linear-gradient(90deg, rgba(20, 14, 26, 0.92) 0%, rgba(20, 14, 26, 0.7) 55%, rgba(20, 14, 26, 0.45) 100%),
      linear-gradient(0deg, rgba(11, 12, 15, 0.9) 0%, transparent 45%); }
  header { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
  .when { display: flex; align-items: center; gap: 10px; font-size: 14px; color: #cdb9d8; }
  .live { display: inline-flex; align-items: center; gap: 6px; padding: 5px 10px; border-radius: 999px;
    background: rgba(255, 90, 97, 0.16); color: #ff9aa0; font-size: 12px; font-weight: 700; }
  .dot { width: 7px; height: 7px; border-radius: 4px; background: var(--live); animation: pulse 2s infinite; }
  @keyframes pulse { 50% { opacity: 0.35; } }
  .body { display: flex; flex-wrap: wrap; align-items: flex-end; gap: 36px; }
  .poster { position: relative; flex: 0 0 240px; aspect-ratio: 2 / 3; border-radius: 16px; overflow: hidden;
    background: #4a3458; box-shadow: 0 30px 70px rgba(0, 0, 0, 0.55), 0 0 0 1px rgba(255, 255, 255, 0.06);
    display: flex; align-items: flex-end; padding: 18px; }
  .poster img { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; }
  .plogo { position: absolute; top: 14px; left: 14px; filter: drop-shadow(0 4px 10px rgba(0, 0, 0, 0.5)); }
  .ptitle { font-family: var(--display); font-weight: 700; font-size: 26px; line-height: 1.05; }
  .info { flex: 1 1 380px; min-width: 0; display: flex; flex-direction: column; gap: 14px; padding-bottom: 6px; }
  h1 { margin: 0; font-size: clamp(34px, 5vw, 56px); line-height: 1; letter-spacing: -0.03em; }
  .ep { font-size: 20px; color: #e3d6ea; }
  .where { display: flex; align-items: center; gap: 10px; font-size: 14px; color: #cdb9d8; }
  .warn { color: var(--accent); }
  .progress { display: flex; flex-direction: column; gap: 8px; max-width: 640px; margin-top: 4px; }
  .bar { height: 6px; border-radius: 3px; background: rgba(255, 255, 255, 0.14); overflow: hidden; }
  .bar span { display: block; height: 100%; background: var(--accent); border-radius: 3px; transition: width 1s linear; }
  .meta { display: flex; justify-content: space-between; gap: 12px; font-size: 13px; color: #cdb9d8;
    font-variant-numeric: tabular-nums; }
  .meta strong { color: var(--text); }
  .dest { font-size: 13px; color: #a995b6; }
  .idle h1 { font-size: 40px; }
  .idle p { color: #cdb9d8; margin: 8px 0 0; }
  @media (max-width: 640px) {
    .hero { padding: 20px 16px 28px; }
    .poster { flex-basis: 150px; }
  }
</style>
