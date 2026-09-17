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
