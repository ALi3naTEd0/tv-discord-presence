<script lang="ts">
  import type { Session } from './api'
  import { current, episodeLine, minutes, time, titleOf } from './format'
  import { slugFor } from './logos'
  import Logo from './Logo.svelte'

  let { sessions }: { sessions: Session[] } = $props()

  type Badge = { text: string; kind: 'live' | 'sent' | 'wait' | 'look' | 'muted' }
  function badge(s: Session): Badge {
    if (s.open) return { text: 'Playing', kind: 'live' }
    if (s.hidden) return { text: 'Hidden', kind: 'muted' }
    const it = current(s)
    if (!it) return { text: 'To identify', kind: 'look' }
    const st = Object.values(it.deliveries).map(d => d.status)
    if (st.length && st.every(x => x === 'added' || x === 'skipped')) return { text: 'Sent ✓', kind: 'sent' }
    if (st.some(x => x === 'error' || x === 'not_found')) return { text: 'Needs a look', kind: 'look' }
    return { text: 'Not sent', kind: 'wait' }
  }
</script>

<section aria-labelledby="today-h">
  <h2 id="today-h">Today</h2>
  {#if sessions.length}
    <div class="list">
      {#each sessions as s (s.id)}
        {@const it = current(s)}
        {@const b = badge(s)}
        <div class="row">
          <div class="thumb">{#if it?.poster}<img src={it.poster} alt="" loading="lazy" />{/if}</div>
          <div class="text">
            <div class="title">{titleOf(s)}{#if it && episodeLine(it)}<span class="sub">{` · ${episodeLine(it)}`}</span>{/if}</div>
            <div class="meta">
              <Logo slug={slugFor(s.app.id, s.app.name)} size={18} fallback={s.app.name} />
              {s.app.name} · {time(s.started_at)} · {minutes(s.played_secs)}
              {#if s.quality?.label}<span class="q">· {s.quality.label}</span>{/if}
            </div>
          </div>
          <span class="badge {b.kind}">{b.text}</span>
        </div>
      {/each}
    </div>
  {:else}
    <p class="empty">Nothing watched yet today.</p>
  {/if}
</section>

<style>
  h2 { margin: 0 0 14px; font-size: 24px; }
  .list { display: flex; flex-direction: column; border-radius: 18px; background: var(--panel);
    border: 1px solid var(--panel-line); overflow: hidden; }
  .row { display: flex; align-items: center; gap: 16px; padding: 14px 18px; border-bottom: 1px solid var(--row-line); }
  .row:last-child { border-bottom: 0; }
  .thumb { flex: 0 0 44px; height: 66px; border-radius: 8px; overflow: hidden; background: #2a2d35;
    box-shadow: 0 0 0 1px rgba(255, 255, 255, 0.05); }
  .thumb img { width: 100%; height: 100%; object-fit: cover; display: block; }
  .text { flex: 1; min-width: 0; }
  .title { font-weight: 600; font-size: 15px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .sub { color: var(--text-2); font-weight: 500; }
  .meta { display: flex; align-items: center; gap: 8px; margin-top: 4px; font-size: 13px; color: var(--muted);
    flex-wrap: wrap; }
  .q { color: #a5abb5; }
  .badge { font-size: 12px; font-weight: 600; padding: 5px 10px; border-radius: 999px; white-space: nowrap; }
  .live { background: rgba(255, 90, 97, 0.16); color: #ff9aa0; }
  .sent { background: var(--sent-bg); color: var(--sent-fg); }
  .wait { background: #23262e; color: var(--text-2); }
  .look { background: var(--accent-soft); color: var(--accent); }
  .muted { background: #1e2128; color: var(--muted); }
  .empty { color: var(--muted); }
</style>
