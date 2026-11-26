package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/rhiza"
)

const maxAgentTaskBytes = 64 << 10

var agentTasksMigration = rhiza.Migration{Version: 15, Name: "agent-tasks", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE agent_tasks (workspace TEXT NOT NULL, request_id TEXT NOT NULL, principal TEXT NOT NULL, request TEXT NOT NULL CHECK(length(request) <= 65536), input_hash TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('queued','running','blocked')), failure_category TEXT NOT NULL DEFAULT '' CHECK(length(failure_category) <= 64), enqueued_at INTEGER NOT NULL, PRIMARY KEY(workspace,request_id))`},
	{SQL: `CREATE INDEX agent_tasks_queue ON agent_tasks(state,enqueued_at,workspace,request_id)`},
}}

type agentTaskPayload struct {
	WorkspaceID           string          `json:"workspace_id"`
	SessionKey            string          `json:"session_key"`
	RequestID             string          `json:"request_id"`
	Provider              string          `json:"provider,omitempty"`
	ConnectionID          string          `json:"connection_id,omitempty"`
	ConductorConnectionID string          `json:"conductor_connection_id,omitempty"`
	Prompt                agentTaskPrompt `json:"prompt"`
	Timeout               time.Duration   `json:"timeout_ns,omitempty"`
}

type agentTaskPrompt struct {
	System string          `json:"system,omitempty"`
	Text   string          `json:"text"`
	Model  *agentTaskModel `json:"model,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type agentTaskModel struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

type agentTask struct {
	workspace, requestID, principal, request, hash, state, failureCategory string
	enqueuedAt                                                             int64
}

type agentTaskWorkerKey struct{}

func isAgentTaskWorker(ctx context.Context) bool { return ctx.Value(agentTaskWorkerKey{}) != nil }

