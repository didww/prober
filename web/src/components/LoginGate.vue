<script setup lang="ts">
// Shown when auth is on and nobody is signed in. The app renders nothing else
// until then — not as a security boundary (the server is that), but because
// every panel would 401 and fill the screen with errors instead of a button.
import { begin } from '../session'
</script>

<template>
  <div class="gate">
    <div class="card">
      <svg viewBox="0 0 24 24" width="40" height="40" fill="none" stroke="currentColor"
           stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
        <circle cx="4" cy="12" r="1.6" fill="currentColor" stroke="none" />
        <path d="M6.2 12h3l2-5 3 10 2-6h3" />
        <circle cx="21" cy="12" r="1.6" fill="currentColor" stroke="none" />
      </svg>
      <h1>prober</h1>
      <button type="button" class="primary" @click="begin">Log in</button>
      <p class="note">You will be sent to your identity provider.</p>
    </div>
  </div>
</template>

<style scoped>
.gate {
  display: grid;
  place-items: center;
  width: 100vw;
  height: 100vh;
  background: var(--bg);
}
.card {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 34px 44px;
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 10px;
  box-shadow: 0 10px 30px rgb(0 0 0 / 18%);
  color: var(--accent);
}
h1 { margin: 8px 0 0; font-size: 19px; color: var(--fg); }
.primary {
  margin-top: 18px;
  padding: 8px 26px;
  border: 0;
  border-radius: var(--ctl-radius);
  background: var(--accent-solid);
  color: #fff;
  font: inherit;
  font-weight: 600;
  cursor: pointer;
}
.note { margin: 12px 0 0; font-size: 11px; color: var(--fg-dim); }
</style>
