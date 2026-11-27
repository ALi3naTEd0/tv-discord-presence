import type { Item, Session } from './api'

const hm = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' })
const day = new Intl.DateTimeFormat(undefined, { weekday: 'long' })

export const time = (iso: string | number) => hm.format(new Date(iso))

/** "Thursday afternoon · 2:31 PM" */
export function moment(ms: number) {
  const d = new Date(ms), h = d.getHours()
  const part = h < 5 ? 'night' : h < 12 ? 'morning' : h < 19 ? 'afternoon' : 'night'
  return `${day.format(d)} ${part} · ${hm.format(d)}`
}

/** 4320 -> "1:12:00", 860 -> "14:20" */
export function clockTime(secs: number) {
  const s = Math.max(0, Math.round(secs)), h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60)
  const ss = String(s % 60).padStart(2, '0')
  return h ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`
}

export function minutes(secs: number) {
  const m = Math.round(secs / 60)
  return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, '0')}`
}

/** The item playing: the last one (autoplay splits, multi-episode sessions). */
export const current = (s: Session): Item | undefined => s.items[s.items.length - 1]

export function episodeLine(it: Item) {
  if (it.kind !== 'episode') return it.year ? String(it.year) : ''
  const se = it.season && it.episode ? `S${it.season} · E${it.episode}` : ''
  return [se, it.episode_title].filter(Boolean).join(' · ')
}

/** Title for a session, identified or not. */
export function titleOf(s: Session) {
  const it = current(s)
  if (it) return it.title
  return s.hint?.title || s.app.name
}

const SINK = { trakt: 'Trakt', simkl: 'Simkl' } as Record<string, string>
export const sinkName = (k: string) => SINK[k] ?? k
