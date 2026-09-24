<script setup lang="ts">
// The Monitors page: every configured monitor with what it probes and how, and
// each site's latest outcome, refreshed every few seconds. A trace monitor
// opens a history panel listing the mtr reports shipped to VictoriaLogs, each
// expandable to the report text.
import { computed, onMounted, onBeforeUnmount, reactive, ref } from 'vue'
import {
  listMonitors,
  listTraceReports,
  REPORT_RANGES,
  type Monitor,
  type MonitorSiteStatus,
  type ReportRange,
  type TraceReport,
} from '../api'
import { dateTime, since } from '../settings'
import { lossPct } from '../format'

const monitors = ref<Monitor[]>([])
const loaded = ref(false)
const loadError = ref('')
const filter = ref('')
const now = ref(Date.now())
let poll = 0
let tick = 0

// Per-monitor history panel state, kept across refreshes.
interface History {
  open: boolean
  site: string
  range: ReportRange
  loading: boolean
  error: string
  reports: TraceReport[]
  expanded: Record<number, boolean>
}
const history = reactive<Record<string, History>>({})

function newHistory(): History {
  return { open: false, site: '', range: '24h', loading: false, error: '', reports: [], expanded: {} }
}

// One request at a time: a slow response must not be overtaken by the next
// tick's and then overwrite it with an older list.
let refreshing = false

async function refresh() {
  if (refreshing) return
  refreshing = true
  try {
    const list = await listMonitors()
    for (const m of list) if (!history[m.id]) history[m.id] = newHistory()
    monitors.value = list
    loadError.value = ''
  } catch (e) {
    loadError.value = String(e)
  } finally {
    loaded.value = true
    refreshing = false
  }
}

onMounted(() => {
  void refresh()
  poll = window.setInterval(refresh, 5000)
  tick = window.setInterval(() => (now.value = Date.now()), 1000)
})
onBeforeUnmount(() => {
  clearInterval(poll)
  clearInterval(tick)
})

const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return monitors.value
  return monitors.value.filter((m) =>
    [m.id, m.target, m.kind, ...Object.entries(m.labels).map(([k, v]) => `${k}=${v}`)].some((s) =>
      s.toLowerCase().includes(q),
    ),
  )
})

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

