// SPDX-License-Identifier: AGPL-3.0-or-later
import { readdirSync, readFileSync } from 'node:fs'
import { extname, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

const pdfjsRoot = fileURLToPath(new URL('./node_modules/pdfjs-dist/', import.meta.url))
const pdfjsRootPrefix = `${resolve(pdfjsRoot)}${sep}`
const pdfjsAssetDirectories = ['cmaps', 'iccs', 'standard_fonts']
const pdfjsDecoderAssets = [
  'wasm/jbig2_nowasm_fallback.js',
  'wasm/openjpeg_nowasm_fallback.js',
  'wasm/LICENSE_JBIG2',
  'wasm/LICENSE_OPENJPEG',
  'wasm/LICENSE_PDFJS_JBIG2',
  'wasm/LICENSE_PDFJS_OPENJPEG',
]
const pdfjsLicenseBanner = `/*! @license PDF.js 6.4.299
 * Copyright 2024 Mozilla Foundation
 * Apache-2.0; complete text: ./pdfjs/LICENSE
 */`
const pdfjsDecoderNotices = {
  'wasm/jbig2_nowasm_fallback.js': [
    'LICENSE',
    'wasm/LICENSE_JBIG2',
    'wasm/LICENSE_PDFJS_JBIG2',
  ],
  'wasm/openjpeg_nowasm_fallback.js': [
    'LICENSE',
    'wasm/LICENSE_OPENJPEG',
    'wasm/LICENSE_PDFJS_OPENJPEG',
  ],
}

function pdfjsAssetSource(source) {
  const name = relative(pdfjsRoot, source).split(sep).join('/')
  const notices = pdfjsDecoderNotices[name]
  if (!notices) return readFileSync(source)
  const banner = notices.map(notice => readFileSync(join(pdfjsRoot, notice), 'utf8')
    .trimEnd()
    .split('\n')
    .map(line => `// ${line}`)
    .join('\n'))
    .join('\n//\n')
  return `${banner}\n\n${readFileSync(source, 'utf8')}`
}

function filesBelow(directory) {
  return readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
    const path = join(directory, entry.name)
    return entry.isDirectory() ? filesBelow(path) : [path]
  })
}

function pdfjsAssets() {
  const files = [
    join(pdfjsRoot, 'LICENSE'),
    ...pdfjsAssetDirectories.flatMap(directory => filesBelow(join(pdfjsRoot, directory))),
    ...pdfjsDecoderAssets.map(path => join(pdfjsRoot, path)),
  ]
  const contentTypes = {
    '.bcmap': 'application/octet-stream',
    '.icc': 'application/vnd.iccprofile',
    '.pfb': 'application/octet-stream',
    '.ttf': 'font/ttf',
    '.js': 'text/javascript; charset=utf-8',
  }

  return {
    name: 'pdfjs-assets',
    enforce: 'post',
    configureServer(server) {
      server.middlewares.use((request, response, next) => {
        const pathname = decodeURIComponent(new URL(request.url, 'http://vite.local').pathname)
        const prefix = '/assets/pdfjs/'
        if (!pathname.startsWith(prefix)) return next()
        const name = pathname.slice(prefix.length)
        const allowed = name === 'LICENSE' ||
          pdfjsDecoderAssets.includes(name) ||
          pdfjsAssetDirectories.some(directory => name.startsWith(`${directory}/`))
        const source = resolve(pdfjsRoot, name)
        if (!allowed || !source.startsWith(pdfjsRootPrefix)) return next()
        try {
          response.setHeader('Content-Type', contentTypes[extname(source)] || 'text/plain; charset=utf-8')
          response.setHeader('Cache-Control', 'no-store')
          response.end(pdfjsAssetSource(source))
        } catch {
          next()
        }
      })
    },
    generateBundle() {
      for (const source of files) {
        this.emitFile({
          type: 'asset',
          fileName: `assets/pdfjs/${relative(pdfjsRoot, source)}`,
          source: pdfjsAssetSource(source),
        })
      }
    },
  }
}

const serverProxy = Object.fromEntries(
  ['/api', '/preview', '/download', '/login', '/setup', '/assets']
    .map(path => [path, 'http://127.0.0.1:8000'])
)

// Dev: run `make run` (suchi on :8000) alongside `make ui-dev`;
// every /api|/preview|/download|/login call proxies to the Go binary.
export default defineConfig({
  plugins: [pdfjsAssets(), svelte({ configFile: false })],
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
        banner: chunk => Object.keys(chunk.modules).some(module =>
          /[\\/]pdfjs-dist[\\/]/.test(module)
        ) ? pdfjsLicenseBanner : '',
        codeSplitting: {
          groups: [{
            name: 'shell',
            test: /[\\/]src[\\/](?:App\.svelte(?:\?|$)|lib[\\/](?:api|systems\.svelte)\.js$)/,
            includeDependenciesRecursively: true,
          }, {
            name: 'document-controls',
            test: /[\\/]src[\\/]lib[\\/](?:(?:ConfirmDialog|ISODateInput|LinkQR|DocumentUnlockStatus|ShareLinkForm|TypeaheadPicker)\.svelte(?:\?|$)|(?:clipboard|queryAssist|upload_bus\.svelte|format|configuration)\.js$)/,
            // Shared controls stay lazy; their shell dependencies keep their existing owner.
            includeDependenciesRecursively: false,
          }],
        },
        comments: { legal: true },
        // Content hashes prevent an upgraded binary from pairing with a
        // browser-cached bundle from the previous release.
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  }
})
