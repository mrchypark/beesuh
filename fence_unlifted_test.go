package beesuh

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/rhiza"
)

// assertNoReservation proves that neither half of a run reservation survived:
// no run row and no session still holding the request.
func assertNoReservation(t *testing.T, r *Runtime, req Request) error {
	t.Helper()
	runs, err := r.query(t.Context(), `SELECT COUNT(*) FROM runs WHERE workspace=? AND request_id=?`, req.WorkspaceID, req.RequestID)
	if err != nil {
		return err
	}
	if got := runs[0][0].(int64); got != 0 {
		return fmt.Errorf("run rows after rejected reservation = %d, want 0", got)
	}
	sessions, err := r.query(t.Context(), `SELECT active_request FROM sessions WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil {
		return err
	}
	if len(sessions) == 1 && sessions[0][0].(string) != "" {
		return fmt.Errorf("active_request after rejected reservation = %q, want empty", sessions[0][0])
	}
	return nil
}

// A rejected reservation is before every agent or Conductor dispatch. The
// reservation statements form one atomic batch, so this trigger leaves no run
// row and no active_request for Recover to reconcile. It must therefore not
// install a run fence: that would make the workspace permanently unavailable
// even though no externally visible work was started.
func TestReservationBatchFailureDoesNotPermanentlyFence(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	// Reject the first statement of Run's reservation batch. SQLite aborts the
	// batch atomically, which is the only storage-failure outcome for which this
	// test asserts immediate admission is safe.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_run_reservation BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT, 'reservation fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	blocked := testRequest("blocked", "reservation-fault", "hello")
	if _, err := r.Run(t.Context(), blocked); err == nil {
		t.Fatal("Run accepted a deliberately rejected reservation")
	}
	// The failed attempt returns before the agent or Conductor is reached, so a
	// workspace fence would protect nothing. Prove the absence of remote work
	// rather than inferring it from the returned error.
	if _, prompts, _ := f.counts(); prompts != 0 {
		t.Fatalf("prompts after rejected reservation = %d, want 0", prompts)
	}
	if r.runFenceHolds(blocked.WorkspaceID) {
		t.Fatal("rejected atomic reservation installed a run fence without recovery evidence")
	}
	// A rolled back batch leaves neither half of the reservation behind.
	if err := assertNoReservation(t, r, blocked); err != nil {
		t.Fatal(err)
	}
	if status, err := r.Status(t.Context()); err != nil || status.Blocked {
		t.Fatalf("status after rejected reservation = %+v, %v; want unblocked", status, err)
	}

	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `DROP TRIGGER reject_run_reservation`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(t.Context(), testRequest("after", "reservation-after", "again")); err != nil {
		t.Fatalf("run after rejected reservation = %v; want admitted workspace", err)
	}
}

// An auth rejection can occur before Run reserves a session. If persisting the
// ready -> needs_login generation change fails, there is no active_request for
// Recover to discover. The next request must be able to re-read the still-ready
// connection instead of being refused forever by an unrelated workspace fence.
func TestConnectionGenerationUncertaintyDoesNotPermanentlyFence(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	c := Connection{ID: "provider", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "work", CredentialRef: "account", State: "ready"}
	if err := r.RegisterConnection(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	ctx := principal(t.Context(), c.UserID, c.WorkspaceID)

	// Force the exact optimistic generation UPDATE in connectionAuthError to
	// reject. The connection row stays ready, so a later authoritative read can
	// safely admit work once the transient storage fault is gone.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_connection_generation BEFORE UPDATE OF state ON connections WHEN NEW.state='needs_login' BEGIN SELECT RAISE(ABORT, 'generation fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	if err := r.connectionAuthError(ctx, c, auth.ErrInvalidCredentials); !errors.Is(err, ErrUncertain) {
		t.Fatalf("connectionAuthError = %v, want ErrUncertain", err)
	}
	workspace := r.scopedWorkspaceForConnection(c)
	if r.runFenceHolds(workspace) {
		t.Fatal("failed connection generation update installed a run fence without recoverable run evidence")
	}

	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `DROP TRIGGER reject_connection_generation`}}); err != nil {
		t.Fatal(err)
	}
	// checkConnection is Run's pre-reservation admission step. Success proves a
	// later request can re-read the connection and pass admission.
	if err := r.checkConnection(ctx, c.ID, "provider", c.TargetID); err != nil {
		t.Fatalf("connection admission after generation fault = %v", err)
	}
	// The fixture does not implement a model response, so execution can fail
	// after reservation. It must still get past Run's fence admission gate.
	_, err := r.Run(ctx, Request{WorkspaceID: c.WorkspaceID, SessionKey: "after", RequestID: "generation-after", ConnectionID: c.ID, Prompt: Prompt{Text: "again"}})
	if errors.Is(err, ErrUncertain) {
		t.Fatalf("run after generation fault = %v; want admission after connection re-read", err)
	}
}

// A reservation batch that commits before its acknowledgement is lost is the
// other half of the same failure mode. It leaves a running row plus an active
// request, which is exactly the evidence Recover reconciles, so the runtime
// stays recoverable without any process-local fence. This is the outcome the
// reservation fence removal relies on, so it is asserted directly rather than
// through a storage fault that cannot reproduce a lost acknowledgement.
func TestCommittedReservationWithoutFenceIsRecoverable(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()

	req := testRequest("session", "committed", "hello")
	// Only the durable session mapping is created. The reservation batch below is
	// then committed directly, which is exactly the state a lost acknowledgement
	// leaves behind, without running a real request first and rewriting its result.
	if _, err := r.session(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, digest(req)}},
		{SQL: `UPDATE sessions SET active_request=? WHERE workspace=? AND session_key=? AND active_request=''`, Args: []any{req.RequestID, req.WorkspaceID, req.SessionKey}, ExpectedRowsAffected: one()},
	}); err != nil {
		t.Fatal(err)
	}
	// No fence is installed and no remote work started, so the workspace stays
	// usable and the unresolved reservation alone denies admission.
	if r.runFenceHolds(req.WorkspaceID) {
		t.Fatal("committed reservation installed a run fence without recovery evidence")
	}
	if _, prompts, _ := f.counts(); prompts != 0 {
		t.Fatalf("prompts after committed reservation = %d, want 0", prompts)
	}
	// No process-local fence is needed: the durable active request still denies
	// admission, so nothing is admitted while the reservation is unresolved.
	if _, err := r.Run(t.Context(), testRequest("session", "blocked", "again")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("run against an unresolved reservation = %v, want ErrUncertain", err)
	}
	// Status.Blocked reports the union of run fences and pending effects only. An
	// unresolved reservation is deliberately outside that aggregate, so the
	// durable session state, not this Boolean, remains the authority for this
	// condition.
	if status, err := r.Status(t.Context()); err != nil || status.Blocked {
		t.Fatalf("status with unresolved reservation = %+v, %v; want unblocked", status, err)
	}

	if err := r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status, err := r.Status(t.Context()); err != nil || status.Blocked {
		t.Fatalf("status after Recover = %+v, %v; want unblocked", status, err)
	}
	if _, err := r.Run(t.Context(), testRequest("session", "after", "again")); err != nil {
		t.Fatalf("run after Recover = %v", err)
	}
}
