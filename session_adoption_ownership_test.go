package beesuh

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

// addSession publishes a session on the fake server so the list endpoint can
// answer title lookups the way OpenCode does.
func (f *fakeOpenCode) addSession(id, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionTitles[id] = title
}

func (f *fakeOpenCode) createsSnapshot() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func (f *fakeOpenCode) deletesSnapshot() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

func (f *fakeOpenCode) promptsSnapshot() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts
}

// The mapping-failure cleanup deletes a remote session only after proving that
// no canonical mapping points at it. This reproduces the interleaving the
// guard exists for: the session already exists remotely, it is mapped under a
// different session key, and only the intended key's own mapping insert fails.
// A delete driven by that failure would destroy a mapped conversation.
func TestMappingFailureCleanupNeverDeletesAMappedSession(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 2, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Publish a session remotely and map it under a different session key, so the
	// runtime owns a live conversation that the intended key does not. The first
	// create this fake serves is ses_1, so the run below collides with it.
	f.addSession("ses_1", "beesuh:pending:someone-elses-token")
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES('w','other-key','test','/workspace',?,'ses_1')`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}

	// Reject only the intended key's insert, so its mapping fails while the
	// session stays mapped under the other key.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_intended_mapping BEFORE INSERT ON sessions WHEN NEW.session_key <> 'other-key' BEGIN SELECT RAISE(ABORT, 'storage fault'); END`}}); err != nil {
		t.Fatal(err)
	}

	// Run a create that fails to map. Cleanup must not delete the session,
	// because a durable mapping references it under a different key.
	if _, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "intended", RequestID: "r1", Prompt: Prompt{Text: "answer"}}); err == nil {
		t.Fatal("run succeeded despite a rejected session mapping")
	}

	if deletes := f.deletesSnapshot(); deletes != 0 {
		t.Fatalf("deletes=%d, want 0: a mapped session must never be deleted after a mapping failure", deletes)
	}

	// The other key's mapping must survive untouched.
	rows, err := r.query(t.Context(), `SELECT session_id FROM sessions WHERE workspace='w' AND session_key='other-key'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][0].(string) != "ses_1" {
		t.Fatalf("mapped sessions=%v, want the other key's mapping to survive", rows)
	}
}

// The ownership check that guards a reclaim must fail closed. An unreadable
// answer is not proof that the session is unreferenced, so it must keep the
// session. A cancelled context is the ordinary way this read fails, because
// cancelling a request is itself a common cause of a failed mapping insert.
func TestUnreadableMappingCheckKeepsTheSession(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 2, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// The session is mapped under a different key, so it is a live conversation
	// that must never be released as untracked.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES('w','other-key','test','/workspace',?,'ses_1')`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}

	req := Request{WorkspaceID: "w", SessionKey: "intended"}

	// A readable check finds the other key's mapping, so the session is
	// referenced. That mapping names a different session key, so it must not be
	// reported as this request key's own canonical session.
	mapped, referenced := r.sessionUntracked(t.Context(), req, "ses_1")
	if !referenced {
		t.Fatal("a mapped session was reported as unreferenced")
	}
	if mapped != "" {
		t.Fatalf("mapped=%q, want empty: a mapping under another key is not this key's session", mapped)
	}

	// A session no mapping references is releasable.
	if _, referenced := r.sessionUntracked(t.Context(), req, "ses_unknown"); referenced {
		t.Fatal("an unreferenced session was reported as referenced")
	}

	// An unreadable check keeps the session. Reporting the error as unreferenced
	// would let a reclaim delete a mapped conversation on unknown evidence.
	dead, cancel := context.WithCancel(t.Context())
	cancel()
	if _, referenced := r.sessionUntracked(dead, req, "ses_1"); !referenced {
		t.Fatal("an unreadable ownership check reported the session as unreferenced")
	}
}

