package approvals_test

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// All fixture timestamps are identical. Only transactionally inserted job IDs
// distinguish a pending decision from one whose transition already committed.
func TestBeta2ApprovalRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, state, status string
		jobID                        int64
		rejectedID                   int64
		tasks                        int
	}{
		{name: "pending entry", fixture: `DELETE FROM approval_tasks; UPDATE jobs SET state='pending';`, jobID: 10, state: "first", status: "running", tasks: 1},
		{name: "running entry", fixture: `DELETE FROM approval_tasks; UPDATE jobs SET state='running';`, jobID: 10, state: "first", status: "running", tasks: 1},
		{name: "existing open task", jobID: 10, state: "first", status: "running", tasks: 1},
		{name: "pending decision", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='approve',resolved_by='user:1',resolved_at=1;
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES(11,'approval:advance','{"run_id":5,"trigger":"approve"}','pending',1,1,1);`, jobID: 11, state: "second", status: "running", tasks: 1},
		{name: "running decision retry", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='approve',resolved_by='user:1',resolved_at=1;
   INSERT INTO jobs(id,kind,payload,state,attempts,next_run_at,created_at,updated_at) VALUES(11,'approval:advance','{"run_id":5,"trigger":"approve"}','running',2,1,1,1);`, jobID: 11, state: "second", status: "running", tasks: 1},
		{name: "crash after transition", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='approve',resolved_by='user:1',resolved_at=1;
   INSERT INTO approval_transitions(id,run_id,from_state,to_state,trigger,payload_json,occurred_at) VALUES(7,5,'first','second','approve','{}',1);
   UPDATE approval_runs SET current_state='second';
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES
    (11,'approval:advance','{"run_id":5,"trigger":"approve"}','running',1,1,1),
    (12,'approval:advance','{"run_id":5,"trigger":""}','pending',1,1,1);`, jobID: 12, rejectedID: 11, state: "second", status: "running", tasks: 1},
		{name: "repeat state visit", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='again',resolved_by='user:1',resolved_at=1;
   INSERT INTO approval_transitions(id,run_id,from_state,to_state,trigger,payload_json,occurred_at) VALUES(7,5,'first','first','again','{}',1);
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES
    (11,'approval:advance','{"run_id":5,"trigger":"again"}','running',1,1,1),
    (12,'approval:advance','{"run_id":5,"trigger":""}','pending',1,1,1);`, jobID: 12, rejectedID: 11, state: "first", status: "running", tasks: 1},
		{name: "timeout before task", fixture: `DELETE FROM approval_tasks; UPDATE jobs SET state='pending';
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES(11,'approval:advance','{"run_id":5,"trigger":"timeout"}','pending',1,1,1);`, jobID: 11, rejectedID: 10, state: "expired", status: "done"},
		{name: "decision before competing timeout", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='approve',resolved_by='user:1',resolved_at=1;
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES
    (11,'approval:advance','{"run_id":5,"trigger":"approve"}','pending',1,1,1),
    (12,'approval:advance','{"run_id":5,"trigger":"timeout"}','pending',1,1,1);`, jobID: 11, rejectedID: 12, state: "second", status: "running", tasks: 1},
		{name: "missing entry evidence", fixture: `DELETE FROM jobs; UPDATE approval_tasks SET status='resolved';
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES(11,'approval:advance','{"run_id":5,"trigger":"approve"}','pending',1,1,1);`, rejectedID: 11, state: "first", status: "running"},
		{name: "prior timeout after new entry", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='approve',resolved_by='user:1',resolved_at=1;
   INSERT INTO approval_transitions(id,run_id,from_state,to_state,trigger,payload_json,occurred_at) VALUES(7,5,'first','second','approve','{}',1);
   UPDATE approval_runs SET current_state='second',deadline_at=NULL;
   INSERT INTO approval_tasks(id,run_id,state_key,assignee,prompt,choices_json,status,created_at) VALUES(10,5,'second','user:1','Second review','["approve"]','open',1);
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES
    (11,'approval:advance','{"run_id":5,"trigger":"approve"}','done',1,1,1),
    (12,'approval:advance','{"run_id":5,"trigger":""}','done',1,1,1),
    (13,'approval:advance','{"run_id":5,"trigger":"timeout"}','pending',1,1,1);`, rejectedID: 13, state: "second", status: "running", tasks: 1},
		{name: "human decision after ambiguous timeout", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='approve',resolved_by='user:1',resolved_at=1;
   INSERT INTO approval_transitions(id,run_id,from_state,to_state,trigger,payload_json,occurred_at) VALUES(7,5,'first','second','approve','{}',1);
   UPDATE approval_runs SET current_state='second',deadline_at=NULL;
   INSERT INTO approval_tasks(id,run_id,state_key,assignee,prompt,choices_json,status,resolved_choice,resolved_by,resolved_at,created_at)
    VALUES(10,5,'second','user:1','Second review','["approve"]','resolved','approve','user:1',1,1);
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES
    (11,'approval:advance','{"run_id":5,"trigger":"approve"}','done',1,1,1),
    (12,'approval:advance','{"run_id":5,"trigger":""}','done',1,1,1),
    (13,'approval:advance','{"run_id":5,"trigger":"timeout"}','pending',1,1,1),
    (14,'approval:advance','{"run_id":5,"trigger":"approve"}','pending',1,1,1);`, jobID: 14, rejectedID: 13, state: "done", status: "done"},
		{name: "late task from prior state", fixture: `INSERT INTO approval_transitions(id,run_id,from_state,to_state,trigger,payload_json,occurred_at) VALUES(7,5,'first','second','approve','{}',1);
   UPDATE approval_runs SET current_state='second';
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES
    (11,'approval:advance','{"run_id":5,"trigger":"approve"}','done',1,1,1),
    (12,'approval:advance','{"run_id":5,"trigger":""}','pending',1,1,1);`, jobID: 12, state: "second", status: "running", tasks: 1},
		{name: "latest entry deleted", fixture: `UPDATE approval_tasks SET status='resolved',resolved_choice='approve',resolved_by='user:1',resolved_at=1;
   INSERT INTO approval_transitions(id,run_id,from_state,to_state,trigger,payload_json,occurred_at) VALUES(7,5,'first','second','approve','{}',1);
   UPDATE approval_runs SET current_state='second';
   INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES(11,'approval:advance','{"run_id":5,"trigger":"approve"}','running',1,1,1);`, rejectedID: 11, state: "second", status: "running"},
		{name: "terminal run", fixture: `UPDATE approval_runs SET state='done',current_state='done'; UPDATE approval_tasks SET status='expired'; UPDATE jobs SET state='running';`, rejectedID: 10, state: "done", status: "done"},
		{name: "missing run", fixture: `DELETE FROM approval_runs; UPDATE jobs SET state='running';`, rejectedID: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "upgrade.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			migs, err := db.LoadMigrations(migrations.FS, ".")
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := db.Migrate(t.Context(), d, migs[:2], log); err != nil {
				t.Fatal(err)
			}
			spec := approvals.Spec{Start: "first", States: map[string]approvals.State{
				"first":  {Kind: "approve", Assignee: "user:1", Choices: []string{"approve", "again"}, TimeoutSec: 60, On: map[string]string{"approve": "second", "again": "first", "timeout": "expired"}},
				"second": {Kind: "approve", Assignee: "user:1", Choices: []string{"approve"}, On: map[string]string{"approve": "done"}},
				"done":   {Kind: "end"}, "expired": {Kind: "end"},
			}}
			raw, err := approvals.EncodeSpec(spec)
			if err != nil {
				t.Fatal(err)
			}
			err = d.WriteTx(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO approval_defs(id,slug,version,spec_json,active,created_at) VALUES(1,'two-reviews',1,?,1,1);
     INSERT INTO approval_runs(id,def_id,state,current_state,vars_json,state_entered_at,deadline_at,started_at) VALUES(5,1,'running','first','{}',1,1,1);
     INSERT INTO approval_tasks(id,run_id,state_key,assignee,prompt,choices_json,status,created_at) VALUES(9,5,'first','user:1','First review','["approve","again"]','open',1);
     INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES(10,'approval:advance','{"run_id":5,"trigger":""}','done',1,1,1);`, raw); err != nil {
					return err
				}
				if tc.fixture != "" {
					if _, err := tx.ExecContext(t.Context(), tc.fixture); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(t.Context(), `INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at) VALUES
     (100,'approval:advance','invalid JSON','pending',1,1,1),
     (101,'approval:advance','{"run_id":"5","trigger":""}','pending',1,1,1);`)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Migrate(t.Context(), d, migs, log); err != nil {
				t.Fatal(err)
			}
			e := approvals.New(d, log)
			sub := approvals.NewSubscriber(e)
			readEvent := func(id int64) (pluginapi.Event, string, string) {
				t.Helper()
				var payload, state, lastError string
				var systemID sql.NullInt64
				if err := d.Read.QueryRowContext(t.Context(), `SELECT payload,system_id,state,COALESCE(last_error,'') FROM jobs WHERE id=?`, id).Scan(&payload, &systemID, &state, &lastError); err != nil {
					t.Fatal(err)
				}
				return pluginapi.Event{Kind: approvals.KindAdvance, SystemID: systemID.Int64, Payload: map[string]any{"raw": payload}}, state, lastError
			}
			for _, id := range []int64{tc.rejectedID, 100, 101} {
				if id == 0 {
					continue
				}
				event, state, lastError := readEvent(id)
				if state != "dead" || lastError == "" {
					t.Fatalf("unbound job %d: state=%s error=%q", id, state, lastError)
				}
				if err := sub.Handle(t.Context(), event); err == nil {
					t.Fatalf("unbound job %d became authorized by retry", id)
				}
			}
			if tc.jobID != 0 {
				event, _, _ := readEvent(tc.jobID)
				if err := sub.Handle(t.Context(), event); err != nil {
					t.Fatal(err)
				}
				// A preserved human choice must enter the next review and create its task.
				if tc.state == "second" && tc.jobID == 11 {
					next := queuedApprovalEvent(t, e, 5)
					if err := sub.Handle(t.Context(), next); err != nil {
						t.Fatal(err)
					}
				}
			}
			run, tasks, err := e.GetRun(t.Context(), 5)
			if tc.status == "" {
				if err != approvals.ErrNoRun {
					t.Fatalf("missing run: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != tc.status || run.CurrentState != tc.state || len(tasks) != tc.tasks {
				t.Fatalf("recovered status=%s state=%s tasks=%d; want %s %s %d", run.Status, run.CurrentState, len(tasks), tc.status, tc.state, tc.tasks)
			}
			var broken int
			if err := d.Read.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&broken); err != nil || broken != 0 {
				t.Fatalf("foreign keys: count=%d error=%v", broken, err)
			}
		})
	}
}
