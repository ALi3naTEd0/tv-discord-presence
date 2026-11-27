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
