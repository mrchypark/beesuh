package beesuh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/rhiza"
)

// A durable operation must report the same public category before and after a
// replay through the real HTTP path. Reconstructing the error from its text
// alone collapsed 429/503/504 into 502 and lost errors.Is.
func TestReplayedFailureKeepsItsPublicCategory(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota","type":"rate_limit"}}`))
	}))
	t.Cleanup(server.Close)

	r := nativeOpen(t, Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1,
		Native: &NativeOptions{Provider: model.Config{BaseURL: server.URL + "/v1", Model: "test"}}})
	ctx := context.Background()
	req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", Prompt: Prompt{Text: "hi"}}

	first, firstErr := r.Run(ctx, req)
	if firstErr == nil {
		t.Fatal("expected an upstream failure")
	}
	if first.FailureCategory != FailureRateLimited {
		t.Fatalf("initial category = %q, want %q", first.FailureCategory, FailureRateLimited)
	}
	if first.Error == "" || first.Error == firstErr.Error() {
		t.Fatalf("stored error is not a safe message: %q", first.Error)
	}
	if !errors.Is(firstErr, auth.ErrRateLimited) {
		t.Fatalf("initial error %v does not match %v", firstErr, auth.ErrRateLimited)
	}

	second, secondErr := r.Run(ctx, req)
	if second.FailureCategory != first.FailureCategory {
		t.Fatalf("replay category = %q, want %q", second.FailureCategory, first.FailureCategory)
	}
	if !errors.Is(secondErr, auth.ErrRateLimited) {
		t.Fatalf("replay error %v does not match %v", secondErr, auth.ErrRateLimited)
	}
	if PublicFailureMessage(FailureCategory(secondErr)) != "service is rate limited" {
		t.Fatalf("replay public message = %q, want %q", PublicFailureMessage(FailureCategory(secondErr)), "service is rate limited")
	}
	if calls != 1 {
		t.Fatalf("upstream called %d times, want exactly 1", calls)
	}
}

// Every category must survive the durable hand-off, not just the one the HTTP
// adapter can produce. This is the unit-level half of the replay contract.
func TestReplayedFailureKeepsEveryCategory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream error
		category string
		is       error
	}{
		{"rate limited", auth.ErrRateLimited, FailureRateLimited, auth.ErrRateLimited},
		{"unavailable", auth.ErrUnavailable, FailureServiceUnavailable, auth.ErrUnavailable},
		{"deadline", context.DeadlineExceeded, FailureDeadlineExceeded, context.DeadlineExceeded},
		{"overloaded", ErrOverloaded, FailureOverloaded, ErrOverloaded},
		{"storage capacity", &CapacityError{Resource: "runs", Limit: 1}, FailureStorageCapacity, ErrCapacity},
		{"connection", ErrConnectionUnavailable, FailureConnectionUnavailable, ErrConnectionUnavailable},
		{"not found", ErrNotFound, FailureNotFound, ErrNotFound},
		{"conflict", ErrConflict, FailureRequestConflict, ErrConflict},
		{"unauthenticated", ErrUnauthenticated, FailureUnauthorized, ErrUnauthenticated},
		{"forbidden", ErrForbidden, FailureForbidden, ErrForbidden},
		{"invalid input", ErrInvalidInput, FailureInvalidInput, ErrInvalidInput},
		{"unclassified", errors.New("boom"), FailureUpstream, ErrUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureCategoryFor(tc.upstream); got != tc.category {
				t.Fatalf("category = %q, want %q", got, tc.category)
			}
			var stored Result
			setFailure(&stored, tc.upstream)
			if stored.FailureCategory != tc.category {
				t.Fatalf("stored category = %q, want %q", stored.FailureCategory, tc.category)
			}
			if stored.Error != publicMessages[tc.category] {
				t.Fatalf("stored error = %q, want the safe public message", stored.Error)
			}
			if stored.Error == tc.upstream.Error() {
				t.Fatal("stored error leaks the reviewed diagnostic text")
			}

			replayed := replayFailure(stored.FailureCategory, stored.Error)
			if !errors.Is(replayed, tc.is) {
				t.Fatalf("replay error %v does not match %v", replayed, tc.is)
			}
			// The transport reads the category, not the text, so a replayed error
			// must classify identically and publish the same safe message.
			if got := FailureCategory(replayed); got != tc.category {
				t.Fatalf("replayed category = %q, want %q", got, tc.category)
			}
			if got := PublicFailureMessage(FailureCategory(replayed)); got != stored.Error {
				t.Fatalf("replayed public message = %q, want %q", got, stored.Error)
			}

			// Seed the completed failure as the first call would persist it, then
			// prove that closing Rhiza and runtime.lock preserves its category.
			dataDir := t.TempDir()
			opts := Options{DataDir: dataDir, BackendID: "replay-" + tc.category, MaxConcurrent: 1,
				Native: &NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "test"}}}
			r := nativeOpen(t, opts)
			req := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "request", Prompt: Prompt{Text: "hello"}}
			stored.RequestID, stored.SessionID, stored.State = req.RequestID, "ses_replay", "failed"
			if err := r.mutate(context.Background(), []rhiza.SQLStatement{
				{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id,active_request) VALUES(?,?,?,?,?,?,?)`, Args: []any{req.WorkspaceID, req.SessionKey, opts.BackendID, "", r.profile, stored.SessionID, req.RequestID}},
				{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,?)`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, digest(req), "running"}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := r.finish(context.Background(), req.WorkspaceID, req.SessionKey, req.RequestID, stored); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r = nativeOpen(t, opts)
			t.Cleanup(func() { _ = r.Close() })
			afterReopen, afterReopenErr := r.Run(context.Background(), req)
			if !errors.Is(afterReopenErr, tc.is) || afterReopen.FailureCategory != tc.category || afterReopen.Error != stored.Error {
				t.Fatalf("after reopen = %+v, %v", afterReopen, afterReopenErr)
			}
		})
	}
}

