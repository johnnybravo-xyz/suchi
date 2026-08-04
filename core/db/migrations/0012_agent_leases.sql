-- 0012_agent_leases.sql
--
-- Lease columns for the agent surface v1. External agents (LLM
-- runtimes, human-in-the-loop workflows, third-party classifiers)
-- claim a job atomically before doing work; the lease auto-expires if
-- the agent crashes mid-task, so no doc stays permanently pinned.
--
-- Only jobs whose `kind` starts with 'agent:' are claimable by
-- outside callers — internal kinds (post-ingest, post-classify,
-- render) stay off the agent surface.

ALTER TABLE jobs ADD COLUMN worker_id       TEXT;
ALTER TABLE jobs ADD COLUMN worker_deadline INTEGER;

-- Sweep index for finding expired leases (`state='running' AND
-- worker_deadline < now`). Partial so millions of finished jobs
-- don't bloat it.
CREATE INDEX jobs_lease_active
    ON jobs(worker_deadline)
    WHERE state = 'running' AND worker_deadline IS NOT NULL;
