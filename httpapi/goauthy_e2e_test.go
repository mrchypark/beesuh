package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/mrchypark/beesuh/model"
)

const goauthyPilotResource = "https://beesuh.local.test"

type goauthyPilot struct {
	issuer, resource, clientID, secret, token string
	memberships                               []goauthyMembership
	negative                                  map[string]string
}

type goauthyMembership struct {
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	WorkspaceID string `json:"workspace_id"`
}

// TestGoAuthyLiveE2E is opt-in: no production authenticator is registered here.
func TestGoAuthyLiveE2E(t *testing.T) {
	if os.Getenv("BEESUH_E2E_GOAUTHY") != "1" {
		t.Skip("opt-in GoAuthy live pilot; not live verification")
	}
	cfg, err := loadGoAuthyPilot(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	runGoAuthyPilot(t, cfg)
}

func loadGoAuthyPilot(getenv func(string) string) (goauthyPilot, error) {
	cfg := goauthyPilot{
		issuer:   getenv("BEESUH_E2E_GOAUTHY_ISSUER"),
		resource: strings.TrimSpace(getenv("BEESUH_E2E_GOAUTHY_RESOURCE")),
		clientID: strings.TrimSpace(getenv("BEESUH_E2E_GOAUTHY_CLIENT_ID")),
		negative: map[string]string{},
	}
	u, err := url.Parse(cfg.issuer)
	if cfg.issuer == "" || strings.TrimSpace(cfg.issuer) != cfg.issuer || strings.HasSuffix(cfg.issuer, "/") || err != nil || u.String() != cfg.issuer || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !isLoopback(u.Hostname()) {
		return cfg, errors.New("GoAuthy pilot requires a host-local issuer without credentials, query or fragment")
	}
	if cfg.resource != goauthyPilotResource || cfg.clientID == "" {
		return cfg, errors.New("GoAuthy pilot requires the Beesuh resource and client ID")
	}
	var loadErr error
	if cfg.secret, loadErr = readGoAuthyFixture(getenv("BEESUH_E2E_GOAUTHY_CLIENT_SECRET_FILE")); loadErr != nil {
		return cfg, errors.New("invalid GoAuthy CLIENT_SECRET fixture file")
	}
	if cfg.token, loadErr = readGoAuthyFixture(getenv("BEESUH_E2E_GOAUTHY_TOKEN_FILE")); loadErr != nil {
		return cfg, errors.New("invalid GoAuthy TOKEN fixture file")
	}
	memberships, loadErr := readGoAuthyFixture(getenv("BEESUH_E2E_GOAUTHY_MEMBERSHIPS_FILE"))
	if loadErr != nil || json.Unmarshal([]byte(memberships), &cfg.memberships) != nil || !validMemberships(cfg.issuer, cfg.memberships) {
		return cfg, errors.New("invalid GoAuthy MEMBERSHIPS fixture file")
	}
	for _, name := range []string{"WRONG_AUD", "WRONG_SCOPE", "EXPIRED", "REVOKED"} {
		path := getenv("BEESUH_E2E_GOAUTHY_" + name + "_TOKEN_FILE")
		if path == "" {
			continue
		}
		value, err := readGoAuthyFixture(path)
		if err != nil || value == cfg.token {
			return cfg, errors.New("invalid GoAuthy " + name + " fixture file")
		}
		cfg.negative[name] = value
	}
	return cfg, nil
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

func validMemberships(issuer string, memberships []goauthyMembership) bool {
	if len(memberships) == 0 || len(memberships) > 64 {
		return false
	}
	seen := map[string]bool{}
	for _, m := range memberships {
		if m.Issuer != issuer || strings.TrimSpace(m.Subject) == "" || strings.TrimSpace(m.WorkspaceID) == "" || len(m.Subject) > 256 || len(m.WorkspaceID) > 256 || strings.ContainsAny(m.Subject+m.WorkspaceID, "\r\n\x00") || seen[m.Subject] {
			return false
		}
		seen[m.Subject] = true
	}
	return true
}

func readGoAuthyFixture(path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		return "", errors.New("fixture must be a regular non-symlink 0600 file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("fixture unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !os.SameFile(before, info) {
		return "", errors.New("fixture changed during open")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(after, info) {
		return "", errors.New("fixture changed during open")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	value := strings.TrimSpace(string(raw))
	if err != nil || len(raw) > 64<<10 || value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid fixture")
	}
	return value, nil
}

func (cfg goauthyPilot) authenticate(r *http.Request) (beesuh.Principal, error) {
	const prefix = "Bearer "
	token := strings.TrimPrefix(r.Header.Get("Authorization"), prefix)
	if token == r.Header.Get("Authorization") || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return beesuh.Principal{}, beesuh.ErrUnauthenticated
	}
	form := url.Values{"token": {token}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.issuer+"/oidc/introspect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(cfg.clientID, cfg.secret)
	var claims map[string]any
	if !goauthyJSON(req, &claims) {
		return beesuh.Principal{}, beesuh.ErrUnauthenticated
	}
	sub, _ := claims["sub"].(string)
	exp, expOK := numberInt64(claims["exp"])
	nbf := int64(0)
	if value, present := claims["nbf"]; present {
		var nbfOK bool
		nbf, nbfOK = numberInt64(value)
		if !nbfOK {
			return beesuh.Principal{}, beesuh.ErrUnauthenticated
		}
	}
	active, _ := claims["active"].(bool)
	issuer, issuerPresent := claims["iss"]
	issuerString, issuerOK := issuer.(string)
	if !active || strings.TrimSpace(sub) == "" || (issuerPresent && (!issuerOK || issuerString != cfg.issuer)) || !audienceContains(claims["aud"], cfg.resource) || !scopeContains(claims["scope"], "openid") || !expOK || exp <= time.Now().Unix() || nbf > time.Now().Unix() {
		return beesuh.Principal{}, beesuh.ErrUnauthenticated
	}
	req, _ = http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.issuer+"/oidc/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	var user map[string]any
	if !goauthyJSON(req, &user) || user["sub"] != sub {
		return beesuh.Principal{}, beesuh.ErrUnauthenticated
	}
	for _, m := range cfg.memberships {
		if m.Subject == sub {
			return beesuh.Principal{UserID: goauthyUserID(cfg.issuer, sub), WorkspaceID: m.WorkspaceID}, nil
		}
	}
	return beesuh.Principal{}, beesuh.ErrForbidden
}

func goauthyJSON(req *http.Request, target any) bool {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	return err == nil && len(raw) <= 64<<10 && json.Unmarshal(raw, target) == nil
}

func goauthyUserID(issuer, subject string) string {
	sum := sha256.Sum256([]byte("beesuh:oidc-user:v1\x00" + issuer + "\x00" + subject))
	return "usr_" + hex.EncodeToString(sum[:16])
}

func audienceContains(value any, expected string) bool {
	if value == expected {
		return true
	}
	values, _ := value.([]any)
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func scopeContains(value any, expected string) bool {
	for _, scope := range strings.Fields(asString(value)) {
		if scope == expected {
			return true
		}
	}
	return false
}

func asString(value any) string { text, _ := value.(string); return text }

func numberInt64(value any) (int64, bool) {
	n, ok := value.(float64)
	return int64(n), ok && n == float64(int64(n))
}

func runGoAuthyPilot(t *testing.T, cfg goauthyPilot) {
	t.Helper()
	principal, err := cfg.authenticate(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.Background()))
	if err == nil {
		t.Fatal("pilot setup must use a bearer token")
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+cfg.token)
	principal, err = cfg.authenticate(request)
	if err != nil {
		t.Fatal("pilot positive token was rejected")
	}

	var modelCalls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		modelCalls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pilot"},"finish_reason":"stop"}]}`))
	}))
	defer modelServer.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "goauthy-pilot", RequireIdentity: true,
		Authorize: func(_ context.Context, p beesuh.Principal) error {
			if p.UserID != principal.UserID || p.WorkspaceID != principal.WorkspaceID {
				return beesuh.ErrForbidden
			}
			return nil
		},
		ResolveCredential: func(context.Context, beesuh.Connection) (beesuh.Credential, error) {
			return beesuh.Credential{APIKey: "fixture", AccountID: "goauthy-pilot"}, nil
		},
		MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"pilot": {BaseURL: modelServer.URL, Model: "fixture"}}, DefaultProvider: "pilot"},
	})
	if err != nil {
		t.Fatal("pilot Rhiza/runtime startup failed")
	}
	defer runtime.Close()
	if err := runtime.RegisterConnection(context.Background(), beesuh.Connection{ID: "pilot-model", UserID: principal.UserID, WorkspaceID: principal.WorkspaceID, Kind: "provider", TargetID: "pilot", CredentialRef: "fixture", State: "ready"}); err != nil {
		t.Fatal("pilot connection setup failed")
	}
	h, err := NewAuthenticated(runtime, cfg.authenticate)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	chat := func(token, workspace, requestID string) int {
		body := []byte(`{"workspace_id":` + quote(workspace) + `,"session_key":"pilot","request_id":` + quote(requestID) + `,"connection_id":"pilot-model","message":"hello"}`)
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("pilot HTTP transport failed")
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := chat(cfg.token, principal.WorkspaceID, "one"); got != http.StatusOK {
		t.Fatalf("pilot chat status=%d", got)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.token)
	repeated, err := cfg.authenticate(request)
	if err != nil || repeated != principal {
		t.Fatal("repeat authentication changed identity")
	}
	if got := chat(cfg.token, principal.WorkspaceID, "two"); got != http.StatusOK || modelCalls.Load() != 2 {
		t.Fatalf("pilot repeat chat status=%d calls=%d", got, modelCalls.Load())
	}
	if got := chat(cfg.token, "workspace-not-a-member", "three"); got != http.StatusForbidden {
		t.Fatalf("pilot non-member workspace status=%d", got)
	}
	for _, name := range []string{"WRONG_AUD", "WRONG_SCOPE", "EXPIRED", "REVOKED"} {
		token, ok := cfg.negative[name]
		if !ok {
			t.Run(name, func(t *testing.T) { t.Skip("negative fixture not supplied; NOT VERIFIED") })
			continue
		}
		if got := chat(token, principal.WorkspaceID, "negative-"+strings.ToLower(name)); got != http.StatusUnauthorized {
			t.Fatalf("pilot %s status=%d", name, got)
		}
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestGoAuthyPilotHarnessContract(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc/introspect":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "pilot" || secret != "fixture" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			claims := map[string]any{"active": true, "sub": "subject", "aud": []string{goauthyPilotResource}, "scope": "openid", "exp": time.Now().Add(time.Hour).Unix()}
			switch r.FormValue("token") {
			case "positive":
			case "wrong-aud":
				claims["aud"] = []string{"https://other.test"}
			case "wrong-scope":
				claims["scope"] = "profile"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			case "wrong-iss":
				claims["iss"] = "http://other.test"
			case "malformed-nbf":
				claims["nbf"] = "later"
			case "outage":
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			default:
				claims["active"] = false
			}
			_ = json.NewEncoder(w).Encode(claims)
		case "/oidc/userinfo":
			switch r.Header.Get("Authorization") {
			case "Bearer positive":
			case "Bearer userinfo-mismatch":
				_ = json.NewEncoder(w).Encode(map[string]any{"sub": "other-subject"})
				return
			default:
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sub": "subject"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()
	cfg := goauthyPilot{issuer: issuer.URL, resource: goauthyPilotResource, clientID: "pilot", secret: "fixture", token: "positive", memberships: []goauthyMembership{{Issuer: issuer.URL, Subject: "subject", WorkspaceID: "workspace"}}, negative: map[string]string{"WRONG_AUD": "wrong-aud", "WRONG_SCOPE": "wrong-scope", "EXPIRED": "expired", "REVOKED": "revoked"}}
	runGoAuthyPilot(t, cfg)
	for _, token := range []string{"wrong-iss", "malformed-nbf", "userinfo-mismatch", "outage"} {
		t.Run(token, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			if _, err := cfg.authenticate(req); !errors.Is(err, beesuh.ErrUnauthenticated) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGoAuthyPilotFixtureValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	values := map[string]string{"BEESUH_E2E_GOAUTHY_ISSUER": "http://localhost:17889", "BEESUH_E2E_GOAUTHY_RESOURCE": goauthyPilotResource, "BEESUH_E2E_GOAUTHY_CLIENT_ID": "pilot"}
	values["BEESUH_E2E_GOAUTHY_CLIENT_SECRET_FILE"] = write("secret", "secret")
	values["BEESUH_E2E_GOAUTHY_TOKEN_FILE"] = write("token", "token")
	values["BEESUH_E2E_GOAUTHY_MEMBERSHIPS_FILE"] = write("memberships", `[{"issuer":"http://localhost:17889","subject":"subject","workspace_id":"workspace"}]`)
	if _, err := loadGoAuthyPilot(func(key string) string { return values[key] }); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(values["BEESUH_E2E_GOAUTHY_TOKEN_FILE"], 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGoAuthyPilot(func(key string) string { return values[key] }); err == nil {
		t.Fatal("world-readable fixture accepted")
	}
	if err := os.Chmod(values["BEESUH_E2E_GOAUTHY_TOKEN_FILE"], 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token-link")
	if err := os.Symlink(values["BEESUH_E2E_GOAUTHY_TOKEN_FILE"], link); err != nil {
		t.Fatal(err)
	}
	values["BEESUH_E2E_GOAUTHY_TOKEN_FILE"] = link
	if _, err := loadGoAuthyPilot(func(key string) string { return values[key] }); err == nil {
		t.Fatal("symlink fixture accepted")
	}
}
