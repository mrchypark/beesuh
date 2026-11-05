package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/grantpolicy"
	"github.com/mrchypark/beesuh/httpapi"
	"github.com/mrchypark/beesuh/model"
)

type sharedCheckCall struct {
	operation  string
	actual     grantpolicy.ActualSnapshot
	credential grantpolicy.Credential
}
type sharedChecker struct {
	calls []sharedCheckCall
	deny  bool
}

func (c *sharedChecker) Check(_ context.Context, op string, s grantpolicy.ActualSnapshot, cred grantpolicy.Credential) error {
	c.calls = append(c.calls, sharedCheckCall{op, s, cred})
	if c.deny {
		return grantpolicy.ErrDenied
	}
	return nil
}

func sharedTestConfig(t *testing.T) sharedServiceConfig {
	t.Helper()
	key := filepath.Join(t.TempDir(), "app-key")
	if err := os.WriteFile(key, []byte("fixture-app-key"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := sharedServiceConfig{GrantURL: "https://grant.invalid", AppKeyFile: key, GrantCredentialHintAudience: "registered-hint-not-oidc-audience"}
	for _, workspace := range []string{"work-a", "work-b"} {
		c := sharedContextConfig{OrganizationID: "org", WorkspaceID: workspace, ProviderID: "beesuh", PEPClientID: "pep", CatalogGeneration: 1, CatalogDigest: grantpolicy.Digest([]byte("candidate catalog")), ResourceType: "workspace", ResourceID: workspace, ResourceEvidence: grantpolicy.ResourceEvidence{Kind: "constant", CanonicalValue: workspace}, Operations: map[string]sharedCapabilityPin{}}
		for _, route := range sharedRoutes() {
			c.Operations[route.operation] = sharedCapabilityPin{CapabilityID: "beesuh." + route.operation, CapabilityVersion: 1, ContractDigest: grantpolicy.Digest([]byte(route.operation)), ActionID: "beesuh." + route.operation}
		}
		cfg.Contexts = append(cfg.Contexts, c)
	}
	return cfg
}
func setSharedTestConfig(t *testing.T, s *serviceConfig, cfg sharedServiceConfig) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Shared = data
	if err := s.configureShared(); err != nil {
		t.Fatal(err)
	}
}

// Only local test authorities are used. No token is decoded as a JWT.
func sharedOAuthService(t *testing.T) (*serviceConfig, *sharedChecker, *atomic.Bool, *atomic.Bool) {
	t.Helper()
	var active, correctClient atomic.Bool
	active.Store(true)
	correctClient.Store(true)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/userinfo" {
			json.NewEncoder(w).Encode(map[string]string{"sub": "user"})
			return
		}
		if r.URL.Path != "/introspect" {
			t.Error("unexpected OIDC path")
			http.NotFound(w, r)
			return
		}
		id := "operable"
		if !correctClient.Load() {
			id = "other-client"
		}
		json.NewEncoder(w).Encode(map[string]any{"active": active.Load(), "sub": "user", "client_id": id, "aud": []string{"explicit-api-audience"}, "exp": time.Now().Add(time.Hour).Unix(), "scope": "openid execute"})
	}))
	t.Cleanup(issuer.Close)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &serviceConfig{Issuer: issuer.URL, IntrospectionURL: issuer.URL + "/introspect", UserInfoURL: issuer.URL + "/userinfo", ClientID: "operable", ClientSecretFile: secret, Audience: "explicit-api-audience", RequiredScopes: []string{"execute"}, client: issuer.Client()}
	setSharedTestConfig(t, s, sharedTestConfig(t))
	checker := &sharedChecker{}
	for _, binding := range s.shared.bindings {
		binding.policy = checker
	}
	return s, checker, &active, &correctClient
}
func sharedTestRequest(method, path, body, workspace string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer opaque-human-token")
	req.Header.Set("X-Organization-ID", "org")
	req.Header.Set("X-Workspace-ID", workspace)
	if method == "POST" && (path == "/v1/chat" || path == "/v1/connections") {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestSharedServiceSelectorsAreNotAuthority(t *testing.T) {
	for _, name := range []string{"missing-org", "missing-workspace", "duplicate-org", "duplicate-workspace", "comma-org", "unknown-org", "unknown-workspace", "body-mismatch", "query-mismatch", "duplicate-query", "unknown-operation", "encoded-path", "wrong-client", "denied"} {
		t.Run(name, func(t *testing.T) {
			s, check, _, client := sharedOAuthService(t)
			req := sharedTestRequest("POST", "/v1/chat", `{"workspace_id":"work-a","session_key":"s","request_id":"r","message":"hi"}`, "work-a")
			switch name {
			case "missing-org":
				req.Header.Del("X-Organization-ID")
			case "missing-workspace":
				req.Header.Del("X-Workspace-ID")
			case "duplicate-org":
				req.Header.Add("X-Organization-ID", "org")
			case "duplicate-workspace":
				req.Header.Add("X-Workspace-ID", "work-a")
			case "comma-org":
				req.Header.Set("X-Organization-ID", "org,org")
			case "unknown-org":
				req.Header.Set("X-Organization-ID", "foreign")
			case "unknown-workspace":
				req.Header.Set("X-Workspace-ID", "foreign")
			case "body-mismatch":
				req.Header.Set("X-Workspace-ID", "work-b")
			case "query-mismatch":
				req = sharedTestRequest("GET", "/v1/history?workspace_id=work-b&session_key=s", "", "work-a")
			case "duplicate-query":
				req = sharedTestRequest("GET", "/v1/history?workspace_id=work-a&workspace_id=work-a&session_key=s", "", "work-a")
			case "unknown-operation":
				req = sharedTestRequest("GET", "/v1/status", "", "work-a")
			case "encoded-path":
				req = sharedTestRequest("GET", "/v1/runs/%72?workspace_id=work-a", "", "work-a")
			case "wrong-client":
				client.Store(false)
			case "denied":
				check.deny = true
			}
			if _, err := s.authenticate(req); err == nil {
				t.Fatal("untrusted selector or denied policy authorized")
			}
			if name != "denied" && len(check.calls) != 0 {
				t.Fatalf("invalid binding reached policy: %d", len(check.calls))
			}
		})
	}
}

func TestSharedServiceSnapshotIsImmutableAndRechecks(t *testing.T) {
	s, check, active, _ := sharedOAuthService(t)
	body := `{"workspace_id":"work-a","session_key":"s","request_id":"r","message":"hi"}`
	req := sharedTestRequest("POST", "/v1/chat", body, "work-a")
	p, err := s.authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.OrganizationID != "org" || p.WorkspaceID != "work-a" || p.UserID != serviceUserID(s.Issuer, "user") {
		t.Fatalf("binding: %+v", p)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil || string(restored) != body {
		t.Fatal("body not restored")
	}
	initial := check.calls[0]
	if initial.actual.ForwardedHeaderDigest != "sha256:787da76e548aa96cf8897406353227899e0d1d455289cd6a313002c995ef8f57" {
		t.Fatalf("snapshot did not bind exact selector header bytes: %s", initial.actual.ForwardedHeaderDigest)
	}
	if initial.actual.BodyDigest != grantpolicy.Digest([]byte(body)) || initial.actual.BodyLength != int64(len(body)) || initial.credential.Subject != "user" || initial.credential.Audience != s.shared.hintAudience || initial.credential.Authorization != "Bearer opaque-human-token" {
		t.Fatalf("incorrect snapshot: %+v", initial)
	}
	req.Method = "DELETE"
	req.URL.Path = "/v1/connections/foreign"
	req.URL.RawQuery = "workspace_id=foreign"
	req.Header.Set("X-Workspace-ID", "foreign")
	req.Header.Set("Content-Type", "text/plain")
	req.Body = io.NopCloser(strings.NewReader("changed"))
	ctx := context.WithoutCancel(req.Context())
	if err := s.authorize(ctx, p); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initial, check.calls[1]) {
		t.Fatal("reauthorization used mutated HTTP request")
	}
	forged := p
	forged.OrganizationID = "foreign"
	if err := s.authorize(ctx, forged); !errors.Is(err, beesuh.ErrForbidden) {
		t.Fatalf("forged org: %v", err)
	}
	check.deny = true
	if err := s.authorize(ctx, p); !errors.Is(err, beesuh.ErrForbidden) {
		t.Fatalf("stale policy allowed: %v", err)
	}
	check.deny = false
	active.Store(false)
	if err := s.authorize(ctx, p); err == nil {
		t.Fatal("revoked human allowed")
	}
}

func TestSharedSnapshotSelectorHeaderDigest(t *testing.T) {
	req := sharedTestRequest("GET", "/v1/providers", "", "work-a")
	_, base, err := sharedSnapshot(req, "operable", "cypark-default", nil)
	if err != nil || base.ForwardedHeaderDigest != "sha256:818f1c996cee0ceea1860229107f8d7ccc367853d0e5c6d822962790a9705363" {
		t.Fatalf("candidate owner header preimage: %s %v", base.ForwardedHeaderDigest, err)
	}
	req.Header.Set("Authorization", "Bearer another-human-token")
	req.Header.Set("X-Organization-ID", "raw-selector-is-not-the-binding")
	req.Header.Set("X-Workspace-ID", "raw-selector-is-not-the-binding")
	req.Header.Set("X-Unrelated", "excluded")
	req.Header.Set("Content-Type", "text/plain")
	_, same, err := sharedSnapshot(req, "operable", "cypark-default", nil)
	if err != nil || same.ForwardedHeaderDigest != base.ForwardedHeaderDigest {
		t.Fatal("raw request headers or Authorization changed the server-binding digest")
	}
	for _, binding := range [][2]string{{"another-org", "cypark-default"}, {"operable", "another-workspace"}} {
		_, changed, err := sharedSnapshot(req, binding[0], binding[1], nil)
		if err != nil || changed.ForwardedHeaderDigest == base.ForwardedHeaderDigest {
			t.Fatalf("server selector change was not bound: %v %v", binding, err)
		}
	}
}

func TestSharedSnapshotBindsExactJSONContentType(t *testing.T) {
	for _, route := range []struct{ path, body string }{
		{"/v1/chat", `{"workspace_id":"work-a","message":"hi"}`},
		{"/v1/connections", `{"collection_id":"models","connection_id":"mine","grant_id":"approved"}`},
	} {
		for _, tc := range []struct{ value, digest string }{
			{"application/json", "sha256:787da76e548aa96cf8897406353227899e0d1d455289cd6a313002c995ef8f57"},
			{"application/json; charset=utf-8", "sha256:6d4fc4f94e118fd7787962b7780b0295babdf856d2419b0b31e36c91ca2ec937"},
		} {
			req := sharedTestRequest("POST", route.path, route.body, "work-a")
			req.Header.Set("Content-Type", tc.value)
			_, actual, err := sharedSnapshot(req, "org", "work-a", nil)
			if err != nil || actual.ForwardedHeaderDigest != tc.digest {
				t.Fatalf("%s Content-Type %q: %s %v", route.path, tc.value, actual.ForwardedHeaderDigest, err)
			}
		}
		req := sharedTestRequest("POST", route.path, route.body, "work-a")
		value := `Application/JSON; profile="a,b"`
		req.Header.Set("Content-Type", value)
		_, actual, err := sharedSnapshot(req, "org", "work-a", nil)
		sum := sha256.Sum256([]byte("content-type:" + value + "\nx-organization-id:org\nx-workspace-id:work-a\n"))
		if err != nil || actual.ForwardedHeaderDigest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("exact mixed-case/quoted parameter value was not retained: %s %v", actual.ForwardedHeaderDigest, err)
		}
	}
}

func TestSharedServiceRejectsUnsafeContentTypeBeforePolicy(t *testing.T) {
	for _, path := range []string{"/v1/chat", "/v1/connections"} {
		for _, headers := range []http.Header{
			{},
			{"Content-Type": {""}},
			{"Content-Type": {"application/json", "application/json"}},
			{"Content-Type": {"application/json"}, "content-type": {"application/json"}},
			{"Content-Type": {"application/json, application/json"}},
			{"Content-Type": {"text/plain"}},
			{"Content-Type": {"application/json; charset"}},
			{"Content-Type": {"application/json\r\nx-injected: yes"}},
			{"Content-Type": {"application/json\x00"}},
		} {
			s, check, _, _ := sharedOAuthService(t)
			req := sharedTestRequest("POST", path, `{"workspace_id":"work-a"}`, "work-a")
			req.Header.Del("Content-Type")
			for key, values := range headers {
				req.Header[key] = values
			}
			if _, err := s.authenticate(req); !errors.Is(err, beesuh.ErrForbidden) || len(check.calls) != 0 {
				t.Fatalf("unsafe Content-Type reached policy on %s: %v checks=%d", path, err, len(check.calls))
			}
		}
	}
}

func TestSharedServiceRoutesAndSnapshotBounds(t *testing.T) {
	for _, route := range sharedRoutes() {
		path := strings.ReplaceAll(strings.ReplaceAll(route.path, "{request_id}", "r"), "{connection_id}", "c")
		query := ""
		body := ""
		switch route.operation {
		case "run_execute":
			body = `{"workspace_id":"work-a"}`
		case "history_read":
			query = "?workspace_id=work-a&session_key=hello+world"
		case "run_read", "run_cancel":
			query = "?workspace_id=work-a"
		}
		req := sharedTestRequest(route.method, path+query, body, "work-a")
		op, snap, err := sharedSnapshot(req, "org", "work-a", nil)
		if err != nil || op != route.operation {
			t.Fatalf("route %s: %s %v", route.operation, op, err)
		}
		if route.operation == "history_read" && snap.CanonicalQuery != "session_key=hello%20world&workspace_id=work-a" {
			t.Fatalf("query: %s", snap.CanonicalQuery)
		}
	}
	req := sharedTestRequest("POST", "/v1/chat", strings.Repeat("x", sharedBodyLimit+1), "work-a")
	if _, _, err := sharedSnapshot(req, "org", "work-a", nil); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
	longToken := "Bearer " + strings.Repeat("a", 1024)
	if _, ok := singleSharedHeader(http.Header{"Authorization": []string{longToken}}, "Authorization"); !ok {
		t.Fatal("bounded long Bearer rejected")
	}
}

func TestSharedServiceRejectsIncompleteConfiguration(t *testing.T) {
	for _, name := range []string{"null", "empty", "no-context", "no-operations", "duplicate-context", "multiple-orgs", "missing-hint", "missing-key", "missing-pin", "unknown-operation"} {
		t.Run(name, func(t *testing.T) {
			cfg := sharedTestConfig(t)
			switch name {
			case "no-context":
				cfg.Contexts = nil
			case "no-operations":
				cfg.Contexts[0].Operations = nil
			case "duplicate-context":
				cfg.Contexts = append(cfg.Contexts, cfg.Contexts[0])
			case "multiple-orgs":
				cfg.Contexts[1].OrganizationID = "foreign"
			case "missing-hint":
				cfg.GrantCredentialHintAudience = ""
			case "missing-key":
				cfg.AppKeyFile = "/nonexistent/fixture-app-key"
			case "missing-pin":
				c := cfg.Contexts[0].Operations["run_read"]
				c.ContractDigest = ""
				cfg.Contexts[0].Operations["run_read"] = c
			case "unknown-operation":
				delete(cfg.Contexts[0].Operations, "run_read")
				cfg.Contexts[0].Operations["arbitrary_action"] = sharedCapabilityPin{}
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if name == "null" {
				data = []byte("null")
			}
			if name == "empty" {
				data = []byte("{}")
			}
			s := &serviceConfig{Shared: data}
			if err := s.configureShared(); err == nil || s.shared != nil {
				t.Fatal("incomplete shared config fell back or initialized")
			}
		})
	}
}

func TestSharedServiceAllowsKnownOperationSubset(t *testing.T) {
	s, _, _, _ := sharedOAuthService(t)
	cfg := sharedTestConfig(t)
	for i := range cfg.Contexts {
		pin := cfg.Contexts[i].Operations["run_execute"]
		cfg.Contexts[i].Operations = map[string]sharedCapabilityPin{"run_execute": pin}
	}
	setSharedTestConfig(t, s, cfg)
	check := &sharedChecker{}
	for _, binding := range s.shared.bindings {
		binding.policy = check
	}
	req := sharedTestRequest("POST", "/v1/chat", `{"workspace_id":"work-a","message":"hi"}`, "work-a")
	if _, err := s.authenticate(req); err != nil || len(check.calls) != 1 {
		t.Fatalf("configured operation refused: %v checks=%d", err, len(check.calls))
	}
	req = sharedTestRequest("POST", "/v1/runs/r/cancel?workspace_id=work-a", "", "work-a")
	if _, err := s.authenticate(req); !errors.Is(err, beesuh.ErrForbidden) || len(check.calls) != 1 {
		t.Fatalf("missing operation reached policy: %v checks=%d", err, len(check.calls))
	}
}

func TestSharedDiscoveredIdentityPreservesPersonalIDs(t *testing.T) {
	ref := "ref"
	p := beesuh.Principal{UserID: "u", WorkspaceID: "w"}
	b, _ := json.Marshal([]string{p.UserID, p.WorkspaceID, ref})
	h := sha256.Sum256(b)
	if discoveredID(p, ref) != "ga_"+hex.EncodeToString(h[:]) {
		t.Fatal("personal discovered ID changed")
	}
	seen := map[string]bool{discoveredID(p, ref): true}
	for _, org := range []string{"a", "b"} {
		p.OrganizationID = org
		id := discoveredID(p, ref)
		if seen[id] {
			t.Fatal("organization ID collision")
		}
		seen[id] = true
	}
	s, check, _, _ := sharedOAuthService(t)
	req := sharedTestRequest("POST", "/v1/chat", `{"workspace_id":"work-a"}`, "work-a")
	principal, err := s.authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.discoveryPrincipal(req.Context(), "user"); err != nil || got != principal {
		t.Fatalf("discovery principal: %+v %v", got, err)
	}
	if _, err := s.discoveryPrincipal(req.Context(), "other"); err == nil {
		t.Fatal("foreign owner mapped")
	}
	for _, change := range []string{"organization", "workspace", "user"} {
		c := beesuh.Connection{OrganizationID: principal.OrganizationID, UserID: principal.UserID, WorkspaceID: principal.WorkspaceID, CredentialRef: "invalid"}
		switch change {
		case "organization":
			c.OrganizationID = "foreign"
		case "workspace":
			c.WorkspaceID = "foreign"
		case "user":
			c.UserID = "foreign"
		}
		before := len(check.calls)
		if _, err := s.resolveDiscovered(req.Context(), c); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("foreign connection resolved: %v", err)
		}
		if len(check.calls) != before {
			t.Fatal("foreign connection reached policy or Goauthy")
		}
	}
}

func TestSharedServiceHTTPIsolationUsesRuntimeChecks(t *testing.T) {
	s, check, _, _ := sharedOAuthService(t)
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected model dispatch") }))
	defer backend.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{DataDir: t.TempDir(), BackendID: "shared-fixture", MaxConcurrent: 1, MaxWaiting: 1, RequireIdentity: true, Authorize: s.authorize, ResolveCredential: func(context.Context, beesuh.Connection) (beesuh.Credential, error) {
		t.Error("unexpected credential resolution")
		return beesuh.Credential{}, beesuh.ErrForbidden
	}, Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {BaseURL: backend.URL, Model: "test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handler, err := httpapi.NewAuthenticated(runtime, s.authenticate)
	if err != nil {
		t.Fatal(err)
	}
	req := sharedTestRequest("GET", "/v1/history?workspace_id=work-a&session_key=s", "", "work-a")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 200 || len(check.calls) < 2 {
		t.Fatalf("runtime did not recheck: %d calls=%d", out.Code, len(check.calls))
	}
	check.deny = true
	req = sharedTestRequest("POST", "/v1/runs/r/cancel?workspace_id=work-a", "", "work-a")
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != 403 {
		t.Fatalf("denied cancel: %d", out.Code)
	}
}

// This is a protocol fixture, not a real Grant/learner registration proof.
func TestSharedServiceRealClientFreshNonceAndDenial(t *testing.T) {
	s, _, _, _ := sharedOAuthService(t)
	var mu sync.Mutex
	var nonces []string
	var denied atomic.Bool
	pdp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/http-capabilities/decide" || r.Header.Get("Authorization") != "Bearer opaque-human-token" || r.Header.Get("X-Organization-ID") != "org" || r.Header.Get("X-Grant-App-Auth-Key") != "fixture-app-key" {
			t.Error("incorrect policy request")
			w.WriteHeader(403)
			return
		}
		if denied.Load() {
			w.WriteHeader(503)
			return
		}
		var in map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			w.WriteHeader(400)
			return
		}
		var nonce string
		json.Unmarshal(in["requestBinding"], &nonce)
		mu.Lock()
		nonces = append(nonces, nonce)
		mu.Unlock()
		var credential map[string]json.RawMessage
		json.Unmarshal(in["providerCredential"], &credential)
		delete(in, "providerCredential")
		delete(in, "resourceEvidence")
		delete(in, "canonicalQuery")
		in["providerSubject"] = credential["subject"]
		in["providerAudience"] = credential["audience"]
		set := func(k string, v any) { in[k], _ = json.Marshal(v) }
		set("allowed", true)
		set("reason", "fixture_allow")
		set("principalDigest", strings.Repeat("a", 64))
		set("actionId", "beesuh.run_execute")
		set("issuedAt", time.Now().Add(-time.Millisecond))
		set("expiresAt", time.Now().Add(time.Second))
		json.NewEncoder(w).Encode(in)
	}))
	defer pdp.Close()
	cfg := sharedTestConfig(t)
	binding := cfg.Contexts[0]
	pin := binding.Operations["run_execute"]
	client, err := grantpolicy.New(grantpolicy.Config{BaseURL: pdp.URL, AllowPrivateHTTP: true, AppKeyFile: cfg.AppKeyFile, OrganizationID: "org", ProviderID: binding.ProviderID, PEPClientID: binding.PEPClientID, CatalogGeneration: binding.CatalogGeneration, CatalogDigest: binding.CatalogDigest, Operations: map[string]grantpolicy.Binding{"run_execute": {CapabilityID: pin.CapabilityID, CapabilityVersion: pin.CapabilityVersion, ContractDigest: pin.ContractDigest, ActionID: pin.ActionID, ResourceType: binding.ResourceType, ResourceID: binding.ResourceID, ResourceEvidence: binding.ResourceEvidence}}})
	if err != nil {
		t.Fatal(err)
	}
	s.shared.bindings[sharedBindingKey{"org", "work-a"}].policy = client
	req := sharedTestRequest("POST", "/v1/chat", `{"workspace_id":"work-a","message":"first"}`, "work-a")
	p, err := s.authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.authorize(req.Context(), p); err != nil {
		t.Fatal(err)
	}
	req2 := sharedTestRequest("POST", "/v1/chat", `{"workspace_id":"work-a","message":"different body"}`, "work-a")
	if _, err := s.authenticate(req2); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(nonces) != 3 || nonces[0] == nonces[1] || nonces[1] == nonces[2] {
		t.Errorf("fresh decisions missing: %v", nonces)
	}
	mu.Unlock()
	denied.Store(true)
	if err := s.authorize(req.Context(), p); !errors.Is(err, beesuh.ErrForbidden) {
		t.Fatalf("PDP outage allowed: %v", err)
	}
}

