import { describe, expect, test } from 'bun:test'
import { getLoginPath, usesExternalLogin } from './login.js'

function shellWith(content) {
  return {
    querySelector() {
      return content == null ? null : { getAttribute: () => content }
    },
  }
}

describe('SPA login path', () => {
  test('defaults to the local login page', () => {
    expect(getLoginPath(shellWith(null))).toBe('/login')
    expect(usesExternalLogin(shellWith(null))).toBe(false)
  })

  test('accepts the server-provided OIDC path', () => {
    expect(getLoginPath(shellWith('/oidc/login'))).toBe('/oidc/login')
    expect(usesExternalLogin(shellWith('/oidc/login'))).toBe(true)
  })

  test('rejects absolute and protocol-relative URLs', () => {
    expect(getLoginPath(shellWith('https://evil.example/login'))).toBe('/login')
    expect(getLoginPath(shellWith('//evil.example/login'))).toBe('/login')
  })
})
