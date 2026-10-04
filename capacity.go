package beesuh

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mrchypark/rhiza"
)

const journalRecordsPerRun = 256

// CapacityLimits bounds retained durable records. Zero selects the finite
// defaults. Negative values are rejected by Open.
type CapacityLimits struct {
	RunsPerWorkspace, RunsGlobal                     int
	SessionsPerWorkspace, SessionsGlobal             int
	PendingCreatesPerWorkspace, PendingCreatesGlobal int
	ConnectionsPerWorkspace, ConnectionsGlobal       int
}

func (c CapacityLimits) withDefaults() CapacityLimits {
	if c.RunsPerWorkspace == 0 {
		c.RunsPerWorkspace = 10000
	}
	if c.RunsGlobal == 0 {
		c.RunsGlobal = 100000
	}
	if c.SessionsPerWorkspace == 0 {
		c.SessionsPerWorkspace = 1000
	}
	if c.SessionsGlobal == 0 {
		c.SessionsGlobal = 10000
	}
	if c.PendingCreatesPerWorkspace == 0 {
		c.PendingCreatesPerWorkspace = 1000
	}
	if c.PendingCreatesGlobal == 0 {
		c.PendingCreatesGlobal = 10000
	}
	if c.ConnectionsPerWorkspace == 0 {
		c.ConnectionsPerWorkspace = 1000
	}
	if c.ConnectionsGlobal == 0 {
		c.ConnectionsGlobal = 10000
	}
	return c
}

func (c CapacityLimits) valid() bool {
	return c.RunsPerWorkspace >= 0 && c.RunsGlobal >= 0 && c.SessionsPerWorkspace >= 0 && c.SessionsGlobal >= 0 && c.PendingCreatesPerWorkspace >= 0 && c.PendingCreatesGlobal >= 0 && c.ConnectionsPerWorkspace >= 0 && c.ConnectionsGlobal >= 0
}

// CapacityError reports a retained resource that reached its configured limit.
type CapacityError struct {
	Resource string
	Limit    int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("beesuh: %s capacity reached (limit %d)", e.Resource, e.Limit)
}

func (e *CapacityError) Unwrap() error { return ErrCapacity }

func (r *Runtime) CapacityLimits() CapacityLimits { return r.capacity }

var capacityMigration = rhiza.Migration{Version: 12, Name: "capacity-run-reservations", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE capacity_run_reservations (workspace TEXT NOT NULL, request_id TEXT NOT NULL, PRIMARY KEY(workspace,request_id))`},
	{SQL: `CREATE INDEX journal_request ON journal(workspace,request_id)`},
}}

func capacityRejected(err error) bool {
	var rejected storageMutationError
	return errors.As(err, &rejected) && rejected.code == "precondition_failed"
}

func (r *Runtime) reserveRun(ctx context.Context, req Request) error {
	err := r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO capacity_run_reservations(workspace,request_id)
SELECT ?,?
WHERE (SELECT COUNT(*) FROM runs WHERE workspace=?) + (SELECT COUNT(*) FROM capacity_run_reservations WHERE workspace=?) < ?
  AND (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM capacity_run_reservations) < ?`, Args: []any{req.WorkspaceID, req.RequestID, req.WorkspaceID, req.WorkspaceID, r.capacity.RunsPerWorkspace, r.capacity.RunsGlobal}, ExpectedRowsAffected: one()}})
	if capacityRejected(err) {
		return r.capacityError(ctx, "runs", req.WorkspaceID, r.capacity.RunsPerWorkspace, r.capacity.RunsGlobal, "runs", "capacity_run_reservations")
	}
	return err
}

func (r *Runtime) releaseRunReservation(req Request) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `DELETE FROM capacity_run_reservations WHERE workspace=? AND request_id=?`, Args: []any{req.WorkspaceID, req.RequestID}}})
}

func (r *Runtime) startRun(ctx context.Context, req Request, hash string) error {
	err := r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, hash}},
		{SQL: `UPDATE sessions SET active_request=? WHERE workspace=? AND session_key=? AND active_request=''`, Args: []any{req.RequestID, req.WorkspaceID, req.SessionKey}, ExpectedRowsAffected: one()},
		{SQL: `DELETE FROM capacity_run_reservations WHERE workspace=? AND request_id=?`, Args: []any{req.WorkspaceID, req.RequestID}, ExpectedRowsAffected: one()},
	})
	return err
}

