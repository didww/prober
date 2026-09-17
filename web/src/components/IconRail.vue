<script setup lang="ts">
// App navigation: one icon per tool, icon-only with a hover label. The foot of
// the rail carries the display preferences and the build version.
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { getVersion, type Build } from '../api'
import { TOOLS } from '../tools'
import ThemeToggle from './ThemeToggle.vue'
import TimeFormatToggle from './TimeFormatToggle.vue'
import UserMenu from './UserMenu.vue'

const route = useRoute()
const isActive = (path: string) => route.path === path || route.path.startsWith(path + '/')

const build = ref<Build | null>(null)
onMounted(async () => {
  try {
    build.value = await getVersion()
  } catch {
    /* not worth breaking the rail over */
  }
})
</script>

<template>
  <nav class="rail" aria-label="Tools">
    <RouterLink to="/" class="logo" aria-label="prober">
      <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor"
           stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <circle cx="4" cy="12" r="1.5" fill="currentColor" stroke="none" />
        <path d="M6.2 12h3l2-5 3 10 2-6h3" />
        <circle cx="21" cy="12" r="1.5" fill="currentColor" stroke="none" />
      </svg>
    </RouterLink>

    <RouterLink
      v-for="t in TOOLS"
      :key="t.id"
      :to="t.path"
      class="tool"
      :class="{ active: isActive(t.path) }"
      :aria-current="isActive(t.path) ? 'page' : undefined"
      :aria-label="t.title"
    >
      <span class="icon" v-html="t.icon" />
      <span class="tip">{{ t.title }}</span>
    </RouterLink>

    <div class="prefs">
      <TimeFormatToggle />
      <ThemeToggle />
      <UserMenu />
      <!-- The version, always on screen — the first question of any incident.
           Its hover carries the flag and the line under it: this is a Ukrainian
           project. Blue over yellow in the light theme, red over black in the
           dark one (the colours are in the CSS, where the theme is). -->
      <span v-if="build" class="build" aria-label="Made in Ukraine">
        {{ build.version }}
        <span class="tip">
          Made in Ukraine
          <svg class="flag" viewBox="0 0 24 16" width="18" height="12" aria-hidden="true">
            <rect class="upper" width="24" height="8" />
            <rect class="lower" y="8" width="24" height="8" />
          </svg>
        </span>
      </span>
    </div>
  </nav>
</template>

<style scoped>
.rail {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  flex: 0 0 48px;
  width: 48px;
  padding: 10px 0;
  background: var(--rail);
  border-right: 1px solid var(--line);
}
.logo {
  display: grid;
  place-items: center;
  width: 40px;
  height: 40px;
  margin-bottom: 6px;
  color: var(--accent);
}
.tool {
  position: relative;
  display: grid;
  place-items: center;
  width: 40px;
  height: 40px;
  border-radius: 8px;
  color: var(--fg-dim);
  text-decoration: none;
}
.tool:hover {
  background: var(--hover);
  color: var(--fg);
}
.tool.active {
  background: var(--accent);
  color: #fff;
}
.icon :deep(svg) {
  display: block;
}
.prefs {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 2px;
  margin-top: auto;
}
.build {
  position: relative;
  max-width: 46px;
  padding: 4px 0;
  color: var(--fg-dim);
  font-size: 10px;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
  cursor: default;
}
.build:hover .tip {
  opacity: 1;
}
/* The label's contents: the words, then the flag, on one line. */
.build .tip {
  display: inline-flex;
  align-items: center;
  gap: 7px;
}
.flag {
  flex: none;
  display: block;
  border-radius: 2px;
  /* A hairline, so the lower band is seen against the label in either theme. */
  outline: 1px solid rgb(128 128 128 / 40%);
}
.flag .upper {
  fill: #0057b7;
}
.flag .lower {
  fill: #ffd700;
}
:root[data-theme='dark'] .flag .upper {
  fill: #d0021b;
}
:root[data-theme='dark'] .flag .lower {
  fill: #0b0b0b;
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
.tool:hover .tip {
  opacity: 1;
}
</style>
