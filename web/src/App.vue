<script setup lang="ts">
// The shell: an icon-only rail on the left, the active tool filling the rest.
import IconRail from './components/IconRail.vue'
import LoginGate from './components/LoginGate.vue'
import { ready, authEnabled, user } from './session'
</script>

<template>
  <!-- Nothing renders until we know who this is: a flash of the app followed by
       a bounce to the IdP would look like a bug. -->
  <div v-if="!ready" class="boot" />
  <LoginGate v-else-if="authEnabled && !user" />
  <div v-else class="app">
    <IconRail />
    <main class="body">
      <RouterView />
    </main>
  </div>
</template>

<style>
:root,
:root[data-theme='light'] {
  color-scheme: light;
  --bg: #f6f7f9;
  --panel: #fff;
  --rail: #fff;
  --line: #e2e5ea;
  --hover: #eef1f5;
  --fg: #1b1f24;
  --fg-dim: #6b7280;
  --accent: #2563eb;
  --accent-solid: #2563eb;
  --tooltip: #23272e;
  --tooltip-fg: #fff;
  --scroll-thumb: rgb(0 0 0 / 20%);
  --scroll-thumb-hover: rgb(0 0 0 / 35%);
  --ok: #15803d;
  --warn: #b45309;
  --bad: #c0392b;

  /* RTT heat cells: fast -> slow, plus loss. Tuned to read against the row. */
  --cell-good: #16a34a;
  --cell-ok: #65a30d;
  --cell-warn: #d97706;
  --cell-bad: #dc2626;
  --cell-loss: #7f1d1d;
  --cell-fg: #fff;

  --ctl-h: 30px;
  --ctl-radius: 5px;
}

@media (prefers-color-scheme: dark) {
  :root:not([data-theme='light']) {
    color-scheme: dark;
    --bg: #14171c;
    --panel: #1b1f26;
    --rail: #171b21;
    --line: #2b313a;
    --hover: #242a33;
    --fg: #e6e8eb;
    --fg-dim: #9aa3ae;
    --accent: #3b82f6;
    --tooltip: #e6e8eb;
    --tooltip-fg: #14171c;
    --scroll-thumb: rgb(255 255 255 / 20%);
    --scroll-thumb-hover: rgb(255 255 255 / 35%);
    --ok: #4ade80;
    --warn: #fbbf24;
    --bad: #f87171;
    --cell-good: #15803d;
    --cell-ok: #4d7c0f;
    --cell-warn: #b45309;
    --cell-bad: #b91c1c;
    --cell-loss: #7f1d1d;
    --cell-fg: #f3f4f6;
  }
}

:root[data-theme='dark'] {
  color-scheme: dark;
  --bg: #14171c;
  --panel: #1b1f26;
  --rail: #171b21;
  --line: #2b313a;
  --hover: #242a33;
  --fg: #e6e8eb;
  --fg-dim: #9aa3ae;
  --accent: #3b82f6;
  --tooltip: #e6e8eb;
  --tooltip-fg: #14171c;
  --scroll-thumb: rgb(255 255 255 / 20%);
  --scroll-thumb-hover: rgb(255 255 255 / 35%);
  --ok: #4ade80;
  --warn: #fbbf24;
  --bad: #f87171;
  --cell-good: #15803d;
  --cell-ok: #4d7c0f;
  --cell-warn: #b45309;
  --cell-bad: #b91c1c;
  --cell-loss: #7f1d1d;
  --cell-fg: #f3f4f6;
}

* {
  box-sizing: border-box;
}
* {
  scrollbar-width: thin;
  scrollbar-color: var(--scroll-thumb) transparent;
}
::-webkit-scrollbar {
  width: 8px;
  height: 8px;
}
::-webkit-scrollbar-thumb {
  background: var(--scroll-thumb);
  border-radius: 4px;
}
::-webkit-scrollbar-thumb:hover {
  background: var(--scroll-thumb-hover);
}
body {
  margin: 0;
  background: var(--bg);
  color: var(--fg);
  font: 14px/1.5 system-ui, -apple-system, sans-serif;
}

/* Visually hidden but available to screen readers and label associations. */
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}
</style>

<style scoped>
.app {
  display: flex;
  height: 100vh;
  overflow: hidden;
}
.body {
  flex: 1;
  min-width: 0;
  overflow: auto;
}
.boot { height: 100vh; }
</style>
