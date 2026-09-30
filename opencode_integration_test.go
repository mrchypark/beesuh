package beesuh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/opencode"
)

// TestOpenCodeSessionPersistence is opt-in because it starts the locally installed
// OpenCode binary. The model endpoint is local and accepts no real credentials.
func TestOpenCodeSessionPersistence(t *testing.T) {
	if os.Getenv("BEESUH_TEST_OPENCODE") != "1" {
		t.Skip("set BEESUH_TEST_OPENCODE=1 to run the local OpenCode smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	model := &mockOpenAI{}
	model.server = httptest.NewServer(http.HandlerFunc(model.serveHTTP))
	defer model.server.Close()

	root := t.TempDir()
	config := fmt.Sprintf(`{
  "provider": {
    "mock": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Mock",
      "options": {"baseURL": %q, "apiKey": "test"},
      "models": {"test": {"name": "test", "limit": {"context": 128000, "output": 4096}}}
    }
  },
  "permission": {"*": "deny"},
  "plugin": []
}`, model.server.URL+"/v1")
	configPath := filepath.Join(root, "opencode.json")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(root, "workspace", ".opencode", "tools", "conductor.ts")
	toolSource, err := os.ReadFile(filepath.Join("examples", "opencode", "tools", "conductor.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(toolPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolPath, toolSource, 0600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	var logs bytes.Buffer
	cmd := exec.CommandContext(ctx, "opencode", "serve", "--pure", "--hostname", "127.0.0.1", "--port", port)
	cmd.Dir = filepath.Join(root, "workspace")
	if err := os.MkdirAll(cmd.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = &logs, &logs
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"OPENCODE_CONFIG="+configPath,
		"OPENCODE_CONFIG_DIR="+filepath.Join(root, "config-dir"),
		"OPENCODE_CONFIG_CONTENT="+config,
		"OPENCODE_SERVER_PASSWORD=",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	baseURL := "http://127.0.0.1:" + port
	if err := waitForHealth(ctx, baseURL); err != nil {
		t.Fatal(err)
	}
	ids, err := toolIDs(ctx, baseURL, cmd.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ids, "conductor_emit") || !contains(ids, "conductor_get_run") {
		t.Fatalf("Conductor tools did not load: %v", ids)
	}

	dataDir := filepath.Join(root, "runtime")
	options := Options{
		DataDir: dataDir, BackendID: "opencode-smoke", MaxConcurrent: 1, MaxWaiting: 1, Timeout: 30 * time.Second,
		Model:    &opencode.Model{ProviderID: "mock", ModelID: "test"},
		OpenCode: opencode.Config{BaseURL: baseURL, Directory: cmd.Dir},
	}
	runtime, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	first, err := runtime.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "agent", RequestID: "one", Prompt: opencode.Prompt{Text: "first-user-marker"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "agent", RequestID: "two", Prompt: opencode.Prompt{Text: "second-user-marker"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID != second.SessionID {
		t.Fatalf("session changed after reopen: %q != %q", first.SessionID, second.SessionID)
	}
	if !strings.Contains(string(second.Output), "remembered") {
		t.Fatalf("second output = %s", second.Output)
	}
	beforeReplay := model.count()
	if _, err := runtime.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "agent", RequestID: "two", Prompt: opencode.Prompt{Text: "second-user-marker"}}); err != nil {
		t.Fatal(err)
	}
	if model.count() != beforeReplay {
		t.Fatal("replayed request reached the model")
	}
	if !model.sawHistory() {
		t.Fatal("second model request omitted the first user marker")
	}
}

type mockOpenAI struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func (m *mockOpenAI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.bodies = append(m.bodies, string(body))
	m.mu.Unlock()
	text := "first"
	if strings.Contains(string(body), "second-user-marker") {
		text = "remembered"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"mock\",\"choices\":[{\"delta\":{\"content\":%q},\"index\":0}]}\n\n", text)
	_, _ = io.WriteString(w, "data: {\"id\":\"mock\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n")
}

func (m *mockOpenAI) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

func (m *mockOpenAI) sawHistory() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, body := range m.bodies {
		if strings.Contains(body, "first-user-marker") && strings.Contains(body, "second-user-marker") {
			return true
		}
	}
	return false
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

func waitForHealth(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/global/health", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func toolIDs(ctx context.Context, baseURL, directory string) ([]string, error) {
	u, err := url.Parse(baseURL + "/experimental/tool/ids")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("directory", directory)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tool IDs status %d", response.StatusCode)
	}
	var ids []string
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
