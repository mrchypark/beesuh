package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatUsageMarksIncompleteValuesUnknown(t *testing.T) {
	tests := []struct {
		name, usage string
		want        Usage
	}{
		{name: "omitted", want: unknownUsage()},
		{name: "null", usage: `,"usage":null`, want: unknownUsage()},
		{name: "partial", usage: `,"usage":{"prompt_tokens":1}`, want: unknownUsage()},
		{name: "negative", usage: `,"usage":{"prompt_tokens":-1,"completion_tokens":0,"total_tokens":0}`, want: unknownUsage()},
		{name: "zeros", usage: `,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, want: Usage{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]` + tt.usage + `}`))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
			if err != nil || out.Usage != tt.want || out.Usage.Available() != tt.want.Available() {
				t.Fatalf("usage=%+v available=%t err=%v, want %+v", out.Usage, out.Usage.Available(), err, tt.want)
			}
		})
	}
}

func TestResponsesUsageMarksIncompleteValuesUnknown(t *testing.T) {
	tests := []struct {
		name, usage string
		want        Usage
	}{
		{name: "omitted", want: unknownUsage()},
		{name: "null", usage: `,"usage":null`, want: unknownUsage()},
		{name: "partial", usage: `,"usage":{"input_tokens":1}`, want: unknownUsage()},
		{name: "negative", usage: `,"usage":{"input_tokens":-1,"output_tokens":0,"total_tokens":0}`, want: unknownUsage()},
		{name: "zeros", usage: `,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, want: Usage{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]` + tt.usage + `}`))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, API: "responses", Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
			if err != nil || out.Usage != tt.want || out.Usage.Available() != tt.want.Available() {
				t.Fatalf("usage=%+v available=%t err=%v, want %+v", out.Usage, out.Usage.Available(), err, tt.want)
			}
		})
	}
}

func TestAnthropicUsageMarksIncompleteValuesUnknown(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name, usage string
		want        Usage
	}{
		{name: "omitted", want: unknownUsage()},
		{name: "null", usage: `,"usage":null`, want: unknownUsage()},
		{name: "partial", usage: `,"usage":{"input_tokens":1}`, want: unknownUsage()},
		{name: "negative cache", usage: `,"usage":{"input_tokens":1,"cache_read_input_tokens":-1,"output_tokens":0}`, want: unknownUsage()},
		{name: "negative output", usage: `,"usage":{"input_tokens":1,"output_tokens":-1}`, want: unknownUsage()},
		{name: "zeros", usage: `,"usage":{"input_tokens":0,"output_tokens":0}`, want: Usage{}},
		{name: "cached", usage: `,"usage":{"input_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3,"output_tokens":4}`, want: Usage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}},
		{name: "overflow", usage: fmt.Sprintf(`,"usage":{"input_tokens":%d,"cache_read_input_tokens":1,"output_tokens":0}`, maxInt), want: unknownUsage()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]` + tt.usage + `}`))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, API: "anthropic", Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
			if err != nil || out.Usage != tt.want || out.Usage.Available() != tt.want.Available() {
				t.Fatalf("usage=%+v available=%t err=%v, want %+v", out.Usage, out.Usage.Available(), err, tt.want)
			}
		})
	}
}
