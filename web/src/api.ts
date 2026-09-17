// The browser's view of the backend: the prober list, starting and cancelling
// runs, and the SSE event stream. The event shapes mirror the backend's
// type-tagged JSON union (internal/backend/convert.go).
import { apiURL } from './base'
import { markSignedOut, probe } from './session'

export interface Build {
  version: string
  commit: string
}

export interface Prober {
  site: string
  version: string
  hostname: string
  ipv4: boolean
  ipv6: boolean
}

export interface Agent {
  site: string
  version: string
  commit: string
  hostname: string
  ipv4: boolean
  ipv6: boolean
  sources: string[]
  started_at: string
  connected_at: string
  rtt_us: number | null
}

export type Protocol = 'icmp' | 'udp' | 'tcp'
export type Family = '' | '4' | '6'
export type Mode = 'mtr' | 'ping'

export interface StartRequest {
  target: string
  sites: string[]
  protocol: Protocol
  family: Family
  port?: number
  resolve_names: boolean
  cycles: number
  mode: Mode
}

export interface HopAddress {
  ip: string
  name: string
  count: number
}

export interface Hop {
  ttl: number
  addresses: HopAddress[]
  sample_us: number | null
  sent: number
  received: number
  loss_pct: number
  last_us: number
  best_us: number
  avg_us: number
  worst_us: number
  stdev_us: number
  jitter_us: number
}

// The discriminated union carried in each SSE "data:" payload.
export type RunEvent =
  | { type: 'started'; seq: number; site: string; target: string; resolved: string; source: string; protocol: string; family: string }
  | { type: 'cycle'; seq: number; site: string; number: number; reached_at: number; hops: Hop[] }
  | { type: 'finished'; seq: number; site: string; reason: string }
  | { type: 'error'; seq: number; site: string; code: string; message: string }

export async function getVersion(): Promise<Build> {
  return getJSON<Build>('version')
}

export async function listProbers(): Promise<Prober[]> {
  return (await getJSON<Prober[] | null>('probers')) ?? []
}

export async function listAgents(): Promise<Agent[]> {
  return (await getJSON<Agent[] | null>('agents')) ?? []
}

export async function startRun(req: StartRequest): Promise<{ id: string; sites: string[] }> {
  const res = await check(
    await fetch(apiURL('runs'), {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(req),
      credentials: 'same-origin',
    }),
  )
  return res.json()
}

export async function cancelRun(id: string): Promise<void> {
  await fetch(apiURL(`runs/${id}`), { method: 'DELETE', credentials: 'same-origin' })
}

// --- SIP OPTIONS ------------------------------------------------------------

export type SipTransport = 'udp' | 'tcp' | 'tls' | 'wss'

export interface SipStartRequest {
  target: string
  sites: string[]
  transport: SipTransport
  family: Family
  port?: number
  cycles: number
  interval_ms: number
  timeout_ms?: number
}

export type SipRunEvent =
  | { type: 'started'; seq: number; site: string; target: string; resolved: string; source: string; transport: string; family: string }
  | {
      type: 'sip_result'
      seq: number
      site: string
      cycle: number
      status_code: number
      reason: string
      rtt_us: number | null
      responded: boolean
      request: string
      response: string
      tls: boolean
      tls_valid: boolean
      tls_error: string
    }
  | { type: 'finished'; seq: number; site: string; reason: string }
  | { type: 'error'; seq: number; site: string; code: string; message: string }

export async function startSipRun(req: SipStartRequest): Promise<{ id: string; sites: string[] }> {
  const res = await check(
    await fetch(apiURL('sip-runs'), {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(req),
      credentials: 'same-origin',
    }),
  )
  return res.json()
}

// subscribe opens the run's SSE stream. The browser reconnects on its own and
// sends Last-Event-ID, so the backend replays what was missed.
// subscribe opens a run's SSE stream, registering listeners for the given
// event names and passing each parsed payload to onEvent. Generic over the
// event type so trace and SIP keep separate unions.
export function subscribe<T>(
  id: string,
  eventNames: string[],
  onEvent: (ev: T) => void,
  onDone: () => void,
): EventSource {
  const es = new EventSource(apiURL(`runs/${id}/events`), { withCredentials: true })
  const handle = (e: MessageEvent) => {
    try {
      onEvent(JSON.parse(e.data) as T)
    } catch {
      /* a frame we cannot parse is one event lost; the stream continues */
    }
  }
  for (const t of eventNames) {
    es.addEventListener(t, handle as EventListener)
  }
  es.addEventListener('done', () => {
    es.close()
    onDone()
  })
  // EventSource cannot report an HTTP status: a stream refused with 401 (an
  // expired session) closes with an empty error. Ask who we are to tell that
  // from a network blip.
  es.addEventListener('error', () => {
    if (es.readyState === EventSource.CLOSED) {
      onDone()
      void probe()
    }
  })
  return es
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await check(await fetch(apiURL(path), { credentials: 'same-origin' }))
  return (await res.json()) as T
}

// check raises the login gate on a 401 rather than throwing a bare error, so an
// expired session shows the sign-in screen instead of a red panel.
async function check(res: Response): Promise<Response> {
  if (res.ok) return res
  let loginURL: string | undefined
  let message = `HTTP ${res.status}`
  try {
    const body = await res.clone().json()
    if (typeof body?.error === 'string') message = body.error
    if (typeof body?.login_url === 'string') loginURL = body.login_url
  } catch {
    /* a proxy may answer with HTML; the status is all we have */
  }
  if (res.status === 401) markSignedOut(loginURL)
  throw new Error(message)
}
