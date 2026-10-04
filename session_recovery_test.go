package beesuh

import (
	"context"
	"errors"
	"testing"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

func sessionRecoveryRuntime(t *testing.T, f *fakeOpenCode, dir string) *Runtime {
	return sessionRecoveryRuntimeWithCapacity(t, f, dir, sessionRecoveryLimits())
}

func sessionRecoveryLimits() CapacityLimits {
	return CapacityLimits{RunsPerWorkspace: 1, RunsGlobal: 1, SessionsPerWorkspace: 1, SessionsGlobal: 1, PendingCreatesPerWorkspace: 1, PendingCreatesGlobal: 1}
}

func sessionRecoveryRuntimeWithCapacity(t *testing.T, f *fakeOpenCode, dir string, capacity CapacityLimits) *Runtime {
	t.Helper()
	r, err := Open(t.Context(), Options{DataDir: dir, BackendID: "test", MaxConcurrent: 1, Timeout: reclaimTimeout,
		Capacity: capacity,
		OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func withRecoveryLock(t *testing.T, r *Runtime, fn func() error) error {
	t.Helper()
	r.life.Lock()
	defer r.life.Unlock()
	return fn()
}

func seedCreateIntent(t *testing.T, r *Runtime, key, token string) {
	t.Helper()
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,phase,remote_id,recorded_at) VALUES(?,?,?,?,?,?,?,'',1)`, Args: []any{"w", key, token, "test", "/workspace", r.profile, pendingPhaseCreating}}}); err != nil {
		t.Fatal(err)
	}
}

func seedCleanupIntent(t *testing.T, r *Runtime, key, remoteID string) {
	t.Helper()
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,phase,remote_id,recorded_at) VALUES(?,?,?,?,?,?,?,?,?)`, Args: []any{"w", key, "token-" + key, "test", "/workspace", r.profile, pendingPhaseCleanup, remoteID, 1}}}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverSessionCreatesAdoptsUnderFullRunCapacity(t *testing.T) {
	f := newFakeOpenCode(t)
	dir := t.TempDir()
	r := sessionRecoveryRuntimeWithCapacity(t, f, dir, CapacityLimits{RunsPerWorkspace: 2, RunsGlobal: 2, SessionsPerWorkspace: 2, SessionsGlobal: 2, PendingCreatesPerWorkspace: 2, PendingCreatesGlobal: 2})
	seedCreateIntent(t, r, "create", "token-create")
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES('w','full','other','hash','succeeded')`}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sessionTitles["ses_adopt"] = pendingCreateTitle("token-create")
	f.mu.Unlock()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// The pending create already owns its mapping slot, so recovering it stays
	// possible after restart under lower limits that block a new admission.
	r = sessionRecoveryRuntime(t, f, dir)
	defer r.Close()

	if err := r.Recover(context.Background()); err != nil {
		t.Fatalf("Recover = %v", err)
	}
	rows, err := r.query(t.Context(), `SELECT session_id FROM sessions WHERE workspace='w' AND session_key='create'`)
	if err != nil || len(rows) != 1 || rows[0][0] != "ses_adopt" {
		t.Fatalf("mapping=%v err=%v", rows, err)
	}
	for _, table := range []string{"pending_session_creates", "capacity_run_reservations"} {
		rows, err = r.query(t.Context(), `SELECT COUNT(*) FROM `+table)
		if err != nil || rows[0][0] != int64(0) {
			t.Fatalf("%s=%v err=%v", table, rows, err)
		}
	}
	runs, err := r.query(t.Context(), `SELECT COUNT(*) FROM runs WHERE workspace='w'`)
	if err != nil || runs[0][0] != int64(1) {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	f.mu.Lock()
	creates, prompts, deletes := f.creates, f.prompts, f.deletes
	f.mu.Unlock()
	if creates != 0 || prompts != 0 || deletes != 0 {
		t.Fatalf("creates=%d prompts=%d deletes=%d, want 0,0,0", creates, prompts, deletes)
	}
}

func TestRecoverSessionCreatesSettlesKnownCleanupWithoutNewWork(t *testing.T) {
	f := newFakeOpenCode(t)
	dir := t.TempDir()
	r := sessionRecoveryRuntimeWithCapacity(t, f, dir, CapacityLimits{RunsPerWorkspace: 2, RunsGlobal: 2, SessionsPerWorkspace: 2, SessionsGlobal: 2, PendingCreatesPerWorkspace: 2, PendingCreatesGlobal: 2})
	seedCleanupIntent(t, r, "cleanup", "ses_cleanup")
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES('w','full','other','hash','succeeded')`}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sessionTitles["ses_cleanup"] = "created-before-crash"
	f.mu.Unlock()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = sessionRecoveryRuntime(t, f, dir)
	defer r.Close()

	if err := r.Recover(context.Background()); err != nil {
		t.Fatalf("Recover = %v", err)
	}
	rows, err := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates`)
	if err != nil || rows[0][0] != int64(0) {
		t.Fatalf("intents=%v err=%v", rows, err)
	}
	runs, err := r.query(t.Context(), `SELECT COUNT(*) FROM runs`)
	if err != nil || runs[0][0] != int64(1) {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	f.mu.Lock()
	creates, prompts, deletes := f.creates, f.prompts, f.deletes
	f.mu.Unlock()
	if creates != 0 || prompts != 0 || deletes != 1 {
		t.Fatalf("creates=%d prompts=%d deletes=%d, want 0,0,1", creates, prompts, deletes)
	}
}

func TestRecoverSessionCreatesRetainsUnsafeIntents(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *Runtime, *fakeOpenCode)
	}{
		{name: "empty creating", setup: func(t *testing.T, r *Runtime, _ *fakeOpenCode) { seedCreateIntent(t, r, "empty", "token-empty") }},
		{name: "ambiguous creating", setup: func(t *testing.T, r *Runtime, f *fakeOpenCode) {
			seedCreateIntent(t, r, "ambiguous", "token-ambiguous")
			f.mu.Lock()
			f.sessionTitles["ses_one"] = pendingCreateTitle("token-ambiguous")
			f.sessionTitles["ses_two"] = pendingCreateTitle("token-ambiguous")
			f.mu.Unlock()
		}},
		{name: "unreadable cleanup mapping", setup: func(t *testing.T, r *Runtime, _ *fakeOpenCode) {
			seedCleanupIntent(t, r, "unreadable", "ses_keep")
			if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `ALTER TABLE sessions RENAME TO sessions_unreadable`}}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mapped cleanup target", setup: func(t *testing.T, r *Runtime, _ *fakeOpenCode) {
			seedCleanupIntent(t, r, "cleanup", "ses_keep")
			if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES('w','other','test','/workspace',?,'ses_keep')`, Args: []any{r.profile}}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeOpenCode(t)
			r := sessionRecoveryRuntime(t, f, t.TempDir())
			defer r.Close()
			test.setup(t, r, f)
			var err error
			if test.name == "unreadable cleanup mapping" {
				err = withRecoveryLock(t, r, func() error { return r.recoverSessionCreates(context.Background()) })
			} else {
				err = r.Recover(context.Background())
			}
			if !errors.Is(err, ErrUncertain) {
				t.Fatalf("recoverSessionCreates = %v, want ErrUncertain", err)
			}
			rows, queryErr := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates`)
			if queryErr != nil || rows[0][0] != int64(1) {
				t.Fatalf("intents=%v err=%v", rows, queryErr)
			}
			f.mu.Lock()
			creates, prompts, deletes := f.creates, f.prompts, f.deletes
			f.mu.Unlock()
			if creates != 0 || prompts != 0 || deletes != 0 {
				t.Fatalf("creates=%d prompts=%d deletes=%d, want 0,0,0", creates, prompts, deletes)
			}
		})
	}
}

func TestRecoverSessionCreatesRetainsIncompatibleMappedCreatingIntent(t *testing.T) {
	f := newFakeOpenCode(t)
	r := sessionRecoveryRuntime(t, f, t.TempDir())
	defer r.Close()
	seedCreateIntent(t, r, "create", "token-create")
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES('w','create','other','/workspace',?,'ses_existing')`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}

	err := r.Recover(context.Background())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Recover = %v, want ErrConflict", err)
	}
	rows, queryErr := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates WHERE workspace='w' AND session_key='create'`)
	if queryErr != nil || rows[0][0] != int64(1) {
		t.Fatalf("intent rows=%v err=%v", rows, queryErr)
	}
	f.mu.Lock()
	creates, prompts, deletes := f.creates, f.prompts, f.deletes
	f.mu.Unlock()
	if creates != 0 || prompts != 0 || deletes != 0 {
		t.Fatalf("creates=%d prompts=%d deletes=%d, want 0,0,0", creates, prompts, deletes)
	}
}

func TestRecoverSessionCreatesStopsBeforeCanceledScan(t *testing.T) {
	f := newFakeOpenCode(t)
	r := sessionRecoveryRuntime(t, f, t.TempDir())
	defer r.Close()
	seedCleanupIntent(t, r, "first", "ses_first")
	seedCleanupIntent(t, r, "second", "ses_second")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := withRecoveryLock(t, r, func() error { return r.recoverSessionCreates(ctx) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("recoverSessionCreates = %v, want context.Canceled", err)
	}
	rows, queryErr := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates`)
	if queryErr != nil || rows[0][0] != int64(2) {
		t.Fatalf("intents=%v err=%v", rows, queryErr)
	}
	f.mu.Lock()
	deletes := f.deletes
	f.mu.Unlock()
	if deletes != 0 {
		t.Fatalf("deletes=%d, want 0", deletes)
	}
}
