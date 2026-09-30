package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/model"
)

func TestAuthenticatedServiceScopesConversationsAndConnections(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer backend.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "identity-test", RequireIdentity: true,
		Authorize: func(context.Context, beesuh.Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c beesuh.Connection) (beesuh.Credential, error) {
			return beesuh.Credential{APIKey: "test-key", AccountID: c.UserID}, nil
		},
		MaxConcurrent: 2, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"team": {BaseURL: backend.URL, Model: "test"}}, DefaultProvider: "team"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for _, connection := range []beesuh.Connection{
		{ID: "alice-model", UserID: "alice", WorkspaceID: "work", Kind: "provider", TargetID: "team", CredentialRef: "account-alice", State: "ready"},
		{ID: "bob-model", UserID: "bob", WorkspaceID: "work", Kind: "provider", TargetID: "team", CredentialRef: "account-bob", State: "ready"},
	} {
		if err := runtime.RegisterConnection(context.Background(), connection); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(runtime, "operator"); err == nil {
		t.Fatal("legacy API accepted an identity runtime")
	}
	auth := func(r *http.Request) (beesuh.Principal, error) {
		switch r.Header.Get("Authorization") {
		case "Bearer alice":
			return beesuh.Principal{UserID: "alice", WorkspaceID: "work"}, nil
		case "Bearer bob":
			return beesuh.Principal{UserID: "bob", WorkspaceID: "work"}, nil
		case "Bearer revoked":
			return beesuh.Principal{}, beesuh.ErrUnauthenticated
		default:
			return beesuh.Principal{}, errors.New("upstream token validation details must not leak")
		}
	}
	h, err := NewAuthenticated(runtime, auth)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()

	// Same user-visible workspace, session, and request IDs must remain isolated
	// by the authenticated user identity added by the runtime.
	alice := chat(t, server.URL, "alice", `{"workspace_id":"work","session_key":"same","request_id":"same","connection_id":"alice-model","message":"hello"}`)
	bob := chat(t, server.URL, "bob", `{"workspace_id":"work","session_key":"same","request_id":"same","connection_id":"bob-model","message":"hello"}`)
	if alice.status != http.StatusOK || bob.status != http.StatusOK {
		t.Fatalf("runs: alice=%d %s bob=%d %s", alice.status, alice.body, bob.status, bob.body)
	}
	if calls.Load() != 2 {
		t.Fatalf("identity-scoped request was replayed across users (calls=%d; alice=%s bob=%s)", calls.Load(), alice.body, bob.body)
	}
	if missing := chat(t, server.URL, "alice", `{"workspace_id":"work","session_key":"none","request_id":"none","message":"hello"}`); missing.status != http.StatusConflict {
		t.Fatalf("connection required = %d: %s", missing.status, missing.body)
	}
	if mismatch := chat(t, server.URL, "alice", `{"workspace_id":"other","session_key":"none","request_id":"other","connection_id":"alice-model","message":"hello"}`); mismatch.status != http.StatusForbidden {
		t.Fatalf("workspace mismatch = %d: %s", mismatch.status, mismatch.body)
	}
	if stolen := chat(t, server.URL, "alice", `{"workspace_id":"work","session_key":"none","request_id":"stolen","connection_id":"bob-model","message":"hello"}`); stolen.status != http.StatusNotFound && stolen.status != http.StatusForbidden {
		t.Fatalf("foreign connection = %d: %s", stolen.status, stolen.body)
	}
	if denied := chat(t, server.URL, "revoked", `{"workspace_id":"work","session_key":"none","request_id":"denied","connection_id":"alice-model","message":"hello"}`); denied.status != http.StatusUnauthorized || strings.Contains(string(denied.body), "upstream") {
		t.Fatalf("revoked service session = %d: %s", denied.status, denied.body)
	}
	if status := api(t, http.MethodGet, server.URL+"/v1/status", "alice", ""); status.status != http.StatusForbidden {
		t.Fatalf("status = %d: %s", status.status, status.body)
	}
	connections := api(t, http.MethodGet, server.URL+"/v1/connections", "alice", "")
	if connections.status != http.StatusOK || !strings.Contains(string(connections.body), "alice-model") || strings.Contains(string(connections.body), "bob-model") {
		t.Fatalf("connections = %d: %s", connections.status, connections.body)
	}
	revoke := api(t, http.MethodDelete, server.URL+"/v1/connections/alice-model", "alice", "")
	if revoke.status != http.StatusOK {
		t.Fatalf("revoke = %d: %s", revoke.status, revoke.body)
	}
	if unavailable := chat(t, server.URL, "alice", `{"workspace_id":"work","session_key":"after","request_id":"after","connection_id":"alice-model","message":"hello"}`); unavailable.status != http.StatusConflict {
		t.Fatalf("revoked connection = %d: %s", unavailable.status, unavailable.body)
	}
}
