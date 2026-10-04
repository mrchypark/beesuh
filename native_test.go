package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

func nativeOptions(dataDir, directory, baseURL string, cond *conductor.Config) Options {
	return Options{
		DataDir: dataDir, BackendID: "local", MaxConcurrent: 2, MaxWaiting: 2,
		OpenCode: opencodeConfig(directory),
		Native:   &NativeOptions{Provider: model.Config{BaseURL: baseURL + "/v1", Model: "test"}, Conductor: cond},
	}
}

func opencodeConfig(directory string) opencode.Config { return opencode.Config{Directory: directory} }

func nativeOpen(t *testing.T, opts Options) *Runtime {
	t.Helper()
	r, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func completion(w http.ResponseWriter, content, finish string, calls ...model.ToolCall) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": model.Message{Role: "assistant", Content: content, ToolCalls: calls}, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
}

func TestNativePersistsHistoryToolJournalAndReplay(t *testing.T) {
	var models, events, gets atomic.Int32
	var effectKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			var in struct {
				Messages []model.Message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Fatal(err)
			}
			n := models.Add(1)
			switch n {
			case 1:
				completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{"version":"1"}}`}})
			case 2:
				completion(w, "", "tool_calls", model.ToolCall{ID: "status", Type: "function", Function: model.FunctionCall{Name: "conductor_get_run", Arguments: `{"run_id":"run-1"}`}})
			case 3:
				completion(w, "done", "stop")
			case 4:
				found := false
				for _, message := range in.Messages {
					found = found || message.Content == "done"
				}
				if !found {
					t.Fatal("persisted history was not sent")
				}
				completion(w, "again", "stop")
			default:
				t.Fatalf("unexpected model call %d", n)
			}
		case "/v1/events":
			events.Add(1)
			var body struct {
				Events []struct {
					IdempotencyKey string `json:"idempotency_key"`
				} `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Events) != 1 {
				t.Fatal("expected one event")
			}
			effectKey = body.Events[0].IdempotencyKey
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "events": []any{map[string]any{"execution_id": "run-1", "execution_request_id": effectKey, "status": "queued"}}})
		case "/v1/executions/run-1":
			gets.Add(1)
			_, _ = w.Write([]byte(`{"state":"running"}`))
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()
	dir, data := t.TempDir(), t.TempDir()
	cond := &conductor.Config{BaseURL: server.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	opts := nativeOptions(data, dir, server.URL, cond)
	r := nativeOpen(t, opts)
	req := Request{WorkspaceID: "workspace", SessionKey: "conversation", RequestID: "one", Prompt: opencode.Prompt{Text: "deploy version 1"}}
	first, err := r.Run(context.Background(), req)
	if err != nil || string(first.Output) != `{"text":"done"}` {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if effectKey != "beesuh_"+digest([]string{"workspace", "conversation", "one", "emit"}) {
		t.Fatalf("unexpected effect key %q", effectKey)
	}
	replayed, err := r.Run(context.Background(), req)
	if err != nil || replayed.SessionID != first.SessionID || replayed.State != first.State || string(replayed.Output) != string(first.Output) || models.Load() != 3 {
		t.Fatalf("replay = %+v, %v, calls=%d", replayed, err, models.Load())
	}
	journal, err := r.History(context.Background(), "workspace", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, entry := range journal {
		kinds = append(kinds, entry.Kind)
	}
	want := []string{"message", "model", "message", "tool_intent", "tool_result", "message", "model", "message", "tool_intent", "tool_result", "message", "model", "message"}
	if !equalStrings(kinds, want) {
		t.Fatalf("journal order = %v", kinds)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	defer r.Close()
	second, err := r.Run(context.Background(), Request{WorkspaceID: "workspace", SessionKey: "conversation", RequestID: "two", Prompt: opencode.Prompt{Text: "what happened?"}})
	if err != nil || second.SessionID != first.SessionID || string(second.Output) != `{"text":"again"}` {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if models.Load() != 4 || events.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("unexpected calls: model=%d event=%d get=%d", models.Load(), events.Load(), gets.Load())
	}
}

func TestNativeAnthropicPreservesLongDecimalThroughJournalAndConductor(t *testing.T) {
	const decimal = "0.1234567890123456789012345678901234567890"
	const arguments = `{"name":"deploy","data":{"amount":` + decimal + `}}`
	var eventData json.RawMessage
	models := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			models++
			if models == 1 {
				_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"emit","name":"conductor_emit","input":` + arguments + `}],"usage":{"input_tokens":1,"output_tokens":1}}`))
				return
			}
			_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		case "/v1/events":
			var body struct {
				Events []struct {
					Data json.RawMessage `json:"data"`
					Key  string          `json:"idempotency_key"`
				} `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 {
				t.Fatal("invalid event payload")
			}
			eventData = body.Events[0].Data
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "events": []any{map[string]any{"execution_id": "run-1", "execution_request_id": body.Events[0].Key, "status": "queued"}}})
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cond := &conductor.Config{BaseURL: server.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, cond)
	opts.Native.Provider.API = "anthropic"
	r := nativeOpen(t, opts)
	defer r.Close()
	result, err := r.Run(context.Background(), Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "request", Prompt: opencode.Prompt{Text: "deploy"}})
	if err != nil || string(result.Output) != `{"text":"done"}` {
		t.Fatalf("run: %+v, %v", result, err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(eventData, &payload); err != nil || string(payload["amount"]) != decimal {
		t.Fatalf("Conductor payload lost decimal: %s", eventData)
	}
	journal, err := r.History(context.Background(), "workspace", "session")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range journal {
		if entry.Kind != "tool_intent" {
			continue
		}
		var intent struct {
			Call model.ToolCall `json:"call"`
		}
		if err := json.Unmarshal(entry.Payload, &intent); err != nil {
			t.Fatal(err)
		}
		if intent.Call.Function.Arguments != arguments {
			t.Fatalf("journal arguments = %s", intent.Call.Function.Arguments)
		}
		return
	}
	t.Fatal("tool intent was not recorded")
}

func TestNativeRejectsSchemaAndUnconfiguredTool(t *testing.T) {
	var models, conductorCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			switch models.Add(1) {
			case 1:
				completion(w, `{"wrong":true}`, "stop")
			case 2:
				completion(w, "", "tool_calls", model.ToolCall{ID: "bad", Type: "function", Function: model.FunctionCall{Name: "not_enabled", Arguments: `{}`}})
			case 3:
				completion(w, "safe", "stop")
			default:
				t.Fatal("unexpected model call")
			}
		case "/v1/events", "/v1/executions/run":
			conductorCalls.Add(1)
			t.Fatal("unconfigured tool reached Conductor")
		}
	}))
	defer server.Close()
	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, &conductor.Config{BaseURL: server.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	r := nativeOpen(t, opts)
	defer r.Close()
	_, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "schema", Prompt: opencode.Prompt{Text: "json", Schema: json.RawMessage(`{"type":"object","required":["ok"]}`)}})
	if err == nil || !strings.Contains(err.Error(), "output does not match") {
		t.Fatalf("schema err = %v", err)
	}
	out, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "other", RequestID: "tool", Prompt: opencode.Prompt{Text: "bad tool"}})
	if err != nil || string(out.Output) != `{"text":"safe"}` || conductorCalls.Load() != 0 {
		t.Fatalf("tool run = %+v, %v, conductor=%d", out, err, conductorCalls.Load())
	}
}

func TestNativeTurnLimitAndOutputBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatal(r.URL.Path)
		}
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.MaxTokens != 7 {
			t.Fatalf("max_tokens=%d", body.MaxTokens)
		}
		calls.Add(1)
		completion(w, "", "tool_calls", model.ToolCall{ID: "call", Type: "function", Function: model.FunctionCall{Name: "load_skill", Arguments: `{"name":"only"}`}})
	}))
	defer server.Close()
	skills := t.TempDir()
	if err := os.WriteFile(filepath.Join(skills, "only.md"), []byte("procedure"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, nil)
	opts.Native.MaxTurns, opts.Native.MaxOutputTokens, opts.Native.SkillsDir = 1, 7, skills
	r := nativeOpen(t, opts)
	defer r.Close()
	_, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "limit", Prompt: opencode.Prompt{Text: "do it"}})
	if err == nil || !strings.Contains(err.Error(), "maximum model turns") || calls.Load() != 1 {
		t.Fatalf("limit err=%v calls=%d", err, calls.Load())
	}
}

func TestNativeUncertainEffectRequiresRecoveryWithoutRetry(t *testing.T) {
	var models, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			if models.Add(1) == 1 {
				completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
			} else {
				completion(w, "recovered", "stop")
			}
		case "/v1/events":
			effects.Add(1)
			h, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server cannot hijack")
			}
			conn, _, err := h.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	data, dir := t.TempDir(), t.TempDir()
	opts := nativeOptions(data, dir, server.URL, &conductor.Config{BaseURL: server.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	r := nativeOpen(t, opts)
	_, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "effect", Prompt: opencode.Prompt{Text: "deploy"}})
	if !errors.Is(err, ErrUncertain) || effects.Load() != 1 {
		t.Fatalf("effect err=%v effects=%d", err, effects.Load())
	}
	if _, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "blocked", Prompt: opencode.Prompt{Text: "anything"}}); !errors.Is(err, ErrUncertain) {
		t.Fatalf("blocked err=%v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	if err := r.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 1 {
		t.Fatalf("Recover reissued effect: %d", effects.Load())
	}
	defer r.Close()
	out, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "after", Prompt: opencode.Prompt{Text: "continue"}})
	if err != nil || string(out.Output) != `{"text":"recovered"}` || effects.Load() != 1 {
		t.Fatalf("after = %+v, %v", out, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The fixture mirrors the single-execution acknowledgement, including its linkage.
func conductorFixtureAck(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body struct {
		Events []struct {
			Key string `json:"idempotency_key"`
		} `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 {
		t.Error("invalid fixture envelope")
		w.WriteHeader(400)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "events": []any{map[string]any{"execution_id": "fixture-execution", "execution_request_id": body.Events[0].Key, "status": "queued"}}})
}

func TestNativeRejectsReservedConductorMetadataWithoutEffect(t *testing.T) {
	for _, key := range []string{"_compos", "subject_ref"} {
		t.Run(key, func(t *testing.T) {
			var models, effects atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/events" {
					effects.Add(1)
					conductorFixtureAck(t, w, r)
					return
				}
				if models.Add(1) == 1 {
					completion(w, "", "tool_calls", model.ToolCall{ID: "reserved", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"echo","data":{"` + key + `":{}}}`}})
					return
				}
				completion(w, "rejected", "stop")
			}))
			defer server.Close()
			r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), server.URL, &conductor.Config{BaseURL: server.URL, AllowedEvents: []string{"echo"}, Bindings: map[string]conductor.Binding{"echo": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}))
			defer r.Close()
			req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", Prompt: Prompt{Text: "echo"}}
			if _, err := r.Run(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if effects.Load() != 0 || runFencedNow(r) {
				t.Fatal("reserved metadata sent an effect or blocked runtime")
			}
			history, err := r.History(context.Background(), "w", "s")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range history {
				if entry.Kind == "tool_intent" {
					t.Fatal("reserved data recorded an effect intent")
				}
			}
		})
	}
}
