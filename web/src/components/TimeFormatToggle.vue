<script setup lang="ts">
// 12h / 24h toggle beside the theme button. Click to switch; the tooltip names
// the current setting and what a click will do.
import { computed } from 'vue'
import { setTimeFormat, timeFormat } from '../settings'

const next = computed(() => (timeFormat.value === '24h' ? '12h' : '24h'))
const tip = computed(() => `Time: ${timeFormat.value} — click for ${next.value}`)
</script>

<template>
  <button class="tf" :aria-label="tip" @click="setTimeFormat(next)">
    <span class="txt">{{ timeFormat }}</span>
    <span class="tip">{{ tip }}</span>
  </button>
</template>

<style scoped>
.tf {
  position: relative;
  display: grid;
  place-items: center;
  width: 40px;
  height: 40px;
  background: none;
  border: 0;
  border-radius: 8px;
  color: var(--fg-dim);
  cursor: pointer;
}
.tf:hover {
  background: var(--hover);
  color: var(--fg);
}
.txt {
  font-size: 12px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.tip {
  position: absolute;
  left: calc(100% + 8px);
  top: 50%;
  transform: translateY(-50%);
  z-index: 40;
  padding: 4px 8px;
  border-radius: 4px;
  background: var(--tooltip);
  color: var(--tooltip-fg);
  font-size: 12px;
  white-space: nowrap;
  opacity: 0;
  pointer-events: none;
  transition: opacity 0.1s;
}
.tf:hover .tip {
  opacity: 1;
}
</style>
