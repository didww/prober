// Theme: light, dark, or follow the OS. The chosen mode is persisted; the
// resolved theme (only light or dark) is what the stylesheet keys off, stamped
// on <html> as data-theme.
import { computed, ref } from 'vue'

export type Mode = 'light' | 'dark' | 'auto'
export type Theme = 'light' | 'dark'

const KEY = 'prober.theme'

function stored(): Mode {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'light' || v === 'dark' || v === 'auto') return v
  } catch {
    /* private mode */
  }
  return 'auto'
}

export const mode = ref<Mode>(stored())

const mql = window.matchMedia('(prefers-color-scheme: dark)')
const systemDark = ref(mql.matches)
mql.addEventListener('change', (e) => {
  systemDark.value = e.matches
})

export const resolved = computed<Theme>(() =>
  mode.value === 'auto' ? (systemDark.value ? 'dark' : 'light') : mode.value,
)

export function setMode(m: Mode) {
  mode.value = m
  try {
    localStorage.setItem(KEY, m)
  } catch {
    /* private mode */
  }
  apply()
}

export function apply() {
  document.documentElement.dataset.theme = resolved.value
}

export function initTheme() {
  apply()
  mql.addEventListener('change', apply)
}
