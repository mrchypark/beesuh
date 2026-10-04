package beesuh

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

func TestNativeAnthropicPreservesNumericToolInputEndToEnd(t *testing.T) {
	const integer = "9007199254740993"
	const decimal = "0.123456789012345678901234567890123456789"
	const exponent = "1.234567890123456789e+40"
	const arguments = `{"name":"deploy","data":{"nested":{"large":` + integer + `,"decimal":` + decimal + `,"exponent":` + exponent + `}}}`

	modelCalls := 0
	conductorCalls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		modelCalls++
		if modelCalls == 2 {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read Anthropic replay: %v", err)
				return
			}
			if !bytes.Contains(body, []byte(`"large":`+integer)) || !bytes.Contains(body, []byte(`"decimal":`+decimal)) || !bytes.Contains(body, []byte(`"exponent":`+exponent)) {
				t.Errorf("Anthropic replay lost numeric tool input: %s", body)
				return
			}
		}
		switch modelCalls {
		case 1:
			_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"emit","name":"conductor_emit","input":` + arguments + `}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		case 2:
			_, _ = w.Write([]byte(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			t.Fatalf("unexpected Anthropic request %d", modelCalls)
		}
	}))
	defer modelServer.Close()

	conductorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		conductorCalls++
		var body struct {
			Events []struct {
				Data json.RawMessage `json:"data"`
				Key  string          `json:"idempotency_key"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 {
			t.Errorf("invalid Conductor event: %v", err)
			return
		}
		var data struct {
			Nested json.RawMessage `json:"nested"`
		}
		if err := json.Unmarshal(body.Events[0].Data, &data); err != nil {
			t.Errorf("decode Conductor data: %v", err)
			return
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(data.Nested, &nested); err != nil {
			t.Errorf("decode nested Conductor data: %v", err)
			return
		}
		if string(nested["large"]) != integer || string(nested["decimal"]) != decimal || string(nested["exponent"]) != exponent {
			t.Errorf("Conductor data lost numeric lexemes: %s", body.Events[0].Data)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "events": []any{map[string]any{"execution_id": "run-1", "execution_request_id": body.Events[0].Key}}})
	}))
	defer conductorServer.Close()

	cond := &conductor.Config{BaseURL: conductorServer.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	opts := nativeOptions(t.TempDir(), t.TempDir(), modelServer.URL, cond)
	opts.Native.Provider.API = "anthropic"
	r := nativeOpen(t, opts)
	defer r.Close()

	req := Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "numeric-input", Prompt: opencode.Prompt{Text: "deploy"}}
	result, err := r.Run(context.Background(), req)
	if err != nil || string(result.Output) != `{"text":"done"}` || modelCalls != 2 {
		t.Fatalf("run = %+v, err=%v, model calls=%d", result, err, modelCalls)
	}
	if conductorCalls != 1 {
		t.Fatalf("Conductor calls = %d, want 1", conductorCalls)
	}

	history, err := r.History(context.Background(), req.WorkspaceID, req.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range history {
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
			t.Fatalf("tool-intent arguments = %s, want %s", intent.Call.Function.Arguments, arguments)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		r = nativeOpen(t, opts)
		defer r.Close()
		if _, err := r.Run(context.Background(), req); err != nil {
			t.Fatalf("reopened replay = %v", err)
		}
		if modelCalls != 2 || conductorCalls != 1 {
			t.Fatalf("reopened replay emitted again: model calls=%d conductor calls=%d", modelCalls, conductorCalls)
		}
		return
	}
	t.Fatal("tool intent was not recorded")
}
