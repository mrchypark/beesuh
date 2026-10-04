package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mrchypark/beesuh/model"
)

func connectionRuntime(t *testing.T, dir string, authorize func(context.Context, Principal) error) *Runtime {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(backend.Close)
	r, err := Open(context.Background(), Options{
		DataDir: dir, BackendID: "connections", MaxConcurrent: 1, MaxWaiting: 1,
		RequireIdentity: true, Authorize: authorize,
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{APIKey: "test-key", AccountID: c.UserID}, nil
		},
		Native: &NativeOptions{Providers: map[string]model.Config{
			"work": {BaseURL: backend.URL + "/v1", Model: "test"},
		}, DefaultProvider: "work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func principal(ctx context.Context, user, workspace string) context.Context {
	return WithPrincipal(ctx, Principal{UserID: user, WorkspaceID: workspace})
}

func TestConnectionsAreOwnedAndPersisted(t *testing.T) {
	dir := t.TempDir()
	r := connectionRuntime(t, dir, func(context.Context, Principal) error { return nil })
	c := Connection{ID: "conn-work", UserID: "u1", WorkspaceID: "w1", Kind: "provider", TargetID: "work", CredentialRef: "account-work", State: "ready"}
	if err := r.RegisterConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ctx := principal(context.Background(), "u1", "w1")
	got, err := r.Connection(ctx, c.ID)
	if err != nil || got.TargetID != "work" {
		t.Fatalf("Connection() = %#v, %v", got, err)
	}
	listed, err := r.Connections(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("Connections() = %#v, %v", listed, err)
	}
	b, err := json.Marshal(listed[0])
	if err != nil || containsJSONField(b, "target_id") || containsJSONField(b, "credential_ref") {
		t.Fatalf("connection JSON leaked private routing or credential reference: %s (%v)", b, err)
	}
	changed := c
	changed.CredentialRef = "account-other"
	if err := r.RegisterConnection(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("credential retarget error = %v", err)
	}
	if _, err := r.Connection(principal(context.Background(), "u2", "w1"), c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner error = %v", err)
	}
	if _, err := r.Connection(context.Background(), c.ID); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous error = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = connectionRuntime(t, dir, func(context.Context, Principal) error { return nil })
	if got, err := r.Connection(ctx, c.ID); err != nil || got.UserID != "u1" {
		t.Fatalf("persisted Connection() = %#v, %v", got, err)
	}
}

func TestConnectionStateAndAuthorization(t *testing.T) {
	inactive := false
	r := connectionRuntime(t, t.TempDir(), func(_ context.Context, _ Principal) error {
		if inactive {
			return ErrForbidden
		}
		return nil
	})
	c := Connection{ID: "conn-work", UserID: "u1", WorkspaceID: "w1", Kind: "provider", TargetID: "work", CredentialRef: "account-work", State: "needs_login"}
	if err := r.RegisterConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ctx := principal(context.Background(), "u1", "w1")
	if _, err := r.Connection(ctx, c.ID); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("needs_login error = %v", err)
	}
	if err := r.SetConnectionState(ctx, c.ID, "ready"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ownedConnection(ctx, c.ID); err != nil || got.Generation != 2 {
		t.Fatalf("generation after ready = %#v, %v", got, err)
	}
	if err := r.SetConnectionState(ctx, c.ID, "needs_login"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetConnectionState(ctx, c.ID, "ready"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ownedConnection(ctx, c.ID); err != nil || got.Generation != 4 {
		t.Fatalf("generation after state changes = %#v, %v", got, err)
	}
	if err := r.checkConnection(ctx, c.ID, "provider", "work"); err != nil {
		t.Fatal(err)
	}
	if err := r.checkConnection(ctx, c.ID, "provider", "other"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wrong target error = %v", err)
	}
	inactive = true
	if _, err := r.Connections(ctx); !errors.Is(err, ErrForbidden) {
		t.Fatalf("inactive principal error = %v", err)
	}
	inactive = false
	if err := r.SetConnectionState(ctx, c.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetConnectionState(ctx, c.ID, "ready"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("revive error = %v", err)
	}
}

func TestConnectionRevocationWinsConcurrentStateChanges(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	c := Connection{ID: "conn-work", UserID: "u1", WorkspaceID: "w1", Kind: "provider", TargetID: "work", CredentialRef: "account-work", State: "ready"}
	if err := r.RegisterConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ctx := principal(context.Background(), "u1", "w1")
	var wg sync.WaitGroup
	for _, state := range []string{"needs_login", "ready", "revoked"} {
		wg.Add(1)
		go func(state string) {
			defer wg.Done()
			_ = r.SetConnectionState(ctx, c.ID, state)
		}(state)
	}
	wg.Wait()
	if err := r.SetConnectionState(ctx, c.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetConnectionState(ctx, c.ID, "ready"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("revoked connection became ready: %v", err)
	}
}

func containsJSONField(b []byte, field string) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(b, &value) == nil && value[field] != nil
}

func TestConnectionReadinessRequiresResolvedAccount(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	r.opts.ResolveCredential = func(context.Context, Connection) (Credential, error) { return Credential{}, ErrConnectionUnavailable }
	c := Connection{ID: "waiting", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "pending-ref", State: "needs_login"}
	if err := r.RegisterConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "u", WorkspaceID: "w"})
	if err := r.SetConnectionState(ctx, c.ID, "ready"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("owner bypassed unresolved login: %v", err)
	}
	list, err := r.Connections(ctx)
	if err != nil || len(list) != 1 || list[0].State != "needs_login" {
		t.Fatalf("readiness changed without credential: %+v %v", list, err)
	}
}

func TestRegisterReadyRequiresResolvedCredential(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	r.opts.ResolveCredential = func(context.Context, Connection) (Credential, error) { return Credential{AccountID: "account"}, nil }
	c := Connection{ID: "unresolved", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "missing", State: "ready"}
	if err := r.RegisterConnection(context.Background(), c); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("registration=%v", err)
	}
	list, err := r.Connections(WithPrincipal(context.Background(), Principal{UserID: "u", WorkspaceID: "w"}))
	if err != nil || len(list) != 0 || runFencedNow(r) {
		t.Fatalf("failed registration mutated state: %+v %v blocked=%v", list, err, runFencedNow(r))
	}
}
