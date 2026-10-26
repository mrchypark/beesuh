package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Re-exec isolates run's flags and signal handlers from the test runner.
func TestOAuthBinaryProcess(t *testing.T) {
	if os.Getenv("BEESUH_TEST_OAUTH_BINARY_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("beesuh", flag.ExitOnError)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestOAuthBinaryEndToEnd(t *testing.T) {
	for _, exchange := range []bool{true, false} {
		t.Run(fmt.Sprintf("exchange=%t", exchange), func(t *testing.T) { testOAuthBinaryFlow(t, exchange) })
	}
}

func testOAuthBinaryFlow(t *testing.T, exchange bool) {
	const browser = "oauth-binary-browser-token"
	const refresh = "oauth-binary-refresh-token"
	const dispatched = "oauth-binary-exchanged-token"
	const secret = "oauth-binary-client-secret"
	var exchanges, calls, inspections, userinfos atomic.Int32
	var inactive, mismatch, wrongAudience atomic.Bool
	var issuer string
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/userinfo" {
			userinfos.Add(1)
			if r.Header.Get("Authorization") == "Bearer "+refresh {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":"invalid_token"}`)
				return
			}
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+browser {
				t.Error("UserInfo must use GET and request Bearer")
			}
			sub := "u"
			if mismatch.Load() {
				sub = "other"
			}
			json.NewEncoder(w).Encode(map[string]any{"sub": sub, "extension": true})
			return
		}
		id, pw, ok := r.BasicAuth()
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || !ok || id != "beesuh" || pw != secret {
			t.Error("OAuth endpoint requires form POST and confidential client Basic authentication")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/introspect":
			inspections.Add(1)
			token := r.Form.Get("token")
			if token != browser && token != dispatched && token != refresh {
				t.Error("unexpected introspection token")
			}
			if r.Form.Get("token_type_hint") != "access_token" {
				t.Error("missing access token hint")
			}
			aud := []string{"beesuh", "model"}
			if wrongAudience.Load() {
				aud = []string{"other"}
			}
			json.NewEncoder(w).Encode(map[string]any{"active": !inactive.Load(), "sub": "u", "iss": issuer, "aud": aud, "exp": time.Now().Add(time.Minute).Unix(), "scope": "openid execute model.invoke", "token_type": "Bearer", "extension": true})
		case "/token":
			exchanges.Add(1)
			if !exchange || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || r.Form.Get("subject_token") != browser || r.Form.Get("audience") != "model" || r.Form.Get("scope") != "model.invoke" || r.Form.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:access_token" || r.Form.Get("requested_token_type") != "urn:ietf:params:oauth:token-type:access_token" {
				t.Error("invalid RFC 8693 exchange form")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": dispatched, "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 60, "extension": true})
		default:
			t.Errorf("unexpected authority path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	issuer = authority.URL
	defer authority.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		want := browser
		if exchange {
			want = dispatched
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+want {
			t.Error("incorrect provider request or Bearer token")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"oauth binary reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	dir := t.TempDir()
	write := func(name string, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err = os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	secretFile := filepath.Join(dir, "client-secret")
	if err := os.WriteFile(secretFile, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"issuer": issuer, "introspection_url": issuer + "/introspect", "client_id": "beesuh", "client_secret_file": secretFile, "audience": "beesuh", "required_scopes": []string{"execute"}, "connections": []map[string]any{{"id": "c", "subject": "u", "workspace_id": "w", "provider": "p", "audience": "model", "scopes": []string{"model.invoke"}, "token_exchange": exchange}}}
	if exchange {
		config["token_url"] = issuer + "/token"
	}
	config["userinfo_url"] = issuer + "/userinfo"
	manifest := write("service.json", config)
	providers := write("providers.json", map[string]any{"default": "p", "providers": []map[string]string{{"id": "p", "kind": "openai-compatible", "base_url": provider.URL + "/v1", "model": "test"}}})
	cmd := exec.Command(os.Args[0], "-test.run=^TestOAuthBinaryProcess$", "--", "-listen", "127.0.0.1:0", "-service-config", manifest, "-providers", providers, "-data-dir", filepath.Join(dir, "data"))
	// Only the mounted file carries client credentials; no model keys enter the child.
	cmd.Env = []string{"BEESUH_TEST_OAUTH_BINARY_PROCESS=1", "HOME=" + dir, "PATH=" + os.Getenv("PATH")}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, logs bytes.Buffer
	cmd.Stdout = &stdout
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	drained := make(chan struct{})
	ready := make(chan string, 1)
	go func() {
		defer close(drained)
		scan := bufio.NewScanner(stderr)
		for scan.Scan() {
			line := scan.Text()
			fmt.Fprintln(&logs, line)
			if address, ok := strings.CutPrefix(line, "beesuh listening on "); ok {
				select {
				case ready <- "http://" + address:
				default:
				}
			}
		}
	}()
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
		<-drained
	}()
	var base string
	select {
	case base = <-ready:
	case err := <-done:
		stopped = true
		<-drained
		t.Fatalf("startup: %v: %s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatal("startup timed out")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	requestToken := browser
	request := func(method, path, body string, authenticated bool, want int) string {
		t.Helper()
		r, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if authenticated {
			r.Header.Set("Authorization", "Bearer "+requestToken)
		}
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{browser, refresh, dispatched, secret} {
			if bytes.Contains(b, []byte(token)) {
				t.Fatal("secret exposed in HTTP response")
			}
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, resp.StatusCode, want, b)
		}
		return string(b)
	}
	if exchanges.Load() != 0 || inspections.Load() != 0 {
		t.Fatal("startup contacted OAuth endpoints")
	}
	request("GET", "/v1/connections", "", false, 401)
	if inspections.Load() != 0 {
		t.Fatal("missing Bearer reached authority")
	}
	if body := request("GET", "/v1/connections", "", true, 200); !strings.Contains(body, `"c"`) {
		t.Fatalf("missing connection: %s", body)
	}
	if exchanges.Load() != 0 {
		t.Fatal("listing exchanged token")
	}
	body := `{"workspace_id":"w","session_key":"s","request_id":"denied","connection_id":"c","message":"hello"}`
	requestToken = refresh
	request("POST", "/v1/chat", body, true, 401)
	requestToken = browser
	inactive.Store(true)
	request("POST", "/v1/chat", body, true, 401)
	inactive.Store(false)
	wrongAudience.Store(true)
	request("POST", "/v1/chat", body, true, 401)
	wrongAudience.Store(false)
	mismatch.Store(true)
	request("POST", "/v1/chat", body, true, 401)
	mismatch.Store(false)
	if exchanges.Load() != 0 || calls.Load() != 0 {
		t.Fatal("denied chat exchanged or dispatched")
	}
	if out := request("POST", "/v1/chat", strings.Replace(body, "denied", "success", 1), true, 200); !strings.Contains(out, "oauth binary reply") {
		t.Fatalf("missing reply: %s", out)
	}
	wantExchanges := int32(0)
	if exchange {
		wantExchanges = 1
	}
	if exchanges.Load() != wantExchanges || calls.Load() != 1 {
		t.Fatalf("exchanges=%d provider calls=%d", exchanges.Load(), calls.Load())
	}
	if out := request("GET", "/v1/history?workspace_id=w&session_key=s", "", true, 200); !strings.Contains(out, "oauth binary reply") {
		t.Fatalf("missing history: %s", out)
	}
	// A previous successful request must not authenticate a later request.
	request("GET", "/v1/history?workspace_id=w&session_key=s", "", false, 401)
	inactive.Store(true)
	request("GET", "/v1/history?workspace_id=w&session_key=s", "", true, 401)
	inactive.Store(false)
	if exchanges.Load() != wantExchanges || calls.Load() != 1 {
		t.Fatal("history exchanged or dispatched")
	}
	if userinfos.Load() == 0 {
		t.Fatal("configured UserInfo not called")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown timed out")
	}
	<-drained
	for _, token := range []string{browser, refresh, dispatched, secret} {
		if strings.Contains(logs.String()+stdout.String(), token) {
			t.Error("secret exposed in process output")
		}
	}
	files := 0
	identityJSON, err := json.Marshal([]string{issuer, "u"})
	if err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("oidc_%x", sha256.Sum256(identityJSON))
	foundIdentity := false
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range []string{browser, refresh, dispatched, secret} {
			if path == secretFile && token == secret {
				continue
			}
			if bytes.Contains(b, []byte(token)) {
				t.Errorf("secret persisted in %s", path)
			}
		}
		if strings.HasPrefix(path, filepath.Join(dir, "data")+string(os.PathSeparator)) {
			files++
			foundIdentity = foundIdentity || bytes.Contains(b, []byte(identity))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no persisted data inspected")
	}
	if !foundIdentity {
		t.Fatal("persisted data lacks the issuer and subject derived identity")
	}
}
