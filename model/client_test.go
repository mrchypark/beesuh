package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompleteRequestShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("path or authorization: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["model"]) != `"local"` || len(body["tools"]) == 0 || len(body["response_format"]) == 0 {
			t.Fatalf("missing expected request fields: %v", body)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[{"id":"call_1","type":"function","function":{"name":"conductor_emit","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret", Model: "local"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: Function{Name: "conductor_emit", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		Schema:   json.RawMessage(`{"type":"object"}`), MaxTokens: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.FinishReason != "tool_calls" || out.Message.ToolCalls[0].ID != "call_1" || out.Usage.TotalTokens != 3 {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestCompleteRejectsStatusRedirectAndInvalidResponses(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		redirect   bool
	}{
		{"status", `nope`, http.StatusBadGateway, false},
		{"multiple choices", `{"choices":[{},{}]}`, http.StatusOK, false},
		{"truncated tool", `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`, http.StatusOK, false},
		{"redirect", ``, http.StatusFound, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.redirect {
					http.Redirect(w, r, "/elsewhere", http.StatusFound)
					return
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			c, err := New(Config{BaseURL: server.URL, Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCompleteCancellationAndConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client must return before this deliberately slow provider does.
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Complete(ctx, Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	for _, cfg := range []Config{{BaseURL: "ftp://example.com", Model: "m"}, {BaseURL: server.URL + "?x=1", Model: "m"}, {BaseURL: server.URL, Model: "line\nbreak"}, {BaseURL: server.URL, Model: "m", APIKey: "line\nbreak"}} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("expected invalid config: %+v", cfg)
		}
	}
	if _, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}, Schema: json.RawMessage(`[]`)}); err == nil || !strings.Contains(err.Error(), "Schema") {
		t.Fatalf("expected schema error, got %v", err)
	}
}
