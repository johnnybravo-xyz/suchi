// One entry point for every route outside the core loop (login,
// dashboard, documents, detail). Rollup emits exactly one chunk for
// this module no matter how many named exports it re-exports, so all
// ten routes below ship as a SINGLE lazy-loaded file instead of ten.
// First navigation to any of them pays one fetch; the rest are
// cache-hits. This trades a little granularity (the whole bundle
// loads on first visit to any secondary page, not per-page) for a
// stable, small dist/ output — which matters here because dist/ is
// committed and hashed per-chunk filenames would churn on every build.
export { default as Search } from '../routes/Search.svelte'
export { default as Tasks } from '../routes/Tasks.svelte'
export { default as Automations } from '../routes/Automations.svelte'
export { default as Upload } from '../routes/Upload.svelte'
export { default as Settings } from '../routes/Settings.svelte'
export { default as Setup } from '../routes/Setup.svelte'
export { default as Trash } from '../routes/Trash.svelte'
export { default as Admin } from '../routes/Admin.svelte'
export { default as Demo } from '../routes/Demo.svelte'
export { default as Views } from '../routes/Views.svelte'
