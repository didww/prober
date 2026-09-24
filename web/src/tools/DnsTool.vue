<script setup lang="ts">
// The DNS tool: resolve a name with each agent's own resolver and lay the
// answers out as a matrix, sites down and record types across, so a GeoDNS
// answer or a broken resolver at one site stands out. An address gets its
// PTR record instead.
import { computed, onMounted, onBeforeUnmount, reactive, ref } from 'vue'
import { listProbers, type DnsRecord, type Prober } from '../api'
import { ADDRESS_COLUMNS, NAME_COLUMNS, createDnsRun, type DnsColumn, type QueryState, type SiteState } from '../dnsRunStore'
import { ms } from '../format'

const probers = ref<Prober[]>([])
const form = reactive({
  name: '',
  sites: [] as string[],
})

const { state, start, stop } = createDnsRun()

onMounted(async () => {
  probers.value = await listProbers().catch(() => [])
  form.sites = probers.value.map((p) => p.site)
})
onBeforeUnmount(stop)

const canRun = computed(() => form.name.trim() !== '' && form.sites.length > 0)

function toggleSite(site: string) {
  const i = form.sites.indexOf(site)
  if (i >= 0) form.sites.splice(i, 1)
  else form.sites.push(site)
}

async function submit() {
  if (!canRun.value) return
  await start({ name: form.name.trim(), sites: form.sites }).catch((e) => alert(String(e)))
}

const rows = computed<SiteState[]>(() => state.order.map((s) => state.sites[s]).filter(Boolean))

// The columns for what the run asked: the SIP set for a name, PTR for an
// address.
const columns = computed<DnsColumn[]>(() => (state.reverse ? ADDRESS_COLUMNS : NAME_COLUMNS))

// The name the columns were asked for, as the first agent to start normalised
// it, or as typed until then.
const askedName = computed(() => rows.value.find((s) => s.name)?.name || form.name.trim())

function queryName(c: DnsColumn, q?: QueryState): string {
  if (q?.name) return q.name
  if (c.type === 'PTR') return 'reverse of ' + askedName.value
  return c.prefix + askedName.value
}

function statusLabel(s: SiteState): string {
  switch (s.status) {
    case 'queued': return 'queued'
    case 'running': return 'running'
    case 'done': return 'done'
    case 'offline': return 'offline'
    case 'error': return 'error'
  }
}

function recordText(r: DnsRecord, c: DnsColumn): string {
  if (c.type === 'SRV') return `${r.priority} ${r.weight} ${r.port} ${r.value}`
  return r.value
}

// What an answered-but-empty cell says: the response code the nameserver
// gave (NXDOMAIN, NODATA, SERVFAIL, REFUSED, ...). When no server answered
// at all, a short word for why; the full error is on hover. An agent too old
// to report a code sends an empty answer with no error: just a dash.
function emptyLabel(q: QueryState): string {
  if (q.status) return q.status
  if (!q.error) return '—'
  if (/i\/o timeout|deadline exceeded/i.test(q.error)) return 'timeout'
  if (/context canceled/i.test(q.error)) return 'cancelled'
  if (/connection refused|unreachable|no route/i.test(q.error)) return 'unreachable'
  return 'error'
}

// How loud an empty cell is: NODATA is routine (a name without AAAA, say),
// NXDOMAIN is worth a look, anything else means the lookup itself failed.
function emptyClass(q: QueryState): string {
  if (q.status === 'NODATA' || (!q.status && !q.error)) return 'dim'
  if (q.status === 'NXDOMAIN') return 'warn'
  return 'bad'
}

function cellTitle(q: QueryState, c: DnsColumn): string {
  const name = `${c.type} ${queryName(c, q)}`
  if (!q.done) return `${name}: waiting`
  if (q.error) return `${name}: ${q.error}`
  const n = q.records.length
  const parts = [q.status, `${n} record${n === 1 ? '' : 's'}`, q.server ? `from ${q.server}` : '', `in ${ms(q.rttUs)} ms`]
  if (n) parts.push(`TTL ${Math.min(...q.records.map((r) => r.ttl))}s`)
  return `${name}: ${parts.filter(Boolean).join(', ')}`
}
</script>

