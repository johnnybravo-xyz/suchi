// Saved views persist API filter names. Browser routes keep their shorter,
// user-facing names and translate back at the Documents boundary.
export function documentListHash(filters = {}) {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(filters)) {
    if (value === '' || value == null) continue
    params.set(key === 'jd_category_id' ? 'jd' : key, String(value))
  }
  const query = params.toString()
  return `#/documents${query ? `?${query}` : ''}`
}
