package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamedProviderConfigUsesIndependentSecrets(t *testing.T) {
	t.Setenv("TEST_GO_A_KEY", "secret-a")
	t.Setenv("TEST_GO_B_KEY", "secret-b")
	file := filepath.Join(t.TempDir(), "providers.json")
	data := `{"default":"go-a","providers":[{"id":"go-a","kind":"opencode-go","model":"test","api_key_env":"TEST_GO_A_KEY"},{"id":"go-b","kind":"opencode-go","model":"test","api_key_env":"TEST_GO_B_KEY"},{"id":"zen","kind":"opencode-zen","api":"responses","model":"test","api_key_env":"TEST_GO_A_KEY"}]}`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	providers, def, err := LoadProviders(file)
	if err != nil || def != "go-a" || providers["go-a"].APIKey != "secret-a" || providers["go-b"].APIKey != "secret-b" {
		t.Fatalf("failed independent credential references: %v", err)
	}
	encoded, _ := json.Marshal(providers)
	if strings.Contains(string(encoded), "secret-") {
		t.Fatal("config serialization leaked credentials")
	}
	zen, err := New(providers["zen"])
	if err != nil || zen.baseURL.String() != "https://opencode.ai/zen/v1" || zen.api != "responses" {
		t.Fatalf("zen default: %v", err)
	}
	for _, invalid := range []string{
		`{"default":"missing","providers":[{"id":"a","kind":"codex-cli","model":"test"}]}`,
		`{"providers":[{"id":"a","kind":"codex-cli","model":"test"},{"id":"a","kind":"codex-cli","model":"test"}]}`,
		`{"providers":[{"id":"a","kind":"opencode-go","model":"test","api_key":"literal-secret"}]}`,
		`{"providers":[{"id":"a","kind":"opencode-go","model":"test","api_key_env":""}],"extra":true}`,
		`{"providers":[{"id":"../a","kind":"codex-cli","model":"test"}]}`,
		`{"providers":[{"id":"a","kind":"opencode-go","model":"test","api_key_env":"TEST_EMPTY_CREDENTIAL"}]}`,
	} {
		t.Setenv("TEST_EMPTY_CREDENTIAL", "")
		if err := os.WriteFile(file, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadProviders(file); err == nil {
			t.Fatal("invalid provider config accepted")
		}
	}
	if _, err := New(Config{Kind: "opencode-go", Model: "test", APIKey: "bad\x00value"}); err == nil {
		t.Fatal("NUL credential accepted")
	}
}

func TestOpenCodeZenProtocols(t *testing.T) {
	for _, tc := range []struct{ api, path, body string }{
		{"chat-completions", "/v1/chat/completions", `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
		{"responses", "/v1/responses", `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`},
		{"anthropic", "/v1/messages", `{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`},
	} {
		t.Run(tc.api, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Header.Get("x-opencode-session") != "session" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("wrong Zen endpoint or headers")
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(Config{Kind: "opencode-zen", API: tc.api, BaseURL: server.URL + "/v1", Model: "test", APIKey: "test-key"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Complete(context.Background(), Request{SessionID: "session", Messages: []Message{{Role: "user", Content: "hello"}}})
			if err != nil || result.Message.Content != "ok" {
				t.Fatalf("Zen %s failed: %v", tc.api, err)
			}
		})
	}
}
