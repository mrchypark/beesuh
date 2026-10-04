package beesuh

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/mrchypark/rhiza"
)

// reconstructFinishedRunWithLostAcknowledgement builds the durable and
// process-local state that remains when finish commits but its caller cannot
// observe the acknowledgement. It deliberately calls the real Rhiza-backed
// finish path and then installs the same volatile fence as Run's error path;
// it does not claim to inject or emulate a network acknowledgement failure.
func reconstructFinishedRunWithLostAcknowledgement(t *testing.T, r *Runtime, req Request) Result {
	t.Helper()
	id, err := r.session(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, digest(req)}},
		{SQL: `UPDATE sessions SET active_request=? WHERE workspace=? AND session_key=? AND active_request=''`, Args: []any{req.RequestID, req.WorkspaceID, req.SessionKey}, ExpectedRowsAffected: one()},
	}); err != nil {
		t.Fatal(err)
	}
	terminal := Result{RequestID: req.RequestID, SessionID: id, State: "succeeded", Output: json.RawMessage(`{"text":"durable"}`)}
	if err := r.finish(t.Context(), req.WorkspaceID, req.SessionKey, req.RequestID, terminal); err != nil {
		t.Fatal(err)
	}
	r.fenceWorkspace(req.WorkspaceID)
	return terminal
}

func reserveRunningRun(t *testing.T, r *Runtime, req Request) string {
	t.Helper()
	id, err := r.session(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, digest(req)}},
		{SQL: `UPDATE sessions SET active_request=? WHERE workspace=? AND session_key=? AND active_request=''`, Args: []any{req.RequestID, req.WorkspaceID, req.SessionKey}, ExpectedRowsAffected: one()},
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertOrphanedRunningRun(t *testing.T, r *Runtime, req Request) {
	t.Helper()
	// There is deliberately no sessions row for this run. This is the durable
	// shape left if its session mapping is lost after reservation; the direct
	// insert reconstructs that database state without pretending a transport
	// fault made it happen.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, digest(req)}}}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverLiftsFenceAfterFinishCommittedButAcknowledgementWasLost(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	req := testRequest("session", "finished", "first")
	terminal := reconstructFinishedRunWithLostAcknowledgement(t, r, req)
	if !r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("reconstructed lost-acknowledgement state must start fenced")
	}

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover = %v", err)
	}
	if r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("Recover left a fence after its run was already durably finished")
	}
	// The terminal row is the durable outcome. Recover must not turn it into an
	// interrupted result or send another prompt while releasing the stale fence.
	replayed, err := r.Run(t.Context(), req)
	if err != nil || !reflect.DeepEqual(replayed, terminal) {
		t.Fatalf("replay = %+v, %v; want %+v", replayed, err, terminal)
	}
	if _, err := r.Run(t.Context(), testRequest("session", "next", "second")); err != nil {
		t.Fatalf("next run after Recover = %v", err)
	}
	if creates, prompts, aborts := f.counts(); creates != 1 || prompts != 1 || aborts != 0 {
		t.Fatalf("creates=%d prompts=%d aborts=%d, want 1,1,0", creates, prompts, aborts)
	}
}

func TestRecoverQueryFailureKeepsLostAcknowledgementFence(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	req := testRequest("session", "finished", "first")
	reconstructFinishedRunWithLostAcknowledgement(t, r, req)
	// Rename only the source used by Recover's authoritative outstanding-work
	// read. This is a test-only SQLite state reconstruction, not a claim that a
	// production acknowledgement failure changes the schema. Restoring the table
	// lets the same runtime prove that a later healthy Recovery clears the fence.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `ALTER TABLE runs RENAME TO runs_recover_fault`}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Recover(t.Context()); err == nil {
		t.Fatal("Recover unexpectedly succeeded after its durable-state query failed")
	}
	if !r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("Recover cleared the fence after it could not read durable state")
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `ALTER TABLE runs_recover_fault RENAME TO runs`}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("second Recover = %v", err)
	}
	if r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("healthy Recover did not clear the retained stale fence")
	}
	if _, prompts, aborts := f.counts(); prompts != 0 || aborts != 0 {
		t.Fatalf("prompts=%d aborts=%d, want 0,0", prompts, aborts)
	}
}

func TestRecoverKeepsLostAcknowledgementFenceWhileSameWorkspaceRunRemainsUnresolved(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	finished := testRequest("finished", "finished", "first")
	reconstructFinishedRunWithLostAcknowledgement(t, r, finished)
	pending := testRequest("pending", "pending", "second")
	pendingID := reserveRunningRun(t, r, pending)
	f.mu.Lock()
	f.abortFail = map[string]bool{pendingID: true}
	f.mu.Unlock()

	if err := r.Recover(t.Context()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("Recover = %v, want ErrUncertain", err)
	}
	if !r.runFenceHolds(finished.WorkspaceID) {
		t.Fatal("Recover reopened a workspace with another unsettled run")
	}
	if _, err := r.Run(t.Context(), testRequest("later", "later", "third")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("run in workspace with unsettled recovery = %v, want ErrUncertain", err)
	}
	if _, prompts, aborts := f.counts(); prompts != 0 || aborts != 1 {
		t.Fatalf("prompts=%d aborts=%d, want 0,1", prompts, aborts)
	}
}

