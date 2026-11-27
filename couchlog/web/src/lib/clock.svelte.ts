// The dashboard's clock is the server's: every API answer carries
// server_time, and the offset to this device's clock is kept, so a phone with
// the wrong time still shows the right "ends at".
let offset = 0
export const clock = $state({ now: Date.now() })

export function syncClock(serverTime: string) {
  const t = Date.parse(serverTime)
  if (!Number.isNaN(t)) offset = t - Date.now()
  clock.now = Date.now() + offset
}

setInterval(() => { clock.now = Date.now() + offset }, 1000)
