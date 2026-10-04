package beesuh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRevocationWaitsForAdmittedDispatch(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stateAtResponse := make(chan Connection, 1)
	var r *Runtime
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		connection, err := r.ownedConnection(principal(context.Background(), "user", "workspace"), "connection")
		if err != nil {
			t.Errorf("connection while dispatch is admitted: %v", err)
			return
		}
		stateAtResponse <- connection
		completion(w, "ok", "stop")
	}))
	defer backend.Close()

	opts := nativeOptions(t.TempDir(), "", backend.URL, nil)
	opts.RequireIdentity = true
	opts.Authorize = func(context.Context, Principal) error { return nil }
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		return Credential{APIKey: "key", AccountID: c.UserID}, nil
	}
	r = nativeOpen(t, opts)
	defer r.Close()
	ctx := principal(context.Background(), "user", "workspace")
	if err := r.RegisterConnection(context.Background(), Connection{ID: "connection", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "default", CredentialRef: "credential", State: "ready"}); err != nil {
		t.Fatal(err)
	}

	dispatch := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "first", ConnectionID: "connection", Prompt: Prompt{Text: "hello"}})
		dispatch <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not reach the provider")
	}

	revoked := make(chan error, 1)
	go func() { revoked <- r.SetConnectionState(ctx, "connection", "revoked") }()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		lock := r.locks["connection\x00connection"]
		refs := 0
		if lock != nil {
			refs = lock.refs
		}
		r.mu.Unlock()
		if refs == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("revocation did not wait on the admitted dispatch lock")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)

	select {
	case connection := <-stateAtResponse:
		if connection.State != "ready" || connection.Generation != 1 {
			t.Fatalf("connection changed inside admitted dispatch: %#v", connection)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not complete")
	}
	if err := <-dispatch; err != nil {
		t.Fatalf("admitted dispatch = %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("revoke = %v", err)
	}

	_, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "after-revoke", ConnectionID: "connection", Prompt: Prompt{Text: "hello"}})
	if !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("dispatch after revocation = %v", err)
	}
}
