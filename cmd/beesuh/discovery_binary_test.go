package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoveryBinaryEndToEnd(t *testing.T) {
	const login = "discovery-human-token"
	const fresh = "discovery-fresh-human-token"
	const key = "discovery-provider-secret-key"
	const secret = "discovery-client-secret"
	const resource = "https://goauthy.example/connections"
	var deliveries, models, metadata atomic.Int32
	var revoked, wrongOwner, wrongConsumer, restarted atomic.Bool
	var issuer string
	expires := time.Now().Add(time.Hour).UnixMilli()

	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token := login
		if restarted.Load() {
			token = fresh
		}
		if r.URL.Path == "/introspect" {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			id, password, ok := r.BasicAuth()
			if r.Method != "POST" || !ok || id != "beesuh" || password != secret || r.Form.Get("token") != token || r.Form.Get("token_type_hint") != "access_token" {
				t.Error("invalid introspection request")
			}
			json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice", "iss": issuer, "client_id": "beesuh", "aud": []string{"https://beesuh.example", resource}, "scope": "openid execute goauthy.connections.read goauthy.connections.use", "exp": time.Now().Add(time.Hour).Unix(), "token_type": "Bearer"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Cookie") != "" {
			t.Error("authority did not receive current user bearer only")
		}
		if r.URL.Path == "/userinfo" {
			if r.Method != "GET" {
				t.Error("userinfo method")
			}
			io.WriteString(w, `{"sub":"alice"}`)
			return
		}
		metadata.Add(1)
		owner := "alice"
		if wrongOwner.Load() {
			owner = "other"
		}
		connection := map[string]any{"id": "personal", "collection_id": "models", "owner_subject": owner, "state": "ready"}
		switch r.URL.Path {
		case "/auth/v1/connection-collections":
			json.NewEncoder(w).Encode([]any{map[string]any{"id": "models", "enabled": true, "auth_method": "api_key", "provider_ids": []string{"external"}}})
		case "/auth/v1/connections/models":
			json.NewEncoder(w).Encode([]any{connection, map[string]any{"id": "foreign", "collection_id": "models", "owner_subject": "other", "state": "ready"}})
		case "/auth/v1/connections/models/personal":
			json.NewEncoder(w).Encode(connection)
		case "/auth/v1/connections/models/personal/grants/approved":
			consumer := "beesuh"
			if wrongConsumer.Load() {
				consumer = "other"
			}
			json.NewEncoder(w).Encode(map[string]any{"connection_generation": "generation", "grant": map[string]any{"id": "approved", "owner_subject": owner, "collection_id": "models", "connection_id": "personal", "consumer_client_id": consumer, "mode": "credential_delivery", "resource": resource, "generation": "generation", "consumer_generation": "consumer-generation", "provider_id": "external", "connector_digest": "digest", "revision": 1, "provider_revision": 1, "expires_at_unix_ms": expires, "revoked": revoked.Load()}})
		case "/auth/v1/connections/models/personal/grants/unapproved":
			http.Error(w, `{"error":"access_denied"}`, http.StatusForbidden)
		case "/auth/v1/connection-grants/approved/credential":
			deliveries.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.Method != "POST" || len(body) != 0 {
				t.Error("credential delivery must be bodyless POST")
			}
			json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": key, "grant_id": "approved", "provider_id": "external", "connection_generation": "generation", "credential_version": 1, "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": expires})
		default:
			t.Errorf("unexpected authority path %s", r.URL.Path)
			http.NotFound(w, r)
		}
		if r.URL.Path != "/auth/v1/connection-grants/approved/credential" && r.Method != "GET" {
			t.Error("metadata must use GET")
		}
	}))
	defer authority.Close()
	issuer = authority.URL
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("Cookie") != "" {
			t.Error("provider credential or route mismatch")
		}
		for _, s := range []string{login, fresh, secret} {
			if strings.Contains(fmt.Sprint(r.Header)+string(b), s) {
				t.Error("user or client credential reached provider")
			}
		}
		if bytes.Contains(b, []byte(key)) {
			t.Error("model key in payload")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"discovery reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
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
	certs := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.Certificate().Raw})
	certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw})...)
	ca := write("ca.pem", certs)
	secretFile := write("client-secret", []byte(secret))
	config := writeJSON("service.json", map[string]any{"issuer": issuer, "introspection_url": issuer + "/introspect", "userinfo_url": issuer + "/userinfo", "client_id": "beesuh", "client_secret_file": secretFile, "audience": "https://beesuh.example", "required_scopes": []string{"execute"}, "goauthy": map[string]any{"resource": resource, "providers": map[string]string{"external": "p"}}})
	providers := writeJSON("providers.json", map[string]any{"default": "p", "providers": []any{map[string]string{"id": "p", "kind": "openai-compatible", "base_url": provider.URL + "/v1", "model": "test"}}})
	checkSecrets := func(b []byte) {
		t.Helper()
		for _, s := range []string{login, fresh, key, secret} {
			if bytes.Contains(b, []byte(s)) {
				t.Error("secret exposed in response, process output, or persisted data")
			}
		}
	}
	start := func() (string, func()) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestOAuthBinaryProcess$", "--", "-listen", "127.0.0.1:0", "-service-config", config, "-providers", providers, "-data-dir", filepath.Join(dir, "data"))
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
		t.Cleanup(func() {
			if !stopped {
				_ = cmd.Process.Kill()
				<-done
			}
			<-drained
			checkSecrets(stdout.Bytes())
			checkSecrets(logs.Bytes())
		})
		stop := func() {
			t.Helper()
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
			checkSecrets(stdout.Bytes())
			checkSecrets(logs.Bytes())
		}
		select {
		case base := <-ready:
			return base, stop
		case err := <-done:
			stopped = true
			<-drained
			t.Fatalf("startup: %v: %s", err, logs.String())
		case <-time.After(30 * time.Second):
			t.Fatal("startup timed out")
		}
		return "", stop
	}
	base, stop := start()
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	token := login
	request := func(method, path, body string, want int) string {
		t.Helper()
		r, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
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
		checkSecrets(b)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, resp.StatusCode, want, b)
		}
		return string(b)
	}
	assertCalls := func(want int32) {
		t.Helper()
		if deliveries.Load() != want || models.Load() != want {
			t.Fatalf("credential=%d provider=%d want=%d", deliveries.Load(), models.Load(), want)
		}
	}
	assertCalls(0)
	if metadata.Load() != 0 {
		t.Fatal("startup fetched metadata")
	}
	options := request("GET", "/v1/connection-options", "", 200)
	if !strings.Contains(options, `"personal"`) || strings.Contains(options, `"foreign"`) {
		t.Fatalf("incorrect options: %s", options)
	}
	request("GET", "/v1/connections", "", 200)
	assertCalls(0)
	selection := `{"collection_id":"models","connection_id":"personal","grant_id":"approved"}`
	wrongOwner.Store(true)
	request("POST", "/v1/connections", selection, 403)
	wrongOwner.Store(false)
	wrongConsumer.Store(true)
	request("POST", "/v1/connections", selection, 403)
	wrongConsumer.Store(false)
	request("POST", "/v1/connections", strings.Replace(selection, "approved", "unapproved", 1), 403)
	request("POST", "/v1/connections", `{"collection_id":"models","connection_id":"personal"}`, 400)
	selected := request("POST", "/v1/connections", selection, 201)
	var connection struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		Generation  int64  `json:"generation"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal([]byte(selected), &connection); err != nil {
		t.Fatal(err)
	}
	identityJSON, _ := json.Marshal([]string{issuer, "alice"})
	identity := fmt.Sprintf("oidc_%x", sha256.Sum256(identityJSON))
	if connection.ID == "" || connection.WorkspaceID != identity || connection.Generation != 1 || connection.State != "ready" {
		t.Fatalf("incorrect runtime identity: %s", selected)
	}
	history := "/v1/history?workspace_id=" + url.QueryEscape(identity) + "&session_key=s"
	request("GET", "/v1/connections", "", 200)
	request("GET", history, "", 200)
	assertCalls(0)
	// Restart before dispatch proves that selection persisted only a reference.
	stop()
	restarted.Store(true)
	token = fresh
	base, stop = start()
	listed := request("GET", "/v1/connections", "", 200)
	if !strings.Contains(listed, connection.ID) {
		t.Fatalf("persisted reference missing: %s", listed)
	}
	request("GET", history, "", 200)
	assertCalls(0)
	chat := func(id string) string {
		b, err := json.Marshal(map[string]string{"workspace_id": identity, "session_key": "s", "request_id": id, "connection_id": connection.ID, "message": "hello"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if out := request("POST", "/v1/chat", chat("success"), 200); !strings.Contains(out, "discovery reply") {
		t.Fatalf("missing reply: %s", out)
	}
	assertCalls(1)
	if out := request("GET", history, "", 200); !strings.Contains(out, "discovery reply") {
		t.Fatalf("missing history: %s", out)
	}
	assertCalls(1)
	revoked.Store(true)
	request("POST", "/v1/chat", chat("revoked"), 403)
	assertCalls(1)
	stop()
	files := 0
	foundRef := false
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
		checkSecrets(b)
		foundRef = foundRef || bytes.Contains(b, []byte("ga_"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 || !foundRef {
		t.Fatal("persisted nonsecret reference not found")
	}
}
