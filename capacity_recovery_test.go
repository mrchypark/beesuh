package beesuh

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/rhiza"
)

func TestReopenReclaimsPreDispatchRunReservation(t *testing.T) {
	ctx := context.Background()
	opts := Options{DataDir: t.TempDir(), BackendID: "capacity-recovery", MaxConcurrent: 1,
		Capacity: CapacityLimits{RunsPerWorkspace: 1, RunsGlobal: 1},
		Native:   &NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "fixture"}},
	}
	r := nativeOpen(t, opts)
	req := Request{WorkspaceID: "w", RequestID: "before-crash"}
	if err := r.reserveRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := r.reservePendingCreate(ctx, Request{WorkspaceID: "w", SessionKey: "remote-create"}, "ownership-token"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.reserveRun(ctx, req); err != nil {
		t.Fatalf("retry reserved request after restart: %v", err)
	}
	rows, err := r.query(ctx, `SELECT ownership_token FROM pending_session_creates WHERE workspace='w' AND session_key='remote-create'`)
	if err != nil || len(rows) != 1 || rows[0][0] != "ownership-token" {
		t.Fatalf("remote creation evidence lost: rows=%v error=%v", rows, err)
	}
}

func TestPendingMappingConsumesReservationAtomically(t *testing.T) {
	r := openCapacityRuntime(t, CapacityLimits{SessionsPerWorkspace: 2, SessionsGlobal: 2, PendingCreatesPerWorkspace: 2, PendingCreatesGlobal: 2})
	req := Request{WorkspaceID: "w", SessionKey: "s"}
	if err := r.reservePendingCreate(t.Context(), req, "token"); err != nil {
		t.Fatal(err)
	}
	if err := r.insertSession(t.Context(), req, "ses_1", true); err != nil {
		t.Fatal(err)
	}
	rows, err := r.query(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions), (SELECT COUNT(*) FROM pending_session_creates)`)
	if err != nil || len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != int64(0) {
		t.Fatalf("mapping and reservation rows=%v err=%v", rows, err)
	}

	// A failed batch must leave the recovery intent intact and no mapping.
	req2 := Request{WorkspaceID: "other", SessionKey: "other"}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_pending_mapping BEFORE INSERT ON sessions WHEN NEW.session_key='other' BEGIN SELECT RAISE(ABORT, 'mapping fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	if err := r.reservePendingCreate(t.Context(), req2, "token-2"); err != nil {
		t.Fatal(err)
	}
	if err := r.insertSession(t.Context(), req2, "ses_2", true); err == nil {
		t.Fatal("mapping succeeded despite trigger")
	}
	rows, err = r.query(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions), (SELECT COUNT(*) FROM pending_session_creates WHERE workspace='other' AND session_key='other')`)
	if err != nil || len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != int64(1) {
		t.Fatalf("failed mapping lost recovery intent: rows=%v err=%v", rows, err)
	}
}

func TestFailedRunReservationDoesNotPoisonLiveRuntime(t *testing.T) {
	f := newFakeOpenCode(t)
	r := capacityRuntime(t, f, t.TempDir(), CapacityLimits{RunsPerWorkspace: 3, RunsGlobal: 3})
	defer r.Close()
	req := testRequest("first", "same", "one")
	r.db = &faultingRuntimeDB{DB: r.db, executeFault: func(req rhiza.ExecuteRequest) bool {
		return len(req.Statements) == 1 && strings.Contains(req.Statements[0].SQL, "INSERT INTO capacity_run_reservations")
	}}
	if _, err := r.Run(t.Context(), req); !errors.Is(err, errPostCommitAcknowledgementLoss) {
		t.Fatalf("run error=%v, want post-commit acknowledgement loss", err)
	}
	creates, prompts, _ := f.counts()
	if creates != 0 || prompts != 0 {
		t.Fatalf("failed admission called remote: creates=%d prompts=%d", creates, prompts)
	}
	if _, err := r.Run(t.Context(), req); err != nil {
		t.Fatalf("same request remained poisoned: %v", err)
	}
	if _, err := r.Run(t.Context(), testRequest("second", "other", "two")); err != nil {
		var capacity *CapacityError
		if errors.As(err, &capacity) {
			t.Fatalf("stale reservation blocked unrelated request: %v", err)
		}
		t.Fatal(err)
	}
}

func TestCommittedPendingMappingErrorConsumesReservation(t *testing.T) {
	data := t.TempDir()
	opts := Options{DataDir: data, BackendID: "capacity-mapping", MaxConcurrent: 1,
		Capacity: CapacityLimits{SessionsPerWorkspace: 2, SessionsGlobal: 2, PendingCreatesPerWorkspace: 2, PendingCreatesGlobal: 2},
		Native:   &NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "fixture"}}}
	r := nativeOpen(t, opts)
	first := Request{WorkspaceID: "w", SessionKey: "first"}
	if err := r.reservePendingCreate(t.Context(), first, "first-token"); err != nil {
		t.Fatal(err)
	}
	r.db = &faultingRuntimeDB{DB: r.db, executeFault: func(req rhiza.ExecuteRequest) bool {
		return len(req.Statements) == 2 && req.Statements[0].SQL == `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES(?,?,?,?,?,?)`
	}}
	if err := r.insertSession(t.Context(), first, "ses_first", true); !errors.Is(err, errPostCommitAcknowledgementLoss) {
		t.Fatalf("insert error=%v, want post-commit acknowledgement loss", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	defer r.Close()
	rows, err := r.query(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions WHERE workspace='w' AND session_key='first'), (SELECT COUNT(*) FROM pending_session_creates WHERE workspace='w' AND session_key='first')`)
	if err != nil || len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != int64(0) {
		t.Fatalf("committed mapping rows=%v err=%v", rows, err)
	}
	if err := r.reservePendingCreate(t.Context(), Request{WorkspaceID: "w", SessionKey: "second"}, "second-token"); err != nil {
		t.Fatalf("remaining session slot was not available: %v", err)
	}
}

func TestMappingCleanupPersistsPhaseBeforeRemoteDelete(t *testing.T) {
	f := newFakeOpenCode(t)
	r := capacityRuntime(t, f, t.TempDir(), CapacityLimits{})
	defer r.Close()
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{
		{SQL: `CREATE TRIGGER reject_mapping BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'mapping fault'); END`},
		{SQL: `CREATE TRIGGER reject_cleanup_phase BEFORE UPDATE OF phase ON pending_session_creates BEGIN SELECT RAISE(ABORT, 'cleanup phase fault'); END`},
	}); err != nil {
		t.Fatal(err)
	}
	req := testRequest("session", "request", "one")
	if _, err := r.Run(t.Context(), req); err == nil {
		t.Fatal("run succeeded despite rejected mapping and cleanup phase")
	}
	creates, _, _ := f.counts()
	deletes := f.deletesSnapshot()
	if creates != 1 || deletes != 0 {
		t.Fatalf("creates=%d deletes=%d, want one create and no delete before durable cleanup phase", creates, deletes)
	}
	rows, err := r.query(t.Context(), `SELECT phase,remote_id FROM pending_session_creates WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil || len(rows) != 1 || rows[0][0] != pendingPhaseCreating || rows[0][1] != "" {
		t.Fatalf("intent after rejected cleanup phase=%v err=%v", rows, err)
	}
}
