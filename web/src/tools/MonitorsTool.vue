<script setup lang="ts">
// The Monitors page: every configured monitor with what it probes and one
// state across its sites, kept current by a server-sent stream that carries
// only changes. Tabs split the list by state, problems first. Expanding a
// row shows each site's latest outcome, polled while open, and for a trace
// monitor the reports shipped to VictoriaLogs.
import { computed, onMounted, onBeforeUnmount, reactive, ref } from 'vue'
import {
  getMonitor,
  listMonitors,
  listProbers,
  listTraceReports,
  subscribeMonitorStatus,
  REPORT_RANGES,
  type Monitor,
  type MonitorAggregate,
  type MonitorSiteStatus,
  type MonitorState,
  type ReportRange,
  type TraceReport,
} from '../api'
import { dateTime, since } from '../settings'
import { lossPct } from '../format'

// --- the list and its state ---------------------------------------------------

const monitors = ref<Monitor[]>([])
const states = reactive<Record<string, MonitorAggregate>>({})
const version = ref(0)
const loaded = ref(false)
const loadError = ref('')
const live = ref(false)
const now = ref(Date.now())
// The connected agents, for how many sites a monitor without a site list
// is expected at.
const connected = ref<string[]>([])

const NO_DATA: MonitorAggregate = { state: 'nodata', up: 0, down: 0, stale: 0, sites: 0 }
function stateOf(id: string): MonitorAggregate {
  return states[id] ?? NO_DATA
}

// The list is fetched once and again whenever the stream reports a
// configuration version other than the one we hold. A request that fails
// is retried with growing delay, since nothing else would refetch it.
let loading = false
let reloadPending = false
let streamVersion = 0
let haveSnapshot = false
let retry = 0
let retryDelay = 5000

async function load() {
  if (loading) {
    reloadPending = true
    return
  }
  loading = true
  clearTimeout(retry)
  try {
    const l = await listMonitors()
    monitors.value = l.monitors
    version.value = l.version
    // The stream is the authority on state once its snapshot has arrived;
    // the list's copy can be older than what it has delivered since.
    if (!haveSnapshot) for (const m of l.monitors) states[m.id] = m.state
    const ids = new Set(l.monitors.map((m) => m.id))
    for (const id of Object.keys(details)) if (!ids.has(id)) delete details[id]
    loadError.value = ''
    retryDelay = 5000
  } catch (e) {
    loadError.value = String(e)
    retry = window.setTimeout(() => void load(), retryDelay)
    retryDelay = Math.min(retryDelay * 2, 60000)
  } finally {
    loaded.value = true
    loading = false
  }
  if (reloadPending || (streamVersion && streamVersion !== version.value)) {
    reloadPending = false
    void load()
  }
}

async function loadProbers() {
  try {
    connected.value = (await listProbers()).map((p) => p.site)
  } catch {
    /* the count is advisory; the next tick tries again */
  }
}

let es: EventSource | null = null
function connect() {
  es = subscribeMonitorStatus(
    (ev) => {
      switch (ev.type) {
        case 'snapshot':
          // A snapshot is the whole truth: a monitor missing from it has no
          // data. A version other than ours means the configuration changed
          // while we were away.
          for (const id of Object.keys(states)) delete states[id]
          Object.assign(states, ev.states)
          haveSnapshot = true
          live.value = true
          streamVersion = ev.version
          if (loaded.value && ev.version !== version.value) void load()
          break
        case 'change': {
          const { type: _t, id, ...agg } = ev
          states[id] = agg
          break
        }
        case 'changes':
          Object.assign(states, ev.states)
          break
        case 'reload':
          streamVersion = ev.version
          void load()
          break
      }
    },
    () => {
      live.value = false
    },
  )
}

// --- tabs and filters ------------------------------------------------------------

type Tab = 'issues' | 'down' | 'partial' | 'up' | 'nodata' | 'all'
const TABS: { key: Tab; label: string }[] = [
  { key: 'issues', label: 'Issues' },
  { key: 'down', label: 'Down' },
  { key: 'partial', label: 'Partial' },
  { key: 'up', label: 'Up' },
  { key: 'nodata', label: 'No data' },
  { key: 'all', label: 'All' },
]
const tab = ref<Tab>('issues')
const filter = ref('')
const kind = ref<'' | 'trace' | 'ping' | 'sip'>('')

