<script setup lang="ts">
// The Trace tool: a ping.pe-style view. One form starts a run across many
// sites at once; each site is a row with live per-cycle cells, expandable to
// its full hop table.
import { computed, onMounted, onBeforeUnmount, reactive, ref } from 'vue'
import { listProbers, type Prober, type Protocol, type Family, type Mode } from '../api'
import { createRun, destHop, type SiteState } from '../runStore'
import { ms, lossPct, rttColor, mtrReport, copyText } from '../format'

const probers = ref<Prober[]>([])
const form = reactive({
  target: '',
  protocol: 'icmp' as Protocol,
  family: '' as Family,
  port: undefined as number | undefined,
  mode: 'mtr' as Mode,
  cycles: 30,
  resolve: true,
  sites: [] as string[],
})

const { state, start, stop } = createRun()

onMounted(async () => {
  probers.value = await listProbers().catch(() => [])
  // Default to every connected site.
  form.sites = probers.value.map((p) => p.site)
})
onBeforeUnmount(stop)

const canTrace = computed(() => form.target.trim() !== '' && form.sites.length > 0)
const needsPort = computed(() => form.protocol === 'tcp' || form.protocol === 'udp')

function toggleSite(site: string) {
  const i = form.sites.indexOf(site)
  if (i >= 0) form.sites.splice(i, 1)
  else form.sites.push(site)
}

async function submit() {
  if (!canTrace.value) return
  await start({
    target: form.target.trim(),
    sites: form.sites,
    protocol: form.protocol,
    family: form.family,
    port: needsPort.value ? form.port || undefined : undefined,
    resolve_names: form.resolve,
    cycles: form.cycles,
    mode: form.mode,
  }).catch((e) => {
    alert(String(e))
  })
}

// The rows, in the run's site order.
const rows = computed<SiteState[]>(() => state.order.map((s) => state.sites[s]).filter(Boolean))

// The timeline shows the most recent cells that fit; older ones scroll off.
const TIMELINE_MAX = 60
function cells(s: SiteState) {
  return s.timeline.slice(-TIMELINE_MAX)
}

function statusLabel(s: SiteState): string {
  switch (s.status) {
    case 'queued':
      return 'queued'
    case 'running':
      return `cycle ${s.cycles}`
    case 'done':
      return 'done'
    case 'offline':
      return 'offline'
    case 'error':
      return 'error'
  }
}

function hopName(s: SiteState): string {
  const d = destHop(s)
  if (!d || d.addresses.length === 0) return '—'
  return d.addresses[0].name || d.addresses[0].ip
}

// Which site's report was just copied, to flash a confirmation on its button.
const copied = ref('')
async function copyReport(s: SiteState) {
  if (await copyText(mtrReport(s))) {
    copied.value = s.site
    setTimeout(() => {
      if (copied.value === s.site) copied.value = ''
    }, 1500)
  }
}
</script>

