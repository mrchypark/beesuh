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

// Every public entry point that takes the lifecycle lock must reject a context
// supplied to a credential resolver before it attempts that lock. Holding the
// write lock makes a missing early check deterministic: the call blocks on the
// old implementation, while the guarded implementation returns immediately.
func TestResolverReentryIsRejectedBeforeLifecycleLock(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Runtime, context.Context) error
	}{
		{
			name: "run",
			call: func(r *Runtime, ctx context.Context) error {
				_, err := r.Run(ctx, Request{})
				return err
			},
		},
		{
			name: "recover",
			call: func(r *Runtime, ctx context.Context) error {
				return r.Recover(ctx)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// closed makes the old implementation return safely after the test
			// releases its lock, without needing a database fixture.
			r := &Runtime{closed: true}
			r.life.Lock()
			locked := true
			unlock := func() {
				if locked {
					r.life.Unlock()
					locked = false
				}
			}
			defer unlock()

			ctx := context.WithValue(context.Background(), resolverReentryKey{}, true)
			done := make(chan error, 1)
			go func() { done <- test.call(r, ctx) }()

			select {
			case err := <-done:
				if !errors.Is(err, ErrResolverReentry) {
					t.Fatalf("reentry error = %v, want ErrResolverReentry", err)
				}
			case <-time.After(time.Second):
				// Let the old implementation return ErrClosed so this test never
				// leaves a goroutine blocked on the lifecycle lock after failing.
				unlock()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("reentry remained blocked after releasing lifecycle lock")
				}
				t.Fatal("reentry attempted the lifecycle lock instead of rejecting immediately")
			}
		})
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
