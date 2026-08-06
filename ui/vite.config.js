import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// Dev: run `just serve` (suchi on :8000) alongside `npm run dev`;
// every /api|/preview|/download|/login call proxies to the Go binary.
export default defineConfig({
  plugins: [svelte()],
  base: './',                       // embeddable at any path
  server: {
    proxy: Object.fromEntries(
      ['/api', '/preview', '/download', '/login', '/setup', '/assets']
        .map(p => [p, 'http://127.0.0.1:8000'])
    )
  },
  build: {
    target: 'es2020',
    assetsInlineLimit: 8192,        // favicon + icons inline into the bundle
    rollupOptions: { output: { manualChunks: undefined } } // one JS file
  }
})
