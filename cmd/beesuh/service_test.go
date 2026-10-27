package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
)

type oauthFixture struct {
	s             *serviceConfig
	active        atomic.Bool
	introspects   atomic.Int32
	wrongAudience atomic.Bool
	wrongInfo     atomic.Bool
	expired       atomic.Bool
	omitType      atomic.Bool
	omitIssuer    atomic.Bool
}

func newOAuthFixture(t *testing.T) *oauthFixture {
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
			if token != "browser-token" && token != "refresh-token" {
				t.Error("unexpected introspected token")
			}
			if f.wrongAudience.Load() {
				aud = []string{"other"}
			}
			expiry := time.Now().Add(time.Minute).Unix()
			if f.expired.Load() {
				expiry = time.Now().Add(-time.Minute).Unix()
			}
			claims := map[string]any{"active": f.active.Load(), "sub": subject, "aud": aud, "exp": expiry, "scope": "openid execute", "extension": true}
			if !f.omitIssuer.Load() {
				claims["iss"] = base
			}
			if !f.omitType.Load() {
				claims["token_type"] = "Bearer"
			}
			json.NewEncoder(w).Encode(claims)
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
	f.s = &serviceConfig{Issuer: base, IntrospectionURL: base + "/introspect", UserInfoURL: base + "/userinfo", ClientID: "beesuh", ClientSecretFile: secret, Audience: "beesuh", RequiredScopes: []string{"execute"}, Goauthy: &serviceDiscovery{Resource: "https://accounts.example/connections", Providers: map[string]string{"external": "p"}}, client: &http.Client{Timeout: time.Second, CheckRedirect: noServiceRedirect}}
	return f
}
func TestServiceAuthValidation(t *testing.T) {
	for _, name := range []string{"audience", "expiry", "userinfo", "scope", "issuer"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthFixture(t)
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
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer browser-token")
			if _, e := f.s.authenticate(r); e == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
func TestServiceRevalidatesQueuedContext(t *testing.T) {
	f := newOAuthFixture(t)
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
			f := newOAuthFixture(t)
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

func TestServiceOptionalClaimsAndAccessTokenProof(t *testing.T) {
	f := newOAuthFixture(t)
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
}

func TestServiceOAuthResponseBoundaries(t *testing.T) {
	for _, body := range []string{strings.Repeat(" ", 64<<10) + `{}`, `{} {}`, `{"active":true} trailing`} {
		t.Run("invalid JSON", func(t *testing.T) {
			f := newOAuthFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			var out any
			if e := f.s.oauth(context.Background(), server.URL, url.Values{"token": {"browser-token"}}, "", &out); !errors.Is(e, auth.ErrUnavailable) {
				t.Fatalf("accepted invalid response: %v", e)
			}
		})
	}
	f := newOAuthFixture(t)
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

func TestServiceIssuerIsolation(t *testing.T) {
	f := newOAuthFixture(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer browser-token")
	p, err := f.s.authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != serviceUserID(f.s.Issuer, "u") || p.WorkspaceID != p.UserID || p.UserID == "u" {
		t.Fatalf("incorrect personal identity: %+v", p)
	}
	if p.UserID == serviceUserID("https://different.example", "u") || serviceUserID("https://a.example", "b,c") == serviceUserID("https://a.example,b", "c") {
		t.Fatal("issuer/subject collision")
	}
}

func TestServiceRejectsLegacyConfiguration(t *testing.T) {
	f := newOAuthFixture(t)
	f.s.Issuer = "https://accounts.example"
	b, err := json.Marshal(f.s)
	if err != nil {
		t.Fatal(err)
	}
	var valid map[string]any
	if err = json.Unmarshal(b, &valid); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "connections", "empty connections", "null connections", "token_url", "authority_url", "missing goauthy"} {
		t.Run(name, func(t *testing.T) {
			config := make(map[string]any)
			for k, v := range valid {
				config[k] = v
			}
			switch name {
			case "connections":
				config["connections"] = []any{map[string]string{"id": "c", "subject": "u", "workspace_id": "w", "provider": "p"}}
			case "empty connections":
				config["connections"] = []any{}
			case "null connections":
				config["connections"] = nil
			case "token_url":
				config["token_url"] = ""
			case "authority_url":
				config["authority_url"] = "https://accounts.example/beesuh"
			case "missing goauthy":
				delete(config, "goauthy")
			}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), "service.json")
			if err = os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = loadService(file)
			if (err == nil) != (name == "valid") {
				t.Fatalf("loadService: %v", err)
			}
		})
	}
	if f.introspects.Load() != 0 {
		t.Fatal("configuration fetched user tokens")
	}
}

func TestServiceConfigureRejectsAmbientCredentials(t *testing.T) {
	for _, name := range []string{"singleton", "singleton key", "named key", "named key env", "named login", "named command", "named client", "named account", "named auth", "conductor", "conductors", "no discovery"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthFixture(t)
			o := beesuh.Options{Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {BaseURL: "https://model.example/v1", Model: "test"}}, DefaultProvider: "p"}}
			// An unmapped provider must be held to the same credential boundary.
			extra := model.Config{BaseURL: "https://unused.example/v1", Model: "test"}
			switch name {
			case "singleton":
				o.Native.Provider = model.Config{BaseURL: "https://model.example/v1", Model: "test"}
			case "singleton key":
				o.Native.Provider.APIKey = "shared"
			case "named key":
				extra.APIKey = "shared"
			case "named key env":
				extra.APIKeyEnv = "SHARED_KEY"
			case "named login":
				extra.CodexHome = t.TempDir()
			case "named command":
				extra.Command = "codex"
			case "named account":
				extra.CodexAccountID = "operator-account"
			case "named auth":
				extra.CodexAuth = func(context.Context, model.CodexAuthRequest) (model.CodexAuthTokens, error) {
					t.Error("local login called")
					return model.CodexAuthTokens{}, nil
				}
			case "named client":
				extra.HTTPClient = &http.Client{}
			case "conductor":
				o.Native.Conductor = &conductor.Config{Token: "shared"}
			case "conductors":
				o.Native.Conductors = map[string]conductor.Config{"unused": {Token: "shared"}}
			case "no discovery":
				f.s.Goauthy = nil
			}
			o.Native.Providers["unused"] = extra
			if err := f.s.configure(&o); err == nil {
				t.Fatal("operator credential path accepted")
			}
			if f.introspects.Load() != 0 {
				t.Fatal("configuration fetched tokens")
			}
		})
	}
}

func TestServiceRejectsOperatorData(t *testing.T) {
	f := newOAuthFixture(t)
	dir := t.TempDir()
	o := beesuh.Options{DataDir: dir, BackendID: "test", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Minute, Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"p": {BaseURL: "https://model.example/v1", Model: "test"}}, DefaultProvider: "p"}}
	runtime, err := beesuh.Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.s.configure(&o); err != nil {
		t.Fatal(err)
	}
	runtime, err = beesuh.Open(context.Background(), o)
	if err == nil {
		runtime.Close()
		t.Fatal("operator data accepted by service")
	}
	if !strings.Contains(err.Error(), "identity mode mismatch") {
		t.Fatalf("wrong rejection: %v", err)
	}
}
