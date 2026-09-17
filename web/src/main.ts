import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import TraceTool from './tools/TraceTool.vue'
import { initTheme } from './theme'
import { BASE } from './base'
import { loadConfig } from './session'

initTheme()

// Find out whether auth is on and who we are before anything renders.
void loadConfig()

const router = createRouter({
  history: createWebHistory(BASE),
  routes: [
    { path: '/', redirect: '/trace' },
    { path: '/trace', component: TraceTool },
  ],
})

createApp(App).use(router).mount('#app')
