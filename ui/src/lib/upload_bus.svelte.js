// Small reactive signal that any component watching for "an upload
// just landed" can subscribe to without a global event bus.
//
// UploadBox bumps `revision` on every successful upload (201 fresh
// row, 200 restored, 409 not counted). Documents.svelte + Dashboard
// list widgets add `uploadBus.revision` to their $effect deps so
// their listDocuments() call re-runs when an in-modal upload finishes
// on the same page — visibilitychange won't fire in that flow.

export const uploadBus = $state({ revision: 0 })

export function markUploaded() {
  uploadBus.revision++
}
