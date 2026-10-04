package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesRequestAndReplay(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("User-Agent") != "beesuh/0.1" {
			t.Fatalf("request: %s %q %q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["store"]) != "false" || string(body["max_output_tokens"]) != "12" || len(body["include"]) == 0 || len(body["text"]) == 0 || len(body["tools"]) == 0 {
			t.Fatalf("missing response fields: %v", body)
		}
		if calls == 2 && !jsonContains(body["input"], `"encrypted_content":"opaque"`) {
			t.Fatalf("state was not replayed: %s", body["input"])
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"conductor_emit","arguments":"{}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	in := Request{Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{{Type: "function", Function: Function{Name: "conductor_emit", Parameters: json.RawMessage(`{"type":"object"}`)}}}, Schema: json.RawMessage(`{"type":"object"}`), MaxTokens: 12}
	out, err := c.completeResponses(context.Background(), in)
	if err != nil || out.FinishReason != "tool_calls" || out.Message.ToolCalls[0].ID != "call_1" || !jsonContains(out.Message.ProviderState, `"reasoning"`) {
		t.Fatalf("response: %+v, %v", out, err)
	}
	in.Messages = append(in.Messages, out.Message, Message{Role: "tool", ToolCallID: "call_1", Content: `{}`})
	if _, err := c.completeResponses(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesRejectsIncompleteAndMixedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"incomplete","output":[]}`))
	}))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, Model: "m"})
	if _, err := c.completeResponses(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("expected incomplete error")
	}
	if _, err := c.completeResponses(context.Background(), Request{Messages: []Message{{Role: "assistant", ProviderState: json.RawMessage(`[{"type":"input_text","text":"x"}]`)}}}); err == nil {
		t.Fatal("expected provider state error")
	}
}

func TestResponsesRejectsIncompleteFunctionCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"conductor_emit","arguments":"{}","status":"incomplete"}]}`))
	}))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, Model: "m"})
	if _, err := c.completeResponses(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("expected incomplete function-call error")
	}
}

func TestAnthropicPreservesThinkingAndToolInput(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			want := `{"type":"thinking","thinking":"hidden","signature":"sig"}`
			if !jsonContains(body["messages"], want) || !jsonContains(body["messages"], `9007199254740993`) {
				t.Fatalf("thinking or tool input was not replayed: %s", body["messages"])
			}
		}
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"tool_use","content":[{"type":"thinking","thinking":"hidden","signature":"sig"},{"type":"tool_use","id":"tool_1","name":"conductor_emit","input":{"n":9007199254740993}}],"usage":{"input_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":5,"output_tokens":7}}`))
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	in := Request{Messages: []Message{{Role: "user", Content: "hi"}}}
	out, err := c.completeAnthropic(context.Background(), in)
	if err != nil || out.Message.ToolCalls[0].Function.Arguments != `{"n":9007199254740993}` || out.Usage.PromptTokens != 10 || out.Usage.CompletionTokens != 7 || out.Usage.TotalTokens != 17 {
		t.Fatalf("response: %+v, %v", out, err)
	}
	in.Messages = append(in.Messages, out.Message, Message{Role: "tool", ToolCallID: "tool_1", Content: `{}`})
	if _, err := c.completeAnthropic(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicReplaysThinkingBeforeFinalText(t *testing.T) {
	calls := 0
	const thinking = `{"type":"thinking","thinking":"hidden","signature":"sig"}`
	const text = `{"type":"text","text":"final"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			messages := string(body["messages"])
			thinkingAt, textAt := strings.Index(messages, thinking), strings.Index(messages, text)
			if thinkingAt < 0 || textAt < 0 || thinkingAt > textAt {
				t.Fatalf("thinking and final text were not replayed in order: %s", body["messages"])
			}
		}
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"hidden","signature":"sig"},{"type":"text","text":"final"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.completeAnthropic(context.Background(), Request{Messages: []Message{{Role: "user", Content: "first"}}})
	if err != nil || out.Message.Content != "final" || !jsonContains(out.Message.ProviderState, thinking) || !jsonContains(out.Message.ProviderState, text) {
		t.Fatalf("response: %+v, %v", out, err)
	}
	if _, err := c.completeAnthropic(context.Background(), Request{Messages: []Message{{Role: "user", Content: "first"}, out.Message, {Role: "user", Content: "continue"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicRequestAndToolResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "secret" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("request: %s %q %q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !jsonContains(body["system"], "JSON Schema") || !jsonContains(body["messages"], `"tool_result"`) || len(body["tools"]) == 0 {
			t.Fatalf("missing anthropic fields: %v", body)
		}
		_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tool_1","name":"conductor_emit","input":{}}],"usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.completeAnthropic(context.Background(), Request{
		Messages: []Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "old_1", Type: "function", Function: FunctionCall{Name: "f", Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "old_1", Content: `{}`}},
		Tools:    []Tool{{Type: "function", Function: Function{Name: "conductor_emit", Parameters: json.RawMessage(`{"type":"object"}`)}}}, Schema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil || out.FinishReason != "tool_calls" || out.Message.ToolCalls[0].ID != "tool_1" || out.Usage.TotalTokens != 5 {
		t.Fatalf("response: %+v, %v", out, err)
	}
}

func TestResponsesOpenCodeGoSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-opencode-session") != "sess_1" {
			t.Fatalf("missing session: %q", r.Header.Get("x-opencode-session"))
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer server.Close()
	c, err := New(Config{Kind: "opencode-go", API: "responses", BaseURL: server.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.completeResponses(context.Background(), Request{SessionID: "sess_1", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.completeResponses(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("expected session error")
	}
}

func jsonContains(raw json.RawMessage, fragment string) bool {
	return len(raw) != 0 && strings.Contains(string(raw), fragment)
}
