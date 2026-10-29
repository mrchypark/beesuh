package beesuh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

func TestInitStorageReopen(t *testing.T) {
	ctx := context.Background()
	data := filepath.Join(t.TempDir(), "new")
	if err := InitStorage(ctx, data); err != nil {
		t.Fatal(err)
	}
	db, lock, err := openStorage(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(); _ = lock.Close() })
	// Init must not create application tables.
	rows, err := db.Query(ctx, rhiza.QueryRequest{SQL: "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('sessions', 'runs')", Consistency: rhiza.ConsistencyLinearizable})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 0 {
		t.Fatalf("init ran application migrations: %v", rows.Rows)
	}
	if err := errors.Join(db.Close(), lock.Close()); err != nil {
		t.Fatal(err)
	}
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, data, 1)
	defer r.Close()
	if err := InitStorage(ctx, data); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("init while runtime open: %v", err)
	}
	want, err := r.Run(ctx, testRequest("agent", "one", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := InitStorage(ctx, data); err != nil {
		t.Fatal(err)
	}
	r = openTestRuntime(t, f, data, 1)
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := r.Run(ctx, testRequest("agent", "one", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != want.RequestID || got.Output == nil {
		t.Fatalf("lost stored result: %+v", got)
	}
	_, prompts, _ := f.counts()
	if prompts != 1 {
		t.Fatalf("replayed %d prompts", prompts)
	}
}

func TestInitStorageLockAndFailures(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	db, lock, err := openStorage(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(); _ = lock.Close() })
	if err := InitStorage(ctx, data); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("init while init open: %v", err)
	}
	f := newFakeOpenCode(t)
	_, err = Open(ctx, Options{DataDir: data, BackendID: "test", MaxConcurrent: 1, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}})
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("runtime while init open: %v", err)
	}
	if err := errors.Join(db.Close(), lock.Close()); err != nil {
		t.Fatal(err)
	}
	if err := InitStorage(ctx, ""); err == nil {
		t.Fatal("empty directory accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitStorage(ctx, file); err == nil {
		t.Fatal("file accepted as directory")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := InitStorage(canceled, data); err == nil {
		t.Fatal("canceled open succeeded")
	}
	if err := InitStorage(ctx, data); err != nil {
		t.Fatalf("failed open leaked lock: %v", err)
	}
}
