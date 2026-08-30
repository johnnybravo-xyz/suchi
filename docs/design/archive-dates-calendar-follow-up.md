# Archive dates and calendar follow-up (not shipped)

This specification is intentionally outside the published product navigation.
Archive chat does not add date schema, extraction jobs, approval UI, calendar
UI, or placeholders.

When scheduled as a separate workflow, extend the existing per-document
classification request to propose zero to three typed ISO dates without a
second model call. Store candidates separately with date role, confidence,
provenance, and pending/accepted/rejected state. Pending dates flow through
Approvals; only accepted dates appear on a calendar.

A user may confirm a preferred date role for a document type. One approval
must never silently create that rule. The workflow may share the current model
configuration and transport, egress and privacy controls, and a dedicated
capability with archive questions; it must not share conversation state.

After acceptance, structured dates may become context and filters for archive
questions and Views. Until then, chat treats dates exactly like other text in
retrieved evidence.
