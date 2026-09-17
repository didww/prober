<script setup lang="ts">
// The SIP tool: send OPTIONS from every selected agent over a chosen transport
// and report per-agent results, expandable to the raw request and response.
import { computed, onMounted, onBeforeUnmount, reactive, ref } from 'vue'
import { listProbers, type Prober, type SipTransport, type Family } from '../api'
import { createSipRun, type SiteState } from '../sipRunStore'
import { ms, rttColor } from '../format'

const probers = ref<Prober[]>([])
const form = reactive({
  target: '',
  transport: 'udp' as SipTransport,
  port: undefined as number | undefined,
  family: '' as Family,
  interval: 1,
  cycles: 10,
  sites: [] as string[],
})

const { state, start, stop } = createSipRun()

onMounted(async () => {
  probers.value = await listProbers().catch(() => [])
  form.sites = probers.value.map((p) => p.site)
})
onBeforeUnmount(stop)

const canSend = computed(() => form.target.trim() !== '' && form.sites.length > 0)

// The default port shown as a placeholder for the chosen transport.
const DEFAULT_PORTS: Record<SipTransport, string> = { udp: '5060', tcp: '5060', tls: '5061', wss: '443' }
const portPlaceholder = computed(() => DEFAULT_PORTS[form.transport])

function toggleSite(site: string) {
  const i = form.sites.indexOf(site)
  if (i >= 0) form.sites.splice(i, 1)
  else form.sites.push(site)
}

async function submit() {
  if (!canSend.value) return
  await start({
    target: form.target.trim(),
    sites: form.sites,
    transport: form.transport,
    family: form.family,
    port: form.port || undefined,
    cycles: form.cycles,
    interval_ms: Math.max(1, form.interval) * 1000,
  }).catch((e) => alert(String(e)))
}

const rows = computed<SiteState[]>(() => state.order.map((s) => state.sites[s]).filter(Boolean))

function codeClass(s: SiteState): string {
  if (s.sent === 0) return 'muted'
  if (!s.responded) return 'bad'
  if (s.code >= 200 && s.code < 300) return 'ok'
  return 'warn'
}
function codeText(s: SiteState): string {
  if (s.sent === 0) return '—'
  if (!s.responded) return 'timeout'
  return `${s.code} ${s.reason}`
}
function lossPctText(s: SiteState): string {
  if (s.sent === 0) return '—'
  return `${Math.round((100 * (s.sent - s.ok)) / s.sent)}%`
}
const TL_MAX = 60
function cells(s: SiteState) {
  return s.timeline.slice(-TL_MAX)
}
function cellColor(c: { rtt: number | null; code: number }): string {
  if (c.rtt == null) return 'var(--cell-loss)'
  if (c.code >= 200 && c.code < 300) return rttColor(c.rtt)
  return 'var(--cell-warn)'
}
function statusLabel(s: SiteState): string {
  switch (s.status) {
    case 'queued': return 'queued'
    case 'running': return `#${s.cycles}`
    case 'done': return 'done'
    case 'offline': return 'offline'
    case 'error': return 'error'
  }
}
</script>

<template>
  <div class="sip">
    <form class="bar" @submit.prevent="submit">
      <label for="sip-target" class="sr-only">SIP target host or IP</label>
      <input id="sip-target" v-model="form.target" class="ctl target" placeholder="SIP host or IP" spellcheck="false" />
      <label for="sip-transport" class="sr-only">Transport</label>
      <select id="sip-transport" v-model="form.transport" class="ctl">
        <option value="udp">UDP</option>
        <option value="tcp">TCP</option>
        <option value="tls">TLS</option>
        <option value="wss">WSS</option>
      </select>
      <label for="sip-port" class="sr-only">Port</label>
      <input id="sip-port" v-model.number="form.port" class="ctl port" type="number" min="1" max="65535" :placeholder="portPlaceholder" />
      <label for="sip-family" class="sr-only">IP version</label>
      <select id="sip-family" v-model="form.family" class="ctl">
        <option value="">v4/v6</option>
        <option value="4">IPv4</option>
        <option value="6">IPv6</option>
      </select>
      <label class="cyc">every
        <input v-model.number="form.interval" class="ctl num" type="number" min="1" max="60" />s
      </label>
      <label class="cyc">count
        <input v-model.number="form.cycles" class="ctl num" type="number" min="1" max="1000" />
      </label>
      <button v-if="!state.running" class="ctl go" :disabled="!canSend" type="submit">Send</button>
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
      <div class="head">
        <span class="c-site">Site</span>
        <span class="c-ip">Address</span>
        <span class="c-code">Response</span>
        <span class="c-n">Loss</span>
        <span class="c-n">Last</span>
        <span class="c-tl">Recent (ms)</span>
      </div>

      <template v-for="s in rows" :key="s.site">
        <button
          type="button"
          class="row"
          :class="s.status"
          :aria-expanded="s.expanded"
          @click="s.expanded = !s.expanded"
        >
          <span class="c-site"><span class="caret" :class="{ open: s.expanded }">▶</span>{{ s.site }}</span>
          <span class="c-ip">
            <span class="ip">{{ s.resolved || '—' }}</span>
            <span class="proto">{{ s.transport }}
              <span v-if="s.tls && !s.tlsValid" class="warn-tri" :title="s.tlsError || 'certificate not trusted'">⚠</span>
            </span>
          </span>
          <span class="c-code" :class="codeClass(s)">{{ codeText(s) }}</span>
          <span class="c-n" :class="{ warnloss: s.sent > 0 && s.ok < s.sent }">{{ lossPctText(s) }}</span>
          <span class="c-n">{{ s.rttUs != null ? ms(s.rttUs) : '—' }}</span>
          <span class="c-tl">
            <span class="status" :class="s.status">{{ statusLabel(s) }}</span>
            <span class="timeline">
              <span v-for="(c, i) in cells(s)" :key="i" class="cell" :style="{ background: cellColor(c) }"
                    :title="c.rtt == null ? 'timeout' : ms(c.rtt) + ' ms (' + c.code + ')'" />
            </span>
          </span>
        </button>

        <div v-if="s.expanded" class="detail">
          <div v-if="s.error" class="err">{{ s.error }}</div>
          <template v-else>
            <div v-if="s.tls" class="tls" :class="s.tlsValid ? 'tls-ok' : 'tls-bad'">
              TLS certificate: {{ s.tlsValid ? 'valid' : ('invalid — ' + (s.tlsError || 'not trusted')) }}
            </div>
            <div class="panes">
              <div class="pane">
                <div class="pane-h">Request</div>
                <pre>{{ s.request || '—' }}</pre>
              </div>
              <div class="pane">
                <div class="pane-h">Response</div>
                <pre>{{ s.response || (s.responded ? '—' : 'no response (timeout)') }}</pre>
              </div>
            </div>
          </template>
        </div>
      </template>
    </div>

    <div v-else class="hint">Enter a SIP target and press Send to probe from every selected site.</div>
  </div>
