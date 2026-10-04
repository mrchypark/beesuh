package beesuh

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

// A remote session created before a failed mapping insert is unreachable by
// any later abort, recovery, or retention pass. Beesuh must reclaim it in the
// same call rather than leak an untracked session on the OpenCode server.
func TestFailedSessionMappingInsertReclaimsRemoteSession(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Make the mapping insert fail the way a storage fault would, after the
	// remote session already exists.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_session_mapping BEFORE INSERT ON sessions WHEN NEW.session_id <> 'ses_existing' BEGIN SELECT RAISE(ABORT, 'storage fault'); END`}}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Run(t.Context(), testRequest("s", "reclaim", "answer")); err == nil {
		t.Fatal("run succeeded despite a conflicting session mapping")
	}
	f.mu.Lock()
	creates, deletes, prompts := f.creates, f.deletes, f.prompts
	f.mu.Unlock()
	t.Logf("creates=%d deletes=%d prompts=%d", creates, deletes, prompts)
	if creates != 1 || deletes != 1 {
		t.Fatalf("creates=%d deletes=%d, want 1 and 1", creates, deletes)
	}
	if prompts != 0 {
		t.Fatalf("prompts=%d, want 0: an untracked session must never be prompted", prompts)
	}
}

// A cancelled request context is the most common way the mapping insert fails
// after the remote session already exists. Reclaim must not inherit the dead
// request context, or the DELETE fails too and the orphan survives forever.
func TestSessionMappingInsertCancellationReclaimsWithDetachedContext(t *testing.T) {
	f := newFakeOpenCode(t)
	gate := make(chan struct{})
	f.createGate = gate
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, testRequest("s", "reclaim-cancel", "answer"))
		done <- err
	}()

	select {
	case <-f.created:
	case <-time.After(10 * time.Second):
		t.Fatal("session create never reached the server")
	}
	// Let the create response complete so the runtime holds a real session ID,
	// then cancel the request before the mapping insert runs.
	close(gate)
	// The runtime now holds the created session ID. Cancel the request so the
	// mapping insert fails on a dead context, the way a client disconnect or an
	// expired deadline does in production.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_session_mapping BEFORE INSERT ON sessions WHEN NEW.session_id <> 'ses_existing' BEGIN SELECT RAISE(ABORT, 'storage fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	cancel()

	// The reclaim must run on its own context and reach the server even though
	// the request that created the session is already cancelled.
	select {
	case <-f.deleted:
	case <-time.After(10 * time.Second):
		t.Fatal("reclaim DELETE never reached the server")
	}

	select {
	case err := <-done:
		t.Logf("run returned: %v", err)
		if err == nil {
			t.Fatal("run succeeded despite a cancelled mapping insert")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}

	f.mu.Lock()
	creates, deletes, prompts := f.creates, f.deletes, f.prompts
	f.mu.Unlock()
	if creates != 1 || deletes != 1 {
		t.Fatalf("creates=%d deletes=%d, want 1 and 1: a cancelled request must not leak the session", creates, deletes)
	}
	if prompts != 0 {
		t.Fatalf("prompts=%d, want 0: an untracked session must never be prompted", prompts)
	}
}

