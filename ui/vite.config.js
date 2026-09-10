import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

const serverProxy = Object.fromEntries(
  ['/api', '/preview', '/download', '/login', '/setup', '/assets']
    .map(path => [path, 'http://127.0.0.1:8000'])
)

// Dev: run `make run` (suchi on :8000) alongside `make ui-dev`;
// every /api|/preview|/download|/login call proxies to the Go binary.
export default defineConfig({
  plugins: [svelte()],
  base: './',                       // embeddable at any path
  server: { proxy: serverProxy },
  // Preview serves its generated /assets locally while retaining backend routes.
  preview: { proxy: Object.fromEntries(Object.entries(serverProxy).filter(([path]) => path !== '/assets')) },
  build: {
    target: 'baseline-widely-available',
    assetsInlineLimit: 8192,        // favicon + icons inline into the bundle
    cssCodeSplit: true,             // load route styles with lazy route chunks
    rolldownOptions: {
      output: {
        codeSplitting: {
          groups: [{
            name: 'document-controls',
            test: /[\\/]src[\\/]lib[\\/](?:(?:ConfirmDialog|LinkQR|DocumentUnlockStatus)\.svelte(?:\?|$)|clipboard\.js$|queryAssist\.js$|upload_bus\.svelte\.js$)/,
            // Shared controls stay lazy; their shell dependencies keep their existing owner.
            includeDependenciesRecursively: false,
          }],
        },
        // Content hashes prevent an upgraded binary from pairing with a
        // browser-cached bundle from the previous release.
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  }
})
