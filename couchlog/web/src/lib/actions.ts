import { api, type Session } from './api'
import { app, replaceSession, toast } from './store.svelte'
import { titleOf } from './format'

export function identify(s: Session) { app.identifying = s }

export async function setHidden(s: Session, hidden: boolean) {
  try {
    replaceSession(await api.setHidden(s.id, hidden))
    toast(hidden ? `Hidden: ${titleOf(s)}` : `Shown again: ${titleOf(s)}`)
  } catch (e) {
    toast(`Could not change it: ${(e as Error).message}`)
  }
}

/** Confirm a guessed episode as it is: it goes to Trakt and Simkl like any other. */
export async function confirmGuess(s: Session) {
  const it = s.items.find(i => i.guessed)
  if (!it?.ids.tmdb) return identify(s)
  try {
    replaceSession(await api.identify(s.id, it.kind === 'movie'
      ? { kind: 'movie', tmdb: it.ids.tmdb }
      : { kind: 'tv', tmdb: it.ids.tmdb, season: it.season, episodes: s.items.filter(i => i.kind === 'episode').map(i => i.episode!) }))
    toast(`Confirmed: ${it.title}${it.season ? ` S${it.season} E${it.episode}` : ''}`)
  } catch (e) {
    toast(`Not confirmed: ${(e as Error).message}`)
  }
}