func TestSharedForwardHeaderConfiguration(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `"x-test"`, `[1]`, `["Authorization"]`, `["authorization"]`, `["x-b","x-a"]`, `["x-a","x-a"]`, `[""]`, `["x bad"]`, `["connection"]`, `["cookie"]`} {
		t.Run(raw, func(t *testing.T) {
			cfg := sharedTestConfig(t)
			pin := cfg.Contexts[0].Operations["run_execute"]
			pin.ForwardHeaders = json.RawMessage(raw)
			cfg.Contexts[0].Operations["run_execute"] = pin
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			s := &serviceConfig{Shared: data}
			if s.configureShared() == nil {
				t.Fatal("invalid header metadata accepted")
			}
		})
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`[]`), json.RawMessage(`["content-type","x-organization-id"]`)} {
		names, ok := sharedForwardHeaders(raw)
		if !ok || (names == nil) != (raw == nil) {
			t.Fatalf("metadata distinction lost: %s %#v", raw, names)
		}
	}
}

func TestSharedExplicitHeaderSnapshot(t *testing.T) {
	body := `{"workspace_id":"work-a","message":"hi"}`
	req := sharedTestRequest("POST", "/v1/chat", body, "work-a")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-Test", " raw\tvalue ")
	req.Header.Set("X-Unknown", "ignored")
	names := []string{"content-type", "x-absent", "x-organization-id", "x-test"}
	_, snap, err := sharedSnapshot(req, "org", "work-a", names)
	want := "sha256:cd02ddd40739d5e00e8c73c37c3bad9d0602ad3af676258fe2fe8dcbae73a978"
	if err != nil || snap.ForwardedHeaderDigest != want || snap.BodyDigest != grantpolicy.Digest([]byte(body)) {
		t.Fatalf("explicit preimage: %+v %v", snap, err)
	}
	_, empty, err := sharedSnapshot(req, "org", "work-a", []string{})
	if err != nil || empty.ForwardedHeaderDigest != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("empty metadata did not hash empty preimage")
	}
	for _, values := range [][]string{{"a", "b"}, {"bad\rvalue"}, {"bad\x00value"}, {string([]byte{255})}} {
		req.Header["X-Test"] = values
		if _, _, err := sharedSnapshot(req, "org", "work-a", names); err == nil {
			t.Fatalf("unsafe values accepted: %q", values)
		}
	}
}

