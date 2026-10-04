package beesuh

import (
	"context"
	"errors"
	"testing"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

func recoveryIndependenceLimits(n int) CapacityLimits {
	return CapacityLimits{RunsPerWorkspace: n, RunsGlobal: n, SessionsPerWorkspace: n, SessionsGlobal: n, PendingCreatesPerWorkspace: n, PendingCreatesGlobal: n}
}

func recoveryIndependenceRuntime(t *testing.T, f *fakeOpenCode, dir string, limits CapacityLimits) *Runtime {
	t.Helper()
	r, err := Open(t.Context(), Options{
		DataDir: dir, BackendID: "test", MaxConcurrent: 1, Timeout: reclaimTimeout,
		Capacity: limits,
		OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecoverSettlesSessionIntentsWhenRunRecoveryRemainsUncertain(t *testing.T) {
	for _, test := range []struct {
		name      string
		seedRun   func(*testing.T, *Runtime, *fakeOpenCode) string
		wantAbort int
	}{
		{
			name: "orphan running row",
			seedRun: func(t *testing.T, r *Runtime, _ *fakeOpenCode) string {
				insertOrphanedRunningRun(t, r, Request{WorkspaceID: "run", SessionKey: "orphan", RequestID: "stuck", Prompt: Prompt{Text: "ignored"}})
				return "run"
			},
		},
		{
			name: "active run abort failure",
			seedRun: func(t *testing.T, r *Runtime, f *fakeOpenCode) string {
				req := Request{WorkspaceID: "run", SessionKey: "active", RequestID: "stuck", Prompt: Prompt{Text: "ignored"}}
				id := reserveRunningRun(t, r, req)
				f.mu.Lock()
				f.abortFail = map[string]bool{id: true}
				f.mu.Unlock()
				return "run"
			},
			wantAbort: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeOpenCode(t)
			dir := t.TempDir()
			r := recoveryIndependenceRuntime(t, f, dir, recoveryIndependenceLimits(4))
			seedCreateIntent(t, r, "adopt", "token-adopt")
			seedCleanupIntent(t, r, "cleanup", "ses_cleanup")
			f.mu.Lock()
			f.sessionTitles["ses_adopt"] = pendingCreateTitle("token-adopt")
			f.sessionTitles["ses_cleanup"] = "created-before-crash"
			f.mu.Unlock()
			workspace := test.seedRun(t, r, f)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}

			// Existing durable work must settle without using a newly-admitted slot
			// after restart under a lower capacity.
			r = recoveryIndependenceRuntime(t, f, dir, recoveryIndependenceLimits(1))
			defer r.Close()
			f.mu.Lock()
			createsBefore, promptsBefore, deletesBefore := f.creates, f.prompts, f.deletes
			f.mu.Unlock()
			err := r.Recover(context.Background())
			if !errors.Is(err, ErrUncertain) {
				t.Fatalf("Recover = %v, want unresolved run error", err)
			}
			if !r.runFenceHolds(workspace) {
				t.Fatal("failed run recovery cleared its workspace fence")
			}
			assertRecoveredIndependentIntents(t, r, f, createsBefore, promptsBefore, deletesBefore)

			// The settled intents cannot generate another request on a retry. The
			// failed active run is deliberately retried, so only create/prompt/delete
			// counts are invariant here.
			err = r.Recover(context.Background())
			if !errors.Is(err, ErrUncertain) {
				t.Fatalf("second Recover = %v, want unresolved run error", err)
			}
			assertRecoveredIndependentIntents(t, r, f, createsBefore, promptsBefore, deletesBefore)
			_, _, aborts := f.counts()
			if aborts != test.wantAbort*2 {
				t.Fatalf("aborts=%d, want %d", aborts, test.wantAbort*2)
			}
		})
	}
}

func assertRecoveredIndependentIntents(t *testing.T, r *Runtime, f *fakeOpenCode, createsBefore, promptsBefore, deletesBefore int) {
	t.Helper()
	rows, err := r.query(t.Context(), `SELECT session_id FROM sessions WHERE workspace='w' AND session_key='adopt'`)
	if err != nil || len(rows) != 1 || rows[0][0] != "ses_adopt" {
		t.Fatalf("adopted mapping=%v err=%v", rows, err)
	}
	rows, err = r.query(t.Context(), `SELECT (SELECT COUNT(*) FROM pending_session_creates), (SELECT COUNT(*) FROM capacity_run_reservations), (SELECT COUNT(*) FROM runs)`)
	if err != nil || len(rows) != 1 || rows[0][0] != int64(0) || rows[0][1] != int64(0) || rows[0][2] != int64(1) {
		t.Fatalf("intents/reservations/runs=%v err=%v", rows, err)
	}
	f.mu.Lock()
	creates, prompts, deletes := f.creates, f.prompts, f.deletes
	f.mu.Unlock()
	if creates != createsBefore || prompts != promptsBefore || deletes != deletesBefore+1 {
		t.Fatalf("recovery calls creates=%d prompts=%d deletes=%d; want %d,%d,%d", creates, prompts, deletes, createsBefore, promptsBefore, deletesBefore+1)
	}
}

func TestRecoverJoinsRunAndSessionRecoveryFailures(t *testing.T) {
	f := newFakeOpenCode(t)
	r := recoveryIndependenceRuntime(t, f, t.TempDir(), recoveryIndependenceLimits(3))
	defer r.Close()
	req := Request{WorkspaceID: "run", SessionKey: "active", RequestID: "stuck", Prompt: Prompt{Text: "ignored"}}
	id := reserveRunningRun(t, r, req)
	f.mu.Lock()
	f.abortFail = map[string]bool{id: true}
	f.mu.Unlock()
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `INSERT INTO pending_session_creates(workspace,session_key,ownership_token,backend,directory,profile,phase,remote_id,recorded_at) VALUES('w','incompatible','token','other','/workspace',?,'creating','',1)`, Args: []any{r.profile}}}); err != nil {
		t.Fatal(err)
	}

	err := r.Recover(context.Background())
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, ErrConflict) {
		t.Fatalf("Recover = %v, want joined ErrUncertain and ErrConflict", err)
	}
	if !r.runFenceHolds("run") {
		t.Fatal("run fence cleared after failed abort")
	}
	rows, queryErr := r.query(t.Context(), `SELECT COUNT(*) FROM pending_session_creates WHERE workspace='w' AND session_key='incompatible'`)
	if queryErr != nil || rows[0][0] != int64(1) {
		t.Fatalf("incompatible intent=%v err=%v", rows, queryErr)
	}
}