function inTab(t: Tab, st: MonitorState): boolean {
  if (t === 'all') return true
  if (t === 'issues') return st === 'down' || st === 'partial'
  return st === t
}

// Worst first within a tab.
const RANK: Record<MonitorState, number> = { down: 0, partial: 1, nodata: 2, up: 3 }

const counts = computed(() => {
  const c: Record<Tab, number> = { issues: 0, down: 0, partial: 0, up: 0, nodata: 0, all: 0 }
  for (const m of monitors.value) {
    const st = stateOf(m.id).state
    for (const t of TABS) if (inTab(t.key, st)) c[t.key]++
  }
  return c
})

const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  const list = monitors.value.filter((m) => {
    if (!inTab(tab.value, stateOf(m.id).state)) return false
    if (kind.value && m.kind !== kind.value) return false
    if (!q) return true
    return [m.id, m.target, ...Object.entries(m.labels).map(([k, v]) => `${k}=${v}`)].some((s) =>
      s.toLowerCase().includes(q),
    )
  })
  return list.sort((a, b) => RANK[stateOf(a.id).state] - RANK[stateOf(b.id).state] || a.id.localeCompare(b.id))
})

// --- presentation helpers ----------------------------------------------------------

const STATE_LABEL: Record<MonitorState, string> = { up: 'up', down: 'down', partial: 'partial', nodata: 'no data' }

function sinceText(id: string): string {
  const at = stateOf(id).since
  return at ? since(at, now.value) : '—'
}

// "up / expected" where expected is the configured site list, or every
// connected agent for a monitor that runs everywhere, so a site that has
// never reported is not hidden behind a clean "1/1 up".
function sitesText(m: Monitor, a: MonitorAggregate): string {
  const expected = Math.max(m.sites.length || connected.value.length, a.sites)
  if (expected === 0) return '—'
  const parts = [`${a.up}/${expected} up`]
  if (a.stale) parts.push(`${a.stale} stale`)
  if (expected > a.sites) parts.push(`${expected - a.sites} no data`)
  return parts.join(', ')
}

function familyLabel(f?: string): string {
  if (!f) return ''
  if (f.includes('6')) return 'IPv6'
  if (f.includes('4')) return 'IPv4'
  return ''
}

// How a monitor probes, in one line.
function params(m: Monitor): string {
  const p: string[] = []
  if (m.kind === 'sip') {
    const s = m.sip
    p.push((s?.transport || 'udp') + (s?.port ? ':' + s.port : ''))
    if (familyLabel(s?.family)) p.push(familyLabel(s?.family))
    if (s?.cycles) p.push(`${s.cycles} cycles`)
    if (s?.timeout_ms) p.push(`timeout ${s.timeout_ms / 1000}s`)
  } else {
    const t = m.trace
    p.push((t?.protocol || 'icmp') + (t?.port ? ':' + t.port : ''))
    if (familyLabel(t?.family)) p.push(familyLabel(t?.family))
    if (t?.cycles) p.push(`${t.cycles} cycles`)
    if (m.kind === 'trace' && t?.max_ttl) p.push(`max ttl ${t.max_ttl}`)
  }
  p.push(`every ${m.interval_s}s`)
  return p.join(' · ')
}

function siteClass(s: MonitorSiteStatus): string {
  if (s.up == null) return 'none'
  if (s.stale) return 'stale'
  return s.up ? 'ok' : 'bad'
}
function siteText(s: MonitorSiteStatus): string {
  if (s.up == null) return s.connected ? 'waiting' : 'no data'
  if (s.stale) return 'stale'
  return s.up ? 'up' : 'down'
}
function siteDetail(m: Monitor, s: MonitorSiteStatus): string {
  if (s.up == null) return '—'
  if (s.error) return s.error
  if (m.kind === 'sip') return s.responded ? `${s.code} ${s.reason}`.trim() : 'timeout'
  if (m.kind === 'ping') return s.reached ? 'answered' : 'no answer'
  return s.reached ? `reached, ${s.hops} hops` : `unreached after ${s.hops} hops`
}
function lossText(m: Monitor, s: MonitorSiteStatus): string {
  if (s.up == null || m.kind === 'sip') return '—'
  return lossPct(s.loss_pct) + '%'
}