// A create whose response was lost leaves the intent durable. The next request
// must adopt that orphan instead of creating a second session, and must never
// prompt a session it did not map locally.
func TestResumedIntentAdoptsTheOrphanAndSettlesTheIntent(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Simulate a crash after the remote create but before the local mapping: the
	// intent survives with its token and a titled session exists server-side.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,recorded_at) VALUES('w','k','tok-adopt','test','/workspace',?,1)`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}
	f.addSession("ses_orphan", "beesuh:pending:tok-adopt")

	out, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "k", RequestID: "r1", Prompt: Prompt{Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.SessionID != "ses_orphan" {
		t.Fatalf("session=%q, want the adopted orphan ses_orphan", out.SessionID)
	}
	if creates := f.createsSnapshot(); creates != 0 {
		t.Fatalf("creates=%d, want 0: a resumed intent must not create a second session", creates)
	}
	rows, err := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates WHERE workspace='w' AND session_key='k'`)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].(int64) != 0 {
		t.Fatalf("pending intent survived settlement: %v", rows)
	}
}

// An empty session list after an uncertain create does not prove the earlier
// POST finished. Recovery must not treat that as permission to create again.
func TestEmptyLookupAfterUncertainCreateDoesNotAuthoriseASecondCreate(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,recorded_at) VALUES('w','k2','tok-empty','test','/workspace',?,1)`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The lookup returns nothing, so the create must be refused rather than
	// retried; a second create could duplicate the still-running first POST.
	_, err = r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "k2", RequestID: "r1", Prompt: Prompt{Text: "hello"}})
	if err == nil {
		t.Fatal("run succeeded despite an unresolved create outcome")
	}
	if creates := f.createsSnapshot(); creates != 0 {
		t.Fatalf("creates=%d, want 0: an empty lookup must not authorise a replacement create", creates)
	}
	rows, qerr := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates WHERE workspace='w' AND session_key='k2'`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if rows[0][0].(int64) != 1 {
		t.Fatalf("recovery evidence was discarded: %v", rows)
	}
}

// Several sessions sharing one ownership token are ambiguous. Recovery must
// fail closed without deleting candidates it cannot prove are untracked.
func TestAmbiguousOwnershipTokenFailsClosedWithoutDeleting(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,recorded_at) VALUES('w','k3','tok-amb','test','/workspace',?,1)`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}
	f.addSession("ses_a", "beesuh:pending:tok-amb")
	f.addSession("ses_b", "beesuh:pending:tok-amb")

	if _, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "k3", RequestID: "r1", Prompt: Prompt{Text: "hello"}}); err == nil {
		t.Fatal("run succeeded despite ambiguous ownership evidence")
	}
	if deletes := f.deletesSnapshot(); deletes != 0 {
		t.Fatalf("deletes=%d, want 0: ambiguous candidates must not be deleted", deletes)
	}
}

// A delete whose outcome is unknown must keep its recovery evidence. Clearing
// the intent would let the next request allocate a different token, so the
// untracked session could never be reclaimed or adopted again.
func TestFailedCleanupKeepsRecoveryEvidence(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Reject the mapping insert so cleanup runs, and fail the delete so its
	// outcome stays unknown.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_cleanup_mapping BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'storage fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.deleteCode = http.StatusInternalServerError
	f.mu.Unlock()

	if _, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "k4", RequestID: "r1", Prompt: Prompt{Text: "hello"}}); err == nil {
		t.Fatal("run succeeded despite a rejected mapping and a failed cleanup")
	}
	rows, qerr := r.query(t.Context(), `SELECT phase,remote_id FROM pending_session_creates WHERE workspace='w' AND session_key='k4'`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 1 {
		t.Fatalf("pending intents=%d, want 1: a failed delete must keep its recovery evidence", len(rows))
	}
	if phase := rows[0][0].(string); phase != pendingPhaseCleanup {
		t.Fatalf("phase=%q, want %q", phase, pendingPhaseCleanup)
	}
	if remote := rows[0][1].(string); remote == "" {
		t.Fatal("the unresolved session id was not recorded, so the delete cannot be retried")
	}
}

// Once a cleanup-pending intent exists, an ordinary request must not put that
// session back into the adoption pool: a late delete would destroy a mapped
// conversation. The next request must finish the delete instead.
func TestCleanupPendingSessionIsNeverAdoptedAgain(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,phase,remote_id,recorded_at) VALUES('w','k5','tok-clean','test','/workspace',?,?, 'ses_doomed',1)`, Args: []any{r.profile, pendingPhaseCleanup}}}); err != nil {
		t.Fatal(err)
	}
	f.addSession("ses_doomed", "beesuh:pending:tok-clean")

	if _, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "k5", RequestID: "r1", Prompt: Prompt{Text: "hello"}}); err == nil {
		t.Fatal("run succeeded although a cleanup outcome was still unresolved")
	}
	if creates := f.createsSnapshot(); creates != 0 {
		t.Fatalf("creates=%d, want 0: an unresolved delete must not be followed by a create", creates)
	}
	if prompts := f.promptsSnapshot(); prompts != 0 {
		t.Fatalf("prompts=%d, want 0: a session with an unresolved delete must never be prompted", prompts)
	}
}

// The lifecycle lock is what makes create, adoption, and cleanup mutually
// exclusive for one workspace and session key. Without it, a second request
// could adopt or delete a session while the first is still resolving its own
// mapping, which is the schedule that destroys a mapped conversation.
// A committed mapping is the authoritative ownership record. A pending intent
// that survived it is stale: it must be cleared, and the mapped session must be
// reused without a create, an adoption lookup, or a prompt into an unrelated
// conversation.
func TestCommittedMappingSettlesAStaleCreateIntent(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// The mapping already owns ses_1, and an intent for the same session key is
	// still present, as it would be after a crash between the mapping commit and
	// the intent cleanup. The session also carries the intent's title, so taking
	// the adoption path would match it and prompt the very session the mapping
	// already owns.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{
		{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES('w','k6','test','/workspace',?,'ses_1')`, Args: []any{r.profile}},
		{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,recorded_at) VALUES('w','k6','tok-stale','test','/workspace',?,1)`, Args: []any{r.profile}},
	}); err != nil {
		t.Fatal(err)
	}
	f.addSession("ses_1", "beesuh:pending:tok-stale")

	out, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "k6", RequestID: "r1", Prompt: Prompt{Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.SessionID != "ses_1" {
		t.Fatalf("session=%q, want the canonical mapped session ses_1", out.SessionID)
	}
	if creates := f.createsSnapshot(); creates != 0 {
		t.Fatalf("creates=%d, want 0: a committed mapping must be reused, not re-created", creates)
	}
	if deletes := f.deletesSnapshot(); deletes != 0 {
		t.Fatalf("deletes=%d, want 0: a stale intent must not reclaim a mapped session", deletes)
	}
	if prompts := f.promptsSnapshot(); prompts != 1 {
		t.Fatalf("prompts=%d, want 1: the mapped session must be prompted once", prompts)
	}
	rows, err := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates WHERE workspace='w' AND session_key='k6'`)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].(int64) != 0 {
		t.Fatalf("stale intent survived a committed mapping: %v", rows)
	}
}

