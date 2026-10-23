// SPDX-License-Identifier: AGPL-3.0-or-later

// App's /api/stats/ poll is the only processing poll. It bumps `revision`
// when a poll may mean work finished: fewer unfinished documents than the
// previous poll, or none at all. Pages showing unfinished work refresh it
// once per bump instead of polling on their own; others ignore the signal.
// `refresh` asks App to poll now, for example right after an upload.

export const processingBus = $state({ revision: 0, refresh: 0 })

let previous = 0

export function noteProcessing(count) {
  if (count < previous || count === 0) processingBus.revision++
  previous = count
}

export function requestStatusRefresh() {
  processingBus.refresh++
}
