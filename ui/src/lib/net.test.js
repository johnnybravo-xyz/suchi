import assert from 'node:assert/strict'
import test from 'node:test'
import { isLocalEndpoint } from './net.js'

test('recognizes local hosts accepted by the backend', () => {
  const hosts = [
    'localhost', 'service.localhost', 'host.local', '127.2.3.4',
    '10.0.0.5', '172.31.255.255', '192.168.1.10', '169.254.2.3',
    '::1', 'fc00::1', 'fd12::1', 'fe80::1', '::ffff:192.168.1.10',
  ]
  for (const host of hosts) {
    const bracketed = host.includes(':') ? `[${host}]` : host
    assert.equal(isLocalEndpoint(`http://${bracketed}:11434/v1`), true, host)
  }
})

test('rejects public and malformed hosts', () => {
  const hosts = [
    'example.com', 'host.internal', 'host.lan', '8.8.8.8', '172.32.0.1',
    '192.169.1.1', '0127.0.0.1', 'fe00::1',
  ]
  for (const host of hosts) {
    const bracketed = host.includes(':') ? `[${host}]` : host
    assert.equal(isLocalEndpoint(`http://${bracketed}:11434/v1`), false, host)
  }
})

test('extracts the host from endpoint URLs', () => {
  assert.equal(isLocalEndpoint('http://10.0.0.5:11434/v1'), true)
  assert.equal(isLocalEndpoint('http://[fd12::1]:11434/v1'), true)
  assert.equal(isLocalEndpoint('https://api.openai.com/v1'), false)
  assert.equal(isLocalEndpoint('not a URL'), true)
})
