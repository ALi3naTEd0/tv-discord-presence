<script lang="ts">
  import { MARKS } from './logos'
  let { slug, size = 24, fallback = '' }: { slug?: string; size?: number; fallback?: string } = $props()
  const m = $derived(slug ? MARKS[slug] : undefined)
</script>

{#if m}
  <span class="tile" role="img" aria-label={m.label}
    style:width="{size}px" style:height="{size}px" style:border-radius="{Math.round(size * 0.28)}px" style:background={m.tile}>
    <svg viewBox="0 0 24 24" width={Math.round(size * m.k)} height={Math.round(size * m.k)} aria-hidden="true"><path d={m.d} fill={m.fill} /></svg>
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
  .plain { background: #2a2d35; color: var(--text-2); font-weight: 700; }
</style>
