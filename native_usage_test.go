package beesuh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

func TestNativeAnthropicCacheUsagePersistsPromptAndTotalTokens(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		requests++
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":100,"cache_read_input_tokens":15,"cache_creation_input_tokens":5,"output_tokens":5}}`))
	}))
	defer server.Close()

	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, nil)
	opts.Native.Provider.API = "anthropic"
	r := nativeOpen(t, opts)
	defer r.Close()

	ctx := context.Background()
	client, err := model.New(model.Config{API: "anthropic", BaseURL: server.URL + "/v1", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(ctx, model.Request{Messages: []model.Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage != (model.Usage{PromptTokens: 120, CompletionTokens: 5, TotalTokens: 125}) {
		t.Fatalf("provider usage = %+v", response.Usage)
	}
	req := Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "cached", Prompt: opencode.Prompt{Text: "hello"}}
	if _, err := r.Run(ctx, req); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("Anthropic requests = %d, want 2", requests)
	}
	assertNativeJournalUsage(t, r, ctx, req, model.Usage{PromptTokens: 120, CompletionTokens: 5, TotalTokens: 125})
}

func TestNativeAnthropicUsageWithoutCacheRemainsUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer server.Close()

	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, nil)
	opts.Native.Provider.API = "anthropic"
	r := nativeOpen(t, opts)
	defer r.Close()

	ctx := context.Background()
	req := Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "uncached", Prompt: opencode.Prompt{Text: "hello"}}
	if _, err := r.Run(ctx, req); err != nil {
		t.Fatal(err)
	}
	assertNativeJournalUsage(t, r, ctx, req, model.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
}

func assertNativeJournalUsage(t *testing.T, r *Runtime, ctx context.Context, req Request, want model.Usage) {
	t.Helper()
	rows, err := r.query(ctx, `SELECT payload FROM journal WHERE workspace=? AND session_key=? AND request_id=? AND kind='model'`, req.WorkspaceID, req.SessionKey, req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("model journal rows = %d, want 1", len(rows))
	}
	var payload struct {
		Usage model.Usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(rows[0][0].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Usage != want {
		t.Fatalf("journal usage = %+v, want %+v", payload.Usage, want)
	}
}