<template>
  <div class="trace">
    <form class="bar" @submit.prevent="submit">
      <label for="trace-target" class="sr-only">Target host or IP</label>
      <input
        id="trace-target"
        v-model="form.target"
        class="ctl target"
        placeholder="host or IP to trace"
        spellcheck="false"
      />
      <label for="trace-protocol" class="sr-only">Protocol</label>
      <select id="trace-protocol" v-model="form.protocol" class="ctl">
        <option value="icmp">ICMP</option>
        <option value="tcp">TCP</option>
        <option value="udp">UDP</option>
      </select>
      <label v-if="needsPort" for="trace-port" class="sr-only">Port</label>
      <input
        v-if="needsPort"
        id="trace-port"
        v-model.number="form.port"
        class="ctl port"
        type="number"
        min="1"
        max="65535"
        placeholder="port"
      />
      <label for="trace-family" class="sr-only">IP version</label>
      <select id="trace-family" v-model="form.family" class="ctl">
        <option value="">v4/v6</option>
        <option value="4">IPv4</option>
        <option value="6">IPv6</option>
      </select>
      <label for="trace-mode" class="sr-only">Mode</label>
      <select id="trace-mode" v-model="form.mode" class="ctl">
        <option value="mtr">trace</option>
        <option value="ping">ping</option>
      </select>
      <label class="cyc">cycles
        <input v-model.number="form.cycles" class="ctl num" type="number" min="1" max="1000" />
      </label>
      <label class="chk"><input v-model="form.resolve" type="checkbox" /> DNS</label>
      <button v-if="!state.running" class="ctl go" :disabled="!canTrace" type="submit">Trace</button>
      <button v-else class="ctl stop" type="button" @click="stop">Stop</button>
    </form>

    <div class="sites">
      <label
        v-for="p in probers"
        :key="p.site"
        class="chip"
        :class="{ on: form.sites.includes(p.site) }"
      >
        <input type="checkbox" :checked="form.sites.includes(p.site)" @change="toggleSite(p.site)" />
        {{ p.site }}
      </label>
      <span v-if="probers.length === 0" class="empty">No probers connected.</span>
    </div>

    <div v-if="rows.length" class="results">
      <div class="head">
        <span class="c-site">Site</span>
        <span class="c-ip">Address</span>
        <span class="c-n">Loss</span>
        <span class="c-n">Last</span>
        <span class="c-n">Avg</span>
        <span class="c-n">Best</span>
        <span class="c-n">Worst</span>
        <span class="c-tl">Recent cycles (ms)</span>
      </div>

      <template v-for="s in rows" :key="s.site">
        <button
          type="button"
          class="row"
          :class="s.status"
          :aria-expanded="s.expanded"
          @click="s.expanded = !s.expanded"
        >
          <span class="c-site">
            <span class="caret" :class="{ open: s.expanded }">▶</span>{{ s.site }}
          </span>
          <span class="c-ip">
            <span class="ip">{{ s.resolved || '—' }}</span>
            <span class="host">{{ hopName(s) }}</span>
          </span>
          <template v-if="destHop(s)">
            <span class="c-n" :class="{ warnloss: (destHop(s)?.loss_pct ?? 0) > 0 }">{{ lossPct(destHop(s)!.loss_pct) }}%</span>
            <span class="c-n">{{ ms(destHop(s)!.last_us) }}</span>
            <span class="c-n">{{ ms(destHop(s)!.avg_us) }}</span>
            <span class="c-n">{{ ms(destHop(s)!.best_us) }}</span>
            <span class="c-n">{{ ms(destHop(s)!.worst_us) }}</span>
          </template>
          <template v-else>
            <span class="c-n muted">—</span><span class="c-n muted">—</span>
            <span class="c-n muted">—</span><span class="c-n muted">—</span><span class="c-n muted">—</span>
          </template>
          <span class="c-tl">
            <span class="status" :class="s.status">{{ statusLabel(s) }}</span>
            <span class="timeline">
              <span
                v-for="(v, i) in cells(s)"
                :key="i"
                class="cell"
                :style="{ background: rttColor(v) }"
                :title="v == null ? 'loss' : ms(v) + ' ms'"
              />
            </span>
          </span>
        </button>

        <div v-if="s.expanded" class="hops">
          <div v-if="s.error" class="err">{{ s.error }}</div>
          <template v-else>
          <div class="hops-bar">
            <span class="src">
              <template v-if="s.source">from <span class="mono">{{ s.source }}</span></template>
            </span>
            <button type="button" class="copy" @click.stop="copyReport(s)">
              {{ copied === s.site ? 'Copied ✓' : 'Copy as text' }}
            </button>
          </div>
          <table>
            <thead>
              <tr><th>#</th><th>Host</th><th>Loss</th><th>Snt</th><th>Rcvd</th><th>Last</th><th>Avg</th><th>Best</th><th>Worst</th><th>StDev</th></tr>
            </thead>
            <tbody>
              <tr v-for="h in s.hops" :key="h.ttl" :class="{ dest: h.ttl === s.reachedAt }">
                <td class="num">{{ h.ttl }}</td>
                <td class="host">
                  <template v-if="h.addresses.length">
                    <div v-for="a in h.addresses" :key="a.ip">{{ a.name || a.ip }}<span v-if="a.name" class="dim"> ({{ a.ip }})</span></div>
                  </template>
                  <span v-else class="dim">???</span>
                </td>
                <td class="num" :class="{ warnloss: h.loss_pct > 0 }">{{ lossPct(h.loss_pct) }}%</td>
                <td class="num">{{ h.sent }}</td>
                <td class="num">{{ h.received }}</td>
                <td class="num">{{ ms(h.last_us) }}</td>
                <td class="num">{{ ms(h.avg_us) }}</td>
                <td class="num">{{ ms(h.best_us) }}</td>
                <td class="num">{{ ms(h.worst_us) }}</td>
                <td class="num">{{ ms(h.stdev_us) }}</td>
              </tr>
            </tbody>
          </table>
          </template>
        </div>
      </template>
    </div>

    <div v-else class="hint">Enter a target and press Trace to run from every selected site.</div>
  </div>
