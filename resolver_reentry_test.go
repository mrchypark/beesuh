package beesuh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/model"
)

// A credential resolver runs while Run holds the lifecycle read lock. If it
// called Connection, the nested read lock would queue behind a pending Close
// and neither operation could finish; context expiry cannot interrupt RLock.
// Runtime entry points therefore reject the reentry before taking the lock.
func TestCredentialResolverReentryDoesNotDeadlockClose(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completion(w, "done", "stop")
	}))
	t.Cleanup(backend.Close)

	insideResolver := make(chan struct{})
	release := make(chan struct{})
	var armed bool
	var armedMu sync.Mutex
	var reentryErr error
	var r *Runtime

	r = nativeOpen(t, Options{
		DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, MaxWaiting: 1,
		RequireIdentity: true,
		Authorize:       func(context.Context, Principal) error { return nil },
		ResolveCredential: func(ctx context.Context, c Connection) (Credential, error) {
			armedMu.Lock()
			if armed {
				// Signal that Run is executing, then reenter while the read lock
				// is held. Both this call and a concurrent Close must return.
				close(insideResolver)
				<-release
			}
			armedMu.Unlock()
			_, reentryErr = r.Connection(ctx, c.ID)
			return Credential{APIKey: "test-key", AccountID: c.UserID}, nil
		},
		Native: &NativeOptions{Providers: map[string]model.Config{
			"work": {BaseURL: backend.URL + "/v1", Model: "test"},
		}, DefaultProvider: "work"},
	})

	if err := r.RegisterConnection(context.Background(), Connection{
		ID: "conn", UserID: "u", WorkspaceID: "w", Kind: "provider",
		TargetID: "work", CredentialRef: "acct", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	ctx := WithPrincipal(context.Background(), Principal{UserID: "u", WorkspaceID: "w"})
	armedMu.Lock()
	armed = true
	armedMu.Unlock()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r",
			ConnectionID: "conn", Prompt: Prompt{Text: "hi"}})
	}()

	<-insideResolver
	// Close now blocks on the lifecycle write lock, exactly as in the reported
	// schedule, so the resolver's nested read lock cannot be granted.
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		_ = r.Close()
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	waitFor(t, runDone, "Run did not return")
	waitFor(t, closeDone, "Close did not return")

	if !errors.Is(reentryErr, ErrResolverReentry) {
		t.Fatalf("resolver reentry error = %v, want ErrResolverReentry", reentryErr)
	}
}

func waitFor(t *testing.T, ch chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}
