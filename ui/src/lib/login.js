const localLoginPath = '/login'

export function getLoginPath(root = globalThis.document) {
  const configured = root
    ?.querySelector('meta[name="suchi-login-path"]')
    ?.getAttribute('content')
    ?.trim()

  return configured?.startsWith('/') && !configured.startsWith('//')
    ? configured
    : localLoginPath
}

export function usesExternalLogin(root = globalThis.document) {
  return getLoginPath(root) !== localLoginPath
}
