// App-wide state: where you are, and the data every screen shares, polled
// from the API. Screens load their own extra data (insights, services).
import { api, type Session, type Status } from './api'
import { syncClock } from './clock.svelte'

export type Route = 'home' | 'history' | 'insights' | 'services'
const ROUTES: Route[] = ['home', 'history', 'insights', 'services']

function parse(): Route {
  const r = location.hash.replace(/^#\/?/, '') as Route
  return ROUTES.includes(r) ? r : 'home'
}

export const app = $state({
  route: parse(),
  status: undefined as Status | undefined,
  now: [] as Session[],
  history: [] as Session[], // last 30 days
  offline: false,
  identifying: undefined as Session | undefined, // the Identify dialog's session
  toast: '',
})

addEventListener('hashchange', () => { app.route = parse(); scrollTo(0, 0) })

export function go(r: Route) { location.hash = r === 'home' ? '/' : `/${r}` }

async function poll(fn: () => Promise<void>) {
  try { await fn(); app.offline = false } catch { app.offline = true }
}
export const loadNow = () => poll(async () => { const r = await api.now(); syncClock(r.server_time); app.now = r.sessions })
export const loadHistory = () => poll(async () => { app.history = (await api.history(30)).sessions })
export const loadStatus = () => poll(async () => { app.status = await api.status() })

/** Put a changed session back into the lists without waiting for the next poll. */
export function replaceSession(s: Session) {
  const swap = (l: Session[]) => l.map(x => (x.id === s.id ? s : x))
  app.history = swap(app.history)
  app.now = swap(app.now)
}

let toastTimer: ReturnType<typeof setTimeout>
export function toast(msg: string) {
  app.toast = msg
  clearTimeout(toastTimer)
  toastTimer = setTimeout(() => (app.toast = ''), 4000)
}

/** Sessions worth a look: finished, shown, not identified (and long enough to matter). */
export function toIdentify(list: Session[]) {
  return list.filter(s => !s.open && !s.hidden && !s.items.length && s.played_secs >= 300)
}

/** Items whose sending went wrong. */
export function sendProblems(list: Session[]) {
  return list.filter(s => !s.hidden && s.items.some(it =>
    Object.values(it.deliveries).some(d => d.status === 'error' || d.status === 'not_found')))
}
