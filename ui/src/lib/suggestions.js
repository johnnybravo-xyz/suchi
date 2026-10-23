// SPDX-License-Identifier: AGPL-3.0-or-later

export const SUGGESTION_FIELDS = ['jd_category', 'correspondent', 'tag', 'title', 'language', 'custom_field']

// A document-change suggestion can be applied only while its evidence is
// current, nothing has changed underneath it, and it is waiting for review.
export function canApplySuggestion(task) {
  const vars = task?.vars
  return Array.isArray(task?.choices) && task.choices.includes('apply') &&
    SUGGESTION_FIELDS.includes(vars?.field) &&
    vars.source_current === true && vars.review_conflict === false &&
    ['review_first', 'low_confidence'].includes(vars.reason)
}

// Applicable filing suggestions keyed by document ID.
export function filingSuggestions(tasks) {
  const byDocument = new Map()
  for (const task of tasks || []) {
    if (task.approval_name !== 'document-change' || task.vars?.field !== 'jd_category') continue
    if (!task.doc_id || !task.vars.proposed_id || !canApplySuggestion(task)) continue
    byDocument.set(task.doc_id, { taskID: task.id, categoryID: task.vars.proposed_id, label: task.vars.proposed_value })
  }
  return byDocument
}
