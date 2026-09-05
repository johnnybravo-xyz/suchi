import { autocomplete } from './api.js'

export function createQueryAssistant(onSuggestions, { delay = 140, limit = 8 } = {}) {
  let timer
  let version = 0
  let activeController // stop stale suggestions at the server too

  function cancel() {
    version++
    clearTimeout(timer)
    timer = undefined
    activeController?.abort()
    activeController = undefined
  }

  function clear() {
    cancel()
    onSuggestions([])
  }

  function update(query) {
    const current = ++version
    clearTimeout(timer)
    activeController?.abort()
    activeController = undefined
    const value = String(query || '')
    if (!value.trim()) {
      onSuggestions([])
      return
    }
    timer = setTimeout(async () => {
      timer = undefined
      const controller = new AbortController()
      activeController = controller
      try {
        const response = await autocomplete(value, limit, controller.signal)
        const next = (response?.results || response || []).filter((suggestion) => suggestion.query)
        if (current === version) onSuggestions(next)
      } catch {
        if (current === version) onSuggestions([])
      } finally {
        if (activeController === controller) activeController = undefined
      }
    }, delay)
  }

  return { update, clear, dispose: cancel }
}
