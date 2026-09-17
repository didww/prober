import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  // Relative asset URLs, resolved by the <base href> the Go server injects, so
  // one binary can be mounted at "/" or under a sub-path with no rebuild.
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    // Dev only: Vite serves the SPA with HMR and proxies the API and the SSE
    // stream to the Go backend.
    proxy: {
      '/api': 'http://127.0.0.1:8080',
    },
  },
})