<template>
  <div class="dns">
    <form class="bar" @submit.prevent="submit">
      <label for="dns-name" class="sr-only">Host name or address to resolve</label>
      <input
        id="dns-name"
        v-model="form.name"
        class="ctl target"
        placeholder="Host name (e.g. sip.example.com) or an IP address for its PTR"
        spellcheck="false"
        autocapitalize="off"
      />
      <button v-if="!state.running" class="ctl go" :disabled="!canRun" type="submit">Resolve</button>
      <button v-else class="ctl stop" type="button" @click="stop">Stop</button>
    </form>

    <div class="sites">
      <label v-for="p in probers" :key="p.site" class="chip" :class="{ on: form.sites.includes(p.site) }">
        <input type="checkbox" :checked="form.sites.includes(p.site)" @change="toggleSite(p.site)" />
        {{ p.site }}
      </label>
      <span v-if="probers.length === 0" class="empty">No probers connected.</span>
    </div>

    <div v-if="rows.length" class="results">
      <table>
        <thead>
          <tr>
            <th class="c-site">Site</th>
            <th v-for="c in columns" :key="c.key" :title="c.type + ' ' + queryName(c)">
              <span v-if="c.type === 'SRV'" class="kind">SRV</span>{{ c.label }}
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="s in rows" :key="s.site">
            <td class="c-site">
              <div class="site">
                {{ s.site }}
                <span class="status" :class="s.status">{{ statusLabel(s) }}</span>
              </div>
              <div v-if="s.nameservers.length" class="ns" :title="'resolver: ' + s.nameservers.join(', ')">
                via {{ s.nameservers.join(', ') }}
              </div>
            </td>
            <td v-if="s.error" :colspan="columns.length" class="err">{{ s.error }}</td>
            <template v-else>
              <td v-for="c in columns" :key="c.key" class="cell" :title="cellTitle(s.queries[c.key], c)">
                <span v-if="!s.queries[c.key].done" class="dim">…</span>
                <span v-else-if="s.queries[c.key].records.length === 0" :class="emptyClass(s.queries[c.key])">{{ emptyLabel(s.queries[c.key]) }}</span>
                <template v-else>
                  <div v-for="(r, i) in s.queries[c.key].records" :key="i" class="rec">
                    {{ recordText(r, c) }}
                    <div v-if="c.type === 'SRV' && r.value !== '.'" class="addrs" :class="{ bad: r.addresses.length === 0 }">
                      {{ r.addresses.length ? r.addresses.join(', ') : 'no address (' + r.address_status + ')' }}
                    </div>
                  </div>
                </template>
              </td>
            </template>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-else class="hint">
      Enter a host name, or an IP address for its PTR record, and press Resolve to look it up from every selected site.
      <br /><span class="small">Names are asked exactly as typed, through each site's own nameservers; the hosts file and search list are not used.</span>
    </div>
  </div>
</template>

<style scoped>
.dns { display: flex; flex-direction: column; min-height: 100%; }
.bar {
  display: flex; flex-wrap: wrap; align-items: center; gap: 8px;
  padding: 12px 16px; border-bottom: 1px solid var(--line); background: var(--panel);
}
.ctl {
  height: var(--ctl-h); padding: 0 8px; border: 1px solid var(--line);
  border-radius: var(--ctl-radius); background: var(--bg); color: var(--fg); font: inherit;
}
.target { flex: 1 1 260px; min-width: 200px; }
.go, .stop { padding: 0 16px; font-weight: 600; cursor: pointer; color: #fff; border-color: transparent; }
.go { background: var(--accent-solid); }
.go:disabled { opacity: 0.5; cursor: default; }
.stop { background: var(--bad); }

.sites { display: flex; flex-wrap: wrap; gap: 6px; padding: 10px 16px; border-bottom: 1px solid var(--line); }
.chip {
  display: inline-flex; align-items: center; gap: 5px; padding: 3px 9px;
  border: 1px solid var(--line); border-radius: 999px; font-size: 12px; color: var(--fg-dim);
  cursor: pointer; user-select: none;
}
.chip.on { color: var(--fg); border-color: var(--accent); background: color-mix(in srgb, var(--accent) 12%, transparent); }
.chip input { display: none; }
.empty { color: var(--fg-dim); font-size: 13px; }

/* The matrix is as wide as its widest SRV answers; it scrolls sideways rather
   than wrapping records, which would break line-by-line comparison. */
.results { padding: 8px 16px 24px; overflow-x: auto; }
table { border-collapse: collapse; min-width: 100%; font-size: 12px; }
th, td { padding: 6px 10px; text-align: left; vertical-align: top; border-bottom: 1px solid var(--line); white-space: nowrap; }
th { color: var(--fg-dim); font-size: 11px; font-weight: 600; text-transform: none; letter-spacing: 0.03em; }
th .kind { margin-right: 5px; padding: 0 4px; border: 1px solid var(--line); border-radius: 3px; font-size: 9px; text-transform: uppercase; }
tbody tr:hover { background: var(--hover); }
.c-site { min-width: 120px; }
.site { display: flex; align-items: center; gap: 8px; font-weight: 600; font-size: 13px; }
.status { font-size: 11px; font-weight: 400; color: var(--fg-dim); }
.status.done { color: var(--ok); }
.status.error, .status.offline { color: var(--bad); }
.ns { margin-top: 2px; color: var(--fg-dim); font-size: 11px; font-variant-numeric: tabular-nums; }
.cell { font: 12px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace; font-variant-numeric: tabular-nums; }
.rec { white-space: nowrap; }
/* An SRV target's addresses, under its record. */
.addrs { padding-left: 14px; color: var(--fg-dim); font-size: 11px; }
.addrs.bad { color: var(--bad); font-weight: 600; }
.dim { color: var(--fg-dim); }
.warn { color: var(--warn); font-weight: 600; cursor: help; }
.bad { color: var(--bad); font-weight: 600; cursor: help; }
.err { color: var(--bad); font-size: 13px; white-space: normal; }
.hint { color: var(--fg-dim); padding: 40px 16px; text-align: center; line-height: 1.7; }
.hint .small { font-size: 12px; }
</style>
