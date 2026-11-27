// Types and calls for Couchlog's /api/v1 (internal/api in the Go module).

export interface Quality {
  resolution?: string
  uhd?: boolean
  codec?: string
  hdr?: string
  width?: number
  height?: number
  label: string
}

export interface Link { site: 'imdb' | 'tmdb' | 'trakt'; name: string; url: string }

export interface Delivery { status: string; error?: string; updated_at: string }

export interface Item {
  id: number
  kind: 'movie' | 'episode'
  title: string
  year?: number
  season?: number
  episode?: number
  episode_title?: string
  poster?: string
  runtime_min?: number
  runtime_estimated?: boolean
  ids: { tmdb?: number; tvdb?: number; imdb?: string }
  links: Link[]
  deliveries: Record<string, Delivery>
}

export interface Session {
  id: number
  device: { id: number; kind: string; name: string }
  app: { id: string; name: string }
  started_at: string
  ended_at?: string
  last_seen_at: string
  open: boolean
  hidden?: string
  played_secs: number
  position_secs?: number
  live_position_secs?: number
  hint?: { title?: string; subtitle?: string; season?: number; episode?: number }
  quality?: Quality
  items: Item[]
  progress?: number
  ends_at?: string
  auto_next_from?: number
}

export interface Sessions { server_time: string; sessions: Session[] }

export interface Status {
  version: string
  server_time: string
  sinks: string[]
  read_only: boolean
  devices: { id: number; kind: string; name: string }[]
}

async function get<T>(path: string): Promise<T> {
  const r = await fetch(path, { headers: { Accept: 'application/json' } })
  if (!r.ok) throw new Error(`${path}: ${r.status}`)
  return r.json()
}

export const api = {
  status: () => get<Status>('/api/v1/status'),
  now: () => get<Sessions>('/api/v1/now'),
  history: (days: number) => get<Sessions>(`/api/v1/history?days=${days}`),
}