func (r *Runtime) insertSession(ctx context.Context, req Request, id string, pending bool) error {
	if pending {
		// The intent already reserved this mapping slot before any remote create.
		// Consume it with the mapping. A later cleanup failure must not leave both
		// records claiming one session slot, and a failed mapping keeps the intent.
		return r.mutate(ctx, []rhiza.SQLStatement{
			{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES(?,?,?,?,?,?)`, Args: []any{req.WorkspaceID, req.SessionKey, r.opts.BackendID, r.opts.OpenCode.Directory, r.profile, id}, ExpectedRowsAffected: one()},
			{SQL: `DELETE FROM pending_session_creates WHERE workspace=? AND session_key=? AND phase=?`, Args: []any{req.WorkspaceID, req.SessionKey, pendingPhaseCreating}, ExpectedRowsAffected: one()},
		})
	}
	sql := `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id)
SELECT ?,?,?,?,?,?
WHERE (SELECT COUNT(*) FROM sessions WHERE workspace=?) + (SELECT COUNT(*) FROM pending_session_creates WHERE workspace=?) < ?
  AND (SELECT COUNT(*) FROM sessions) + (SELECT COUNT(*) FROM pending_session_creates) < ?`
	args := []any{req.WorkspaceID, req.SessionKey, r.opts.BackendID, r.opts.OpenCode.Directory, r.profile, id, req.WorkspaceID, req.WorkspaceID, r.capacity.SessionsPerWorkspace, r.capacity.SessionsGlobal}
	err := r.mutate(ctx, []rhiza.SQLStatement{{SQL: sql, Args: args, ExpectedRowsAffected: one()}})
	if capacityRejected(err) {
		return r.capacityError(ctx, "sessions", req.WorkspaceID, r.capacity.SessionsPerWorkspace, r.capacity.SessionsGlobal, "sessions", "pending_session_creates")
	}
	return err
}

func (r *Runtime) appendJournalBounded(ctx context.Context, req Request, kind, payload string) error {
	err := r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO journal(workspace,session_key,request_id,kind,payload)
SELECT ?,?,?,?,? WHERE (SELECT COUNT(*) FROM journal WHERE workspace=? AND request_id=?) < ?`, Args: []any{req.WorkspaceID, req.SessionKey, req.RequestID, kind, payload, req.WorkspaceID, req.RequestID, journalRecordsPerRun}, ExpectedRowsAffected: one()}})
	if capacityRejected(err) {
		return &CapacityError{Resource: "journal records", Limit: journalRecordsPerRun}
	}
	return err
}

// journalSpace is checked before a provider or effect call. A run has one
// execution owner, and each actual insert still has its atomic limit guard.
func (r *Runtime) journalSpace(ctx context.Context, req Request, need int) error {
	rows, err := r.query(ctx, `SELECT COUNT(*) FROM journal WHERE workspace=? AND request_id=?`, req.WorkspaceID, req.RequestID)
	if err != nil {
		return err
	}
	if len(rows) != 1 || rows[0][0].(int64)+int64(need) > journalRecordsPerRun {
		return &CapacityError{Resource: "journal records", Limit: journalRecordsPerRun}
	}
	return nil
}

func (r *Runtime) reservePendingCreate(ctx context.Context, req Request, token string) error {
	err := r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,phase,remote_id,recorded_at)
SELECT ?,?,?,?,?,?,?,'',?
WHERE (SELECT COUNT(*) FROM pending_session_creates WHERE workspace=?) < ?
  AND (SELECT COUNT(*) FROM pending_session_creates) < ?
  AND (SELECT COUNT(*) FROM sessions WHERE workspace=?) + (SELECT COUNT(*) FROM pending_session_creates WHERE workspace=?) < ?
  AND (SELECT COUNT(*) FROM sessions) + (SELECT COUNT(*) FROM pending_session_creates) < ?
ON CONFLICT(workspace,session_key) DO NOTHING`, Args: []any{req.WorkspaceID, req.SessionKey, token, r.opts.BackendID, r.opts.OpenCode.Directory, r.profile, pendingPhaseCreating, time.Now().Unix(), req.WorkspaceID, r.capacity.PendingCreatesPerWorkspace, r.capacity.PendingCreatesGlobal, req.WorkspaceID, req.WorkspaceID, r.capacity.SessionsPerWorkspace, r.capacity.SessionsGlobal}, ExpectedRowsAffected: one()}})
	if capacityRejected(err) {
		// DO NOTHING can also lose to another writer for this same intent.
		// That row already owns capacity; pendingCreateToken will re-read and
		// use its durable token rather than the caller's discarded token.
		existing, queryErr := r.query(ctx, `SELECT 1 FROM pending_session_creates WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
		if queryErr != nil {
			return errors.Join(err, queryErr)
		}
		if len(existing) != 0 {
			return nil
		}
		rows, queryErr := r.query(ctx, `SELECT
 (SELECT COUNT(*) FROM pending_session_creates WHERE workspace=?),
 (SELECT COUNT(*) FROM pending_session_creates)`, req.WorkspaceID)
		if queryErr != nil {
			return errors.Join(err, queryErr)
		}
		if len(rows) == 1 && rows[0][0].(int64) < int64(r.capacity.PendingCreatesPerWorkspace) && rows[0][1].(int64) < int64(r.capacity.PendingCreatesGlobal) {
			return r.capacityError(ctx, "sessions", req.WorkspaceID, r.capacity.SessionsPerWorkspace, r.capacity.SessionsGlobal, "sessions", "pending_session_creates")
		}
		return r.capacityError(ctx, "pending session creates", req.WorkspaceID, r.capacity.PendingCreatesPerWorkspace, r.capacity.PendingCreatesGlobal, "pending_session_creates", "")
	}
	return err
}

func (r *Runtime) capacityError(ctx context.Context, resource, workspace string, perWorkspace, global int, primary, extra string) error {
	globalSQL := `SELECT COUNT(*) FROM ` + primary
	if extra != "" {
		globalSQL += ` UNION ALL SELECT COUNT(*) FROM ` + extra
	}
	rows, err := r.query(ctx, globalSQL)
	if err == nil {
		var total int64
		for _, row := range rows {
			total += row[0].(int64)
		}
		if total >= int64(global) {
			return &CapacityError{Resource: resource + " global", Limit: global}
		}
	}
	return &CapacityError{Resource: resource + " per workspace", Limit: perWorkspace}
}