// A request cancelled while the create response is in flight is the hardest
// orphan: the server has already created the session, but the client never
// learns its ID, so no ID-based abort, recovery, or retention pass can reach
// it. Beesuh names each pending session with an unguessable ownership token
// recorded durably before the create, so the runtime can prove the orphan is
// its own and adopt it instead of leaking it.
func TestSessionCreateCancellationAdoptsTheOrphanedSession(t *testing.T) {
	f := newFakeOpenCode(t)
	// The create is recorded on the server but the response fails, so the
	// client never learns the ID: the exact orphan shape a cancelled or expired
	// request produces.
	f.createCode = http.StatusBadGateway
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.Run(t.Context(), testRequest("s", "reclaim-create-cancel", "answer")); err != nil {
		t.Fatalf("run with a lost create response: %v", err)
	}
	f.mu.Lock()
	creates, prompts, deletes, live := f.creates, f.prompts, f.deletes, len(f.sessionTitles)
	var adoptedTitle string
	for _, title := range f.sessionTitles {
		adoptedTitle = title
	}
	f.mu.Unlock()
	if creates != 1 || prompts != 1 {
		t.Fatalf("creates=%d prompts=%d, want 1 and 1: the orphan must be adopted and prompted, never duplicated", creates, prompts)
	}
	if deletes != 0 {
		t.Fatalf("deletes=%d, want 0: a single titled session must be adopted, not deleted", deletes)
	}
	if live != 1 {
		t.Fatalf("live sessions=%d, want 1: the orphan must be adopted, not leaked or duplicated", live)
	}
	// The prompt must reach the single surviving session, which is this run's
	// own orphan, and no other session.
	f.mu.Lock()
	adoptedID := ""
	for id := range f.sessionTitles {
		adoptedID = id
	}
	firstTargets := append([]string(nil), f.promptTargets...)
	f.mu.Unlock()
	if len(firstTargets) != 1 || firstTargets[0] != adoptedID {
		t.Fatalf("prompt targets %v, want exactly the adopted session %q", firstTargets, adoptedID)
	}
	// The adopted title must carry an ownership token, never the caller-supplied
	// workspace and session key: a predictable title could be matched by a
	// session a user created by hand and adopted as if it were Beesuh's own.
	if !strings.HasPrefix(adoptedTitle, "beesuh:pending:") {
		t.Fatalf("adopted title %q must use an ownership token, not caller-supplied identifiers", adoptedTitle)
	}
	if strings.Contains(adoptedTitle, "reclaim-create-cancel") {
		t.Fatalf("adopted title %q leaks the caller-supplied session key", adoptedTitle)
	}

	// A follow-up request must reuse the adopted session instead of creating a
	// second one with the same title.
	if _, err := r.Run(t.Context(), testRequest("s", "reclaim-create-followup", "answer")); err != nil {
		t.Fatalf("follow-up run: %v", err)
	}
	f.mu.Lock()
	creates, live = f.creates, len(f.sessionTitles)
	f.mu.Unlock()
	if creates != 1 || live != 1 {
		t.Fatalf("creates=%d live=%d, want 1 and 1: the adopted session must be reused", creates, live)
	}
}

// A session the runtime cannot prove it created must never be adopted. An
// unrelated session whose title happens to look like a Beesuh session, or a
// session left behind under a token from a different attempt, is reclaimed
// only when the token matches this run's own durable intent. A colliding
// foreign session proves nothing and must be left untouched.
func TestSessionCreateCancellationNeverAdoptsUnprovenSessions(t *testing.T) {
	f := newFakeOpenCode(t)
	// The create fails after recording, so recovery runs. Before the run, plant
	// a foreign session titled exactly like the deterministic legacy title the
	// old implementation used. The new token design must not touch it.
	f.createCode = http.StatusBadGateway
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// A user-created session that collides with the legacy deterministic title,
	// and one that looks like an internal pending session but with a different
	// (unknown) token. Neither is provably ours.
	f.mu.Lock()
	f.sessionTitles["ses_foreign"] = "beesuh:workspace:unproven"
	f.sessionTitles["ses_other"] = "beesuh:pending:not-our-token"
	f.mu.Unlock()

	// This run's own create fails and leaves exactly one session titled with the
	// token recorded for this attempt, which is adopted.
	if _, err := r.Run(t.Context(), testRequest("unproven", "r1", "answer")); err != nil {
		t.Fatalf("run with a lost create response: %v", err)
	}
	f.mu.Lock()
	deletes, prompts, live := f.deletes, f.prompts, len(f.sessionTitles)
	f.mu.Unlock()
	// Only this run's own orphan is adopted, and it is the single session that
	// receives a prompt. The two foreign sessions are never deleted and never
	// prompted, because neither carries this attempt's ownership token.
	if deletes != 0 {
		t.Fatalf("deletes=%d, want 0: foreign sessions must never be reclaimed", deletes)
	}
	if prompts != 1 {
		t.Fatalf("prompts=%d, want 1: only the proven own orphan may be prompted", prompts)
	}
	f.mu.Lock()
	targets := append([]string(nil), f.promptTargets...)
	f.mu.Unlock()
	for _, target := range targets {
		if target == "ses_foreign" || target == "ses_other" {
			t.Fatalf("prompted %q: a session that is not provably ours must never be prompted", target)
		}
	}
	if live != 3 {
		t.Fatalf("live sessions=%d, want 3: two foreign sessions plus the adopted orphan must survive", live)
	}
}

