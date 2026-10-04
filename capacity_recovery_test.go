package beesuh

import (
	"context"
	"testing"

	"github.com/mrchypark/beesuh/model"
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
