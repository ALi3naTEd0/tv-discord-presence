<script lang="ts">
  import Logo from './Logo.svelte'
  import { app } from './store.svelte'
  import { sinkName } from './format'
</script>

<div class="page">
  <h1>About Couchlog</h1>
  <p class="lead">Your own history of what you watch on TV, kept on this computer. Couchlog organizes what already exists;
    Trakt, Simkl and Discord are optional places it sends to.</p>

  <section class="card">
    <h2>This copy</h2>
    <dl>
      <dt>Version</dt><dd>{app.status?.version ?? '…'}</dd>
      <dt>Devices</dt><dd>{app.status?.devices.map(d => d.name).join(', ') || '—'}</dd>
      <dt>Sends to</dt><dd>{app.status?.sinks.map(sinkName).join(', ') || 'nowhere: it only records'}{app.status?.read_only ? ' (checking only)' : ''}</dd>
    </dl>
  </section>

  <section class="card tmdb">
    <a href="https://www.themoviedb.org/" target="_blank" rel="noopener noreferrer" aria-label="The Movie Database (TMDB)">
      <Logo slug="themoviedatabase" size={56} />
    </a>
    <div>
      <h2>Titles, posters and episodes from TMDB</h2>
      <p>This product uses the TMDB API but is not endorsed or certified by TMDB.</p>
    </div>
  </section>

  <section class="card">
    <h2>Credits</h2>
    <ul>
      <li>Posters TMDB does not have yet come from IMDb's public pages.</li>
      <li>Service logos are from <a href="https://simpleicons.org/" target="_blank" rel="noopener noreferrer">Simple Icons</a> (CC0).
        Netflix, HBO Max, Plex, YouTube, Tubi, Trakt, Simkl, Discord, IMDb and TMDB are trademarks of their owners;
        Couchlog is not affiliated with them.</li>
      <li>Fonts: Inter and Space Grotesk (SIL Open Font License).</li>
    </ul>
  </section>
</div>

<style>
  .page { padding: 32px 40px 56px; max-width: 820px; width: 100%; margin: 0 auto; box-sizing: border-box; display: flex; flex-direction: column; gap: 20px; }
  h1 { margin: 0; font-size: 34px; }
  .lead { margin: 0; color: var(--text-2); font-size: 15px; line-height: 1.5; }
  .card { padding: 22px 24px; border-radius: 18px; background: #16181d; border: 1px solid #23262e; }
  h2 { margin: 0 0 10px; font-size: 18px; }
  dl { display: grid; grid-template-columns: 120px 1fr; gap: 8px 16px; margin: 0; font-size: 14px; }
  dt { color: var(--muted); }
  dd { margin: 0; }
  .tmdb { display: flex; align-items: center; gap: 20px; }
  .tmdb p { margin: 0; color: var(--text-2); font-size: 14px; }
  ul { margin: 0; padding-left: 18px; color: var(--text-2); font-size: 14px; line-height: 1.6; }
  @media (max-width: 640px) { .page { padding: 24px 16px 40px; } dl { grid-template-columns: 1fr; } }
</style>
