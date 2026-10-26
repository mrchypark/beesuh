package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/pem"
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

func TestGoauthyAPIKeyBinaryEndToEnd(t *testing.T) {
	const login = "goauthy-binary-human-token"
	const key = "goauthy-binary-model-key"
	const secret = "goauthy-binary-client-secret"
	const resource = "https://goauthy.example/connections"
	var deliveries, models, inspections, userinfos atomic.Int32
	var wrongConsumer atomic.Bool
	var issuer string
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/introspect":
			inspections.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			id, password, ok := r.BasicAuth()
			if r.Method != "POST" || !ok || id != "beesuh" || password != secret || r.Form.Get("token") != login || r.Form.Get("token_type_hint") != "access_token" {
				t.Error("invalid introspection request")
			}
			consumer := "beesuh"
			if wrongConsumer.Load() {
				consumer = "other"
			}
			json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice", "client_id": consumer, "iss": issuer, "aud": []string{"https://beesuh.example", resource}, "scope": "openid execute goauthy.connections.use", "exp": time.Now().Add(time.Minute).Unix(), "token_type": "Bearer"})
		case "/userinfo":
			userinfos.Add(1)
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+login {
				t.Error("invalid UserInfo request")
			}
			io.WriteString(w, `{"sub":"alice"}`)
		case "/auth/v1/connection-grants/grant/credential":
			deliveries.Add(1)
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+login || r.Header.Get("Cookie") != "" {
				t.Error("invalid credential delivery request")
			}
			json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": key, "grant_id": "grant", "provider_id": "opencode", "connection_generation": "generation", "credential_version": 1, "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()})
		default:
			t.Errorf("unexpected authority path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer authority.Close()
	issuer = authority.URL
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("Cookie") != "" {
			t.Error("provider did not receive only the model credential")
		}
		if strings.Contains(fmt.Sprint(r.Header)+string(body), login) || bytes.Contains(body, []byte(key)) {
			t.Error("credential leaked into provider payload")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"goauthy binary reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	writeJSON := func(name string, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return write(name, b)
	}
	// Trust only public fixture certificates; the child retains normal TLS verification.
	certs := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.Certificate().Raw})
	certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw})...)
	ca := write("fixture-ca.pem", certs)
	secretFile := write("client-secret", []byte(secret))
	manifest := writeJSON("service.json", map[string]any{"issuer": issuer, "introspection_url": issuer + "/introspect", "userinfo_url": issuer + "/userinfo", "client_id": "beesuh", "client_secret_file": secretFile, "audience": "https://beesuh.example", "required_scopes": []string{"execute"}, "connections": []any{map[string]any{"id": "personal", "subject": "alice", "workspace_id": "team", "provider": "p", "goauthy_api_key": map[string]any{"resource": resource, "binding": map[string]any{"grant_id": "grant", "provider_id": "opencode", "connection_generation": "generation", "connector_digest": "digest", "credential_version": 1, "header": "Authorization", "prefix": "Bearer ", "endpoint": provider.URL + "/v1/chat/completions"}}}}})
	providers := writeJSON("providers.json", map[string]any{"default": "p", "providers": []any{map[string]string{"id": "p", "kind": "opencode-go", "base_url": provider.URL + "/v1", "model": "test"}}})
	cmd := exec.Command(os.Args[0], "-test.run=^TestOAuthBinaryProcess$", "--", "-listen", "127.0.0.1:0", "-service-config", manifest, "-providers", providers, "-data-dir", filepath.Join(dir, "data"))
	cmd.Env = []string{"BEESUH_TEST_OAUTH_BINARY_PROCESS=1", "HOME=" + dir, "PATH=" + os.Getenv("PATH"), "SSL_CERT_FILE=" + ca}
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
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
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
	request := func(method, path, body string, want int) string {
		t.Helper()
		r, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+login)
		r.Header.Set("Content-Type", "application/json")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		b, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{login, key, secret} {
			if bytes.Contains(b, []byte(s)) {
				t.Fatal("credential exposed in HTTP response")
			}
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, response.StatusCode, want, b)
		}
		return string(b)
	}
	assertCalls := func(want int32) {
		t.Helper()
		if deliveries.Load() != want || models.Load() != want {
			t.Fatalf("delivery=%d model=%d want=%d", deliveries.Load(), models.Load(), want)
		}
	}
	assertCalls(0)
	if inspections.Load() != 0 || userinfos.Load() != 0 {
		t.Fatal("startup contacted authority")
	}
	if out := request("GET", "/v1/connections", "", 200); !strings.Contains(out, `"personal"`) {
		t.Fatalf("missing connection: %s", out)
	}
	request("GET", "/v1/history?workspace_id=team&session_key=s", "", 200)
	assertCalls(0)
	chat := `{"workspace_id":"team","session_key":"s","request_id":"success","connection_id":"personal","message":"hello"}`
	if out := request("POST", "/v1/chat", chat, 200); !strings.Contains(out, "goauthy binary reply") {
		t.Fatalf("missing reply: %s", out)
	}
	assertCalls(1)
	if out := request("GET", "/v1/history?workspace_id=team&session_key=s", "", 200); !strings.Contains(out, "goauthy binary reply") {
		t.Fatalf("missing history: %s", out)
	}
	assertCalls(1)
	wrongConsumer.Store(true)
	request("POST", "/v1/chat", strings.Replace(chat, "success", "wrong-consumer", 1), 403)
	assertCalls(1)
	if inspections.Load() == 0 || userinfos.Load() == 0 {
		t.Fatal("standard OAuth endpoints were not used")
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
	for _, s := range []string{login, key, secret} {
		if strings.Contains(stdout.String()+logs.String(), s) {
			t.Error("credential exposed in process output")
		}
	}
	files := 0
	if err := filepath.WalkDir(filepath.Join(dir, "data"), func(path string, d fs.DirEntry, err error) error {
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
		files++
		for _, s := range []string{login, key, secret} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("credential persisted in %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no persisted data inspected")
	}
}