// --- expanded rows ---------------------------------------------------------------------

interface History {
  open: boolean
  site: string
  range: ReportRange
  loading: boolean
  error: string
  reports: TraceReport[]
  expanded: Record<number, boolean>
}
interface Detail {
  open: boolean
  loading: boolean
  refreshing: boolean
  error: string
  status: MonitorSiteStatus[]
  history: History
}
const details = reactive<Record<string, Detail>>({})

function newDetail(): Detail {
  return {
    open: false,
    loading: false,
    refreshing: false,
    error: '',
    status: [],
    history: { open: false, site: '', range: '24h', loading: false, error: '', reports: [], expanded: {} },
  }
}

async function toggle(m: Monitor) {
  const d = details[m.id] ?? (details[m.id] = newDetail())
  d.open = !d.open
  if (d.open) {
    d.loading = d.status.length === 0
    await refreshDetail(m.id)
  }
}

// refreshDetail fetches one open monitor's per-site outcomes; a fetch still
// in flight is not overtaken by the next tick's.
async function refreshDetail(id: string) {
  const d = details[id]
  if (!d || d.refreshing) return
  d.refreshing = true
  try {
    d.status = (await getMonitor(id)).status
    d.error = ''
  } catch (e) {
    d.error = String(e)
  } finally {
    d.loading = false
    d.refreshing = false
  }
}

async function toggleHistory(m: Monitor) {
  const h = details[m.id]?.history
  if (!h) return
  h.open = !h.open
  if (h.open && h.reports.length === 0 && !h.error) await loadHistory(m.id)
}

async function loadHistory(id: string) {
  const h = details[id]?.history
  if (!h) return
  h.loading = true
  h.error = ''
  try {
    h.reports = await listTraceReports(id, { site: h.site || undefined, range: h.range, limit: 100 })
    h.expanded = {}
  } catch (e) {
    h.error = String(e)
    h.reports = []
  } finally {
    h.loading = false
  }
}

let poll = 0
let tick = 0
let probers = 0
onMounted(() => {
  void load()
  void loadProbers()
  connect()
  // Only expanded rows cost anything: their outcomes refresh every few
  // seconds while open.
  poll = window.setInterval(() => {
    for (const [id, d] of Object.entries(details)) if (d.open) void refreshDetail(id)
  }, 5000)
  probers = window.setInterval(loadProbers, 60000)
  tick = window.setInterval(() => (now.value = Date.now()), 1000)
})
onBeforeUnmount(() => {
  clearInterval(poll)
  clearInterval(probers)
  clearInterval(tick)
  clearTimeout(retry)
  es?.close()
})
</script>

