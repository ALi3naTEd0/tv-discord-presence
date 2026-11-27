<script lang="ts">
  import type { Quality } from './api'
  let { q }: { q?: Quality } = $props()
</script>

{#if q}
  <div class="chips" title={q.width ? `${q.width}×${q.height} as decoded by the device` : ''}>
    {#if q.resolution}<span class="chip gold">{q.resolution}{q.uhd ? ' UHD' : ''}</span>{/if}
    {#if q.hdr}<span class="chip gold">{q.hdr}</span>{/if}
    {#if q.codec}<span class="chip">{q.codec}</span>{/if}
  </div>
{/if}

<style>
  .chips { display: flex; flex-wrap: wrap; gap: 8px; }
  .chip { padding: 5px 10px; border-radius: 8px; background: rgba(255, 255, 255, 0.08); color: #e3d6ea;
    font-size: 12px; font-weight: 700; letter-spacing: 0.02em; }
  .gold { background: var(--accent-soft); color: var(--accent); font-weight: 800; letter-spacing: 0.04em; }
</style>