// A cleanup intent whose session id was never recorded cannot be retried, and
// clearing it would put that session back into the adoption pool, where a late
// delete could destroy a mapped conversation. Recovery must fail closed with
// ErrUncertain and change nothing: no delete, no create, no adoption.
func TestCleanupIntentWithoutARemoteIDFailsClosed(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,phase,remote_id,recorded_at) VALUES('w','k7','tok-noid','test','/workspace',?,?,'',1)`, Args: []any{r.profile, pendingPhaseCleanup}}}); err != nil {
		t.Fatal(err)
	}
	// The session exists under the intent's title. Adopting it would prompt a
	// conversation whose delete outcome is unknown.
	f.addSession("ses_stranded", "beesuh:pending:tok-noid")

	_, err = r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "k7", RequestID: "r1", Prompt: Prompt{Text: "hello"}})
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("error = %v, want ErrUncertain", err)
	}
	if deletes := f.deletesSnapshot(); deletes != 0 {
		t.Fatalf("deletes=%d, want 0: an unresolvable cleanup must not issue a delete", deletes)
	}
	if creates := f.createsSnapshot(); creates != 0 {
		t.Fatalf("creates=%d, want 0: an unresolvable cleanup must not create a new session", creates)
	}
	if prompts := f.promptsSnapshot(); prompts != 0 {
		t.Fatalf("prompts=%d, want 0: a session with an unresolved delete must never be prompted", prompts)
	}
	rows, qerr := r.query(t.Context(), `SELECT phase,remote_id FROM pending_session_creates WHERE workspace='w' AND session_key='k7'`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(rows) != 1 {
		t.Fatalf("pending intents=%d, want 1: an unresolvable cleanup must keep its evidence for an operator", len(rows))
	}
	if phase := rows[0][0].(string); phase != pendingPhaseCleanup {
		t.Fatalf("phase=%q, want %q", phase, pendingPhaseCleanup)
	}
}

// The lifecycle lock is what makes create, adoption, and cleanup mutually
// exclusive for one workspace and session key. Without it, a second request
// could adopt or delete a session while the first is still resolving its own
// mapping, which is the schedule that destroys a mapped conversation.
func TestSessionLifecycleLockSerialisesConcurrentRequestsForOneKey(t *testing.T) {
	f := newFakeOpenCode(t)
	gate := make(chan struct{})
	// Hold the first request after the remote create returns, so it owns the
	// session while its mapping is still unwritten.
	f.createGate = gate
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 2, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	req := Request{WorkspaceID: "w", SessionKey: "shared", RequestID: "r1", Prompt: Prompt{Text: "hello"}}
	first := make(chan error, 1)
	go func() {
		_, err := r.Run(t.Context(), req)
		first <- err
	}()

	// The first request reached the remote create and is now paused holding the
	// session lifecycle lock.
	receive(t, f.created, "first create")

	second := make(chan error, 1)
	go func() {
		_, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "shared", RequestID: "r2", Prompt: Prompt{Text: "again"}})
		second <- err
	}()

	// The second request must not reach the provider while the first holds the
	// lock. Any create, adoption lookup, or delete would be visible as a second
	// remote operation before the first one is released.
	select {
	case err := <-second:
		t.Fatalf("second request returned before the first released the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if creates := f.createsSnapshot(); creates != 1 {
		t.Fatalf("creates=%d, want 1: the second request must not create behind the lifecycle lock", creates)
	}
	if deletes := f.deletesSnapshot(); deletes != 0 {
		t.Fatalf("deletes=%d, want 0: the second request must not delete behind the lifecycle lock", deletes)
	}

	// Let the first request finish. The second may then proceed and must reuse
	// the mapped session rather than creating another one.
	close(gate)
	if err := <-first; err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if creates := f.createsSnapshot(); creates != 1 {
		t.Fatalf("creates=%d, want 1: the second request must reuse the mapped session", creates)
	}
	if prompts := f.promptsSnapshot(); prompts != 2 {
		t.Fatalf("prompts=%d, want 2: both requests must prompt the same mapped session", prompts)
	}
}