// Enqueue durably accepts a request without resolving its credential or
// dispatching it. Run remains the sole execution path.
func (r *Runtime) Enqueue(ctx context.Context, req Request) (Result, error) {
	if err := rejectResolverReentry(ctx); err != nil {
		return Result{}, err
	}
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return Result{}, ErrClosed
	}

	var principal Principal
	var err error
	if r.opts.RequireIdentity {
		principal, err = r.requirePrincipal(ctx)
		if err != nil {
			return Result{}, err
		}
	}

	prepared, _, hash, err := r.prepareRun(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if r.runFenceHolds(prepared.WorkspaceID) {
		return Result{}, ErrUncertain
	}
	payload, principalJSON, err := encodeAgentTask(principal, req)
	if err != nil {
		return Result{}, err
	}
	if out, found, err := r.enqueuePrevious(ctx, prepared, hash); found || err != nil {
		return out, err
	}
	requested, err := r.cancellationRequested(ctx, prepared.WorkspaceID, prepared.RequestID)
	if err != nil {
		return Result{}, err
	}
	if requested {
		return cancelledResult(prepared.RequestID), nil
	}
	if r.opts.RequireIdentity {
		if r.opts.PrepareAsync == nil {
			return Result{}, auth.ErrUnavailable
		}
		_, cleanup, err := r.prepareAsyncContext(WithPrincipal(context.Background(), principal), principal)
		cleanup()
		if err != nil {
			return Result{}, auth.ErrUnavailable
		}
	}

	unlock, err := r.lockSession(ctx, "request\x00"+prepared.WorkspaceID+"\x00"+prepared.RequestID)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	if err := r.lockCancellation(ctx); err != nil {
		return Result{}, err
	}
	defer r.cancelMu.Unlock()
	if out, found, err := r.enqueuePrevious(ctx, prepared, hash); found || err != nil {
		return out, err
	}
	requested, err = r.cancellationRequested(ctx, prepared.WorkspaceID, prepared.RequestID)
	if err != nil {
		return Result{}, err
	}
	if requested {
		return cancelledResult(prepared.RequestID), nil
	}

	err = r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO agent_tasks(workspace,request_id,principal,request,input_hash,state,enqueued_at)
SELECT ?,?,?,?,?,'queued',?
WHERE (SELECT COUNT(*) FROM runs WHERE workspace=?) + (SELECT COUNT(*) FROM capacity_run_reservations WHERE workspace=?) + (SELECT COUNT(*) FROM agent_tasks WHERE workspace=?) < ?
  AND (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM capacity_run_reservations) + (SELECT COUNT(*) FROM agent_tasks) < ?`, Args: []any{
		prepared.WorkspaceID, prepared.RequestID, principalJSON, payload, hash, time.Now().UnixNano(),
		prepared.WorkspaceID, prepared.WorkspaceID, prepared.WorkspaceID, r.capacity.RunsPerWorkspace,
		r.capacity.RunsGlobal,
	}, ExpectedRowsAffected: one()}})
	if err != nil {
		if out, found, readErr := r.enqueuePrevious(ctx, prepared, hash); found || readErr != nil {
			return out, readErr
		}
		if capacityRejected(err) {
			return Result{}, r.agentTaskCapacityError(ctx, prepared.WorkspaceID)
		}
		return Result{}, err
	}
	r.wakeAgentWorker()
	return Result{RequestID: prepared.RequestID, State: "queued"}, nil
}

func (r *Runtime) agentTaskCapacityError(ctx context.Context, workspace string) error {
	rows, err := r.query(ctx, "SELECT (SELECT COUNT(*) FROM runs WHERE workspace=?) + (SELECT COUNT(*) FROM capacity_run_reservations WHERE workspace=?) + (SELECT COUNT(*) FROM agent_tasks WHERE workspace=?), (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM capacity_run_reservations) + (SELECT COUNT(*) FROM agent_tasks)", workspace, workspace, workspace)
	if err == nil && len(rows) == 1 {
		perWorkspace, _ := rows[0][0].(int64)
		global, _ := rows[0][1].(int64)
		if global >= int64(r.capacity.RunsGlobal) {
			return &CapacityError{Resource: "agent tasks global", Limit: r.capacity.RunsGlobal}
		}
		if perWorkspace >= int64(r.capacity.RunsPerWorkspace) {
			return &CapacityError{Resource: "agent tasks", Limit: r.capacity.RunsPerWorkspace}
		}
	}
	return &CapacityError{Resource: "agent tasks", Limit: r.capacity.RunsPerWorkspace}
}

func (r *Runtime) enqueuePrevious(ctx context.Context, req Request, hash string) (Result, bool, error) {
	rows, err := r.query(ctx, `SELECT input_hash,state,result FROM runs WHERE workspace=? AND request_id=?`, req.WorkspaceID, req.RequestID)
	if err != nil {
		return Result{}, false, err
	}
	if len(rows) != 0 {
		if rows[0][0] != hash {
			return Result{}, true, ErrConflict
		}
		if rows[0][1] == "running" {
			return Result{RequestID: req.RequestID, State: "running"}, true, nil
		}
		var out Result
		if err := json.Unmarshal([]byte(rows[0][2].(string)), &out); err != nil {
			return Result{}, true, err
		}
		if out.State != "succeeded" && out.State != "cancelled" {
			return out, true, replayFailure(out.FailureCategory, out.Error)
		}
		return out, true, nil
	}
	return r.previousAgentTask(ctx, req, hash)
}

func (r *Runtime) previousAgentTask(ctx context.Context, req Request, hash string) (Result, bool, error) {
	task, found, err := r.agentTask(ctx, req.WorkspaceID, req.RequestID)
	if err != nil || !found {
		return Result{}, found, err
	}
	if task.hash != hash {
		return Result{}, true, ErrConflict
	}
	switch task.state {
	case "queued", "running":
		return Result{RequestID: req.RequestID, State: task.state}, true, nil
	case "blocked":
		message := PublicFailureMessage(task.failureCategory)
		return Result{RequestID: req.RequestID, State: "blocked", FailureCategory: task.failureCategory, Error: message}, true, replayFailure(task.failureCategory, message)
	default:
		return Result{}, true, ErrInvalidInput
	}
}

func encodeAgentTask(principal Principal, req Request) (string, string, error) {
	p := agentTaskPayload{
		WorkspaceID: req.WorkspaceID, SessionKey: req.SessionKey, RequestID: req.RequestID,
		Provider: req.Provider, ConnectionID: req.ConnectionID, ConductorConnectionID: req.ConductorConnectionID,
		Prompt:  agentTaskPrompt{System: req.Prompt.System, Text: req.Prompt.Text, Schema: req.Prompt.Schema},
		Timeout: req.Timeout,
	}
	if req.Prompt.Model != nil {
		p.Prompt.Model = &agentTaskModel{ProviderID: req.Prompt.Model.ProviderID, ModelID: req.Prompt.Model.ModelID}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", "", errors.Join(ErrInvalidInput, err)
	}
	pb, err := json.Marshal(principal)
	if err != nil {
		return "", "", errors.Join(ErrInvalidInput, err)
	}
	if len(b)+len(pb) > maxAgentTaskBytes {
		return "", "", errors.Join(ErrInvalidInput, errors.New("beesuh: asynchronous task payload exceeds 64 KiB"))
	}
	return string(b), string(pb), nil
}

func decodeAgentTask(raw string) (Request, error) {
	var p agentTaskPayload
	if err := decodeAgentJSON(raw, &p); err != nil {
		return Request{}, err
	}
	req := Request{
		WorkspaceID: p.WorkspaceID, SessionKey: p.SessionKey, RequestID: p.RequestID,
		Provider: p.Provider, ConnectionID: p.ConnectionID, ConductorConnectionID: p.ConductorConnectionID,
		Prompt: Prompt{System: p.Prompt.System, Text: p.Prompt.Text, Schema: p.Prompt.Schema}, Timeout: p.Timeout,
	}
	if p.Prompt.Model != nil {
		req.Prompt.Model = &Model{ProviderID: p.Prompt.Model.ProviderID, ModelID: p.Prompt.Model.ModelID}
	}
	return req, nil
}

func decodeAgentJSON(raw string, out any) error {
	if len(raw) > maxAgentTaskBytes {
		return errors.New("beesuh: asynchronous task payload exceeds 64 KiB")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("beesuh: trailing asynchronous task data")
		}
		return err
	}
	return nil
}

func decodeAgentPrincipal(raw string) (Principal, error) {
	var p Principal
	if len(raw) > 4096 {
		return p, ErrUnauthenticated
	}
	if err := decodeAgentJSON(raw, &p); err != nil || !validOrganizationID(p.OrganizationID) || validIdentity(p.UserID, p.WorkspaceID) != nil || strings.ContainsAny(p.UserID+p.WorkspaceID, "\r\n") || !utf8.ValidString(p.UserID+p.WorkspaceID) {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

func (r *Runtime) prepareAsyncContext(ctx context.Context, p Principal) (context.Context, func(), error) {
	if !r.opts.RequireIdentity {
		return ctx, func() {}, nil
	}
	if r.opts.PrepareAsync == nil {
		return nil, func() {}, auth.ErrUnavailable
	}
	hookCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	prepared, err := r.opts.PrepareAsync(WithPrincipal(hookCtx, p), p)
	if err != nil || prepared == nil {
		cancel()
		return nil, func() {}, auth.ErrUnavailable
	}
	return WithPrincipal(prepared, p), cancel, nil
}

func (r *Runtime) wakeAgentWorker() {
	select {
	case r.agentWake <- struct{}{}:
	default:
	}
}

// StartAgentWorker reconciles durable intent and starts one FIFO worker. The
// worker can idle without PrepareAsync; Enqueue remains closed in that case.
func (r *Runtime) StartAgentWorker(ctx context.Context) error {
	for {
		r.workerMu.Lock()
		if r.workerClosing {
			r.workerMu.Unlock()
			return ErrClosed
		}
		r.life.RLock()
		if r.closed {
			r.life.RUnlock()
			r.workerMu.Unlock()
			return ErrClosed
		}
		if r.workerDone == nil {
			if ctx.Err() == nil {
				if err := r.reconcileAgentTasks(ctx); err != nil {
					r.life.RUnlock()
					r.workerMu.Unlock()
					return err
				}
			}
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			r.workerCtx, r.workerCancel, r.workerDone = workerCtx, cancel, done
			go func() {
				r.agentLoop(workerCtx)
				r.workerMu.Lock()
				if !r.workerClosing && r.workerDone == done {
					r.workerCtx, r.workerCancel, r.workerDone = nil, nil, nil
				}
				r.workerMu.Unlock()
				close(done)
			}()
			r.life.RUnlock()
			r.workerMu.Unlock()
			return nil
		}
		if r.workerCtx != nil && r.workerCtx.Err() == nil {
			r.life.RUnlock()
			r.workerMu.Unlock()
			return nil
		}
		done := r.workerDone
		r.life.RUnlock()
		r.workerMu.Unlock()
		<-done
	}
}

func (r *Runtime) stopAgentWorker() {
	r.workerMu.Lock()
	r.workerClosing = true
	cancel, done := r.workerCancel, r.workerDone
	r.workerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (r *Runtime) reconcileAgentTasks(ctx context.Context) error {
	return r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `DELETE FROM agent_tasks WHERE EXISTS (SELECT 1 FROM runs WHERE runs.workspace=agent_tasks.workspace AND runs.request_id=agent_tasks.request_id)`},
		{SQL: `DELETE FROM agent_tasks WHERE EXISTS (SELECT 1 FROM run_cancellations WHERE run_cancellations.workspace=agent_tasks.workspace AND run_cancellations.request_id=agent_tasks.request_id)`},
		{SQL: `UPDATE agent_tasks SET state='queued' WHERE state='running' AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.workspace=agent_tasks.workspace AND runs.request_id=agent_tasks.request_id)`},
	})
}

func (r *Runtime) agentLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil {
			task, found, err := r.claimAgentTask(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("beesuh: claiming agent task failed", "error", err)
				}
				break
			}
			if !found {
				break
			}
			r.processAgentTask(ctx, task)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.agentWake:
		case <-ticker.C:
		}
	}
}

func (r *Runtime) claimAgentTask(ctx context.Context) (agentTask, bool, error) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	if err := r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `DELETE FROM agent_tasks WHERE EXISTS (SELECT 1 FROM runs WHERE runs.workspace=agent_tasks.workspace AND runs.request_id=agent_tasks.request_id)`},
		{SQL: `UPDATE agent_tasks SET state='queued' WHERE state='running' AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.workspace=agent_tasks.workspace AND runs.request_id=agent_tasks.request_id)`},
	}); err != nil {
		return agentTask{}, false, err
	}
	rows, err := r.query(ctx, `SELECT workspace,request_id,principal,request,input_hash,state,failure_category,enqueued_at FROM agent_tasks WHERE state='queued' ORDER BY enqueued_at,workspace,request_id LIMIT 1`)
	if err != nil || len(rows) == 0 {
		return agentTask{}, false, err
	}
	task, err := taskFromRow(rows[0])
	if err != nil {
		return agentTask{}, false, err
	}
	requested, err := r.cancellationRequested(ctx, task.workspace, task.requestID)
	if err != nil {
		return agentTask{}, false, err
	}
	if requested {
		return agentTask{}, false, r.deleteAgentTask(ctx, task.workspace, task.requestID)
	}
	err = r.mutate(ctx, []rhiza.SQLStatement{{SQL: `UPDATE agent_tasks SET state='running' WHERE workspace=? AND request_id=? AND state='queued'`, Args: []any{task.workspace, task.requestID}, ExpectedRowsAffected: one()}})
	if err != nil {
		return agentTask{}, false, err
	}
	task.state = "running"
	return task, true, nil
}

func (r *Runtime) processAgentTask(ctx context.Context, task agentTask) {
	req, err := decodeAgentTask(task.request)
	if err != nil {
		r.blockAgentTask(task.workspace, task.requestID, errors.Join(ErrInvalidInput, err))
		return
	}
	base := ctx
	cleanup := func() {}
	defer func() { cleanup() }()
	var principal Principal
	if r.opts.RequireIdentity {
		principal, err = decodeAgentPrincipal(task.principal)
		if err == nil {
			base, cleanup, err = r.prepareAsyncContext(ctx, principal)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.blockAgentTask(task.workspace, task.requestID, err)
			return
		}
	}
	prepared, _, hash, err := r.prepareRun(base, req)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		r.blockAgentTask(task.workspace, task.requestID, err)
		return
	}
	if prepared.WorkspaceID != task.workspace || prepared.RequestID != task.requestID || hash != task.hash {
		r.blockAgentTask(task.workspace, task.requestID, ErrConflict)
		return
	}
	workerCtx := context.WithValue(base, agentTaskWorkerKey{}, true)
	_, runErr := r.Run(workerCtx, req)
	r.finishAgentTask(ctx, task, runErr)
}

func (r *Runtime) finishAgentTask(ctx context.Context, task agentTask, runErr error) {
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := r.query(cleanup, `SELECT 1 FROM runs WHERE workspace=? AND request_id=?`, task.workspace, task.requestID)
	if err != nil {
		slog.Error("beesuh: checking completed agent task failed", "workspace", task.workspace, "request", task.requestID, "error", err)
		return
	}
	if len(rows) != 0 {
		if err := r.deleteAgentTask(cleanup, task.workspace, task.requestID); err != nil {
			slog.Error("beesuh: deleting completed agent task failed", "workspace", task.workspace, "request", task.requestID, "error", err)
		}
		return
	}
	requested, cancelErr := r.cancellationRequested(cleanup, task.workspace, task.requestID)
	if cancelErr != nil {
		slog.Error("beesuh: checking agent task cancellation failed", "workspace", task.workspace, "request", task.requestID, "error", cancelErr)
		return
	}
	if requested {
		if err := r.deleteAgentTask(cleanup, task.workspace, task.requestID); err != nil {
			slog.Error("beesuh: deleting cancelled agent task failed", "workspace", task.workspace, "request", task.requestID, "error", err)
		}
		return
	}
	if ctx.Err() != nil {
		// Leave running intent for startup reconciliation instead of turning a
		// process shutdown into a permanently blocked task.
		return
	}
	r.blockAgentTask(task.workspace, task.requestID, runErr)
}

func (r *Runtime) blockAgentTask(workspace, request string, err error) {
	category := FailureCategory(err)
	if category == "" {
		category = FailureUpstream
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.mutate(ctx, []rhiza.SQLStatement{{SQL: `UPDATE agent_tasks SET state='blocked',failure_category=? WHERE workspace=? AND request_id=? AND state='running'`, Args: []any{category, workspace, request}}}); err != nil {
		slog.Error("beesuh: blocking agent task failed", "workspace", workspace, "request", request, "error", err)
	}
}

func (r *Runtime) deleteAgentTask(ctx context.Context, workspace, request string) error {
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `DELETE FROM agent_tasks WHERE workspace=? AND request_id=?`, Args: []any{workspace, request}}})
}

func (r *Runtime) agentTask(ctx context.Context, workspace, request string) (agentTask, bool, error) {
	rows, err := r.query(ctx, `SELECT workspace,request_id,principal,request,input_hash,state,failure_category,enqueued_at FROM agent_tasks WHERE workspace=? AND request_id=?`, workspace, request)
	if err != nil || len(rows) == 0 {
		return agentTask{}, false, err
	}
	task, err := taskFromRow(rows[0])
	return task, err == nil, err
}

func taskFromRow(row []any) (agentTask, error) {
	if len(row) != 8 {
		return agentTask{}, fmt.Errorf("beesuh: invalid agent task row")
	}
	workspace, a := row[0].(string)
	requestID, b := row[1].(string)
	principal, c := row[2].(string)
	request, d := row[3].(string)
	hash, e := row[4].(string)
	state, f := row[5].(string)
	category, g := row[6].(string)
	enqueued, h := row[7].(int64)
	if !a || !b || !c || !d || !e || !f || !g || !h {
		return agentTask{}, fmt.Errorf("beesuh: invalid agent task row")
	}
	return agentTask{workspace: workspace, requestID: requestID, principal: principal, request: request, hash: hash, state: state, failureCategory: category, enqueuedAt: enqueued}, nil
}

func agentTaskResult(request, state, category string) Result {
	out := Result{RequestID: request, State: state, FailureCategory: category}
	if category != "" {
		out.Error = PublicFailureMessage(category)
	}
	return out
}