func TestSharedImplicitWorkspaceRequiresExplicitConstantContract(t *testing.T) {
	for _, scenario := range []string{"allowed", "empty-list", "present-valid", "legacy", "multiple", "declared-workspace", "wrong-resource", "wrong-type", "wrong-evidence", "named-evidence", "wrong-org", "no-org", "empty", "duplicate", "wrong-workspace", "body-mismatch", "query-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _, _ := sharedOAuthService(t)
			cfg := sharedTestConfig(t)
			cfg.Contexts = cfg.Contexts[:1]
			for op, pin := range cfg.Contexts[0].Operations {
				pin.ForwardHeaders = json.RawMessage(`["content-type","x-organization-id"]`)
				if scenario == "empty-list" {
					pin.ForwardHeaders = json.RawMessage(`[]`)
				}
				if scenario == "legacy" {
					pin.ForwardHeaders = nil
				}
				if scenario == "declared-workspace" {
					pin.ForwardHeaders = json.RawMessage(`["x-workspace-id"]`)
				}
				cfg.Contexts[0].Operations[op] = pin
			}
			switch scenario {
			case "multiple":
				other := cfg.Contexts[0]
				other.WorkspaceID = "work-b"
				cfg.Contexts = append(cfg.Contexts, other)
			case "wrong-resource":
				cfg.Contexts[0].ResourceID = "other"
				cfg.Contexts[0].ResourceEvidence.CanonicalValue = "other"
			case "wrong-type":
				cfg.Contexts[0].ResourceType = "item"
			case "wrong-evidence":
				cfg.Contexts[0].ResourceEvidence.Kind = "queryScalar"
				cfg.Contexts[0].ResourceEvidence.Name = "workspace_id"
			case "named-evidence":
				cfg.Contexts[0].ResourceEvidence.Name = "workspace_id"
			}
			setSharedTestConfig(t, s, cfg)
			check := &sharedChecker{}
			for _, binding := range s.shared.bindings {
				binding.policy = check
			}
			body := `{"workspace_id":"work-a","message":"hi"}`
			if scenario == "body-mismatch" {
				body = `{"workspace_id":"work-b","message":"hi"}`
			}
			req := sharedTestRequest("POST", "/v1/chat", body, "work-a")
			req.Header.Del("X-Workspace-ID")
			switch scenario {
			case "present-valid":
				req.Header.Set("X-Workspace-ID", "work-a")
			case "wrong-org":
				req.Header.Set("X-Organization-ID", "other")
			case "no-org":
				req.Header.Del("X-Organization-ID")
			case "empty":
				req.Header.Set("X-Workspace-ID", "")
			case "duplicate":
				req.Header["X-Workspace-ID"] = []string{"work-a", "work-a"}
			case "wrong-workspace":
				req.Header.Set("X-Workspace-ID", "work-b")
			case "query-mismatch":
				req = sharedTestRequest("GET", "/v1/runs/r?workspace_id=work-b", "", "work-a")
				req.Header.Del("X-Workspace-ID")
			}
			beforeHeaders := req.Header.Clone()
			beforeURI, beforeURL := req.RequestURI, req.URL.String()
			p, err := s.authenticate(req)
			allowed := scenario == "allowed" || scenario == "empty-list" || scenario == "present-valid"
			if allowed {
				if err != nil || p.WorkspaceID != "work-a" || len(check.calls) != 1 {
					t.Fatalf("constant selection failed: %+v %v", p, err)
				}
			} else if err == nil || len(check.calls) != 0 {
				t.Fatalf("unsafe selection reached policy: %v calls=%d", err, len(check.calls))
			}
			if !reflect.DeepEqual(beforeHeaders, req.Header) || req.RequestURI != beforeURI || req.URL.String() != beforeURL {
				t.Fatal("request metadata changed")
			}
			got, readErr := io.ReadAll(req.Body)
			if readErr != nil || (scenario != "query-mismatch" && string(got) != body) {
				t.Fatal("body changed")
			}
		})
	}
}
