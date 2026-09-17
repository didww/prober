<script setup lang="ts">
// The Agents page: which probers are connected right now, with their versions,
// how long the process and the current connection have been up, and the source
// addresses their probes leave from.
import { onMounted, onBeforeUnmount, ref } from 'vue'
import { listAgents, type Agent } from '../api'
import { since } from '../settings'

const agents = ref<Agent[]>([])
const loaded = ref(false)
const now = ref(Date.now())
let poll = 0
let tick = 0

async function refresh() {
  try {
    agents.value = await listAgents()
  } finally {
    loaded.value = true
  }
}

onMounted(() => {
  void refresh()
  poll = window.setInterval(refresh, 5000) // re-fetch the list
  tick = window.setInterval(() => (now.value = Date.now()), 1000) // live-tick the durations
})
onBeforeUnmount(() => {
  clearInterval(poll)
  clearInterval(tick)
})
</script>

<template>
  <div class="agents">
    <header class="bar">
      <h1>Agents</h1>
      <span class="count">{{ agents.length }} connected</span>
    </header>

    <table v-if="agents.length" class="grid">
      <thead>
        <tr>
          <th>Site</th>
          <th>Host</th>
          <th>Version</th>
          <th>Families</th>
          <th class="r">Uptime</th>
          <th class="r">Connected</th>
          <th>Source addresses</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="a in agents" :key="a.site">
          <td class="site">{{ a.site }}</td>
          <td class="dim">{{ a.hostname || '—' }}</td>
          <td>
            {{ a.version || '—' }}<span v-if="a.commit" class="dim"> ({{ a.commit }})</span>
          </td>
          <td>
            <span v-if="a.ipv4" class="fam">IPv4</span>
            <span v-if="a.ipv6" class="fam">IPv6</span>
          </td>
          <td class="r mono">{{ since(a.started_at, now) }}</td>
          <td class="r mono">{{ since(a.connected_at, now) }}</td>
          <td class="mono">
            <div v-if="a.sources && a.sources.length" class="srcs">
              <span v-for="s in a.sources" :key="s" class="src">{{ s }}</span>
            </div>
            <span v-else class="dim">—</span>
          </td>
        </tr>
      </tbody>
    </table>

    <p v-else-if="loaded" class="hint">No agents connected.</p>
    <p v-else class="hint">Loading…</p>
  </div>
</template>

<style scoped>
.agents { padding: 16px; }
.bar { display: flex; align-items: baseline; gap: 12px; border-bottom: 1px solid var(--line); padding-bottom: 10px; margin-bottom: 12px; }
h1 { font-size: 16px; margin: 0; }
.count { color: var(--fg-dim); font-size: 12px; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th { text-align: left; color: var(--fg-dim); font-weight: 500; font-size: 11px; text-transform: uppercase; letter-spacing: 0.03em; padding: 6px 10px; border-bottom: 1px solid var(--line); }
.grid td { padding: 8px 10px; border-bottom: 1px solid var(--line); }
.grid th.r, .grid td.r { text-align: right; }
.site { font-weight: 600; }
.dim { color: var(--fg-dim); }
.mono { font-variant-numeric: tabular-nums; }
.fam { display: inline-block; margin-right: 4px; padding: 1px 6px; border: 1px solid var(--line); border-radius: 999px; font-size: 11px; color: var(--fg-dim); }
.srcs { display: flex; flex-direction: column; gap: 2px; }
.src { display: block; }
.hint { color: var(--fg-dim); padding: 30px 0; text-align: center; }
</style>
