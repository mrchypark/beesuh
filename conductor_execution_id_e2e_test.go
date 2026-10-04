package beesuh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

type executionIDRoundTrip func(*http.Request) (*http.Response, error)

func (f executionIDRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNativePreservesLargeAcknowledgementExecutionIDForLookup(t *testing.T) {
	const executionID = "exec-with-a-very-long-identifier-0123456789"
	acknowledgement := func(requestID string) string {
		return fmt.Sprintf(`{"accepted":true,"events":[{"execution_id":%q,"execution_request_id":%q,"detail":"%s"}]}`,
			executionID, requestID, strings.Repeat(`\\`, 7000))
	}
	if size := acknowledgement("key"); len(size) >= 16<<10 || len(fmt.Sprintf("%q", size)) <= 22<<10 {
		t.Fatal("invalid oversized acknowledgement fixture")
	}

	var modelCalls int
	var lookupID string
	transport := executionIDRoundTrip(func(request *http.Request) (*http.Response, error) {
		body := ""
		status := http.StatusOK
		switch request.URL.Path {
		case "/v1/chat/completions":
			modelCalls++
			switch modelCalls {
			case 1:
				bodyBytes, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: "{\"name\":\"deploy\",\"data\":{}}"}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
				if err != nil {
					t.Fatal(err)
				}
				body = string(bodyBytes)
			case 2:
				bodyBytes, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "lookup", Type: "function", Function: model.FunctionCall{Name: "conductor_get_run", Arguments: "{\"run_id\":\"" + executionID + "\"}"}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
				if err != nil {
					t.Fatal(err)
				}
				body = string(bodyBytes)
			case 3:
				body = `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
			default:
				t.Fatalf("unexpected model call %d", modelCalls)
			}
		case "/v1/events":
			status = http.StatusAccepted
			var event struct {
				Events []struct {
					IdempotencyKey string `json:"idempotency_key"`
				} `json:"events"`
			}
			if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
				t.Fatal(err)
			}
			if len(event.Events) != 1 {
				t.Fatalf("events = %d, want 1", len(event.Events))
			}
			body = acknowledgement(event.Events[0].IdempotencyKey)
		case "/v1/executions/" + executionID:
			lookupID = strings.TrimPrefix(request.URL.Path, "/v1/executions/")
			body = `{"state":"running"}`
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})

	cond := &conductor.Config{
		BaseURL:       "http://conductor.invalid",
		AllowedEvents: []string{"deploy"},
		Bindings:      map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}},
		HTTPClient:    &http.Client{Transport: transport},
	}
	opts := Options{
		DataDir: t.TempDir(), BackendID: "execution-id-e2e", MaxConcurrent: 1, MaxWaiting: 1,
		OpenCode: opencodeConfig(t.TempDir()),
		Native: &NativeOptions{
			Provider:  model.Config{BaseURL: "http://model.invalid/v1", Model: "test", HTTPClient: &http.Client{Transport: transport}},
			Conductor: cond,
		},
	}
	r := nativeOpen(t, opts)
	defer r.Close()

	_, err := r.Run(context.Background(), Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "request", Prompt: opencode.Prompt{Text: "deploy"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(lookupID), []byte(executionID)) {
		t.Fatalf("Conductor lookup ID = %q, want exact %q", lookupID, executionID)
	}

	journal, err := r.History(context.Background(), "workspace", "session")
	if err != nil {
		t.Fatal(err)
	}
	var results []struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	for _, entry := range journal {
		if entry.Kind != "tool_result" {
			continue
		}
		var result struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(entry.Payload, &result); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(results))
	}
	var emitted struct {
		ExecutionID string `json:"execution_id"`
	}
	if results[0].Name != "conductor_emit" || json.Unmarshal([]byte(results[0].Content), &emitted) != nil || !bytes.Equal([]byte(emitted.ExecutionID), []byte(executionID)) {
		t.Fatalf("emit tool result = %+v, want exact execution ID %q", results[0], executionID)
	}
	if results[1].Name != "conductor_get_run" || !bytes.Equal([]byte(results[1].Content), []byte(`{"state":"running"}`)) {
		t.Fatalf("lookup tool result = %+v, want successful lookup", results[1])
	}
}