</template>

<style scoped>
.trace {
  display: flex;
  flex-direction: column;
  min-height: 100%;
}
.bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--line);
  background: var(--panel);
}
.ctl {
  height: var(--ctl-h);
  padding: 0 8px;
  border: 1px solid var(--line);
  border-radius: var(--ctl-radius);
  background: var(--bg);
  color: var(--fg);
  font: inherit;
}
.target { flex: 1 1 260px; min-width: 200px; }
.port { width: 74px; }
.num { width: 64px; text-align: right; }
.cyc { display: inline-flex; align-items: center; gap: 4px; color: var(--fg-dim); font-size: 12px; }
.chk { display: inline-flex; align-items: center; gap: 4px; color: var(--fg-dim); font-size: 12px; }
.go, .stop {
  padding: 0 16px;
  font-weight: 600;
  cursor: pointer;
  color: #fff;
  border-color: transparent;
}
.go { background: var(--accent-solid); }
.go:disabled { opacity: 0.5; cursor: default; }
.stop { background: var(--bad); }

.sites {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  padding: 10px 16px;
  border-bottom: 1px solid var(--line);
}
.chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 3px 9px;
  border: 1px solid var(--line);
  border-radius: 999px;
  font-size: 12px;
  color: var(--fg-dim);
  cursor: pointer;
  user-select: none;
}
.chip.on { color: var(--fg); border-color: var(--accent); background: color-mix(in srgb, var(--accent) 12%, transparent); }
.chip input { display: none; }
.empty { color: var(--fg-dim); font-size: 13px; }

.results { padding: 8px 16px 24px; }
.head, .row {
  display: grid;
  grid-template-columns: 120px 1.4fr 64px 64px 64px 64px 64px 3fr;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
}
.head {
  color: var(--fg-dim);
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.03em;
  border-bottom: 1px solid var(--line);
}
.row {
  appearance: none;
  width: 100%;
  border: 0;
  border-bottom: 1px solid var(--line);
  background: none;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.row:hover { background: var(--hover); }
.row:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
.c-n { text-align: right; font-variant-numeric: tabular-nums; }
.warnloss { color: var(--bad); font-weight: 600; }
.muted { color: var(--fg-dim); }
.c-site { display: flex; align-items: center; gap: 6px; font-weight: 600; }
.caret { display: inline-block; font-size: 9px; color: var(--fg-dim); transition: transform 0.1s; }
.caret.open { transform: rotate(90deg); }
.c-ip { display: flex; flex-direction: column; line-height: 1.25; min-width: 0; }
.c-ip .ip { font-variant-numeric: tabular-nums; }
.c-ip .host { color: var(--fg-dim); font-size: 11px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.c-tl { display: flex; align-items: center; gap: 10px; min-width: 0; }
.status { flex: none; width: 62px; font-size: 11px; color: var(--fg-dim); }
.status.done { color: var(--ok); }
.status.error, .status.offline { color: var(--bad); }
.timeline { display: flex; gap: 1px; flex-wrap: nowrap; overflow: hidden; }
.cell {
  width: 7px;
  height: 18px;
  border-radius: 1px;
  flex: none;
}

.hops { padding: 4px 8px 14px 34px; }
.hops table { width: 100%; border-collapse: collapse; font-size: 12px; }
.hops th { text-align: right; color: var(--fg-dim); font-weight: 500; padding: 3px 8px; }
.hops th:nth-child(2) { text-align: left; }
.hops td { padding: 2px 8px; border-top: 1px solid var(--line); }
.hops td.host { text-align: left; }
.hops td.host .dim { color: var(--fg-dim); }
.hops tr.dest td { font-weight: 600; }
.hops-bar { display: flex; align-items: center; justify-content: space-between; padding: 0 0 6px; }
.src { color: var(--fg-dim); font-size: 12px; }
.src .mono { font-variant-numeric: tabular-nums; color: var(--fg); }
.copy {
  height: 24px;
  padding: 0 10px;
  border: 1px solid var(--line);
  border-radius: var(--ctl-radius);
  background: var(--bg);
  color: var(--fg-dim);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}
.copy:hover { color: var(--fg); border-color: var(--accent); }
.err { color: var(--bad); font-size: 13px; padding: 6px 0; }

.hint { color: var(--fg-dim); padding: 40px 16px; text-align: center; }
</style>
