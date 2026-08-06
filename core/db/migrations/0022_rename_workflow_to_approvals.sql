-- Rename the state-machine tables from workflow_* → approval_* to
-- match the user-facing "Approvals" concept. Internal-only rename:
-- JSON tags on the /api/tasks/ wire (workflow_tasks, workflow_id) stay
-- unchanged for mobile-compat. See docs/approvals.mdx and the
-- ADR-quality note in core/approvals/approvals.go for what the
-- boundary is.
--
-- Forward-only. Every operator hitting this migration is moving from a
-- pre-rename schema; there is no downgrade path. The three ALTER
-- TABLE statements are single-writer safe (WriteTx pool guarantees
-- serialization) and O(1) — SQLite doesn't rewrite the table on
-- rename, only the schema entry.
--
-- Indexes are dropped-and-recreated because SQLite has no ALTER INDEX
-- RENAME; the new names carry the approval_ prefix so a grep for
-- "workflow" against the live DB stops returning positives.
--
-- The final UPDATE rewrites any in-flight jobs rows whose kind was
-- "workflow:advance" or "workflow:timeout-sweep" so the dispatcher's
-- subscriber registry (which now registers "approval:*" kinds) finds
-- them. Safe on an empty jobs table; safe on a live queue because
-- WriteTx serializes it against pollOnce.

ALTER TABLE workflow_defs        RENAME TO approval_defs;
ALTER TABLE workflow_runs        RENAME TO approval_runs;
ALTER TABLE workflow_transitions RENAME TO approval_transitions;
ALTER TABLE workflow_tasks       RENAME TO approval_tasks;

DROP INDEX IF EXISTS idx_workflow_defs_active;
DROP INDEX IF EXISTS idx_workflow_runs_active;
DROP INDEX IF EXISTS idx_workflow_runs_doc;
DROP INDEX IF EXISTS idx_workflow_transitions_run;
DROP INDEX IF EXISTS idx_workflow_tasks_open;
DROP INDEX IF EXISTS idx_workflow_tasks_run;

CREATE INDEX idx_approval_defs_active        ON approval_defs(active, slug);
CREATE INDEX idx_approval_runs_active        ON approval_runs(state, deadline_at);
CREATE INDEX idx_approval_runs_doc           ON approval_runs(doc_id);
CREATE INDEX idx_approval_transitions_run    ON approval_transitions(run_id, occurred_at);
CREATE INDEX idx_approval_tasks_open         ON approval_tasks(status, assignee);
CREATE INDEX idx_approval_tasks_run          ON approval_tasks(run_id);

-- In-flight dispatcher rows. New code registers "approval:advance" and
-- "approval:timeout-sweep"; any pending row with the old kind would
-- dead-letter as "no subscriber". Rewriting is safe: the payload
-- shape hasn't changed.
UPDATE jobs SET kind = 'approval:advance'      WHERE kind = 'workflow:advance';
UPDATE jobs SET kind = 'approval:timeout-sweep' WHERE kind = 'workflow:timeout-sweep';
