import assert from 'node:assert/strict'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { gzipSync } from 'node:zlib'
import { build } from 'vite'

test('production chunks group document controls without pulling lazy screens into the shell', async () => {
  const oldEnvironment = process.env.NODE_ENV
  let result
  try {
    // Bun sets NODE_ENV=test; compile the same Svelte production output as make ui.
    process.env.NODE_ENV = 'production'
    result = await build({
      root: fileURLToPath(new URL('..', import.meta.url)),
      logLevel: 'error',
      build: { write: false },
    })
  } finally {
    if (oldEnvironment === undefined) delete process.env.NODE_ENV
    else process.env.NODE_ENV = oldEnvironment
  }
  const chunks = result.output.filter(asset => asset.type === 'chunk')
  const byName = new Map(chunks.map(chunk => [chunk.fileName, chunk]))
  const owner = suffix => chunks.find(chunk => Object.keys(chunk.modules).some(id => id.endsWith(suffix)))
  const entry = chunks.find(chunk => chunk.isEntry)

  function dependencies(chunk, found = new Set(), ancestors = new Set()) {
    assert.ok(!ancestors.has(chunk.fileName), `Circular static chunk dependency: ${chunk.fileName}`)
    if (found.has(chunk.fileName)) return found
    found.add(chunk.fileName)
    const next = new Set([...ancestors, chunk.fileName])
    for (const name of chunk.imports) if (byName.has(name)) dependencies(byName.get(name), found, next)
    return found
  }

  const initialFiles = dependencies(entry)
  const initialGzip = [...initialFiles].reduce((total, name) => total + gzipSync(byName.get(name).code).length, 0)
  assert.ok(initialGzip <= 35 * 1024, `Initial JavaScript is ${initialGzip} gzip bytes; budget is 35 KiB`)
  const controls = owner('/lib/ConfirmDialog.svelte')
  assert.ok(controls)
  assert.equal(owner('/lib/clipboard.js'), controls)
  assert.equal(owner('/lib/queryAssist.js'), controls)
  assert.equal(owner('/lib/LinkQR.svelte'), controls)
  assert.equal(owner('/lib/upload_bus.svelte.js'), controls)
  assert.ok(!initialFiles.has(controls.fileName))
  const qr = owner('/qrcode-generator/dist/qrcode.mjs')
  assert.ok(qr)
  assert.ok(!dependencies(controls).has(qr.fileName), 'QR encoder must load only when requested')

  for (const path of [
    '/routes/Documents.svelte', '/routes/DocumentDetail.svelte', '/routes/Search.svelte',
    '/routes/Calendar.svelte', '/routes/Tasks.svelte', '/routes/Trash.svelte',
    '/routes/Settings.svelte', '/routes/ArchiveSettings.svelte', '/routes/Setup.svelte',
    '/lib/EmailAccounts.svelte', '/lib/ArchiveChat.svelte', '/lib/UploadBox.svelte',
  ]) {
    const chunk = owner(path)
    assert.ok(chunk, `Missing bundled module ${path}`)
    assert.ok(!initialFiles.has(chunk.fileName), `${path} was bundled into the initial page`)
    dependencies(chunk)
  }
  for (const route of ['Documents', 'DocumentDetail']) {
    const files = [...dependencies(owner(`/routes/${route}.svelte`))].filter(name => !initialFiles.has(name))
    assert.ok(files.length <= 2, `${route} requires ${files.length} additional JavaScript requests`)
  }
})
