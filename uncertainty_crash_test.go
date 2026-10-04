package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

// A dispatch that is acknowledged must not leave a pending row behind, or the
// workspace would be fenced forever after a healthy effect.
func TestAcknowledgedConductorEmitClearsPendingIntent(t *testing.T) {
	var models atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
		if models.Add(1) == 1 {
			completion(w, "emitted", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
			return
		}
		completion(w, "done", "stop")
	}))
	defer server.Close()
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		var body struct {
			Events []struct {
				IdempotencyKey string `json:"idempotency_key"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 || body.Events[0].IdempotencyKey == "" {
			t.Fatalf("conductor request carried no idempotency key: %v", err)
		}
		key := body.Events[0].IdempotencyKey
		_, _ = w.Write([]byte(`{"accepted":true,"events":[{"execution_id":"exec-1","execution_request_id":"` + key + `"}]}`))
	}))
	defer conduit.Close()

	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	r := nativeOpen(t, opts)
	defer r.Close()

	if _, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "ack", Prompt: opencode.Prompt{Text: "deploy"}}); err != nil {
		t.Fatal(err)
	}
	page, err := r.UnresolvedEffects(context.Background(), "w", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Effects) != 0 {
		t.Fatalf("acknowledged effect left a pending row: %+v", page.Effects)
	}
	// The workspace must stay usable for a second, independent effect.
	if _, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "second", Prompt: opencode.Prompt{Text: "deploy"}}); err != nil {
		t.Fatalf("workspace fenced after acknowledged effect: %v", err)
	}
}

// The pending row must exist BEFORE the POST leaves the process. A crash during
// the dispatch therefore still leaves durable evidence that the workspace may
// hold an accepted external effect.
func TestPendingIntentIsDurableBeforeDispatch(t *testing.T) {
	var models atomic.Int32
	entered := make(chan struct{})
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
		if models.Add(1) == 1 {
			completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
			return
		}
		completion(w, "done", "stop")
	}))
	defer server.Close()
	// This conductor never answers. The dispatch stays in flight, so the row
	// can be observed from a second reader while the POST has not completed.
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-blocked:
		}
	}))
	defer func() {
		close(blocked)
		conduit.Close()
	}()

	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	opts.Timeout = 3 * time.Second
	r := nativeOpen(t, opts)
	defer r.Close()

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "inflight", Prompt: opencode.Prompt{Text: "deploy"}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never reached the conductor")
	}
	// The POST is in flight and cannot have been acknowledged. The pending row
	// must already be readable, proving intent is committed before dispatch.
	page, err := r.UnresolvedEffects(context.Background(), "w", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Effects) != 1 {
		t.Fatalf("in-flight dispatch left %d pending rows, want 1", len(page.Effects))
	}
	if err := <-done; !errors.Is(err, ErrUncertain) {
		t.Fatalf("expected uncertain after timeout, got %v", err)
	}
}

// Reusing an effect key after it reached a terminal disposition must not let a
// new dispatch slip past the fence: the insert conflicts and the call is
// refused instead of silently proceeding.
func TestEffectKeyReuseAfterResolutionIsRefused(t *testing.T) {
	var models, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			if models.Add(1) == 1 {
				completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
				return
			}
			completion(w, "done", "stop")
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
		}
	}))
	defer server.Close()
	data, dir := t.TempDir(), t.TempDir()
	opts := nativeOptions(data, dir, server.URL, &conductor.Config{BaseURL: server.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	r := nativeOpen(t, opts)
	defer r.Close()

	_, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "reuse", Prompt: opencode.Prompt{Text: "deploy"}})
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("expected uncertain, got %v", err)
	}
	page, err := r.UnresolvedEffects(context.Background(), "w", "", 0)
	if err != nil || len(page.Effects) != 1 {
		t.Fatalf("expected one pending effect, got %+v (%v)", page.Effects, err)
	}
	key := page.Effects[0].EffectKey
	if err := r.ResolveUncertainEffect(context.Background(), "w", key, dispositionResolved); err != nil {
		t.Fatal(err)
	}
	// The key is now terminal. Replaying the same effect must be refused rather
	// than silently re-dispatching under a key that no longer fences.
	if _, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "reuse", Prompt: opencode.Prompt{Text: "deploy"}}); !errors.Is(err, ErrUncertain) && !errors.Is(err, ErrConflict) {
		t.Fatalf("reused effect key was admitted: %v", err)
	}
	if effects.Load() != 1 {
		t.Fatalf("effect re-dispatched %d times", effects.Load())
	}
}
