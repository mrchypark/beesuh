package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mrchypark/beesuh/model"
)

func TestProviderAliasesPreserveSessionAndIsolateRemoteNamespace(t *testing.T) {
	var mu sync.Mutex
	var remoteIDs, auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		remoteIDs = append(remoteIDs, req.Header.Get("x-opencode-session"))
		auths = append(auths, req.Header.Get("Authorization"))
		mu.Unlock()
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		b, _ := json.Marshal(body)
		if strings.Contains(string(b), "provider_id") || strings.Contains(string(b), "provider_state") {
			t.Error("internal metadata leaked to chat API")
		}
		if req.Header.Get("User-Agent") != "beesuh/0.1" {
			t.Error("missing application identity")
		}
		completion(w, "ok", "stop")
	}))
	defer server.Close()
	opts := Options{DataDir: t.TempDir(), BackendID: "local", MaxConcurrent: 2, MaxWaiting: 2, Native: &NativeOptions{DefaultProvider: "a", Providers: map[string]model.Config{
		"a": {Kind: "opencode-go", BaseURL: server.URL + "/v1", Model: "test", APIKey: "a"},
		"b": {Kind: "opencode-go", BaseURL: server.URL + "/v1", Model: "test", APIKey: "b"},
	}}}
	r := nativeOpen(t, opts)
	req := Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "1", Prompt: Prompt{Text: "hello"}}
	a, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit default and omitted default are the same durable request.
	req.Provider = "a"
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Provider = "b"
	if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed alias err=%v", err)
	}
	req.RequestID = "2"
	b, err := r.Run(context.Background(), req)
	if err != nil || a.SessionID != b.SessionID {
		t.Fatalf("provider switch: %+v %v", b, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	req.Provider, req.RequestID = "a", "3"
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	r.Close()
	// A second data directory has a distinct durable session namespace.
	opts.DataDir = t.TempDir()
	r = nativeOpen(t, opts)
	defer r.Close()
	req.RequestID = "4"
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(remoteIDs) != 4 || remoteIDs[0] == "" || remoteIDs[0] == remoteIDs[1] || remoteIDs[0] != remoteIDs[2] || remoteIDs[0] == remoteIDs[3] {
		t.Fatalf("remote session isolation/reuse failed: %v", remoteIDs)
	}
	if strings.Join(auths, ",") != "Bearer a,Bearer b,Bearer a,Bearer a" {
		t.Fatal("wrong account routed")
	}
}

func TestOpaqueProviderStateDoesNotCrossAccountsOrRestart(t *testing.T) {
	var mu sync.Mutex
	var replayed []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		b, _ := json.Marshal(body)
		mu.Lock()
		replayed = append(replayed, strings.Contains(string(b), "opaque-secret"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "reasoning", "id": "rs_test", "summary": []any{}, "encrypted_content": "opaque-secret"}, map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}}})
	}))
	defer server.Close()
	cfg := model.Config{Kind: "codex-api", BaseURL: server.URL + "/v1", Model: "test"}
	opts := Options{DataDir: t.TempDir(), BackendID: "local", MaxConcurrent: 1, Native: &NativeOptions{DefaultProvider: "a", Providers: map[string]model.Config{"a": cfg, "b": cfg}}}
	r := nativeOpen(t, opts)
	for i, p := range []string{"a", "a", "b"} {
		_, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: string(rune('1' + i)), Provider: p, Prompt: Prompt{Text: "hello"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	r = nativeOpen(t, opts)
	defer r.Close()
	if _, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "4", Provider: "a", Prompt: Prompt{Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(replayed) != 4 || replayed[0] || !replayed[1] || replayed[2] || replayed[3] {
		t.Fatalf("opaque state isolation failed: %v", replayed)
	}
}