func TestRecoverLiftsStaleRunFenceButKeepsPendingExternalEffectBlocking(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	req := testRequest("session", "finished", "first")
	reconstructFinishedRunWithLostAcknowledgement(t, r, req)
	if err := r.recordUncertainIntent(t.Context(), req, "effect", "deploy"); err != nil {
		t.Fatal(err)
	}
	if err := r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("Recover did not release the stale run fence")
	}
	if err := r.assertWorkspaceRecoverable(t.Context(), req.WorkspaceID); !errors.Is(err, ErrUncertain) {
		t.Fatalf("pending external effect = %v, want ErrUncertain", err)
	}
	if status, err := r.Status(t.Context()); err != nil || !status.Blocked {
		t.Fatalf("status after stale fence recovery = %+v, %v; want blocked", status, err)
	}
}

func TestRecoverKeepsFenceForOrphanedRunningRun(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	req := testRequest("orphan", "orphan", "first")
	insertOrphanedRunningRun(t, r, req)
	if r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("orphaned durable run unexpectedly started with a process-local fence")
	}

	if err := r.Recover(t.Context()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("Recover = %v, want ErrUncertain for an orphaned running row", err)
	}
	if !r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("Recover reopened a workspace with an orphaned running row")
	}
	if _, err := r.Run(t.Context(), testRequest("later", "later", "second")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("run in workspace with orphaned recovery = %v, want ErrUncertain", err)
	}
	if creates, prompts, aborts := f.counts(); creates != 0 || prompts != 0 || aborts != 0 {
		t.Fatalf("creates=%d prompts=%d aborts=%d, want 0,0,0", creates, prompts, aborts)
	}
}

func TestRecoverKeepsFenceForOrphanAfterSettlingOtherRunInWorkspace(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	orphan := testRequest("orphan", "orphan", "first")
	insertOrphanedRunningRun(t, r, orphan)
	active := testRequest("active", "active", "second")
	reserveRunningRun(t, r, active)
	r.fenceWorkspace(orphan.WorkspaceID)

	if err := r.Recover(t.Context()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("Recover = %v, want ErrUncertain for the orphaned running row", err)
	}
	settled, err := r.GetRun(t.Context(), active.WorkspaceID, active.RequestID)
	if err != nil || settled.State != "interrupted" {
		t.Fatalf("active run after Recover = %+v, %v; want interrupted", settled, err)
	}
	if !r.runFenceHolds(orphan.WorkspaceID) {
		t.Fatal("Recover reopened the workspace after settling only its mapped run")
	}
	if _, err := r.Run(t.Context(), testRequest("later", "later", "third")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("run in workspace with orphaned recovery = %v, want ErrUncertain", err)
	}
	if _, prompts, aborts := f.counts(); prompts != 0 || aborts != 1 {
		t.Fatalf("prompts=%d aborts=%d, want 0,1", prompts, aborts)
	}
}

func TestRecoverClearsStaleWorkspaceBeforeLaterWorkspaceAbortFailure(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	finished := Request{WorkspaceID: "workspace-a", SessionKey: "finished", RequestID: "finished", Prompt: Prompt{Text: "first"}}
	reconstructFinishedRunWithLostAcknowledgement(t, r, finished)
	pending := Request{WorkspaceID: "workspace-b", SessionKey: "pending", RequestID: "pending", Prompt: Prompt{Text: "second"}}
	pendingID := reserveRunningRun(t, r, pending)
	r.fenceWorkspace(pending.WorkspaceID)
	f.mu.Lock()
	f.abortFail = map[string]bool{pendingID: true}
	f.mu.Unlock()

	if err := r.Recover(t.Context()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("Recover = %v, want ErrUncertain from workspace-b abort", err)
	}
	if r.runFenceHolds(finished.WorkspaceID) {
		t.Fatal("workspace-a remained fenced after its durable run was already complete")
	}
	if !r.runFenceHolds(pending.WorkspaceID) {
		t.Fatal("workspace-b fence cleared after its abort failed")
	}
}

func TestClearCompletedRunFenceReadFailureRetainsFinishedRunFence(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	req := testRequest("session", "interrupted", "first")
	id := reserveRunningRun(t, r, req)
	r.fenceWorkspace(req.WorkspaceID)

	// Recover normally holds life exclusively across stop, finish and the final
	// per-workspace proof. Reconstruct that exact interval here so this test
	// exercises clearCompletedRunFence after a real durable finish. Renaming the
	// table is a SQLite test fault for that final authoritative read only; it
	// does not claim to model an agent or network acknowledgement failure.
	r.life.Lock()
	func() {
		defer r.life.Unlock()
		if err := r.stopSession(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		out := Result{SessionID: id, State: "interrupted", Error: "execution interrupted; external tool effects may have occurred"}
		if err := r.finish(t.Context(), req.WorkspaceID, req.SessionKey, req.RequestID, out); err != nil {
			t.Fatal(err)
		}
		if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `ALTER TABLE runs RENAME TO runs_clear_fence_fault`}}); err != nil {
			t.Fatal(err)
		}
		if err := r.clearCompletedRunFence(t.Context(), req.WorkspaceID); err == nil {
			t.Fatal("clearCompletedRunFence unexpectedly succeeded while its durable query was unavailable")
		}
		if !r.runFenceHolds(req.WorkspaceID) {
			t.Fatal("completion proof cleared the fence after its authoritative read failed")
		}
	}()
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `ALTER TABLE runs_clear_fence_fault RENAME TO runs`}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover after restoring durable reads = %v", err)
	}
	if r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("Recover did not clear the retained fence after its durable read recovered")
	}
	stored, err := r.GetRun(t.Context(), req.WorkspaceID, req.RequestID)
	if err != nil || stored.State != "interrupted" {
		t.Fatalf("stored run = %+v, %v; want interrupted", stored, err)
	}
	if _, prompts, aborts := f.counts(); prompts != 0 || aborts != 1 {
		t.Fatalf("prompts=%d aborts=%d, want 0,1", prompts, aborts)
	}
}
