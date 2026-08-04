-- Workflow engine tables. State-machine core: one row per definition
-- (versioned by slug), one row per running instance (single current_state
-- for the skeleton — AND-splits are v2), one row per transition (replay
-- log for audit + debugging), one row per human-in-the-loop task. Task
-- rows are surfaced through /api/tasks/ so mobile clients pick them up
-- without a new poller. Every transition rides through the durable outbox
-- (jobs table) via kind='workflow:advance' — restart-safety, retries, and
-- run_after-based timeout scheduling come for free.

CREATE TABLE workflow_defs (
  id             INTEGER PRIMARY KEY,
  slug           TEXT NOT NULL,
  version        INTEGER NOT NULL,
  spec_json      TEXT NOT NULL,             -- {states:[{key,kind,assignee,timeout_sec,on:{event:next}}], start}
  active         INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  created_by     INTEGER NOT NULL REFERENCES users(id),
  UNIQUE(slug, version)
) STRICT;
CREATE INDEX idx_workflow_defs_active ON workflow_defs(active, slug);

CREATE TABLE workflow_runs (
  id               INTEGER PRIMARY KEY,
  def_id           INTEGER NOT NULL REFERENCES workflow_defs(id),
  doc_id           INTEGER REFERENCES documents(id),
  state            TEXT NOT NULL,           -- running|done|failed|cancelled
  current_state    TEXT NOT NULL,           -- key into spec_json.states
  vars_json        TEXT NOT NULL DEFAULT '{}',
  state_entered_at INTEGER NOT NULL,
  deadline_at      INTEGER,                 -- unix epoch; NULL = no timeout
  started_by       INTEGER REFERENCES users(id),
  started_at       INTEGER NOT NULL,
  ended_at         INTEGER
) STRICT;
CREATE INDEX idx_workflow_runs_active ON workflow_runs(state, deadline_at);
CREATE INDEX idx_workflow_runs_doc    ON workflow_runs(doc_id);

CREATE TABLE workflow_transitions (
  id            INTEGER PRIMARY KEY,
  run_id        INTEGER NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
  from_state    TEXT NOT NULL,
  to_state      TEXT NOT NULL,
  trigger       TEXT NOT NULL,              -- system|timeout|approve|reject|<custom>
  actor         TEXT,                       -- "user:5" | "system" | NULL
  payload_json  TEXT NOT NULL DEFAULT '{}',
  occurred_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_workflow_transitions_run ON workflow_transitions(run_id, occurred_at);

CREATE TABLE workflow_tasks (
  id              INTEGER PRIMARY KEY,
  run_id          INTEGER NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
  state_key       TEXT NOT NULL,            -- state that spawned this task
  assignee        TEXT NOT NULL,            -- "user:5" | "role:finance"
  prompt          TEXT NOT NULL,
  choices_json    TEXT NOT NULL,            -- ["approve","reject"]
  status          TEXT NOT NULL,            -- open|claimed|resolved|expired
  deadline_at     INTEGER,
  resolved_choice TEXT,
  resolved_by     TEXT,
  resolved_at     INTEGER,
  created_at      INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_workflow_tasks_open ON workflow_tasks(status, assignee);
CREATE INDEX idx_workflow_tasks_run  ON workflow_tasks(run_id);
