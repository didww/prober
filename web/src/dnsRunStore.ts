// Live state of one DNS lookup run, built from its SSE events. One SiteState
// per site; the DNS tool lays these out as a matrix, sites down and record
// types across, so answers compare across sites at a glance.
import { reactive } from 'vue'
import { cancelRun, startDnsRun, subscribe, type DnsRecord, type DnsRunEvent, type DnsStartRequest } from './api'

export type SiteStatus = 'queued' | 'running' | 'done' | 'error' | 'offline'

// A column of the matrix: one question asked at every site. The agent reports
// each answer with its record type and the full owner name; an SRV answer is
// matched to its column by the _service._proto prefix of that name.
export interface DnsColumn {
  key: string
  type: 'A' | 'AAAA' | 'SRV' | 'PTR'
  prefix: string
  label: string
}

// The questions asked for a host name, and the one asked for an address.
export const NAME_COLUMNS: DnsColumn[] = [
  { key: 'a', type: 'A', prefix: '', label: 'A' },
  { key: 'aaaa', type: 'AAAA', prefix: '', label: 'AAAA' },
  { key: 'sip_udp', type: 'SRV', prefix: '_sip._udp.', label: '_sip._udp' },
  { key: 'sip_tcp', type: 'SRV', prefix: '_sip._tcp.', label: '_sip._tcp' },
  { key: 'sip_tls', type: 'SRV', prefix: '_sip._tls.', label: '_sip._tls' },
  { key: 'sips_tcp', type: 'SRV', prefix: '_sips._tcp.', label: '_sips._tcp' },
]
export const ADDRESS_COLUMNS: DnsColumn[] = [{ key: 'ptr', type: 'PTR', prefix: '', label: 'PTR' }]
const COLUMNS = [...NAME_COLUMNS, ...ADDRESS_COLUMNS]

export function columnOf(recordType: string, name: string): DnsColumn | undefined {
  return COLUMNS.find((c) => c.type === recordType && name.startsWith(c.prefix))
}

// isAddress is a quick check of what was typed, to pick the columns; the
// agent does the real parsing.
export function isAddress(s: string): boolean {
  const v = s.trim().replace(/^\[|\]$/g, '')
  return /^\d{1,3}(\.\d{1,3}){3}$/.test(v) || (v.includes(':') && /^[0-9a-f:.%]+$/i.test(v))
}

export interface QueryState {
  done: boolean
  // The owner name actually asked for, once answered (the reverse name for
  // PTR).
  name: string
  records: DnsRecord[]
  status: string
  server: string
  error: string
  rttUs: number
}

export interface SiteState {
  site: string
  status: SiteStatus
  // The name as the agent normalised it, once started.
  name: string
  // The agent host's resolver addresses, when it could read them.
  nameservers: string[]
  queries: Record<string, QueryState>
  error: string
}

export interface RunState {
  id: string
  running: boolean
  // What was asked for, as typed.
  name: string
  // Whether the run is a reverse lookup, so the page shows the PTR column.
  // Guessed from the input at start, then settled by the first answer's
  // record type, since the agent has the final say on what is an address.
  reverse: boolean
  sites: Record<string, SiteState>
  order: string[]
}

function newSite(site: string): SiteState {
  const queries: Record<string, QueryState> = {}
  for (const c of COLUMNS) queries[c.key] = { done: false, name: '', records: [], status: '', server: '', error: '', rttUs: 0 }
  return { site, status: 'queued', name: '', nameservers: [], queries, error: '' }
}

export function createDnsRun(): {
  state: RunState
  start: (req: DnsStartRequest) => Promise<void>
  stop: () => void
} {
  const state = reactive<RunState>({ id: '', running: false, name: '', reverse: false, sites: {}, order: [] })
  let es: EventSource | null = null

  function reset(sites: string[]) {
    state.sites = {}
    state.order = sites.slice()
    for (const site of sites) state.sites[site] = newSite(site)
  }

  function apply(ev: DnsRunEvent) {
    const s = state.sites[ev.site]
    if (!s) return
    switch (ev.type) {
      case 'started':
        s.status = 'running'
        s.name = ev.target
        s.nameservers = ev.nameservers ?? []
        break
      case 'dns_result': {
        state.reverse = ev.record_type === 'PTR'
        const col = columnOf(ev.record_type, ev.name)
        if (!col) return
        const q = s.queries[col.key]
        q.done = true
        q.name = ev.name
        q.records = ev.records
        q.status = ev.status
        q.server = ev.server
        q.error = ev.error
        q.rttUs = ev.rtt_us
        break
      }
      case 'finished':
        s.status = 'done'
        break
      case 'error':
        s.status = /no connected agent/i.test(ev.message) ? 'offline' : 'error'
        s.error = ev.message
        break
    }
  }

  async function start(req: DnsStartRequest) {
    stop()
    const { id, sites } = await startDnsRun(req)
    reset(sites)
    state.id = id
    state.name = req.name
    state.reverse = isAddress(req.name)
    state.running = true
    es = subscribe<DnsRunEvent>(id, ['started', 'dns_result', 'finished', 'error'], apply, () => {
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