<template>
  <div class="monitors">
    <header class="bar">
      <h1>Monitors</h1>
      <span class="count">{{ monitors.length }} configured</span>
      <span class="live" :class="{ on: live }" :title="live ? 'status updates are live' : 'reconnecting to status updates'">
        {{ live ? 'live' : 'reconnecting…' }}
      </span>
      <span class="spacer" />
      <label for="mon-kind" class="sr-only">Kind</label>
      <select id="mon-kind" v-model="kind" class="ctl">
        <option value="">all kinds</option>
        <option value="trace">trace</option>
        <option value="ping">ping</option>
        <option value="sip">sip</option>
      </select>
      <label for="mon-filter" class="sr-only">Filter monitors</label>
      <input id="mon-filter" v-model="filter" class="ctl filter" placeholder="Filter by id, target or label" spellcheck="false" />
    </header>

    <nav class="tabs" aria-label="Filter by state">
      <button
        v-for="t in TABS"
        :key="t.key"
        type="button"
        class="tab"
        :class="[t.key, { on: tab === t.key }]"
        :aria-pressed="tab === t.key"
        @click="tab = t.key"
      >
        {{ t.label }} <span class="n">{{ counts[t.key] }}</span>
      </button>
    </nav>

    <p v-if="loadError" class="err">{{ loadError }}</p>

    <table v-if="shown.length" class="grid">
      <thead>
        <tr>
          <th>State</th>
          <th>Sites</th>
          <th>Kind</th>
          <th>Monitor</th>
          <th>Target</th>
          <th>Probe</th>
          <th>Labels</th>
          <th class="r">Since</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="m in shown" :key="m.id">
          <tr class="row" :class="{ open: details[m.id]?.open }" tabindex="0" :aria-expanded="!!details[m.id]?.open" @click="toggle(m)" @keydown.enter.prevent="toggle(m)" @keydown.space.prevent="toggle(m)">
            <td><span class="st" :class="stateOf(m.id).state">{{ STATE_LABEL[stateOf(m.id).state] }}</span></td>
            <td class="mono small">{{ sitesText(m, stateOf(m.id)) }}</td>
            <td><span class="kind" :class="m.kind">{{ m.kind }}</span></td>
            <td class="id"><span class="caret" :class="{ open: details[m.id]?.open }">▶</span>{{ m.id }}</td>
            <td class="mono">{{ m.target }}</td>
            <td class="dim small">{{ params(m) }}</td>
            <td class="labels">
              <span v-for="(v, k) in m.labels" :key="k" class="label">{{ k }}=<b>{{ v }}</b></span>
            </td>
            <td class="r mono dim small">{{ sinceText(m.id) }}</td>
          </tr>

          <tr v-if="details[m.id]?.open" class="detail-row">
            <td colspan="8">
              <div class="detail">
                <div class="dhead">
                  <span class="dim small">
                    {{ details[m.id].loading ? 'Loading…' : 'each site\'s latest result, refreshed every 5 s' }}
                    <template v-if="m.sites.length"> · configured sites: {{ m.sites.join(', ') }}</template>
                  </span>
                  <span class="spacer" />
                  <button
                    v-if="m.kind === 'trace'"
                    type="button"
                    class="ctl hbtn"
                    :disabled="!m.history"
                    :title="m.history ? 'Reports shipped to VictoriaLogs' : 'VictoriaLogs is not configured'"
                    @click.stop="toggleHistory(m)"
                  >
                    {{ details[m.id].history.open ? 'Hide history' : 'History' }}
                  </button>
                </div>
                <p v-if="details[m.id].error" class="err">{{ details[m.id].error }}</p>
                <table class="sites">
                  <thead>
                    <tr>
                      <th>Site</th>
                      <th>Status</th>
                      <th>Resolved</th>
                      <th class="r">Loss</th>
                      <th class="r">RTT</th>
                      <th>Detail</th>
                      <th class="r">Last</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="s in details[m.id].status" :key="s.site">
                      <td class="site">
                        {{ s.site }}
                        <span v-if="!s.connected" class="off" title="no connected agent for this site">offline</span>
                      </td>
                      <td><span class="st" :class="siteClass(s)">{{ siteText(s) }}</span></td>
                      <td class="mono dim">{{ s.resolved || '—' }}</td>
                      <td class="r mono" :class="{ warnloss: s.up != null && m.kind !== 'sip' && s.loss_pct > 0 }">{{ lossText(m, s) }}</td>
                      <td class="r mono">{{ s.rtt_ms != null ? s.rtt_ms.toFixed(1) + ' ms' : '—' }}</td>
                      <td :class="{ bad: s.error }">{{ siteDetail(m, s) }}</td>
                      <td class="r mono dim">{{ s.at ? since(s.at, now) + ' ago' : '—' }}</td>
                    </tr>
                    <tr v-if="!details[m.id].loading && details[m.id].status.length === 0">
                      <td colspan="7" class="dim">No site runs this monitor: no agent is connected.</td>
                    </tr>
                  </tbody>
                </table>

                <div v-if="details[m.id].history.open" class="history">
                  <div class="hbar">
                    <label class="hctl">site
                      <select v-model="details[m.id].history.site" class="ctl" @change="loadHistory(m.id)">
                        <option value="">all</option>
                        <option v-for="s in details[m.id].status" :key="s.site" :value="s.site">{{ s.site }}</option>
                      </select>
                    </label>
                    <label class="hctl">range
                      <select v-model="details[m.id].history.range" class="ctl" @change="loadHistory(m.id)">
                        <option v-for="r in REPORT_RANGES" :key="r" :value="r">{{ r }}</option>
                      </select>
                    </label>
                    <button type="button" class="ctl" :disabled="details[m.id].history.loading" @click="loadHistory(m.id)">Refresh</button>
                    <span class="dim small">
                      {{ details[m.id].history.loading ? 'Loading…' : details[m.id].history.reports.length + ' reports, newest first' }}
                    </span>
                  </div>
                  <p v-if="details[m.id].history.error" class="err">{{ details[m.id].history.error }}</p>
                  <div v-for="(r, i) in details[m.id].history.reports" :key="i" class="report">
                    <button type="button" class="rrow" :aria-expanded="!!details[m.id].history.expanded[i]" @click="details[m.id].history.expanded[i] = !details[m.id].history.expanded[i]">
                      <span class="caret" :class="{ open: details[m.id].history.expanded[i] }">▶</span>
                      <span class="mono when">{{ dateTime(r.time) }}</span>
                      <span class="site">{{ r.site }}</span>
                      <span class="st" :class="r.reached ? 'ok' : 'bad'">{{ r.reached ? 'reached' : 'unreached' }}</span>
                      <span class="mono" :class="{ warnloss: r.loss_pct > 0 }">loss {{ lossPct(r.loss_pct) }}%</span>
                      <span v-if="r.avg_ms != null" class="mono">avg {{ r.avg_ms.toFixed(1) }} ms</span>
                      <span class="mono dim">{{ r.hops }} hops · {{ r.cycles }} cycles</span>
                      <span v-if="r.error" class="bad">{{ r.error }}</span>
                    </button>
                    <pre v-if="details[m.id].history.expanded[i]">{{ r.report }}</pre>
                  </div>
                  <p v-if="!details[m.id].history.loading && !details[m.id].history.error && details[m.id].history.reports.length === 0" class="dim small">
                    No reports in this range.
                  </p>
                </div>
              </div>
            </td>
          </tr>
        </template>
      </tbody>
    </table>

    <p v-else-if="loaded && monitors.length === 0" class="hint">No monitors configured. Add them under "monitors" in the backend config.</p>
    <p v-else-if="loaded" class="hint">
      {{ tab === 'issues' && !filter && !kind ? 'No monitor has an issue.' : 'No monitor matches.' }}
    </p>
    <p v-else class="hint">Loading…</p>
  </div>