// A crash between a successful create and the local mapping insert strands the
// remote session under an intent that is still durable. The next attempt must
// reuse that intent's token to adopt the stranded session instead of creating a
// second session for the same key and orphaning the first one forever.
func TestStrandedSessionFromEarlierAttemptIsAdoptedNotDuplicated(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Reproduce the crash shape: a durable intent plus the remote session it
	// named, with no sessions row mapping them.
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,recorded_at) VALUES('workspace','k','tok-crash','test','/workspace',?,1)`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sessionTitles["ses_stranded"] = "beesuh:pending:tok-crash"
	f.mu.Unlock()

	if _, err := r.Run(t.Context(), testRequest("k", "req", "answer")); err != nil {
		t.Fatalf("run after a stranded create: %v", err)
	}
	f.mu.Lock()
	creates, prompts, live := f.creates, f.prompts, len(f.sessionTitles)
	targets := append([]string(nil), f.promptTargets...)
	titles := make(map[string]string, len(f.sessionTitles))
	for id, title := range f.sessionTitles {
		titles[id] = title
	}
	f.mu.Unlock()
	if creates != 0 {
		t.Fatalf("creates=%d titles=%v, want 0: the stranded session must be adopted, not duplicated", creates, titles)
	}
	if prompts != 1 || len(targets) != 1 || targets[0] != "ses_stranded" {
		t.Fatalf("prompts=%d targets=%v titles=%v, want one prompt to ses_stranded", prompts, targets, titles)
	}
	if live != 1 {
		t.Fatalf("live sessions=%d, want 1: the stranded session must be reused, not leaked", live)
	}
	// The intent is settled once the mapping is durable, so a later create for
	// the same key starts from a fresh token.
	rows, err := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates WHERE workspace='workspace' AND session_key='k'`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0][0]; got != int64(0) && fmt.Sprint(got) != "0" {
		t.Fatalf("pending intents=%v, want 0 once the session mapping is durable", got)
	}
}

// A create intent is written with ON CONFLICT DO NOTHING, which reports success
// even when another writer committed first. A writer that loses that conflict
// must not create under its own discarded token, because nothing could later
// adopt or reclaim a session recorded under it. Whatever token comes back must
// therefore be the one that is actually durable in storage.
func TestPendingCreateTokenIsAlwaysTheDurableToken(t *testing.T) {
	f := newFakeOpenCode(t)
	r, err := Open(t.Context(), Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, Timeout: 30 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// The pre-fix code trusted the token it generated even when the insert lost a
	// conflict. Run two writers against one key so the loser takes that path,
	// then require that the token each one used is the durable one.
	req := testRequest("k", "req", "answer")
	tokens := make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			token, _, _, _, err := r.pendingCreateToken(context.WithoutCancel(t.Context()), req)
			tokens <- token
			errs <- err
		}()
	}
	first, second := <-tokens, <-tokens
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}

	rows, err := r.query(t.Context(), `SELECT ownership_token FROM pending_session_creates WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("intents=%d, want exactly one durable intent", len(rows))
	}
	durable := rows[0][0].(string)
	for i, token := range []string{first, second} {
		if token != "" && token != durable {
			t.Fatalf("writer %d used token %q, want the durable token %q: a discarded token strands its session", i, token, durable)
		}
	}
}