</template>

<style scoped>
.sip { display: flex; flex-direction: column; min-height: 100%; }
.bar {
  display: flex; flex-wrap: wrap; align-items: center; gap: 8px;
  padding: 12px 16px; border-bottom: 1px solid var(--line); background: var(--panel);
}
.ctl {
  height: var(--ctl-h); padding: 0 8px; border: 1px solid var(--line);
  border-radius: var(--ctl-radius); background: var(--bg); color: var(--fg); font: inherit;
}
.target { flex: 1 1 240px; min-width: 180px; }
.port { width: 78px; }
.num { width: 58px; text-align: right; }
.cyc { display: inline-flex; align-items: center; gap: 4px; color: var(--fg-dim); font-size: 12px; }
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

.results { padding: 8px 16px 24px; }
.head, .row {
  display: grid; grid-template-columns: 120px 1.4fr 160px 64px 64px 3fr;
  align-items: center; gap: 8px; padding: 6px 8px;
}
.head { color: var(--fg-dim); font-size: 11px; text-transform: uppercase; letter-spacing: 0.03em; border-bottom: 1px solid var(--line); }
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
.c-code { font-variant-numeric: tabular-nums; font-weight: 600; }
.c-code.ok { color: var(--ok); }
.c-code.warn { color: var(--warn); }
.c-code.bad { color: var(--bad); }
.c-code.muted, .muted { color: var(--fg-dim); }
.warnloss { color: var(--bad); font-weight: 600; }
.c-site { display: flex; align-items: center; gap: 6px; font-weight: 600; }
.caret { display: inline-block; font-size: 9px; color: var(--fg-dim); transition: transform 0.1s; }
.caret.open { transform: rotate(90deg); }
.c-ip { display: flex; flex-direction: column; line-height: 1.25; min-width: 0; }
.c-ip .ip { font-variant-numeric: tabular-nums; }
.c-ip .proto { color: var(--fg-dim); font-size: 11px; text-transform: uppercase; }
.warn-tri { color: var(--warn); cursor: help; font-size: 12px; }
.c-tl { display: flex; align-items: center; gap: 10px; min-width: 0; }
.status { flex: none; width: 52px; font-size: 11px; color: var(--fg-dim); }
.status.done { color: var(--ok); }
.status.error, .status.offline { color: var(--bad); }
.timeline { display: flex; gap: 1px; flex-wrap: nowrap; overflow: hidden; }
.cell { width: 7px; height: 18px; border-radius: 1px; flex: none; }

.detail { padding: 6px 8px 14px 34px; }
.err { color: var(--bad); font-size: 13px; padding: 6px 0; }
.tls { font-size: 12px; padding: 4px 0 8px; }
.tls-ok { color: var(--ok); }
.tls-bad { color: var(--warn); }
.panes { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.pane { min-width: 0; }
.pane-h { font-size: 11px; text-transform: uppercase; letter-spacing: 0.03em; color: var(--fg-dim); margin-bottom: 4px; }
.pane pre {
  margin: 0; padding: 8px 10px; background: var(--bg); border: 1px solid var(--line); border-radius: 6px;
  font: 12px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace; white-space: pre-wrap; overflow-wrap: anywhere;
  max-height: 320px; overflow: auto;
}
@media (max-width: 720px) { .panes { grid-template-columns: 1fr; } }
.hint { color: var(--fg-dim); padding: 40px 16px; text-align: center; }
</style>
