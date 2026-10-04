package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/model"
)

func TestToolErrorJournalUsesPublicFailure(t *testing.T) {
	const diagnostic = "/private/beesuh/rhiza.db table=memories"
	r := nativeOpen(t, Options{DataDir: t.TempDir(), BackendID: "tool-error", MaxConcurrent: 1,
		Native: &NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "test"}}})
	t.Cleanup(func() { _ = r.Close() })

	err := errors.New("rhiza: " + diagnostic)
	result := toolFailureResult(err)
	req := Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "request"}
	if err := r.appendJournal(context.Background(), req, "message", model.Message{Role: "tool", ToolCallID: "recall", Content: string(result)}); err != nil {
		t.Fatal(err)
	}

	history, err := r.History(context.Background(), req.WorkspaceID, req.SessionKey)
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if strings.Contains(string(history[0].Payload), "/private/beesuh/rhiza.db") || strings.Contains(string(history[0].Payload), "memories") {
		t.Fatalf("journal payload leaks diagnostic: %s", history[0].Payload)
	}
	var message model.Message
	if err := json.Unmarshal(history[0].Payload, &message); err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(message.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["failure_category"] != FailureUpstream || payload["error"] != PublicFailureMessage(FailureUpstream) {
		t.Fatalf("tool error payload = %#v", payload)
	}
}
