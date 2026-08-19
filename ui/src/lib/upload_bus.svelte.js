// Successful uploads bump this signal so same-tab lists refresh.

export const uploadBus = $state({ revision: 0 })

export function markUploaded() {
  uploadBus.revision++
}
