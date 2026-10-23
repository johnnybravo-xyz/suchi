// SPDX-License-Identifier: AGPL-3.0-or-later

import { captureScope, scopeCurrent } from './systems.svelte.js'
import { requestStatusRefresh } from './processing_bus.svelte.js'

// Successful uploads bump this signal so same-tab lists refresh.

export const uploadBus = $state({ revision: 0 })

export function markUploaded(scope = captureScope()) {
  if (!scopeCurrent(scope)) return
  uploadBus.revision++
  requestStatusRefresh()
}
