import { captureScope, scopeCurrent } from './systems.svelte.js'

// Successful uploads bump this signal so same-tab lists refresh.

export const uploadBus = $state({ revision: 0 })

export function markUploaded(scope = captureScope()) {
  if (scopeCurrent(scope)) uploadBus.revision++
}
