import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// Dev: run `make run` (suchi on :8000) alongside `make ui-dev`;
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
    target: 'baseline-widely-available',
    assetsInlineLimit: 8192,        // favicon + icons inline into the bundle
    cssCodeSplit: false,            // one CSS file; dist/ is committed,
                                     // per-chunk CSS would just be more
                                     // hashed files to churn in git
    rollupOptions: {
      output: {
        // Content hashes prevent an upgraded binary from pairing with a
        // browser-cached bundle from the previous release.
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  }
})
