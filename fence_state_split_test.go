package beesuh

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

// mappedSessionID reads the remote session a request was bound to, which the
// abort failure has to name by provider session ID rather than by key.
func mappedSessionID(t *testing.T, r *Runtime, req Request) string {
	t.Helper()
	rows, err := r.query(t.Context(), `SELECT session_id FROM sessions WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("session rows for %s = %d, want 1", req.SessionKey, len(rows))
	}
	return rows[0][0].(string)
}

func TestEffectResolutionKeepsRunFenceUntilRecovery(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptCode = http.StatusInternalServerError
	f.abortOK = false
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	stuck := testRequest("stuck", "stuck-request", "first")
	if _, err := r.Run(context.Background(), stuck); !errors.Is(err, ErrUncertain) {
		t.Fatalf("stuck run = %v, want ErrUncertain", err)
	}
	if err := r.recordUncertainIntent(context.Background(), stuck, "effect-1", "deploy"); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveUncertainEffect(context.Background(), stuck.WorkspaceID, "effect-1", dispositionResolved); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.promptCode = 0
	f.mu.Unlock()

	if _, err := r.Run(context.Background(), testRequest("later", "later-request", "second")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("run after effect resolution = %v, want ErrUncertain until Recover", err)
	}
	if status, err := r.Status(context.Background()); err != nil || !status.Blocked {
		t.Fatalf("status after effect resolution = %+v, %v; want blocked", status, err)
	}

	f.mu.Lock()
	f.abortOK = true
	f.promptCode = 0
	f.mu.Unlock()
	if err := r.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := r.Status(context.Background()); err != nil || status.Blocked {
		t.Fatalf("status after Recover = %+v, %v; want unblocked", status, err)
	}
	if _, err := r.Run(context.Background(), testRequest("later", "later-request", "second")); err != nil {
		t.Fatalf("run after Recover = %v", err)
	}
}

// Recover discovers work only through sessions that still hold an
// active_request, and finishing a run clears that column. Once a workspace's
// run is persisted it can never be rediscovered, so a fence lifted only after
// the whole scan would strand an earlier workspace whenever a later one fails:
// the retry finds nothing to reconcile and the workspace stays fenced forever.
// Lifting each workspace as soon as its own runs are persisted keeps an
// unrelated failure from blocking a workspace whose recovery already finished.
func TestRecoverFailureOnLaterWorkspaceDoesNotStrandEarlierFence(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptCode = http.StatusInternalServerError
	f.abortOK = false
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	first := Request{WorkspaceID: "workspace-a", SessionKey: "a", RequestID: "a-request", Prompt: opencode.Prompt{Text: "a"}}
	second := Request{WorkspaceID: "workspace-b", SessionKey: "b", RequestID: "b-request", Prompt: opencode.Prompt{Text: "b"}}
	for _, req := range []Request{first, second} {
		if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrUncertain) {
			t.Fatalf("run %s = %v, want ErrUncertain", req.WorkspaceID, err)
		}
	}
	if !r.runFenceHolds(first.WorkspaceID) || !r.runFenceHolds(second.WorkspaceID) {
		t.Fatal("both workspaces must carry a run fence before Recover")
	}

	// Aborts succeed for workspace-a and are refused for workspace-b. The scan
	// order is workspace,session_key, so A is persisted first and B then fails.
	f.mu.Lock()
	f.abortOK = true
	f.abortFail = map[string]bool{mappedSessionID(t, r, second): true}
	f.mu.Unlock()
	if err := r.Recover(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("Recover = %v, want ErrUncertain from the refused abort", err)
	}
	if r.runFenceHolds(first.WorkspaceID) {
		t.Fatal("workspace-a stayed fenced after its own recovery was persisted")
	}
	if !r.runFenceHolds(second.WorkspaceID) {
		t.Fatal("workspace-b must stay fenced until its own recovery succeeds")
	}

	// A retry after the fault clears must reopen the recovered workspace too.
	f.mu.Lock()
	f.abortFail = nil
	f.promptCode = 0
	f.mu.Unlock()
	if err := r.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.runFenceHolds(first.WorkspaceID) || r.runFenceHolds(second.WorkspaceID) {
		t.Fatal("fences remained after a successful retry")
	}
}

// The other side of the same requirement: a workspace must not reopen while it
// still owes recovery. Two interrupted runs share one workspace, and the
// second cannot be persisted. Its fence has to survive that partial pass and
// clear only once a retry settles the remaining run.
func TestRecoverKeepsFenceUntilEveryRunInWorkspaceIsPersisted(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptCode = http.StatusInternalServerError
	f.abortOK = false
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	workspace := "workspace"
	first := Request{WorkspaceID: workspace, SessionKey: "a", RequestID: "a-request", Prompt: opencode.Prompt{Text: "a"}}
	second := Request{WorkspaceID: workspace, SessionKey: "b", RequestID: "b-request", Prompt: opencode.Prompt{Text: "b"}}
	if _, err := r.Run(context.Background(), first); !errors.Is(err, ErrUncertain) {
		t.Fatalf("run a = %v, want ErrUncertain", err)
	}
	if !r.runFenceHolds(workspace) {
		t.Fatal("workspace must be fenced while its run is unresolved")
	}

	// The fence denies a second Run in the same workspace, so the second
	// reservation is written directly. That is the same durable shape a crash
	// between two overlapping runs leaves behind, and it is the only way one
	// workspace can owe recovery twice at once.
	if _, err := r.session(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')`, Args: []any{workspace, second.RequestID, second.SessionKey, digest(second)}},
		{SQL: `UPDATE sessions SET active_request=? WHERE workspace=? AND session_key=? AND active_request=''`, Args: []any{second.RequestID, workspace, second.SessionKey}, ExpectedRowsAffected: one()},
	}); err != nil {
		t.Fatal(err)
	}
	if !r.runFenceHolds(workspace) {
		t.Fatal("workspace must stay fenced while both runs are unresolved")
	}

	// Let the first run settle but refuse the second, so the workspace keeps a
	// fence even though one of its two runs is already durable.
	f.mu.Lock()
	f.abortOK = true
	f.abortFail = map[string]bool{mappedSessionID(t, r, second): true}
	f.mu.Unlock()
	if err := r.Recover(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("Recover = %v, want ErrUncertain from the refused abort", err)
	}
	if !r.runFenceHolds(workspace) {
		t.Fatal("workspace reopened while a second interrupted run was still unsettled")
	}

	f.mu.Lock()
	f.abortFail = nil
	f.promptCode = 0
	f.mu.Unlock()
	if err := r.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.runFenceHolds(workspace) {
		t.Fatal("workspace stayed fenced after every run was persisted")
	}
	if _, err := r.Run(context.Background(), Request{WorkspaceID: workspace, SessionKey: "c", RequestID: "c-request", Prompt: opencode.Prompt{Text: "c"}}); err != nil {
		t.Fatalf("run after recovery = %v", err)
	}
}

