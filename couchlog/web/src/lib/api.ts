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
  devices: DeviceInfo[]
  identify: boolean
  discord: 'on' | 'dry-run' | 'off'
}

export interface DeviceState { state: 'playing' | 'paused' | 'idle' | 'off' | 'waiting'; since?: string; app?: string }
export interface DeviceInfo { id: number; kind: string; name: string; state?: DeviceState }
export interface FoundDevice { kind: 'webos' | 'androidtv'; name: string; host: string; model?: string; adb: boolean; added_as?: string }
export interface NetflixStatus { enabled: boolean; saved: boolean; saved_at?: string; profile?: string; connecting: boolean; message?: string; error?: string }

export interface ServiceInfo { key: string; name: string; free?: 'library' | 'free' | 'ads' }
export interface Price { amount: number; currency: string; period: 'month' | 'year'; monthly: number }
export interface ServiceHours extends ServiceInfo { hours: number; price?: Price; per_hour?: number }

export interface Insights {
  days: number
  from: string
  first_seen?: string
  hours: number
  sessions: number
  movies: { n: number; hours: number }
  episodes: { n: number; hours: number }
  sent: number
  services: ServiceHours[]
  weeks: { start: string; hours: number }[]
  top_shows: { tmdb: number; title: string; poster?: string; episodes: number; hours: number }[]
  currency: string
}

export interface ServiceRow extends ServiceHours { devices: string[]; profile?: string; profile_from?: 'you' | 'netflix' }
export interface Services { currency: string; services: ServiceRow[] }

export interface UpNext {
  tmdb: number
  title: string
  poster?: string
  season: number
  episode: number
  episode_title?: string
  service: ServiceInfo
  last_at: string
}

export interface Result { kind: 'movie' | 'tv'; tmdb: number; title: string; original_title?: string; year?: number; poster?: string; overview?: string }
export interface Show { tmdb: number; title: string; year?: number; poster?: string; seasons: { number: number; episodes: number }[] }
export interface Episode { number: number; name?: string; runtime_min?: number; air_date?: string; watched?: boolean }

export interface IdentifyRequest { kind: 'movie' | 'tv'; tmdb: number; season?: number; episodes?: number[]; remember?: boolean }

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const r = await fetch(path, {
    method,
    headers: body === undefined ? { Accept: 'application/json' } : { Accept: 'application/json', 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!r.ok) throw new Error((await r.text()).trim() || `${path}: ${r.status}`)
  return r.status === 204 ? (undefined as T) : r.json()
}
const get = <T>(path: string) => call<T>('GET', path)

export const api = {
  status: () => get<Status>('/api/v1/status'),
  now: () => get<Sessions>('/api/v1/now'),
  history: (days: number) => get<Sessions>(`/api/v1/history?days=${days}`),
  insights: (days: number) => get<Insights>(`/api/v1/insights?days=${days}`),
  upNext: () => get<UpNext[]>('/api/v1/upnext'),
  services: () => get<Services>('/api/v1/services'),
  search: (q: string) => get<Result[]>(`/api/v1/search?q=${encodeURIComponent(q)}`),
  show: (tmdb: number) => get<Show>(`/api/v1/shows/${tmdb}`),
  season: (tmdb: number, n: number) => get<Episode[]>(`/api/v1/shows/${tmdb}/seasons/${n}`),
  identify: (id: number, req: IdentifyRequest) => call<Session>('POST', `/api/v1/sessions/${id}/identify`, req),
  setHidden: (id: number, hidden: boolean) => call<Session>('POST', `/api/v1/sessions/${id}/hidden`, { hidden }),
  setPrice: (key: string, price: number, currency: string, period: string) =>
    call<Services>('PUT', `/api/v1/services/${encodeURIComponent(key)}`, { price, currency, period }),
  setCurrency: (value: string) => call<void>('PUT', '/api/v1/settings/currency', { value }),
  scan: () => get<FoundDevice[]>('/api/v1/setup/scan'),
  addDevice: (kind: string, host: string, name: string) => call<{ id: number }>('POST', '/api/v1/setup/devices', { kind, host, name }),
  netflix: () => get<NetflixStatus>('/api/v1/netflix'),
  connectNetflix: () => call<NetflixStatus>('POST', '/api/v1/netflix/connect', {}),
  setProfile: (key: string, value: string) => call<void>('PUT', `/api/v1/settings/profile:${encodeURIComponent(key)}`, { value }),
}
