// SPDX-License-Identifier: AGPL-3.0-or-later

// App's /api/stats/ poll is the only processing poll; pages react to it
// instead of polling on their own.
// - `revision` bumps when a poll may mean work finished: fewer unfinished
//   documents than the previous poll, or none at all. Pages showing one
//   unfinished item re-check it once per bump.
// - `finished` bumps only when the count actually drops, so lists of
//   unfinished work refresh as documents finish and stay quiet when idle.
// - `refresh` asks App to poll now, for example right after an upload.

export const processingBus = $state({ revision: 0, finished: 0, refresh: 0 })

let previous = 0

export function noteProcessing(count) {
  if (count < previous) processingBus.finished++
  if (count < previous || count === 0) processingBus.revision++
  previous = count
}

export function requestStatusRefresh() {
  processingBus.refresh++
}
