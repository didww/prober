<script setup lang="ts">
// The signed-in user at the foot of the rail, above the version. Rendered only
// when auth is on. Click for a small popover with the identity and Sign out.
import { computed, ref, watch } from 'vue'
import { user, logout } from '../session'

const open = ref(false)
const root = ref<HTMLElement | null>(null)

const label = computed(() => user.value?.name || user.value?.email || user.value?.sub || '')
const initials = computed(() =>
  label.value
    .split(/[\s@._-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0]?.toUpperCase() ?? '')
    .join(''),
)

function onDocClick(e: MouseEvent) {
  if (root.value && !root.value.contains(e.target as Node)) open.value = false
}
watch(open, (isOpen) => {
  if (isOpen) document.addEventListener('mousedown', onDocClick)
  else document.removeEventListener('mousedown', onDocClick)
})
</script>

<template>
  <div v-if="user" ref="root" class="user">
    <button type="button" class="btn" :aria-label="label" :aria-expanded="open" @click="open = !open">
      <span class="ini">{{ initials }}</span>
      <span class="tip">{{ label }}</span>
    </button>
    <div v-if="open" class="pop">
      <div class="who">
        <strong>{{ user.name || user.email || user.sub }}</strong>
        <span v-if="user.email && user.name" class="muted">{{ user.email }}</span>
      </div>
      <button type="button" class="out" @click="logout">Sign out</button>
    </div>
  </div>
</template>

<style scoped>
.user { position: relative; }
.btn {
  position: relative;
  display: grid;
  place-items: center;
  width: 40px;
  height: 40px;
  background: none;
  border: 0;
  border-radius: 8px;
  cursor: pointer;
}
.btn:hover { background: var(--hover); }
.ini {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border-radius: 50%;
  background: var(--accent-solid);
  color: #fff;
  font-size: 11px;
  font-weight: 700;
}
.pop {
  position: absolute;
  left: calc(100% + 6px);
  bottom: 0;
  z-index: 40;
  width: 220px;
  padding: 8px;
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 8px;
  box-shadow: 0 8px 24px rgb(0 0 0 / 25%);
}
.who {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding-bottom: 8px;
  font-size: 12px;
  overflow-wrap: anywhere;
}
.muted { color: var(--fg-dim); }
.out {
  width: 100%;
  height: 28px;
  border: 1px solid var(--line);
  border-radius: var(--ctl-radius);
  background: var(--bg);
  color: var(--fg);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}
.out:hover { border-color: var(--accent); }
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
.btn:hover .tip { opacity: 1; }
</style>
