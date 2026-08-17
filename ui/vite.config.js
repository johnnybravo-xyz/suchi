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
    cssCodeSplit: false,            // one CSS file; dist/ is committed,
                                     // per-chunk CSS would just be more
                                     // hashed files to churn in git
    rollupOptions: {
      output: {
        // Stable, unhashed names. dist/ is embedded and committed
        // (see Justfile), so a build that only touches one file
        // should show as one modified file in git, not an
        // add/delete pair from a changed content hash.
        entryFileNames: 'assets/[name].js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: 'assets/[name].[ext]',
      },
    },
  }
})
