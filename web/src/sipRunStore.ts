// Live state of one SIP OPTIONS run, built from its SSE events. One SiteState
// per site; the SIP tool renders these as rows with an expandable request and
// response.
import { reactive } from 'vue'
import { cancelRun, startSipRun, subscribe, type SipRunEvent, type SipStartRequest } from './api'

export type SiteStatus = 'queued' | 'running' | 'done' | 'error' | 'offline'

export interface SiteState {
  site: string
  status: SiteStatus
  resolved: string
  source: string
  transport: string
  cycles: number
  // Latest result.
  code: number
  reason: string
  rttUs: number | null
  responded: boolean
  tls: boolean
  tlsValid: boolean
  tlsError: string
  request: string
  response: string
  // Per-cycle outcomes for the timeline strip.
  timeline: { rtt: number | null; code: number }[]
  sent: number
  ok: number
  error: string
  expanded: boolean
}

export interface RunState {
  id: string
  running: boolean
  sites: Record<string, SiteState>
  order: string[]
}

function newSite(site: string): SiteState {
  return {
    site,
    status: 'queued',
    resolved: '',
    source: '',
    transport: '',
    cycles: 0,
    code: 0,
    reason: '',
    rttUs: null,
    responded: false,
    tls: false,
    tlsValid: false,
    tlsError: '',
    request: '',
    response: '',
    timeline: [],
    sent: 0,
    ok: 0,
    error: '',
    expanded: false,
  }
}

const TIMELINE_MAX = 120

export function createSipRun(): {
  state: RunState
  start: (req: SipStartRequest) => Promise<void>
  stop: () => void
} {
  const state = reactive<RunState>({ id: '', running: false, sites: {}, order: [] })
  let es: EventSource | null = null

  function reset(sites: string[]) {
    state.sites = {}
    state.order = sites.slice()
    for (const site of sites) state.sites[site] = newSite(site)
  }

  function apply(ev: SipRunEvent) {
    const s = state.sites[ev.site]
    if (!s) return
    switch (ev.type) {
      case 'started':
        s.status = 'running'
        s.resolved = ev.resolved
        s.source = ev.source
        s.transport = ev.transport
        break
      case 'sip_result':
        s.status = 'running'
        s.cycles = ev.cycle
        s.sent++
        s.code = ev.status_code
        s.reason = ev.reason
        s.rttUs = ev.rtt_us
        s.responded = ev.responded
        s.tls = ev.tls
        s.tlsValid = ev.tls_valid
        s.tlsError = ev.tls_error
        s.request = ev.request
        s.response = ev.response
        if (ev.responded && ev.status_code >= 200 && ev.status_code < 300) s.ok++
        s.timeline.push({ rtt: ev.rtt_us, code: ev.status_code })
        if (s.timeline.length > TIMELINE_MAX) s.timeline.splice(0, s.timeline.length - TIMELINE_MAX)
        break
      case 'finished':
        s.status = 'done'
        break
      case 'error':
        s.status = /no connected agent/i.test(ev.message) ? 'offline' : 'error'
        s.error = ev.message
        break
    }
  }

  async function start(req: SipStartRequest) {
    stop()
    const { id, sites } = await startSipRun(req)
    reset(sites)
    state.id = id
    state.running = true
    es = subscribe<SipRunEvent>(id, ['started', 'sip_result', 'finished', 'error'], apply, () => {
      state.running = false
    })
  }

  function stop() {
    if (es) {
      es.close()
      es = null
    }
    if (state.id && state.running) void cancelRun(state.id)
    state.running = false
  }

  return { state, start, stop }
}
