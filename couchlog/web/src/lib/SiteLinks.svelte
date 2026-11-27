<script lang="ts">
  import type { Link } from './api'
  import Logo from './Logo.svelte'
  let { links, size = 28 }: { links: Link[]; size?: number } = $props()
  const slug = { tmdb: 'themoviedatabase', trakt: 'trakt' } as Record<string, string>
</script>

{#if links.length}
  <div class="links">
    {#each links as l (l.site)}
      <a href={l.url} target="_blank" rel="noopener noreferrer" title="Open on {l.name}" aria-label="Open on {l.name}">
        {#if l.site === 'imdb'}
          <span class="imdb" style:height="{size}px" style:font-size="{Math.round(size * 0.42)}px">IMDb</span>
        {:else}
          <Logo slug={slug[l.site]} {size} />
        {/if}
      </a>
    {/each}
  </div>
{/if}

<style>
  .links { display: flex; gap: 8px; align-items: center; }
  a { display: inline-flex; border-radius: 8px; transition: transform 0.12s ease, opacity 0.12s; opacity: 0.9; }
  a:hover { transform: translateY(-1px); opacity: 1; }
  .imdb { display: inline-flex; align-items: center; padding: 0 7px; border-radius: 6px; background: #f5c518;
    color: #000; font-weight: 900; letter-spacing: -0.02em; font-family: var(--font); }
</style>
