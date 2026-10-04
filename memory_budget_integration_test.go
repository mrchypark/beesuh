package beesuh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/rhiza"
)

// A saved note must never strand a session. With the smallest supported
// context budget the runtime used to inject the whole recall payload and then
// reject the turn before the model ran, so the model could never call
// memory_forget to repair it.
func TestMemoryInjectionStaysWithinSmallContextBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completion(w, "ok", "stop")
	}))
	t.Cleanup(server.Close)

	r := nativeOpen(t, Options{
		DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1,
		Native: &NativeOptions{
			Provider:        model.Config{BaseURL: server.URL + "/v1", Model: "test"},
			Memory:          true,
			MaxContextBytes: 4096,
		},
	})

	ctx := context.Background()
	req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r"}
	// Store a note directly, so this test isolates injection from the save
	// round-trip, which echoes the content back inside a tool result.
	save := model.ToolCall{ID: "1", Type: "function", Function: model.FunctionCall{
		Name: "memory_save", Arguments: `{"name":"big","content":"` + strings.Repeat("z", 2000) + `"}`}}
	if _, err := r.nativeTool(ctx, req, save); err != nil {
		t.Fatalf("save note: %v", err)
	}
	rows, err := r.query(ctx, `SELECT content FROM memories WHERE workspace=? AND session_key=?`, "w", "s")
	if err != nil || len(rows) != 1 {
		t.Fatalf("note was not stored: rows=%v err=%v", rows, err)
	}
	stored := len(rows[0][0].(string))

	// The follow-up minimal turn must reach the model rather than fail on the
	// budget, so the model stays able to call memory_forget to repair the note.
	out, err := r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r1", Prompt: Prompt{Text: "hi"}})
	if err != nil {
		t.Fatalf("turn after a %d-byte note failed: %v", stored, err)
	}
	if out.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded", out.State)
	}
}

// The smallest supported budget must still leave room for a real turn once
// notes are omitted. Report the actual sizes so a future schema growth is
// visible rather than silently breaking the minimum configuration.
func TestSmallestSupportedBudgetStillRunsATurn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completion(w, "ok", "stop")
	}))
	t.Cleanup(server.Close)
	r := nativeOpen(t, Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1,
		Native: &NativeOptions{Provider: model.Config{BaseURL: server.URL + "/v1", Model: "test"}, Memory: true, MaxContextBytes: 4096}})
	if _, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", Prompt: Prompt{Text: "hi"}}); err != nil {
		t.Fatalf("minimum context budget cannot run a turn: %v", err)
	}
}

// Legacy oversized rows must not suppress a later small note.
func TestOversizedLegacyNoteDoesNotSuppressLaterNote(t *testing.T) {
	r := nativeOpen(t, Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1,
		Native: &NativeOptions{Provider: model.Config{BaseURL: "http://127.0.0.1:1/v1", Model: "test"}, Memory: true, MaxContextBytes: 4096}})
	ctx := context.Background()
	req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r"}
	if err := r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `INSERT INTO memories(workspace,session_key,name,content,source_request) VALUES(?,?,?,?,?)`, Args: []any{"w", "s", "a-legacy", strings.Repeat("<", 2048), "old"}},
		{SQL: `INSERT INTO memories(workspace,session_key,name,content,source_request) VALUES(?,?,?,?,?)`, Args: []any{"w", "s", "z-small", "tea", "new"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := r.memoryRecall(ctx, req, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "z-small") {
		t.Fatalf("later small note was suppressed: %s", got)
	}
}
