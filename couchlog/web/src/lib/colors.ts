// A service's color in bars and charts (its brand color, toned for a dark page).
const C: Record<string, string> = {
  netflix: '#E5484D', hbomax: '#5B7CFF', plex: '#EBAF00', youtube: '#FF6B6B', tubi: '#A275FF',
  disney: '#3BA3FF', prime: '#22C3E6', appletv: '#C9CDD4', paramount: '#4F8BFF', crunchyroll: '#FF8A3D',
}
export const serviceColor = (key: string) => C[key] ?? '#6EA8FE'