</template>

<style scoped>
.monitors { padding: 16px; }
.bar { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; padding-bottom: 10px; }
h1 { font-size: 16px; margin: 0; }
.count { color: var(--fg-dim); font-size: 12px; }
.live { font-size: 11px; color: var(--fg-dim); padding: 1px 8px; border: 1px solid var(--line); border-radius: 999px; }
.live.on { color: var(--ok); border-color: color-mix(in srgb, var(--ok) 40%, transparent); }
.spacer { flex: 1; }
.ctl {
  height: var(--ctl-h); padding: 0 8px; border: 1px solid var(--line);
  border-radius: var(--ctl-radius); background: var(--bg); color: var(--fg); font: inherit;
}
.filter { width: 260px; max-width: 50%; }
.err { color: var(--bad); font-size: 13px; }
.dim { color: var(--fg-dim); }
.small { font-size: 12px; }
.mono { font-variant-numeric: tabular-nums; }
.bad { color: var(--bad); }
.warnloss { color: var(--bad); font-weight: 600; }

.tabs { display: flex; flex-wrap: wrap; gap: 4px; border-bottom: 1px solid var(--line); margin-bottom: 10px; }
.tab {
  appearance: none; border: 0; border-bottom: 2px solid transparent; margin-bottom: -1px; background: none;
  padding: 8px 12px; color: var(--fg-dim); font: inherit; font-size: 13px; cursor: pointer;
}
.tab:hover { color: var(--fg); }
.tab.on { color: var(--fg); border-bottom-color: var(--accent); font-weight: 600; }
.tab .n { display: inline-block; min-width: 18px; margin-left: 4px; padding: 0 6px; border-radius: 999px; background: var(--hover); font-size: 11px; font-weight: 600; text-align: center; }
.tab.down.on .n, .tab.issues.on .n { background: color-mix(in srgb, var(--bad) 16%, transparent); color: var(--bad); }
.tab.partial.on .n { background: color-mix(in srgb, var(--warn) 16%, transparent); color: var(--warn); }
.tab.up.on .n { background: color-mix(in srgb, var(--ok) 16%, transparent); color: var(--ok); }