function statusClass(s: MonitorSiteStatus): string {
  if (s.up == null) return 'none'
  return s.up ? 'ok' : 'bad'
}
function statusText(s: MonitorSiteStatus): string {
  if (s.up == null) return s.connected ? 'waiting' : 'no data'
  return s.up ? 'up' : 'down'
}
function detail(m: Monitor, s: MonitorSiteStatus): string {
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

async function toggleHistory(m: Monitor) {
  const h = history[m.id]
  h.open = !h.open
  if (h.open && h.reports.length === 0 && !h.error) await loadHistory(m.id)
}

async function loadHistory(id: string) {
  const h = history[id]
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
</script>

<template>
  <div class="monitors">
    <header class="bar">
      <h1>Monitors</h1>
      <span class="count">{{ monitors.length }} configured</span>
      <label for="mon-filter" class="sr-only">Filter monitors</label>
      <input id="mon-filter" v-model="filter" class="ctl filter" placeholder="Filter by id, target, kind or label" spellcheck="false" />
    </header>

    <p v-if="loadError" class="err">{{ loadError }}</p>

    <section v-for="m in shown" :key="m.id" class="mon">
      <div class="mon-head">
        <span class="kind" :class="m.kind">{{ m.kind }}</span>
        <span class="id">{{ m.id }}</span>
        <span class="target mono">{{ m.target }}</span>
        <span class="params">{{ params(m) }}</span>
        <span v-if="Object.keys(m.labels).length" class="labels">
          <span v-for="(v, k) in m.labels" :key="k" class="label">{{ k }}=<b>{{ v }}</b></span>
        </span>
        <span class="spacer" />
        <span v-if="m.sites.length" class="dim small">sites: {{ m.sites.join(', ') }}</span>
        <button
          v-if="m.kind === 'trace'"
          type="button"
          class="ctl hbtn"
          :disabled="!m.history"
          :title="m.history ? 'Reports shipped to VictoriaLogs' : 'VictoriaLogs is not configured'"
          @click="toggleHistory(m)"
        >
          {{ history[m.id]?.open ? 'Hide history' : 'History' }}
        </button>
      </div>

      <table class="grid">
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
          <tr v-for="s in m.status" :key="s.site">
            <td class="site">
              {{ s.site }}
              <span v-if="!s.connected" class="off" title="no connected agent for this site">offline</span>
            </td>
            <td><span class="st" :class="statusClass(s)">{{ statusText(s) }}</span></td>
            <td class="mono dim">{{ s.resolved || '—' }}</td>
            <td class="r mono" :class="{ warnloss: s.up != null && m.kind !== 'sip' && s.loss_pct > 0 }">{{ lossText(m, s) }}</td>
            <td class="r mono">{{ s.rtt_ms != null ? s.rtt_ms.toFixed(1) + ' ms' : '—' }}</td>
            <td :class="{ bad: s.error }">{{ detail(m, s) }}</td>
            <td class="r mono dim">{{ s.at ? since(s.at, now) + ' ago' : '—' }}</td>
          </tr>
          <tr v-if="m.status.length === 0">
            <td colspan="7" class="dim">No site runs this monitor: no agent is connected.</td>
          </tr>
        </tbody>
      </table>

      <div v-if="history[m.id]?.open" class="history">
        <div class="hbar">
          <label class="hctl">site
            <select v-model="history[m.id].site" class="ctl" @change="loadHistory(m.id)">
              <option value="">all</option>
              <option v-for="s in m.status" :key="s.site" :value="s.site">{{ s.site }}</option>
            </select>
          </label>
          <label class="hctl">range
            <select v-model="history[m.id].range" class="ctl" @change="loadHistory(m.id)">
              <option v-for="r in REPORT_RANGES" :key="r" :value="r">{{ r }}</option>
            </select>
          </label>
          <button type="button" class="ctl" :disabled="history[m.id].loading" @click="loadHistory(m.id)">Refresh</button>
          <span class="dim small">
            {{ history[m.id].loading ? 'Loading…' : history[m.id].reports.length + ' reports, newest first' }}
          </span>
        </div>
        <p v-if="history[m.id].error" class="err">{{ history[m.id].error }}</p>
        <div v-for="(r, i) in history[m.id].reports" :key="i" class="report">
          <button type="button" class="rrow" :aria-expanded="!!history[m.id].expanded[i]" @click="history[m.id].expanded[i] = !history[m.id].expanded[i]">
            <span class="caret" :class="{ open: history[m.id].expanded[i] }">▶</span>
            <span class="mono when">{{ dateTime(r.time) }}</span>
            <span class="site">{{ r.site }}</span>
            <span class="st" :class="r.reached ? 'ok' : 'bad'">{{ r.reached ? 'reached' : 'unreached' }}</span>
            <span class="mono" :class="{ warnloss: r.loss_pct > 0 }">loss {{ lossPct(r.loss_pct) }}%</span>
            <span v-if="r.avg_ms != null" class="mono">avg {{ r.avg_ms.toFixed(1) }} ms</span>
            <span class="mono dim">{{ r.hops }} hops · {{ r.cycles }} cycles</span>
            <span v-if="r.error" class="bad">{{ r.error }}</span>
          </button>
          <pre v-if="history[m.id].expanded[i]">{{ r.report }}</pre>
        </div>
        <p v-if="!history[m.id].loading && !history[m.id].error && history[m.id].reports.length === 0" class="dim small">
          No reports in this range.
        </p>
      </div>
    </section>

    <p v-if="loaded && monitors.length === 0" class="hint">No monitors configured. Add them under "monitors" in the backend config.</p>
    <p v-else-if="loaded && shown.length === 0" class="hint">No monitor matches the filter.</p>
    <p v-else-if="!loaded" class="hint">Loading…</p>
  </div>
</template>

<style scoped>
.monitors { padding: 16px; }
.bar { display: flex; align-items: center; gap: 12px; border-bottom: 1px solid var(--line); padding-bottom: 10px; margin-bottom: 12px; }
h1 { font-size: 16px; margin: 0; }
.count { color: var(--fg-dim); font-size: 12px; }
.ctl {
  height: var(--ctl-h); padding: 0 8px; border: 1px solid var(--line);
  border-radius: var(--ctl-radius); background: var(--bg); color: var(--fg); font: inherit;
}
.filter { margin-left: auto; width: 280px; max-width: 50%; }
.err { color: var(--bad); font-size: 13px; }
.dim { color: var(--fg-dim); }
.small { font-size: 12px; }
.mono { font-variant-numeric: tabular-nums; }
.bad { color: var(--bad); }
.warnloss { color: var(--bad); font-weight: 600; }

.mon { margin-bottom: 18px; border: 1px solid var(--line); border-radius: 8px; background: var(--panel); overflow: hidden; }
.mon-head { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; padding: 10px 12px; border-bottom: 1px solid var(--line); }
.kind { padding: 1px 7px; border-radius: 4px; font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: #fff; background: var(--fg-dim); }
.kind.trace { background: var(--accent-solid, var(--accent)); }
.kind.ping { background: var(--ok); }
.kind.sip { background: var(--warn); }
.id { font-weight: 600; font-size: 14px; }
.target { color: var(--fg); }
.params { color: var(--fg-dim); font-size: 12px; }
.labels { display: flex; flex-wrap: wrap; gap: 4px; }
.label { padding: 1px 7px; border: 1px solid var(--line); border-radius: 999px; font-size: 11px; color: var(--fg-dim); }
.label b { color: var(--fg); font-weight: 500; }
.spacer { flex: 1; }
.hbtn { padding: 0 12px; cursor: pointer; }
.hbtn:disabled { opacity: 0.5; cursor: default; }

.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th { text-align: left; color: var(--fg-dim); font-weight: 500; font-size: 11px; text-transform: uppercase; letter-spacing: 0.03em; padding: 6px 12px; border-bottom: 1px solid var(--line); }
.grid td { padding: 7px 12px; border-bottom: 1px solid var(--line); }
.grid tbody tr:last-child td { border-bottom: 0; }
.grid th.r, .grid td.r { text-align: right; }
.site { font-weight: 600; }
.off { margin-left: 6px; padding: 0 6px; border: 1px solid var(--line); border-radius: 999px; font-size: 10px; font-weight: 400; color: var(--fg-dim); text-transform: uppercase; }
.st { display: inline-block; min-width: 52px; padding: 1px 8px; border-radius: 999px; font-size: 11px; font-weight: 600; text-align: center; text-transform: uppercase; letter-spacing: 0.03em; }
.st.ok { color: var(--ok); background: color-mix(in srgb, var(--ok) 14%, transparent); }
.st.bad { color: var(--bad); background: color-mix(in srgb, var(--bad) 14%, transparent); }
.st.none { color: var(--fg-dim); background: var(--hover); }

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
.caret { display: inline-block; font-size: 9px; color: var(--fg-dim); transition: transform 0.1s; }
.caret.open { transform: rotate(90deg); }
.when { min-width: 190px; }
.report pre {
  margin: 0 0 8px 22px; padding: 8px 10px; background: var(--panel); border: 1px solid var(--line); border-radius: 6px;
  font: 12px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace; white-space: pre; overflow-x: auto;
}
.hint { color: var(--fg-dim); padding: 30px 0; text-align: center; }
</style>
