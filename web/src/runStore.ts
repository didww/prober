// The live state of one run, built from its SSE events. One SiteState per site
// the run fanned out to; the trace tool renders these as ping.pe-style rows.
import { reactive } from 'vue'
import { cancelRun, startRun, subscribe, type Hop, type RunEvent, type StartRequest } from './api'

export type SiteStatus = 'queued' | 'running' | 'done' | 'error' | 'offline'

export interface SiteState {
  site: string
  status: SiteStatus
  resolved: string
  source: string
  protocol: string
  reachedAt: number
  hops: Hop[]
  // Destination RTT per cycle (µs, null = loss), for the timeline strip.
  timeline: (number | null)[]
  cycles: number
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
    protocol: '',
    reachedAt: 0,
    hops: [],
    timeline: [],
    cycles: 0,
    error: '',
    expanded: false,
  }
}

// destHop is the hop the timeline tracks: the one the target answered at, or
// the last hop while the target has not been reached.
function destHop(s: SiteState): Hop | undefined {
  if (s.reachedAt > 0) return s.hops.find((h) => h.ttl === s.reachedAt)
  return s.hops[s.hops.length - 1]
}

export function createRun(): {
  state: RunState
  start: (req: StartRequest) => Promise<void>
  stop: () => void
} {
  const state = reactive<RunState>({ id: '', running: false, sites: {}, order: [] })
  let es: EventSource | null = null

  function reset(sites: string[]) {
    state.sites = {}
    state.order = sites.slice()
    for (const site of sites) state.sites[site] = newSite(site)
  }

  function apply(ev: RunEvent) {
    const s = state.sites[ev.site]
    if (!s) return
    switch (ev.type) {
      case 'started':
        s.status = 'running'
        s.resolved = ev.resolved
        s.source = ev.source
        s.protocol = ev.protocol
        break
      case 'cycle':
        s.status = 'running'
        s.reachedAt = ev.reached_at
        s.hops = ev.hops
        s.cycles = ev.number
        {
          const d = destHop(s)
          s.timeline.push(d?.sample_us ?? null)
        }
        break
      case 'finished':
        s.status = 'done'
        break
      case 'error':
        // A site with no connected agent is reported as an error before it ever
        // runs; call that "offline" so the row reads right.
        s.status = /no connected agent/i.test(ev.message) ? 'offline' : 'error'
        s.error = ev.message
        break
    }
  }

  async function start(req: StartRequest) {
    stop()
    const { id, sites } = await startRun(req)
    reset(sites)
    state.id = id
    state.running = true
    es = subscribe(
      id,
      apply,
      () => {
        state.running = false
      },
    )
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

export { destHop }
