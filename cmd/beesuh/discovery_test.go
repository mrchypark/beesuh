package main

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestDiscoveredConnectionSelectionAndExecution(t *testing.T) {
	var deliveries, dispatches atomic.Int32
	var revoked, wrongOwner, wrongConsumer, wrongGeneration, outage atomic.Bool
	var version atomic.Int64
	var malformedGrant atomic.Int32
	version.Store(7)
	resource := "https://accounts.example/connections"
	expires := time.Now().Add(time.Hour).UnixMilli()
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if outage.Load() {
			http.Error(w, "private remote error", 503)
			return
		}
		switch r.URL.Path {
		case "/introspect":
			_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice", "client_id": "beesuh", "aud": []string{"beesuh", resource}, "exp": time.Now().Add(time.Hour).Unix(), "scope": "openid execute goauthy.connections.read goauthy.connections.use"})
		case "/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]string{"sub": "alice"})
		case "/auth/v1/connection-collections":
			_, _ = w.Write([]byte(`[{"id":"models","enabled":true,"auth_method":"api_key"}]`))
		case "/auth/v1/connections/models":
			_, _ = w.Write([]byte(`[{"id":"mine","collection_id":"models","owner_subject":"alice","state":"draft","metadata":{"unexpected":"do-not-expose"}},{"id":"other","collection_id":"models","owner_subject":"bob"}]`))
		case "/auth/v1/connections/models/mine":
			owner := "alice"
			if wrongOwner.Load() {
				owner = "bob"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "mine", "collection_id": "models", "owner_subject": owner, "state": "draft"})
		case "/auth/v1/connections/models/mine/grants/approved":
			consumer := "beesuh"
			if wrongConsumer.Load() {
				consumer = "other"
			}
			generation := "gen"
			if wrongGeneration.Load() {
				generation = "new-gen"
			}
			grant := map[string]any{"id": "approved", "owner_subject": "alice", "collection_id": "models", "connection_id": "mine", "consumer_client_id": consumer, "mode": "credential_delivery", "resource": resource, "generation": "gen", "consumer_generation": "consumer-gen", "provider_id": "opencode", "connector_digest": "digest", "revision": 1, "provider_revision": 2, "expires_at_unix_ms": expires, "revoked": revoked.Load()}
			switch malformedGrant.Load() {
			case 1:
				delete(grant, "revoked")
			case 2:
				grant["revoked"] = nil
			case 3:
				delete(grant, "connector_digest")
			case 4:
				grant["connector_digest"] = nil
			case 5:
				grant["revoked"] = "false"
			case 6:
				grant["connector_digest"] = false
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"connection_generation": generation, "grant": grant})
		case "/auth/v1/connection-grants/approved/credential":
			deliveries.Add(1)
			if r.Method != "POST" || r.ContentLength > 0 || r.Header.Get("Authorization") != "Bearer human" {
				t.Error("incorrect credential request")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": fmt.Sprintf("model-only-secret-v%d", version.Load()), "grant_id": "approved", "provider_id": "opencode", "connection_generation": "gen", "credential_version": version.Load(), "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": time.Now().Add(time.Hour).UnixMilli()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		if r.Header.Get("Authorization") != "Bearer model-only-secret-v7" {
			t.Error("incorrect model credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer provider.Close()
	secret := filepath.Join(t.TempDir(), "client-secret")
	if e := os.WriteFile(secret, []byte("service-only"), 0600); e != nil {
		t.Fatal(e)
	}
	s := &serviceConfig{Issuer: issuer.URL, IntrospectionURL: issuer.URL + "/introspect", UserInfoURL: issuer.URL + "/userinfo", ClientID: "beesuh", ClientSecretFile: secret, Audience: "beesuh", RequiredScopes: []string{"execute"}, Goauthy: &serviceDiscovery{Resource: resource, Providers: map[string]string{"opencode": "p"}}, client: issuer.Client(), providerClient: provider.Client()}
	dir := t.TempDir()
	o := beesuh.Options{DataDir: dir, BackendID: "test", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Minute, Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {BaseURL: provider.URL + "/v1", Model: "test"}}, DefaultProvider: "p"}}
	if e := s.configure(&o); e != nil {
		t.Fatal(e)
	}
	runtime, e := beesuh.Open(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Close()
	h, e := httpapi.NewAuthenticated(runtime, s.authenticate)
	if e != nil {
		t.Fatal(e)
	}
	h = s.discoveryHandler(runtime, h)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer human")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		if strings.Contains(out.Body.String(), "model-only-secret") || strings.Contains(out.Body.String(), "private remote error") {
			t.Fatal("secret/error leakage")
		}
		return out
	}
	if deliveries.Load() != 0 {
		t.Fatal("startup fetched credentials")
	}
	options := request("GET", "/v1/connection-options", "")
	if options.Code != 200 || !strings.Contains(options.Body.String(), "mine") || strings.Contains(options.Body.String(), "bob") || strings.Contains(options.Body.String(), "do-not-expose") {
		t.Fatalf("unsafe options: %d %s", options.Code, options.Body.String())
	}
	body := `{"collection_id":"models","connection_id":"mine","grant_id":"approved"}`
	selection := request("POST", "/v1/connections", body)
	if selection.Code != 201 {
		t.Fatalf("selection %d %s", selection.Code, selection.Body.String())
	}
	var c beesuh.Connection
	if e = json.Unmarshal(selection.Body.Bytes(), &c); e != nil {
		t.Fatal(e)
	}
	if c.Generation != 1 || c.State != "ready" {
		t.Fatalf("not persisted connection: %+v", c)
	}
	if deliveries.Load() != 0 {
		t.Fatal("discovery/selection fetched credentials")
	}
	for mode := int32(1); mode <= 6; mode++ {
		malformedGrant.Store(mode)
		if out := request("POST", "/v1/connections", body); out.Code != 403 {
			t.Fatalf("unknown consent mode %d admitted: %d", mode, out.Code)
		}
		if deliveries.Load() != 0 {
			t.Fatal("unknown consent fetched credentials")
		}
	}
	malformedGrant.Store(0)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer human")
	p, e := s.authenticate(req)
	if e != nil {
		t.Fatal(e)
	}
	refs := discoveryRef{Collection: "models", Connection: "mine", Grant: "approved"}
	internal := c
	internal.TargetID = "p"
	internal.CredentialRef = refs.encoded()
	first, e := o.ResolveCredential(req.Context(), internal)
	if e != nil {
		t.Fatal(e)
	}
	version.Store(8)
	second, e := o.ResolveCredential(req.Context(), internal)
	if e != nil {
		t.Fatal(e)
	}
	if first.AccountID == second.AccountID || deliveries.Load() != 2 {
		t.Fatal("version not bound before dispatch")
	}
	modelRequest, _ := http.NewRequestWithContext(req.Context(), "POST", provider.URL+"/v1/chat/completions", nil)
	resp, e := first.HTTPClient.Do(modelRequest)
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if deliveries.Load() != 2 || dispatches.Load() != 1 {
		t.Fatal("snapshot fetched again")
	}
	for _, flag := range []*atomic.Bool{&wrongOwner, &wrongConsumer, &wrongGeneration, &revoked} {
		flag.Store(true)
		if out := request("POST", "/v1/connections", body); out.Code != 403 {
			t.Fatalf("bad binding admitted: %d", out.Code)
		}
		flag.Store(false)
	}
	outage.Store(true)
	if out := request("GET", "/v1/connection-options", ""); out.Code != 503 {
		t.Fatalf("outage = %d", out.Code)
	}
	outage.Store(false)
	if out := request("POST", "/v1/connections", `{"collection_id":"models","connection_id":"mine","grant_id":"approved","endpoint":"https://attacker.example"}`); out.Code != 400 {
		t.Fatalf("caller binding = %d", out.Code)
	}
	if e = runtime.SetConnectionState(beesuh.WithPrincipal(req.Context(), p), c.ID, "revoked"); e != nil {
		t.Fatal(e)
	}
	if out := request("POST", "/v1/connections", body); out.Code != 403 {
		t.Fatalf("local revocation reactivated: %d %s", out.Code, out.Body.String())
	}
	if deliveries.Load() != 2 {
		t.Fatal("denial fetched credentials")
	}
}
