<script lang="ts">
  import type { UpNext } from './api'
  import Logo from './Logo.svelte'
  let { items }: { items: UpNext[] } = $props()
</script>

{#if items.length}
  <section aria-labelledby="next-h">
    <div class="head">
      <h2 id="next-h">Up next</h2>
      <span>The next episode of what you are watching, and where you watch it</span>
    </div>
    <div class="grid">
      {#each items.slice(0, 4) as n (n.tmdb)}
        <article>
          <div class="poster">
            {#if n.poster}<img src={n.poster} alt="" loading="lazy" />{:else}<span class="pt">{n.title}</span>{/if}
            <span class="logo"><Logo slug={n.service.key} size={28} fallback={n.service.name} /></span>
          </div>
          <div class="t">{n.title}</div>
          <div class="ep">S{n.season} · E{n.episode}{n.episode_title ? ` · ${n.episode_title}` : ''}</div>
          <div class="where">{n.service.name}</div>
        </article>
      {/each}
    </div>
  </section>
{/if}

<style>
  .head { display: flex; flex-wrap: wrap; align-items: baseline; justify-content: space-between; gap: 8px; margin-bottom: 16px; }
  h2 { margin: 0; font-size: 24px; }
  .head span { font-size: 13px; color: var(--muted); }
  .grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 18px; }
  .poster { position: relative; aspect-ratio: 2 / 3; border-radius: 14px; overflow: hidden; background: #2a2d35;
    box-shadow: 0 16px 40px rgba(0, 0, 0, 0.45), 0 0 0 1px rgba(255, 255, 255, 0.05); display: flex; align-items: flex-end; padding: 12px; }
  .poster img { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; }
  .pt { font-family: var(--display); font-weight: 700; font-size: 18px; position: relative; }
  .logo { position: absolute; top: 10px; left: 10px; filter: drop-shadow(0 3px 8px rgba(0, 0, 0, 0.5)); }
  .t { margin-top: 10px; font-weight: 600; font-size: 15px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .ep { font-size: 13px; color: var(--text-2); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .where { font-size: 13px; color: var(--muted); }
  @media (max-width: 760px) { .grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
