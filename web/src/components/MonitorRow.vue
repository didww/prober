<script setup lang="ts">
// One monitor's row on the Monitors page. A component of its own so that a
// status change re-renders only the row it is about: Vue skips a row whose
// props are unchanged, and every other monitor keeps its aggregate object.
import { computed } from 'vue'
import type { Monitor, MonitorAggregate, MonitorState } from '../api'
import SinceText from './SinceText.vue'

const props = defineProps<{
  m: Monitor
  agg: MonitorAggregate
  // Connected agents, the sites a monitor without a site list is expected at.
  agents: number
  open: boolean
}>()
const emit = defineEmits<{ toggle: [] }>()

const STATE_LABEL: Record<MonitorState, string> = { up: 'up', down: 'down', partial: 'partial', nodata: 'no data' }

// "up / expected" where expected is the configured site list, or every
// connected agent for a monitor that runs everywhere, so a site that has
// never reported is not hidden behind a clean "1/1 up".
const sitesText = computed(() => {
  const { m, agg: a } = props
  const expected = Math.max(m.sites.length || props.agents, a.sites)
  if (expected === 0) return '—'
  const parts = [`${a.up}/${expected} up`]
  if (a.stale) parts.push(`${a.stale} stale`)
  if (expected > a.sites) parts.push(`${expected - a.sites} no data`)
  return parts.join(', ')
})

function familyLabel(f?: string): string {
  if (!f) return ''
  if (f.includes('6')) return 'IPv6'
  if (f.includes('4')) return 'IPv4'
  return ''
}

// How a monitor probes, in one line.
const params = computed(() => {
  const m = props.m
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
})
</script>

<template>
  <tr class="row" :class="{ open }" tabindex="0" :aria-expanded="open" @click="emit('toggle')" @keydown.enter.prevent="emit('toggle')" @keydown.space.prevent="emit('toggle')">
    <td><span class="st" :class="agg.state">{{ STATE_LABEL[agg.state] }}</span></td>
    <td class="mono small">{{ sitesText }}</td>
    <td><span class="kind" :class="m.kind">{{ m.kind }}</span></td>
    <td class="id"><span class="caret" :class="{ open }">▶</span>{{ m.id }}</td>
    <td class="mono">{{ m.target }}</td>
    <td class="dim small">{{ params }}</td>
    <td class="labels">
      <span v-for="(v, k) in m.labels" :key="k" class="label">{{ k }}=<b>{{ v }}</b></span>
    </td>
    <td class="r mono dim small"><SinceText :at="agg.since" minutes /></td>
  </tr>
</template>

<style scoped>
.dim { color: var(--fg-dim); }
.small { font-size: 12px; }
.mono { font-variant-numeric: tabular-nums; }
.row { cursor: pointer; }
.row:hover { background: var(--hover); }
.row:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
.row.open td { border-bottom: 0; }
.id { font-weight: 600; white-space: nowrap; }
.caret { display: inline-block; margin-right: 6px; font-size: 9px; color: var(--fg-dim); transition: transform 0.1s; }
.caret.open { transform: rotate(90deg); }
/* Solid badges take the cell palette, which is made for light text on it in
   both themes; the semantic text colours are tuned for text on the panel and
   go too bright in dark mode. */
.kind { padding: 1px 7px; border-radius: 4px; font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; color: var(--cell-fg); background: var(--fg-dim); }
.kind.trace { background: var(--accent-solid, var(--accent)); }
.kind.ping { background: var(--cell-good); }
.kind.sip { background: var(--cell-warn); }
.labels { display: flex; flex-wrap: wrap; gap: 4px; }
.label { padding: 1px 7px; border: 1px solid var(--line); border-radius: 999px; font-size: 11px; color: var(--fg-dim); white-space: nowrap; }
.label b { color: var(--fg); font-weight: 500; }
.st { display: inline-block; min-width: 52px; padding: 1px 8px; border-radius: 999px; font-size: 11px; font-weight: 600; text-align: center; text-transform: uppercase; letter-spacing: 0.03em; white-space: nowrap; }
.st.up { color: var(--ok); background: color-mix(in srgb, var(--ok) 14%, transparent); }
.st.down { color: var(--bad); background: color-mix(in srgb, var(--bad) 14%, transparent); }
.st.partial { color: var(--warn); background: color-mix(in srgb, var(--warn) 14%, transparent); }
.st.nodata { color: var(--fg-dim); background: var(--hover); }
</style>
