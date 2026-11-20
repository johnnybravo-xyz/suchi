// SPDX-License-Identifier: AGPL-3.0-or-later

// Two side-by-side panes that show the same items highlight the partner of a
// hovered entry. The tint follows at once; the other pane scrolls only after
// the pointer rests, so crossing a pane on the way elsewhere doesn't scroll it.
// Focus is deliberate, so callers reveal at once.
export const REVEAL_DELAY_MS = 300

// Side by side, each pane scrolls on its own, so a linked entry can sit
// outside the other pane's view. Stacked panes share the page scroll and are
// skipped: scrolling there would move the hovered entry away.
export function revealLinked(scroller, selector, source) {
  if (!scroller || !source || getComputedStyle(scroller).overflowY !== 'auto') return
  const linked = [...scroller.querySelectorAll(selector)]
  if (!linked.length) return
  const view = scroller.getBoundingClientRect()
  if (linked.some(entry => {
    const box = entry.getBoundingClientRect()
    return box.top >= view.top && box.bottom <= view.bottom
  })) return
  // Level the entry with the hovered one, so moving straight across lands on
  // it, while keeping it inside the pane; a tall entry keeps its top in view.
  const box = linked[0].getBoundingClientRect()
  const from = source.getBoundingClientRect()
  const margin = 8
  const levelTop = from.top + from.height / 2 - box.height / 2
  const top = Math.max(view.top + margin, Math.min(levelTop, view.bottom - margin - box.height))
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches
  scroller.scrollBy({ top: box.top - top, behavior: reduceMotion ? 'auto' : 'smooth' })
}
