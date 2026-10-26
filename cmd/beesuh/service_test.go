package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/goauthy"
	"github.com/mrchypark/beesuh/httpapi"
	"github.com/mrchypark/beesuh/model"
)

type oauthFixture struct {
	s             *serviceConfig
	active        atomic.Bool
	exchanges     atomic.Int32
	introspects   atomic.Int32
	wrongSubject  atomic.Bool
	wrongAudience atomic.Bool
	wrongInfo     atomic.Bool
	expired       atomic.Bool
	omitType      atomic.Bool
	omitIssuer    atomic.Bool
	omitExpiresIn atomic.Bool
}

func newOAuthFixture(t *testing.T, exchange bool) *oauthFixture {
	t.Helper()
	f := &oauthFixture{}
	f.active.Store(true)
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/userinfo" {
			if r.Header.Get("Authorization") == "Bearer refresh-token" {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"invalid_token"}`))
				return
			}
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer browser-token" {
				t.Error("bad UserInfo request")
			}
			subject := "u"
			if f.wrongInfo.Load() {
				subject = "other"
			}
			json.NewEncoder(w).Encode(map[string]any{"sub": subject, "name": "Extra claim"})
			return
		}
		id, secret, ok := r.BasicAuth()
		if !ok || id != "beesuh" || secret != "client-secret" {
			t.Error("missing OAuth client authentication")
		}
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("not a standard form POST")
		}
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/introspect":
			f.introspects.Add(1)
			token := r.Form.Get("token")
			subject := "u"
			aud := []string{"beesuh", "model"}
			if token != "browser-token" && token != "dispatch-only-token" && token != "refresh-token" {
				t.Error("unexpected introspected token")
			}
			if token == "dispatch-only-token" {
				aud = []string{"model"}
				if f.wrongSubject.Load() {
					subject = "other"
				}
			}
			if f.wrongAudience.Load() {
				aud = []string{"other"}
			}
			expiry := time.Now().Add(time.Minute).Unix()
			if f.expired.Load() {
				expiry = time.Now().Add(-time.Minute).Unix()
			}
			claims := map[string]any{"active": f.active.Load(), "sub": subject, "aud": aud, "exp": expiry, "scope": "openid execute model.invoke", "extension": true}
			if !f.omitIssuer.Load() {
				claims["iss"] = base
			}
			if !f.omitType.Load() {
				claims["token_type"] = "Bearer"
			}
			json.NewEncoder(w).Encode(claims)
		case "/token":
			f.exchanges.Add(1)
			expectedAudience := "model"
			if f.s.Connections[0].Resource != "" {
				expectedAudience = ""
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || r.Form.Get("subject_token") != "browser-token" || r.Form.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:access_token" || r.Form.Get("requested_token_type") != "urn:ietf:params:oauth:token-type:access_token" || r.Form.Get("audience") != expectedAudience || r.Form.Get("scope") != "model.invoke" || r.Form.Get("resource") != f.s.Connections[0].Resource {
				t.Error("bad token exchange request")
			}
			out := map[string]any{"access_token": "dispatch-only-token", "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "extra": "ignored"}
			if !f.omitExpiresIn.Load() {
				out["expires_in"] = 60
			}
			json.NewEncoder(w).Encode(out)
		default:
			t.Error("custom endpoint called")
			w.WriteHeader(404)
		}
	}))
	base = server.URL
	t.Cleanup(server.Close)
	secret := filepath.Join(t.TempDir(), "client-secret")
	if e := os.WriteFile(secret, []byte("client-secret"), 0600); e != nil {
		t.Fatal(e)
	}
	f.s = &serviceConfig{Issuer: base, IntrospectionURL: base + "/introspect", UserInfoURL: base + "/userinfo", TokenURL: base + "/token", ClientID: "beesuh", ClientSecretFile: secret, Audience: "beesuh", RequiredScopes: []string{"execute"}, Connections: []serviceConnection{{ID: "c", Subject: "u", WorkspaceID: "w", Provider: "p", Audience: "model", Scopes: []string{"model.invoke"}, TokenExchange: exchange}}, client: &http.Client{Timeout: time.Second, CheckRedirect: noServiceRedirect}}
	return f
}
func TestServiceExecution(t *testing.T) {
	for _, exchange := range []bool{false, true} {
		t.Run(map[bool]string{false: "bearer", true: "exchange"}[exchange], func(t *testing.T) {
			f := newOAuthFixture(t, exchange)
			var dispatches atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dispatches.Add(1)
				want := "browser-token"
				if exchange {
					want = "dispatch-only-token"
				}
				if r.Header.Get("Authorization") != "Bearer "+want {
					t.Error("wrong model bearer")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer provider.Close()
			o := beesuh.Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Minute, Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {BaseURL: provider.URL + "/v1", Model: "test"}}, DefaultProvider: "p"}}
			if e := f.s.configure(&o); e != nil {
				t.Fatal(e)
			}
			runtime, e := beesuh.Open(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			defer runtime.Close()
			if e := runtime.RegisterConnection(context.Background(), f.s.Connections[0].connection(f.s.Issuer)); e != nil {
				t.Fatal(e)
			}
			if f.introspects.Load() != 0 || f.exchanges.Load() != 0 {
				t.Fatal("startup fetched tokens")
			}
			h, e := httpapi.NewAuthenticated(runtime, f.s.authenticate)
			if e != nil {
				t.Fatal(e)
			}
			request := func(method, path, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer browser-token")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			if w := request("GET", "/v1/connections", ""); w.Code != 200 {
				t.Fatalf("list %d %s", w.Code, w.Body)
			}
			if f.exchanges.Load() != 0 {
				t.Fatal("listing exchanged token")
			}
			body := `{"workspace_id":"other","session_key":"s","request_id":"bad","connection_id":"c","message":"hi"}`
			if w := request("POST", "/v1/chat", body); w.Code == 200 {
				t.Fatal("cross workspace allowed")
			}
			if f.exchanges.Load() != 0 || dispatches.Load() != 0 {
				t.Fatal("denial dispatched")
			}
			body = strings.ReplaceAll(strings.ReplaceAll(body, "other", "w"), "bad", "good")
			if w := request("POST", "/v1/chat", body); w.Code != 200 {
				t.Fatalf("chat %d %s", w.Code, w.Body)
			}
			want := int32(0)
			if exchange {
				want = 1
			}
			if f.exchanges.Load() != want || dispatches.Load() != 1 {
				t.Fatal("wrong dispatch count")
			}
			f.active.Store(false)
			if w := request("POST", "/v1/chat", strings.ReplaceAll(body, "good", "revoked")); w.Code == 200 {
				t.Fatal("revoked token allowed")
			}
			if dispatches.Load() != 1 {
				t.Fatal("revoked token dispatched")
			}
		})
	}
}
func TestServiceAuthValidation(t *testing.T) {
	for _, name := range []string{"audience", "expiry", "userinfo", "scope", "issuer", "unmapped"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthFixture(t, true)
			switch name {
			case "audience":
				f.wrongAudience.Store(true)
			case "expiry":
				f.expired.Store(true)
			case "userinfo":
				f.wrongInfo.Store(true)
			case "scope":
				f.s.RequiredScopes = []string{"missing"}
			case "issuer":
				f.s.Issuer = "https://other.example"
			case "unmapped":
				f.s.Connections[0].Subject = "other"
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer browser-token")
			if _, e := f.s.authenticate(r); e == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
func TestServiceDispatchBoundary(t *testing.T) {
	for _, name := range []string{"destination", "missing context", "cross user", "revoked", "wrong exchange subject", "wrong audience"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthFixture(t, true)
			r := httptest.NewRequest("POST", "https://model.example/v1/chat/completions", nil)
			r.Header.Set("Authorization", "Bearer browser-token")
			p, e := f.s.authenticate(r)
			if e != nil {
				t.Fatal(e)
			}
			*r = *r.WithContext(beesuh.WithPrincipal(r.Context(), p))
			tr := serviceTransport{s: f.s, binding: f.s.Connections[0], endpoint: r.URL.String()}
			before := f.introspects.Load()
			switch name {
			case "destination":
				tr.endpoint = "https://else.example/"
			case "missing context":
				*r = *r.WithContext(context.Background())
			case "cross user":
				tr.binding.Subject = "other"
			case "revoked":
				f.active.Store(false)
			case "wrong exchange subject":
				f.wrongSubject.Store(true)
			case "wrong audience":
				f.wrongAudience.Store(true)
			}
			if _, e := tr.RoundTrip(r); e == nil {
				t.Fatal("unsafe dispatch accepted")
			}
			if name == "destination" && (f.exchanges.Load() != 0 || f.introspects.Load() != before) {
				t.Fatal("destination checked too late")
			}
			if name != "wrong exchange subject" && f.exchanges.Load() != 0 {
				t.Fatal("exchange before authorization")
			}
		})
	}
}
func TestServiceRevalidatesQueuedContext(t *testing.T) {
	f := newOAuthFixture(t, true)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer browser-token")
	p, e := f.s.authenticate(r)
	if e != nil {
		t.Fatal(e)
	}
	// The scheduler preserves the authenticated context; revocation must be seen
	// when a previously admitted request reaches execution.
	ctx := context.WithoutCancel(r.Context())
	f.active.Store(false)
	if e := f.s.authorize(ctx, p); e == nil {
		t.Fatal("stale queued authority accepted")
	}
	f.active.Store(true)
	if e := f.s.authorize(ctx, beesuh.Principal{UserID: "other", WorkspaceID: "w"}); e == nil {
		t.Fatal("token reused for another owner")
	}
	if e := f.s.authorize(context.Background(), p); e == nil {
		t.Fatal("restart without token accepted")
	}
}
func TestServiceOAuthErrors(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		want   error
	}{{"invalid_client", 401, auth.ErrUnavailable}, {"invalid_grant", 400, beesuh.ErrUnauthenticated}, {"invalid_token", 401, beesuh.ErrUnauthenticated}, {"invalid_target", 400, auth.ErrForbidden}, {"unknown", 500, auth.ErrUnavailable}, {"slow_down", 429, auth.ErrRateLimited}} {
		t.Run(tc.code, func(t *testing.T) {
			f := newOAuthFixture(t, true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]string{"error": tc.code, "error_description": "sensitive secret"})
			}))
			defer server.Close()
			var out any
			e := f.s.oauth(context.Background(), server.URL, url.Values{"token": {"browser-token"}}, "", &out)
			if !errors.Is(e, tc.want) || strings.Contains(e.Error(), "sensitive") {
				t.Fatalf("unsafe error %v", e)
			}
		})
	}
}
func TestServiceEndpoints(t *testing.T) {
	for _, s := range []string{"http://example.com", "https://u:p@example.com", "https://example.com?key=x", "http://localhost:80"} {
		if secureEndpoint(s) {
			t.Errorf("allowed %s", s)
		}
	}
}
func TestServiceJSONLimit(t *testing.T) {
	for _, input := range []string{`{"allowed":true}` + strings.Repeat(" ", 100) + "garbage", `{"allowed":true} {}`, `{"unexpected":true}`} {
		var out struct {
			Allowed bool `json:"allowed"`
		}
		if decodeServiceJSON(strings.NewReader(input), 64, &out) == nil {
			t.Fatal("accepted invalid config")
		}
	}
}

func TestServiceIssuerIsolation(t *testing.T) {
	f := newOAuthFixture(t, false)
	original := f.s.Connections[0].connection(f.s.Issuer)
	oldID := original.UserID
	if oldID == "u" || oldID != serviceUserID(f.s.Issuer, "u") {
		t.Fatal("identity is not issuer namespaced")
	}
	f.s.Issuer = "https://different-issuer.example"
	if next := f.s.Connections[0].connection(f.s.Issuer); next.UserID == oldID {
		t.Fatal("issuer switch reused owner")
	}
	if _, e := f.s.resolve(context.Background(), original); e == nil {
		t.Fatal("issuer switch reused prior connection")
	}
	if serviceUserID("https://a.example", "b,c") == serviceUserID("https://a.example,b", "c") {
		t.Fatal("ambiguous tuple encoding")
	}
}

func TestServiceOptionalClaimsAndAccessTokenProof(t *testing.T) {
	f := newOAuthFixture(t, false)
	f.omitIssuer.Store(true)
	f.omitType.Store(true)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer browser-token")
	if p, e := f.s.authenticate(r); e != nil || p.UserID != serviceUserID(f.s.Issuer, "u") {
		t.Fatalf("valid access token without optional claims rejected: %v", e)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer refresh-token")
	if _, e := f.s.authenticate(r); e == nil {
		t.Fatal("refresh token accepted via introspection hint")
	}
	f.s.UserInfoURL = ""
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer browser-token")
	if _, e := f.s.authenticate(r); e == nil {
		t.Fatal("token of unknown type accepted without access-only proof")
	}
	f.omitType.Store(false)
	if _, e := f.s.authenticate(r); e == nil {
		t.Fatal("Bearer introspection without UserInfo accepted")
	}
	f.s.UserInfoURL = f.s.Issuer + "/userinfo"
	r.Header.Set("Authorization", "Bearer refresh-token")
	if _, e := f.s.authenticate(r); e == nil {
		t.Fatal("refresh token marked Bearer accepted")
	}
	if f.exchanges.Load() != 0 {
		t.Fatal("refresh token exchanged")
	}
}

func TestServiceResourceExchangeWithoutExpiresIn(t *testing.T) {
	f := newOAuthFixture(t, true)
	f.omitExpiresIn.Store(true)
	f.omitIssuer.Store(true)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dispatch-only-token" {
			t.Error("missing exchange token")
		}
		w.Write([]byte("ok"))
	}))
	defer provider.Close()
	f.s.Connections[0].Resource = provider.URL
	r := httptest.NewRequest("POST", provider.URL+"/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer browser-token")
	if _, e := f.s.authenticate(r); e != nil {
		t.Fatal(e)
	}
	tr := serviceTransport{s: f.s, binding: f.s.Connections[0], endpoint: r.URL.String()}
	resp, e := tr.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if f.exchanges.Load() != 1 {
		t.Fatal("missing resource exchange")
	}
}

func TestServiceRejectsCustomAuthorityAndAmbiguousACL(t *testing.T) {
	f := newOAuthFixture(t, false)
	for _, tc := range []struct {
		name   string
		config any
	}{
		{"custom authority", map[string]any{"authority_url": "https://accounts.example/beesuh", "connections": f.s.Connections}},
		{"ambiguous workspace", func() any {
			copy := *f.s
			other := copy.Connections[0]
			other.ID = "other"
			other.WorkspaceID = "other"
			copy.Connections = append([]serviceConnection{other}, copy.Connections...)
			return copy
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, e := json.Marshal(tc.config)
			if e != nil {
				t.Fatal(e)
			}
			filename := filepath.Join(t.TempDir(), "service.json")
			os.WriteFile(filename, b, 0600)
			if _, e := loadService(filename); e == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	b, e := json.Marshal(f.s)
	if e != nil {
		t.Fatal(e)
	}
	filename := filepath.Join(t.TempDir(), "service.json")
	os.WriteFile(filename, b, 0600)
	if _, e := loadService(filename); e != nil {
		t.Fatalf("valid configuration rejected: %v", e)
	}
	if f.introspects.Load() != 0 || f.exchanges.Load() != 0 {
		t.Fatal("configuration fetched tokens")
	}
}

func TestServiceOAuthResponseBoundaries(t *testing.T) {
	for _, body := range []string{strings.Repeat(" ", 64<<10) + `{}`, `{} {}`, `{"active":true} trailing`} {
		t.Run("invalid JSON", func(t *testing.T) {
			f := newOAuthFixture(t, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			var out any
			if e := f.s.oauth(context.Background(), server.URL, url.Values{"token": {"browser-token"}}, "", &out); !errors.Is(e, auth.ErrUnavailable) {
				t.Fatalf("accepted invalid response: %v", e)
			}
		})
	}
	f := newOAuthFixture(t, false)
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	var out any
	if e := f.s.oauth(context.Background(), redirect.URL, url.Values{"token": {"browser-token"}}, "", &out); e == nil || followed.Load() != 0 {
		t.Fatal("OAuth redirect followed")
	}
}

func TestServiceGoauthyAPIKey(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "service.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logFile, nil)))
	defer slog.SetDefault(oldLogger)
	var mode atomic.Value
	mode.Store("")
	var deliveries, models atomic.Int32
	var issuer string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := mode.Load().(string)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/introspect":
			_ = r.ParseForm()
			if id, _, ok := r.BasicAuth(); !ok || id != "beesuh" || r.Form.Get("token") != "human-token" {
				t.Error("invalid introspection request")
			}
			audiences := []string{"https://beesuh.example", "https://goauthy.example/connections"}
			client, subject, scope := "beesuh", "alice", "openid agent.execute goauthy.connections.use"
			switch mode {
			case "bee-only":
				audiences = audiences[:1]
			case "delivery-only":
				audiences = audiences[1:]
			case "consumer":
				client = "other"
			case "subject":
				subject = "other"
			case "scope":
				scope = "openid agent.execute"
			}
			json.NewEncoder(w).Encode(map[string]any{"active": mode != "revoked", "sub": subject, "client_id": client, "iss": issuer, "aud": audiences, "scope": scope, "exp": time.Now().Add(time.Minute).Unix(), "token_type": "Bearer"})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer human-token" {
				t.Error("bad UserInfo bearer")
			}
			json.NewEncoder(w).Encode(map[string]any{"sub": "alice"})
		case "/auth/v1/connection-grants/grant/credential":
			deliveries.Add(1)
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer human-token" || r.Header.Get("Cookie") != "" {
				t.Error("unsafe delivery")
			}
			if mode == "outage" {
				w.WriteHeader(503)
				w.Write([]byte("private-delivery-error"))
				return
			}
			grant := "grant"
			if mode == "binding" {
				grant = "other"
			}
			json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": "private-model-key", "grant_id": grant, "provider_id": "opencode", "connection_generation": "generation", "credential_version": 1, "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()})
		case "/v1/chat/completions":
			models.Add(1)
			if r.Header.Get("Authorization") != "Bearer private-model-key" {
				t.Error("human token forwarded to provider")
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	issuer = server.URL
	secret := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secret, []byte("client-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &serviceConfig{Issuer: issuer, IntrospectionURL: issuer + "/introspect", UserInfoURL: issuer + "/userinfo", ClientID: "beesuh", ClientSecretFile: secret, Audience: "https://beesuh.example", RequiredScopes: []string{"agent.execute"}, client: server.Client(), Connections: []serviceConnection{{ID: "personal", Subject: "alice", WorkspaceID: "team", Provider: "p", GoauthyAPIKey: &serviceGoauthyAPIKey{Resource: "https://goauthy.example/connections", Binding: goauthy.APIKeyBinding{GrantID: "grant", ProviderID: "opencode", ConnectionGeneration: "generation", ConnectorDigest: "digest", CredentialVersion: 1, Header: "Authorization", Prefix: "Bearer ", Endpoint: issuer + "/v1/chat/completions"}}}}}
	s.providerClient = server.Client()
	o := beesuh.Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Minute, Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {Kind: "opencode-go", BaseURL: issuer + "/v1", Model: "test"}}, DefaultProvider: "p"}}
	if err := s.configure(&o); err != nil {
		t.Fatal(err)
	}
	runtime, err := beesuh.Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.RegisterConnection(context.Background(), s.Connections[0].connection(issuer)); err != nil {
		t.Fatal(err)
	}
	if deliveries.Load() != 0 || models.Load() != 0 {
		t.Fatal("startup fetched credentials")
	}
	handler, err := httpapi.NewAuthenticated(runtime, s.authenticate)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer human-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		for _, secret := range []string{"private-model-key", "human-token", "client-secret"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Error("secret in HTTP response")
			}
		}
		return w
	}
	if w := request("GET", "/v1/connections", ""); w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if deliveries.Load() != 0 {
		t.Fatal("listing fetched credentials")
	}
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"workspace_id":"team","session_key":"s","request_id":"r%d","connection_id":"personal","message":"hi"}`, i)
		if w := request("POST", "/v1/chat", body); w.Code != 200 {
			t.Fatalf("execute: %d %s", w.Code, w.Body)
		}
	}
	if deliveries.Load() != 2 || models.Load() != 2 {
		t.Fatalf("calls delivery=%d model=%d", deliveries.Load(), models.Load())
	}
	credential, err := o.ResolveCredential(context.Background(), s.Connections[0].connection(issuer))
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{"bee-only", "delivery-only", "consumer", "subject", "scope", "revoked", "binding", "outage", "destination", "missing-context"} {
		t.Run(denied, func(t *testing.T) {
			mode.Store(denied)
			beforeDelivery, beforeModel := deliveries.Load(), models.Load()
			ctx := context.WithValue(context.Background(), serviceAccessKey{}, "human-token")
			if denied == "missing-context" {
				ctx = context.Background()
			}
			endpoint := issuer + "/v1/chat/completions"
			if denied == "destination" {
				endpoint = issuer + "/other"
			}
			r, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(`{}`))
			response, err := credential.HTTPClient.Do(r)
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("denial dispatched")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("remote error leaked")
			}
			if models.Load() != beforeModel {
				t.Fatal("denial reached model")
			}
			if denied != "binding" && denied != "outage" && deliveries.Load() != beforeDelivery {
				t.Fatal("invalid authority fetched credential")
			}
		})
	}
	mode.Store("")
	if b, err := os.ReadFile(logPath); err != nil {
		t.Fatal(err)
	} else {
		for _, secret := range []string{"private-model-key", "human-token", "client-secret"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Error("secret in logs")
			}
		}
	}
	if err := filepath.WalkDir(o.DataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		for _, secret := range []string{"private-model-key", "human-token", "client-secret"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("persisted secret in %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceGoauthyConfiguration(t *testing.T) {
	s := &serviceConfig{Issuer: "https://accounts.example", Audience: "https://beesuh.example"}
	binding := goauthy.APIKeyBinding{GrantID: "grant", ProviderID: "opencode", ConnectionGeneration: "generation", ConnectorDigest: "digest", CredentialVersion: 1, Header: "Authorization", Prefix: "Bearer ", Endpoint: "https://model.example/v1/chat/completions"}
	makeConnection := func() serviceConnection {
		return serviceConnection{ID: "c", Subject: "alice", WorkspaceID: "w", Provider: "p", GoauthyAPIKey: &serviceGoauthyAPIKey{Resource: "urn:goauthy:connections", Binding: binding}}
	}
	for _, test := range []struct {
		name   string
		change func(*serviceConnection)
		valid  bool
	}{
		{"valid", func(*serviceConnection) {}, true},
		{"exchange", func(c *serviceConnection) { c.TokenExchange = true }, false},
		{"oauth-audience", func(c *serviceConnection) { c.Audience = "model" }, false},
		{"oauth-scopes", func(c *serviceConnection) { c.Scopes = []string{"execute"} }, false},
		{"oauth-resource", func(c *serviceConnection) { c.Resource = "https://model.example" }, false},
		{"missing-resource", func(c *serviceConnection) { c.GoauthyAPIKey.Resource = "" }, false},
		{"missing-grant", func(c *serviceConnection) { c.GoauthyAPIKey.Binding.GrantID = "" }, false},
		{"non-bearer", func(c *serviceConnection) { c.GoauthyAPIKey.Binding.Header = "X-Api-Key" }, false},
		{"http-model", func(c *serviceConnection) { c.GoauthyAPIKey.Binding.Endpoint = "http://127.0.0.1/chat" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := makeConnection()
			test.change(&c)
			if got := s.validCredentialMode(c); got != test.valid {
				t.Fatalf("valid=%v, want %v", got, test.valid)
			}
		})
	}
	c := makeConnection()
	s.Connections = []serviceConnection{c}
	o := beesuh.Options{Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {BaseURL: "https://different.example/v1"}}}}
	if err := s.configure(&o); err != nil {
		t.Fatal(err)
	}
	if _, err := o.ResolveCredential(context.Background(), c.connection(s.Issuer)); err == nil {
		t.Fatal("mismatched provider endpoint accepted")
	}
	c.GoauthyAPIKey.Binding = binding
	s.Connections = []serviceConnection{c}
	first, err := s.resolve(context.Background(), c.connection(s.Issuer))
	if err != nil {
		t.Fatal(err)
	}
	s.Connections[0].GoauthyAPIKey.Binding.CredentialVersion++
	second, err := s.resolve(context.Background(), c.connection(s.Issuer))
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID == second.AccountID {
		t.Fatal("credential rotation retained account binding")
	}
}
