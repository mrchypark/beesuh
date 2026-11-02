package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mrchypark/rhiza"
)

// Tombstones also cover admitted work that has not yet created a runs row.
var cancellationMigration = rhiza.Migration{Version: 13, Name: "run-cancellation", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE run_cancellations (workspace TEXT NOT NULL, request_id TEXT NOT NULL, recorded_at INTEGER NOT NULL, PRIMARY KEY(workspace,request_id))`},
}}

type runControl struct{ cancel context.CancelFunc }

func cancellationKey(workspace, request string) string { return workspace + "\x00" + request }

func cancelledResult(request string) Result {
	return Result{RequestID: request, State: "cancelled", CancellationRequested: true}
}

func (r *Runtime) cancellationRequested(ctx context.Context, workspace, request string) (bool, error) {
	rows, err := r.query(ctx, `SELECT 1 FROM run_cancellations WHERE workspace=? AND request_id=?`, workspace, request)
	return len(rows) != 0, err
}

func (r *Runtime) registerRun(workspace, request string, cancel context.CancelFunc) func() {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	if r.controls == nil {
		r.controls = make(map[string]map[*runControl]struct{})
	}
	key := cancellationKey(workspace, request)
	if r.controls[key] == nil {
		r.controls[key] = make(map[*runControl]struct{})
	}
	c := &runControl{cancel: cancel}
	r.controls[key][c] = struct{}{}
	return func() {
		r.cancelMu.Lock()
		defer r.cancelMu.Unlock()
		delete(r.controls[key], c)
		if len(r.controls[key]) == 0 {
			delete(r.controls, key)
		}
	}
}

// CancelRun durably requests cancellation before signalling live work. It does
// not undo effects or settle an uncertain execution; those retain their fences.
func (r *Runtime) CancelRun(ctx context.Context, workspace, request string) (Result, error) {
	if err := rejectResolverReentry(ctx); err != nil {
		return Result{}, err
	}
	if validIdentity(workspace, request) != nil {
		return Result{}, ErrInvalidInput
	}
	workspace, err := r.scopedWorkspace(ctx, workspace)
	if err != nil {
		return Result{}, err
	}
	// Close takes cancelMu before closing the DB. Avoid the lifecycle read lock:
	// a queued Recover/Close writer must not block cancellation of a live Run.
	if err := r.lockCancellation(ctx); err != nil {
		return Result{}, err
	}
	defer r.cancelMu.Unlock()
	if r.closed {
		return Result{}, ErrClosed
	}
	rows, err := r.query(ctx, `SELECT state,result FROM runs WHERE workspace=? AND request_id=?`, workspace, request)
	if err != nil {
		return Result{}, err
	}
	// A completed result always wins, including a lost completion acknowledgement.
	if len(rows) != 0 && rows[0][0] != "running" {
		var out Result
		err := json.Unmarshal([]byte(rows[0][1].(string)), &out)
		return out, err
	}
	requested, err := r.cancellationRequested(ctx, workspace, request)
	if err != nil {
		return Result{}, err
	}
	controls := r.controls[cancellationKey(workspace, request)]
	if len(rows) == 0 && len(controls) == 0 && !requested {
		return Result{}, ErrNotFound
	}
	if !requested {
		// Bound tombstones independently, including queued requests without runs.
		err = r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO run_cancellations(workspace,request_id,recorded_at)
SELECT ?,?,? WHERE (SELECT COUNT(*) FROM run_cancellations WHERE workspace=?) < ? AND (SELECT COUNT(*) FROM run_cancellations) < ?`,
			Args: []any{workspace, request, time.Now().Unix(), workspace, r.capacity.RunsPerWorkspace, r.capacity.RunsGlobal}, ExpectedRowsAffected: one()}})
		if err != nil {
			// A failed acknowledgement can follow a commit. If confirmation also
			// fails, fence and stop work rather than allow an ambiguous dispatch.
			committed, readErr := r.cancellationRequested(ctx, workspace, request)
			var rejected storageMutationError
			ambiguous := !errors.As(err, &rejected) && readErr != nil
			if (readErr == nil && committed) || ambiguous {
				for c := range controls {
					c.cancel()
				}
			}
			if ambiguous {
				r.fenceWorkspace(workspace)
				return Result{}, errors.Join(ErrUncertain, err, readErr)
			}
			if capacityRejected(err) {
				return Result{}, &CapacityError{Resource: "run cancellations", Limit: r.capacity.RunsPerWorkspace}
			}
			return Result{}, errors.Join(err, readErr)
		}
	}
	for c := range controls {
		c.cancel()
	}
	if len(rows) != 0 {
		return Result{RequestID: request, State: "running", CancellationRequested: true}, nil
	}
	return cancelledResult(request), nil
}

func (r *Runtime) lockCancellation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.cancelMu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if r.cancelMu.TryLock() {
				return nil
			}
		}
	}
}

// cancellationReplay is checked before admission as a tombstone may have no run.
func (r *Runtime) cancellationReplay(ctx context.Context, workspace, request string) (Result, bool, error) {
	requested, err := r.cancellationRequested(ctx, workspace, request)
	if err != nil || !requested {
		return Result{}, false, err
	}
	rows, err := r.query(ctx, `SELECT 1 FROM runs WHERE workspace=? AND request_id=?`, workspace, request)
	if err != nil {
		return Result{}, false, err
	}
	if len(rows) == 0 {
		return cancelledResult(request), true, nil
	}
	// Existing runs still pass through previous's original input-hash check.
	return Result{}, false, nil
}

func (r *Runtime) startCancellableRun(ctx context.Context, req Request, hash string) error {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	requested, err := r.cancellationRequested(ctx, req.WorkspaceID, req.RequestID)
	if err != nil {
		return err
	}
	if requested {
		return context.Canceled
	}
	return r.startRun(ctx, req, hash)
}

func (r *Runtime) finishResult(ctx context.Context, workspace, key, request string, out *Result) error {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	requested, err := r.cancellationRequested(ctx, workspace, request)
	if err != nil {
		return err
	}
	if requested {
		*out = Result{Provider: out.Provider, RequestID: request, SessionID: out.SessionID, State: "cancelled", CancellationRequested: true}
	}
	return r.finishStored(ctx, workspace, key, request, *out)
}

// A queued cancellation can interrupt any lock/slot wait before a run exists.
func (r *Runtime) cancelledAdmission(workspace, request string, out *Result, runErr *error) {
	if !errors.Is(*runErr, context.Canceled) || errors.Is(*runErr, ErrUncertain) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if cancelled, found, err := r.cancellationReplay(ctx, workspace, request); found && err == nil && cancelled.State == "cancelled" {
		*out, *runErr = cancelled, nil
	}
}
