// Keep secondary routes in one lazy chunk so committed builds do not churn a
// hashed file per route.
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