func TestStorageFailureDoesNotPersistDiagnostic(t *testing.T) {
	const diagnostic = "/private/beesuh/rhiza.db table=storage_failures"
	dataDir := t.TempDir()
	opts := Options{DataDir: dataDir, BackendID: "storage-failure", MaxConcurrent: 1,
		Native: &NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "test"}}}
	r := nativeOpen(t, opts)
	ctx := context.Background()
	// This is the diagnostic text returned by a local Rhiza/SQL driver. It is
	// deliberately unlike the generic mutation error that Runtime.mutate wraps.
	storageErr := errors.New("rhiza: " + diagnostic)

	req := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "storage-failure", Prompt: Prompt{Text: "hello"}}
	if err := r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id,active_request) VALUES(?,?,?,?,?,?,?)`, Args: []any{req.WorkspaceID, req.SessionKey, opts.BackendID, "", r.profile, "ses_storage", req.RequestID}},
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,?)`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, digest(req), "running"}},
	}); err != nil {
		t.Fatal(err)
	}
	out := Result{RequestID: req.RequestID, SessionID: "ses_storage", State: "failed"}
	setFailure(&out, storageErr)
	if err := r.finish(ctx, req.WorkspaceID, req.SessionKey, req.RequestID, out); err != nil {
		t.Fatal(err)
	}
	rows, err := r.query(ctx, `SELECT result FROM runs WHERE workspace=? AND request_id=?`, req.WorkspaceID, req.RequestID)
	if err != nil || len(rows) != 1 || strings.Contains(rows[0][0].(string), diagnostic) {
		t.Fatalf("persisted result = %#v, %v", rows, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	t.Cleanup(func() { _ = r.Close() })
	replayed, replayErr := r.Run(ctx, req)
	if replayErr == nil || strings.Contains(replayed.Error, diagnostic) || strings.Contains(replayErr.Error(), diagnostic) {
		t.Fatalf("replayed result = %+v, error = %v", replayed, replayErr)
	}
}

// Rows written before categories existed must still replay, with a generic
// fallback rather than a parse of the stored diagnostic text.
func TestReplayFallsBackForRowsWithoutCategory(t *testing.T) {
	if err := replayFailure("", ""); !errors.Is(err, ErrUpstream) {
		t.Fatalf("empty replay error = %v, want ErrUpstream", err)
	}
	legacy := errors.New("opencode: directory=/secret/path: connection refused")
	if err := replayFailure("", legacy.Error()); err == nil || err.Error() != legacy.Error() {
		t.Fatalf("legacy replay = %v, want the stored message", err)
	}
	if err := replayFailure("not_a_category", "x"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("unknown category replay = %v, want ErrUpstream", err)
	}
}
