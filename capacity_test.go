package beesuh

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mrchypark/beesuh/model"
)

func openCapacityRuntime(t *testing.T, capacity CapacityLimits) *Runtime {
	t.Helper()
	r, err := Open(context.Background(), Options{DataDir: t.TempDir(), BackendID: "capacity", MaxConcurrent: 2, Capacity: capacity, Native: &NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "test"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRunCapacityReservationIsAtomic(t *testing.T) {
	r := openCapacityRuntime(t, CapacityLimits{RunsPerWorkspace: 1, RunsGlobal: 1})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- r.reserveRun(context.Background(), Request{WorkspaceID: "w", RequestID: id})
		}(id)
	}
	wg.Wait()
	close(errs)
	var admitted, refused int
	for err := range errs {
		if err == nil {
			admitted++
			continue
		}
		var capacity *CapacityError
		if errors.As(err, &capacity) {
			refused++
			continue
		}
		t.Fatal(err)
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted=%d refused=%d", admitted, refused)
	}
}

func TestPendingCreateCapacityDoesNotWriteAnIntent(t *testing.T) {
	r := openCapacityRuntime(t, CapacityLimits{PendingCreatesPerWorkspace: 1, PendingCreatesGlobal: 1})
	first := Request{WorkspaceID: "w", SessionKey: "one"}
	if err := r.reservePendingCreate(context.Background(), first, "one"); err != nil {
		t.Fatal(err)
	}
	err := r.reservePendingCreate(context.Background(), Request{WorkspaceID: "w", SessionKey: "two"}, "two")
	var capacity *CapacityError
	if !errors.As(err, &capacity) {
		t.Fatalf("err=%v", err)
	}
	rows, err := r.query(context.Background(), `SELECT session_key FROM pending_session_creates WHERE workspace='w'`)
	if err != nil || len(rows) != 1 || rows[0][0] != "one" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestCapacityDefaultsAndNegativeValues(t *testing.T) {
	r := openCapacityRuntime(t, CapacityLimits{})
	if got := r.CapacityLimits(); got.RunsPerWorkspace != 10000 || got.RunsGlobal != 100000 || got.SessionsPerWorkspace != 1000 || got.SessionsGlobal != 10000 || got.PendingCreatesPerWorkspace != 1000 || got.PendingCreatesGlobal != 10000 {
		t.Fatalf("defaults=%+v", got)
	}
	_, err := Open(context.Background(), Options{DataDir: t.TempDir(), BackendID: "bad", MaxConcurrent: 1, Capacity: CapacityLimits{RunsGlobal: -1}})
	if err == nil {
		t.Fatal("negative capacity accepted")
	}
}
