package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/httpapi"
	"github.com/mrchypark/beesuh/model"
)

// Local protocol fixtures prove the two independent gates, not live registration.
func TestSharedServiceDiscoveryKeepsModelConsentAndWorkspaceOwnership(t *testing.T) {
	var consentRevoked atomic.Bool
	var deliveries, dispatches, metadata atomic.Int32
	resource := "https://accounts.invalid/connections"
	// Repeated consent reads describe the same grant until explicitly revoked.
	grantExpires := time.Now().Add(time.Hour).UnixMilli()
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/auth/v1/") {
			metadata.Add(1)
		}
		switch r.URL.Path {
		case "/introspect":
			json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "user", "client_id": "operable", "aud": []string{"explicit-api-audience", resource}, "exp": time.Now().Add(time.Hour).Unix(), "scope": "openid execute goauthy.connections.read goauthy.connections.use"})
		case "/userinfo":
			json.NewEncoder(w).Encode(map[string]string{"sub": "user"})
		case "/auth/v1/connection-collections":
			fmt.Fprint(w, `[{"id":"models","enabled":true,"auth_method":"api_key"}]`)
		case "/auth/v1/connections/models":
			fmt.Fprint(w, `[{"id":"mine","collection_id":"models","owner_subject":"user","state":"ready"}]`)
		case "/auth/v1/connections/models/mine":
			fmt.Fprint(w, `{"id":"mine","collection_id":"models","owner_subject":"user","state":"ready"}`)
		case "/auth/v1/connections/models/mine/grants/approved":
			json.NewEncoder(w).Encode(map[string]any{"connection_generation": "gen", "grant": map[string]any{"id": "approved", "owner_subject": "user", "collection_id": "models", "connection_id": "mine", "consumer_client_id": "operable", "mode": "credential_delivery", "resource": resource, "generation": "gen", "consumer_generation": "consumer-gen", "provider_id": "model-provider", "connector_digest": "digest", "revision": 1, "provider_revision": 1, "expires_at_unix_ms": grantExpires, "revoked": consentRevoked.Load()}})
		case "/auth/v1/connection-grants/approved/credential":
			body, err := io.ReadAll(r.Body)
			if err != nil || r.Method != http.MethodPost || len(body) != 0 || r.Header.Get("Authorization") != "Bearer opaque-human-token" {
				t.Error("delivery requires bodyless POST with original consumer human Bearer")
			}
			deliveries.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": "model-only-fixture", "grant_id": "approved", "provider_id": "model-provider", "connection_generation": "gen", "credential_version": 1, "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": time.Now().Add(time.Hour).UnixMilli()})
		default:
			t.Errorf("unexpected issuer route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		var payload struct {
			Model string `json:"model"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/inference/go/openai/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != "minimax-m3.1-flash" {
			t.Error("incorrect fixed OpenCode model dispatch")
		}
		if r.Header.Get("Authorization") != "Bearer model-only-fixture" {
			t.Error("human or app credential reached model")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	secret := filepath.Join(t.TempDir(), "oidc-secret")
	if err := os.WriteFile(secret, []byte("fixture-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &serviceConfig{Issuer: issuer.URL, IntrospectionURL: issuer.URL + "/introspect", UserInfoURL: issuer.URL + "/userinfo", ClientID: "operable", ClientSecretFile: secret, Audience: "explicit-api-audience", RequiredScopes: []string{"execute"}, Goauthy: &serviceDiscovery{Resource: resource, Providers: map[string]string{"model-provider": "p"}}, client: issuer.Client(), providerClient: provider.Client()}
	sharedConfig := sharedTestConfig(t)
	for i := range sharedConfig.Contexts {
		delete(sharedConfig.Contexts[i].Operations, "run_cancel")
	}
	delete(sharedConfig.Contexts[1].Operations, "run_execute")
	setSharedTestConfig(t, s, sharedConfig)
	opts := beesuh.Options{DataDir: t.TempDir(), BackendID: "shared-discovery-fixture", MaxConcurrent: 1, MaxWaiting: 1, Timeout: 10 * time.Second, Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {Kind: "openai-compatible", API: "chat-completions", BaseURL: provider.URL + "/inference/go/openai/v1", Model: "minimax-m3.1-flash"}}}}
	if err := s.configure(&opts); err != nil {
		t.Fatal(err)
	}
	check := &sharedChecker{}
	for _, binding := range s.shared.bindings {
		binding.policy = check
	}
	runtime, err := beesuh.Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	h, err := httpapi.NewAuthenticated(runtime, s.authenticate)
	if err != nil {
		t.Fatal(err)
	}
	h = s.discoveryHandler(runtime, h)
	request := func(method, path, body, workspace string) *httptest.ResponseRecorder {
		req := sharedTestRequest(method, path, body, workspace)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		return out
	}
	selection := `{"collection_id":"models","connection_id":"mine","grant_id":"approved"}`
	for _, path := range []string{"/v1/chat", "/v1/connections"} {
		req := sharedTestRequest("POST", path, `{"workspace_id":"work-a"}`, "work-a")
		req.Header.Del("Content-Type")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if out.Code != 403 || len(check.calls) != 0 || metadata.Load() != 0 || deliveries.Load() != 0 || dispatches.Load() != 0 {
			t.Fatalf("missing Content-Type reached policy, Goauthy or model: %s status=%d", path, out.Code)
		}
	}
	var connections []beesuh.Connection
	for _, workspace := range []string{"work-a", "work-b"} {
		out := request("POST", "/v1/connections", selection, workspace)
		if out.Code != 201 {
			t.Fatalf("selection %s: %d %s", workspace, out.Code, out.Body.String())
		}
		var c beesuh.Connection
		if err := json.Unmarshal(out.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		if c.OrganizationID != "org" || c.WorkspaceID != workspace || c.UserID != serviceUserID(s.Issuer, "user") {
			t.Fatalf("stored binding: %+v", c)
		}
		connections = append(connections, c)
	}
	if connections[0].ID == connections[1].ID || deliveries.Load() != 0 {
		t.Fatal("workspace IDs collided or selection fetched model keys")
	}
	chat := func(workspace, connection, id string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"workspace_id": workspace, "session_key": "s", "request_id": id, "connection_id": connection, "message": "hi"})
		return request("POST", "/v1/chat", string(body), workspace)
	}
	// Missing pins deny locally, even when the fixture policy would allow them.
	beforeMissing, checksBeforeMissing := metadata.Load(), len(check.calls)
	if out := request("POST", "/v1/runs/r/cancel?workspace_id=work-a", "", "work-a"); out.Code != 403 {
		t.Fatalf("unconfigured cancel: %d %s", out.Code, out.Body.String())
	}
	if metadata.Load() != beforeMissing || len(check.calls) != checksBeforeMissing || deliveries.Load() != 0 || dispatches.Load() != 0 {
		t.Fatal("missing operation reached policy, Goauthy or model")
	}
	if out := chat("work-b", connections[1].ID, "missing-run-pin"); out.Code != 403 {
		t.Fatalf("unconfigured run: %d %s", out.Code, out.Body.String())
	}
	if metadata.Load() != beforeMissing || len(check.calls) != checksBeforeMissing || deliveries.Load() != 0 || dispatches.Load() != 0 {
		t.Fatal("missing run pin reached policy, Goauthy or model")
	}
	if out := chat("work-a", connections[1].ID, "foreign"); out.Code != 404 {
		t.Fatalf("foreign workspace: %d %s", out.Code, out.Body.String())
	}
	if deliveries.Load() != 0 || dispatches.Load() != 0 {
		t.Fatal("foreign workspace fetched credentials or dispatched")
	}
	if out := chat("work-a", connections[0].ID, "allowed"); out.Code != 200 {
		t.Fatalf("allowed run: %d %s (metadata=%d deliveries=%d dispatches=%d policy_checks=%d)", out.Code, out.Body.String(), metadata.Load(), deliveries.Load(), dispatches.Load(), len(check.calls))
	}
	if deliveries.Load() != 1 || dispatches.Load() != 1 {
		t.Fatalf("expected one owner credential dispatch: deliveries=%d dispatches=%d", deliveries.Load(), dispatches.Load())
	}
	consentRevoked.Store(true)
	if out := chat("work-a", connections[0].ID, "consent-revoked"); out.Code != 403 {
		t.Fatalf("Grant replaced model consent: %d %s", out.Code, out.Body.String())
	}
	consentRevoked.Store(false)
	check.deny = true
	before := metadata.Load()
	if out := chat("work-a", connections[0].ID, "policy-denied"); out.Code != 403 {
		t.Fatalf("policy denial: %d %s", out.Code, out.Body.String())
	}
	if metadata.Load() != before || deliveries.Load() != 1 || dispatches.Load() != 1 {
		t.Fatal("policy denial reached Goauthy or model")
	}
}