func TestFenceStateIsWorkspaceScopedAndStatusIncludesPendingEffects(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	a := Request{WorkspaceID: "workspace-a", SessionKey: "a", RequestID: "a-request", Prompt: Prompt{Text: "a"}}
	if err := r.recordUncertainIntent(context.Background(), a, "effect-a", "deploy"); err != nil {
		t.Fatal(err)
	}
	if status, err := r.Status(context.Background()); err != nil || !status.Blocked {
		t.Fatalf("status with pending effect = %+v, %v; want blocked", status, err)
	}
	if err := r.assertWorkspaceRecoverable(context.Background(), "workspace-b"); err != nil {
		t.Fatalf("workspace-a effect fenced workspace-b: %v", err)
	}
	b := Request{WorkspaceID: "workspace-b", SessionKey: "b", RequestID: "b-request", Prompt: Prompt{Text: "b"}}
	if _, err := r.Run(context.Background(), b); err != nil {
		t.Fatalf("workspace-b run = %v", err)
	}

	if err := r.ResolveUncertainEffect(context.Background(), a.WorkspaceID, "effect-a", dispositionAbandoned); err != nil {
		t.Fatal(err)
	}
	if status, err := r.Status(context.Background()); err != nil || status.Blocked {
		t.Fatalf("status after resolving only pending effect = %+v, %v; want unblocked", status, err)
	}
}

// A pending row written by another runtime is durable evidence, so it blocks
// dispatch in this process too. An in-process mirror of the effect fence would
// miss it and send a second external effect that may duplicate the first.
func TestForeignPendingEffectStillBlocksDispatch(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	req := Request{WorkspaceID: "shared", SessionKey: "s", RequestID: "r", Prompt: Prompt{Text: "a"}}
	// Write the row directly, standing in for another runtime sharing the
	// database. Nothing in this process records it, so only a durable lookup can
	// see it.
	if err := r.mutate(context.Background(), []rhiza.SQLStatement{{SQL: `INSERT INTO uncertain_effects(workspace,effect_key,session_key,request_id,event,disposition,recorded_at) VALUES(?,?,?,?,?,?,?)`, Args: []any{"shared", "foreign-effect", "s", "other", "deploy", dispositionPending, 1}}}); err != nil {
		t.Fatal(err)
	}

	if err := r.assertWorkspaceRecoverable(context.Background(), "shared"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("dispatch with foreign pending effect = %v, want ErrUncertain", err)
	}
	if status, err := r.Status(context.Background()); err != nil || !status.Blocked {
		t.Fatalf("status with foreign pending effect = %+v, %v; want blocked", status, err)
	}
	if err := r.ResolveUncertainEffect(context.Background(), "shared", "foreign-effect", dispositionAbandoned); err != nil {
		t.Fatal(err)
	}
	if err := r.assertWorkspaceRecoverable(context.Background(), req.WorkspaceID); err != nil {
		t.Fatalf("dispatch after resolving foreign effect = %v", err)
	}
	if status, err := r.Status(context.Background()); err != nil || status.Blocked {
		t.Fatalf("status after resolving foreign effect = %+v, %v; want unblocked", status, err)
	}
}

// A run fence installed before the call begins must always be reported, no
// matter what the pending table contains.
func TestStatusReportsRunFenceAlreadyPresentAtCallStart(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	r.fenceWorkspace("workspace")
	for i := 0; i < 50; i++ {
		if status, err := r.Status(context.Background()); err != nil || !status.Blocked {
			t.Fatalf("status with installed run fence = %+v, %v; want blocked", status, err)
		}
	}
}

// The reverse direction matters too: once the last fence is lifted, Status must
// stop reporting blocked without waiting for any cache expiry.
func TestStatusStopsReportingBlockedAfterFenceIsLifted(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	r.fenceWorkspace("workspace")
	r.unfenceRunWorkspace("workspace")
	if status, err := r.Status(context.Background()); err != nil || status.Blocked {
		t.Fatalf("status after lifting the last fence = %+v, %v; want unblocked", status, err)
	}
}