.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th { text-align: left; color: var(--fg-dim); font-weight: 500; font-size: 11px; text-transform: uppercase; letter-spacing: 0.03em; padding: 6px 10px; border-bottom: 1px solid var(--line); }
.grid td { padding: 6px 10px; border-bottom: 1px solid var(--line); vertical-align: middle; }
.grid th.r, .grid td.r { text-align: right; }
.row { cursor: pointer; }
.row:hover { background: var(--hover); }
.row:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
.row.open td { border-bottom: 0; }
.id { font-weight: 600; white-space: nowrap; }
.caret { display: inline-block; margin-right: 6px; font-size: 9px; color: var(--fg-dim); transition: transform 0.1s; }
.caret.open { transform: rotate(90deg); }
.kind { padding: 1px 7px; border-radius: 4px; font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: #fff; background: var(--fg-dim); }
.kind.trace { background: var(--accent-solid, var(--accent)); }
.kind.ping { background: var(--ok); }
.kind.sip { background: var(--warn); }
.labels { display: flex; flex-wrap: wrap; gap: 4px; }
.label { padding: 1px 7px; border: 1px solid var(--line); border-radius: 999px; font-size: 11px; color: var(--fg-dim); white-space: nowrap; }
.label b { color: var(--fg); font-weight: 500; }
.st { display: inline-block; min-width: 52px; padding: 1px 8px; border-radius: 999px; font-size: 11px; font-weight: 600; text-align: center; text-transform: uppercase; letter-spacing: 0.03em; white-space: nowrap; }
.st.up, .st.ok { color: var(--ok); background: color-mix(in srgb, var(--ok) 14%, transparent); }
.st.down, .st.bad { color: var(--bad); background: color-mix(in srgb, var(--bad) 14%, transparent); }
.st.partial, .st.stale { color: var(--warn); background: color-mix(in srgb, var(--warn) 14%, transparent); }
.st.nodata, .st.none { color: var(--fg-dim); background: var(--hover); }

.detail-row td { padding: 0 10px 10px 34px; background: var(--panel); }
.detail { border: 1px solid var(--line); border-radius: 8px; overflow: hidden; }
.dhead { display: flex; align-items: center; gap: 10px; padding: 8px 12px; border-bottom: 1px solid var(--line); }
.hbtn { padding: 0 12px; cursor: pointer; }
.hbtn:disabled { opacity: 0.5; cursor: default; }
.sites { width: 100%; border-collapse: collapse; font-size: 13px; }
.sites th { text-align: left; color: var(--fg-dim); font-weight: 500; font-size: 11px; text-transform: uppercase; letter-spacing: 0.03em; padding: 6px 12px; border-bottom: 1px solid var(--line); }
.sites td { padding: 6px 12px; border-bottom: 1px solid var(--line); }
.sites tbody tr:last-child td { border-bottom: 0; }
.sites th.r, .sites td.r { text-align: right; }
.site { font-weight: 600; }
.off { margin-left: 6px; padding: 0 6px; border: 1px solid var(--line); border-radius: 999px; font-size: 10px; font-weight: 400; color: var(--fg-dim); text-transform: uppercase; }

.history { border-top: 1px solid var(--line); padding: 10px 12px 12px; background: var(--bg); }
.hbar { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; margin-bottom: 8px; }
.hctl { display: inline-flex; align-items: center; gap: 6px; color: var(--fg-dim); font-size: 12px; }
.report { border-bottom: 1px solid var(--line); }
.report:last-of-type { border-bottom: 0; }
.rrow {
  display: flex; flex-wrap: wrap; align-items: center; gap: 12px; width: 100%; padding: 6px 4px;
  appearance: none; border: 0; background: none; color: inherit; font: inherit; font-size: 13px; text-align: left; cursor: pointer;
}
.rrow:hover { background: var(--hover); }
.rrow:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
.when { min-width: 190px; }
.report pre {
  margin: 0 0 8px 22px; padding: 8px 10px; background: var(--panel); border: 1px solid var(--line); border-radius: 6px;
  font: 12px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace; white-space: pre; overflow-x: auto;
}
.hint { color: var(--fg-dim); padding: 30px 0; text-align: center; }
</style>
