<script lang="ts">
  import { MARKS, WORDS } from './logos'
  let { slug, size = 24, fallback = '' }: { slug?: string; size?: number; fallback?: string } = $props()
  const m = $derived(slug ? MARKS[slug] : undefined)
  const w = $derived(slug && !m ? WORDS[slug] : undefined)
  // marks are square (24×24) except wordmarks like Disney+'s, kept at their shape
  const vb = $derived((m?.vb ?? '0 0 24 24').split(' ').map(Number))
  const iw = $derived(Math.round(size * (m?.k ?? 1)))
  const ih = $derived(Math.round(iw * vb[3] / vb[2]))
</script>

{#if m}
  <span class="tile" role="img" aria-label={m.label}
    style:width="{size}px" style:height="{size}px" style:border-radius="{Math.round(size * 0.28)}px" style:background={m.tile}>
    <svg viewBox={m.vb ?? '0 0 24 24'} width={iw} height={ih} aria-hidden="true"><path d={m.d} fill={m.fill} /></svg>
  </span>
{:else if w}
  <span class="tile word" role="img" aria-label={w.label}
    style:width="{size}px" style:height="{size}px" style:border-radius="{Math.round(size * 0.28)}px" style:background={w.tile}
    style:color={w.fill} style:font-size="{Math.round(size * (w.text.length <= 2 ? 0.42 : w.text.length <= 3 ? 0.34 : 0.26))}px">
    {w.text}
  </span>
{:else}
  <span class="tile plain" role="img" aria-label={fallback}
    style:width="{size}px" style:height="{size}px" style:border-radius="{Math.round(size * 0.28)}px" style:font-size="{Math.round(size * 0.45)}px">
    {fallback.slice(0, 1).toUpperCase()}
  </span>
{/if}

<style>
  .tile { display: inline-flex; align-items: center; justify-content: center; flex: 0 0 auto;
    box-shadow: inset 0 0 0 1px rgba(255, 255, 255, 0.06); }
  .word { font-weight: 800; letter-spacing: -0.02em; font-family: var(--font); }
  .plain { background: #2a2d35; color: var(--text-2); font-weight: 700; }
</style>
