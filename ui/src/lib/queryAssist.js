import { autocomplete } from './api.js'

export async function querySuggestions(query, limit = 8) {
  const value = String(query || '')
  if (!value.trim()) return []
  const response = await autocomplete(value, limit)
  return (response?.results || response || []).filter((suggestion) => suggestion.query)
}

export function createQueryAssistant(onSuggestions, { delay = 140, limit = 8 } = {}) {
  let timer
  let version = 0

  function cancel() {
    version++
    clearTimeout(timer)
    timer = undefined
  }

  function clear() {
    cancel()
    onSuggestions([])
  }

  function update(query) {
    const current = ++version
    clearTimeout(timer)
    const value = String(query || '')
    if (!value.trim()) {
      onSuggestions([])
      return
    }
    timer = setTimeout(async () => {
      timer = undefined
      try {
        const next = await querySuggestions(value, limit)
        if (current === version) onSuggestions(next)
      } catch {
        if (current === version) onSuggestions([])
      }
    }, delay)
  }

  return { update, clear, dispose: cancel }
}

export function queryErrorMessage(error, fallback) {
  if (error?.code === 'bad_query') return error.message || 'Invalid query.'
  return error?.message || fallback
}
