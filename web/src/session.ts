// Who is signed in, and how that changes.
//
// A module rather than state in App.vue, because a 401 can arrive from any
// request at any time — a session that expired while a tab sat open, or a
// Sign out — and every one has to reach the same login gate.
import { ref } from 'vue'
import { apiURL, BASE } from './base'

export interface User {
  sub: string
  email?: string
  name?: string
  groups?: string[]
}

// ready flips true once we know whether auth is on and who we are, so the shell
// shows nothing (not a flash of the app) until then.
export const ready = ref(false)
export const authEnabled = ref(false)
export const user = ref<User | null>(null)

// signedOut is raised when a request comes back 401. The app shows the login
// gate and nothing else while it is set.
export const signedOut = ref(false)
const loginPath = ref('')

// loadConfig asks the server whether auth is on and who we are. Called once at
// boot, before the app renders.
export async function loadConfig(): Promise<void> {
  try {
    const res = await fetch(apiURL('config'), { credentials: 'same-origin' })
    if (res.ok) {
      const cfg = await res.json()
      authEnabled.value = !!cfg.auth_enabled
      user.value = cfg.user ?? null
      if (authEnabled.value && !user.value) signedOut.value = true
    }
  } catch {
    // The server is unreachable. Render anyway; API calls will surface it.
  } finally {
    ready.value = true
  }
}

export function loginURL(): string {
  return loginPath.value || apiURL('auth/login')
}

// markSignedOut is called from the API layer on any 401. It does NOT navigate:
// bouncing straight to the IdP is what makes "Sign out" appear to do nothing,
// since the provider still has a session and signs the browser back in.
export function markSignedOut(url?: string) {
  if (url) loginPath.value = url
  user.value = null
  signedOut.value = true
}

// begin sends the browser to the provider, remembering where to return to.
export function begin() {
  window.location.assign(`${loginURL()}?return_to=${encodeURIComponent(returnTo())}`)
}

// logout drops our session, then optionally the IdP's if the server says so.
export async function logout() {
  try {
    const res = await fetch(apiURL('auth/logout'), { method: 'POST', credentials: 'same-origin' })
    const body = await res.json().catch(() => ({}))
    if (typeof body?.provider_logout_url === 'string') {
      window.location.assign(body.provider_logout_url)
      return
    }
  } catch {
    /* even if the call fails, the local session is what mattered */
  }
  user.value = null
  signedOut.value = true
}

// returnTo is where to come back to after login, relative to the mount point,
// which is how the server expects it (it prepends base_path).
function returnTo(): string {
  const base = BASE.endsWith('/') ? BASE : BASE + '/'
  let p = window.location.pathname
  if (base !== '/' && p.startsWith(base)) p = '/' + p.slice(base.length)
  return p + window.location.search + window.location.hash
}

// probe asks the server who we are, for EventSource — which cannot report an
// HTTP status, so a stream refused with 401 looks like a network blip until we
// ask.
export async function probe(): Promise<void> {
  try {
    const res = await fetch(apiURL('config'), { credentials: 'same-origin' })
    if (!res.ok) return
    const cfg = await res.json()
    if (cfg.auth_enabled && !cfg.user) markSignedOut()
  } catch {
    /* unreachable is an outage, not a logout */
  }
}
