// Per-operator display preferences, kept in the browser. For a diagnostics tool
// these are purely cosmetic: how to read a timestamp, nothing sent to the
// server.
import { computed, ref } from 'vue'

export type TimeFormat = '12h' | '24h'

const FMT_KEY = 'prober.timeFormat'

function storedFormat(): TimeFormat {
  try {
    const v = localStorage.getItem(FMT_KEY)
    if (v === '12h' || v === '24h') return v
  } catch {
    /* private mode */
  }
  // Default to the operator's locale rather than imposing one.
  return new Intl.DateTimeFormat(undefined, { hour: 'numeric' }).resolvedOptions().hour12
    ? '12h'
    : '24h'
}

export const timeFormat = ref<TimeFormat>(storedFormat())

export function setTimeFormat(f: TimeFormat) {
  timeFormat.value = f
  try {
    localStorage.setItem(FMT_KEY, f)
  } catch {
    /* private mode */
  }
}

export const hour12 = computed(() => timeFormat.value === '12h')

// A wall-clock timestamp in the operator's locale and chosen 12/24h format.
export function clockTime(ms: number): string {
  return new Date(ms).toLocaleTimeString(undefined, {
    hour: 'numeric',
    minute: '2-digit',
    second: '2-digit',
    hour12: hour12.value,
  })
}

// A compact human duration from an ISO timestamp to now: "3d 4h", "12m", "45s".
export function since(iso: string, now = Date.now()): string {
  if (!iso) return '—'
  const ms = now - new Date(iso).getTime()
  if (!isFinite(ms) || ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m ${sec}s`
  return `${sec}s`
}
