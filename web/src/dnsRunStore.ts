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
  type: 'A' | 'AAAA' | 'SRV'
  prefix: string
  label: string
}

export const COLUMNS: DnsColumn[] = [
  { key: 'a', type: 'A', prefix: '', label: 'A' },
  { key: 'aaaa', type: 'AAAA', prefix: '', label: 'AAAA' },
  { key: 'sip_udp', type: 'SRV', prefix: '_sip._udp.', label: '_sip._udp' },
  { key: 'sip_tcp', type: 'SRV', prefix: '_sip._tcp.', label: '_sip._tcp' },
  { key: 'sip_tls', type: 'SRV', prefix: '_sip._tls.', label: '_sip._tls' },
  { key: 'sips_tcp', type: 'SRV', prefix: '_sips._tcp.', label: '_sips._tcp' },
]

export function columnOf(recordType: string, name: string): DnsColumn | undefined {
  return COLUMNS.find((c) => c.type === recordType && name.startsWith(c.prefix))
}

export interface QueryState {
  done: boolean
  records: DnsRecord[]
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
  sites: Record<string, SiteState>
  order: string[]
}

function newSite(site: string): SiteState {
  const queries: Record<string, QueryState> = {}
  for (const c of COLUMNS) queries[c.key] = { done: false, records: [], error: '', rttUs: 0 }
  return { site, status: 'queued', name: '', nameservers: [], queries, error: '' }
}

export function createDnsRun(): {
  state: RunState
  start: (req: DnsStartRequest) => Promise<void>
  stop: () => void
} {
  const state = reactive<RunState>({ id: '', running: false, sites: {}, order: [] })
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
        const col = columnOf(ev.record_type, ev.name)
        if (!col) return
        const q = s.queries[col.key]
        q.done = true
        q.records = ev.records
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
