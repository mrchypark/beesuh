package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

// An operator reconciliation must not be able to lift the workspace fence while
// the POST for that effect is still on the wire. If it could, a second request
// would pass the reservation and the first POST would still reach Conductor,
// applying the same operation twice.
//
// The ordering matters as much as the lock. The intent row must be written while
// the fence read lock is already held, otherwise an operator can resolve the key
// in the gap between the INSERT and the lock acquisition. Driving the real
// nativeTool conductor_emit path is what makes the production order the thing
// under test instead of a hand-written sequence.
func TestResolutionWaitsForAnInFlightDispatch(t *testing.T) {
	dispatched := make(chan struct{})
	release := make(chan struct{})
	var posts atomic.Int32
	var appliedMu sync.Mutex
	var applied []string
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []struct {
				IdempotencyKey string `json:"idempotency_key"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 {
			t.Errorf("decode emit: %v", err)
		}
		appliedMu.Lock()
		applied = append(applied, body.Events[0].IdempotencyKey)
		appliedMu.Unlock()
		if posts.Add(1) == 1 {
			close(dispatched)
			<-release // The first POST is applied but its ACK has not returned.
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "events": []any{map[string]any{"execution_id": "run-1", "execution_request_id": body.Events[0].IdempotencyKey}}})
	}))
	defer conduit.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		completion(w, "done", "stop")
	}))
	defer server.Close()

	cond := &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, cond)
	r := nativeOpen(t, opts)
	defer r.Close()

	req := Request{WorkspaceID: "workspace", SessionKey: "conversation", RequestID: "one", Prompt: opencode.Prompt{Text: "deploy"}}
	effectKey := "beesuh_" + digest([]string{"workspace", "conversation", "one", "emit"})

	emit := make(chan error, 1)
	go func() {
		_, err := r.nativeTool(context.Background(), req, model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{"version":"1"}}`}})
		emit <- err
	}()
	<-dispatched // The first POST is in flight and its effect already applied.

	resolved := make(chan error, 1)
	go func() {
		resolved <- r.resolveUncertainEffect(context.Background(), "workspace", effectKey, dispositionAbandoned)
	}()

	select {
	case err := <-resolved:
		close(release)
		t.Fatalf("resolution completed while the POST was in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// The fence is still closed, so a second request cannot reserve the effect.
	if err := r.assertWorkspaceRecoverable(context.Background(), "workspace"); err == nil {
		close(release)
		t.Fatal("in-flight effect did not keep the workspace fenced")
	}
	// A second, differently keyed effect in the same workspace must also be
	// refused while the first one is still on the wire.
	if _, err := r.nativeTool(context.Background(), Request{WorkspaceID: "workspace", SessionKey: "conversation", RequestID: "two"}, model.ToolCall{ID: "other", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}}); !errors.Is(err, ErrUncertain) {
		close(release)
		t.Fatalf("second effect in a fenced workspace: %v", err)
	}

	close(release)
	if err := <-emit; err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	select {
	case err := <-resolved:
		// The acknowledged dispatch already cleared the pending row, so an
		// operator disposition has nothing left to update. That precondition
		// failure is the correct outcome here, and it proves the acknowledgement
		// cleanup ran inside the fence rather than racing the resolver.
		if err == nil {
			t.Fatal("resolution updated a row the acknowledged dispatch already cleared")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolution did not complete after the dispatch finished")
	}
	appliedMu.Lock()
	defer appliedMu.Unlock()
	if len(applied) != 1 || applied[0] != effectKey {
		t.Fatalf("applied effects = %v, want exactly one %q", applied, effectKey)
	}
}

// The fence read lock must be taken before the pending row is written. Taking
// the write lock first pins the ordering: while a resolver is queued, the
// dispatch must not yet have produced a durable intent row. A row written in
// that gap could be resolved away before the POST left the process.
func TestIntentIsWrittenInsideTheFenceCriticalSection(t *testing.T) {
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []struct {
				IdempotencyKey string `json:"idempotency_key"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 {
			t.Errorf("decode emit: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "events": []any{map[string]any{"execution_id": "run-1", "execution_request_id": body.Events[0].IdempotencyKey}}})
	}))
	defer conduit.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completion(w, "done", "stop")
	}))
	defer server.Close()

	cond := &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, cond)
	r := nativeOpen(t, opts)
	defer r.Close()

	req := Request{WorkspaceID: "workspace", SessionKey: "conversation", RequestID: "one"}
	r.fenceMu.Lock()
	emit := make(chan error, 1)
	go func() {
		_, err := r.nativeTool(context.Background(), req, model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
		emit <- err
	}()

	// The dispatch cannot reach recordUncertainIntent while the write lock is
	// held, so no pending row may exist yet.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		rows, err := r.query(context.Background(), `SELECT effect_key FROM uncertain_effects WHERE workspace=?`, "workspace")
		if err != nil {
			r.fenceMu.Unlock()
			t.Fatal(err)
		}
		if len(rows) != 0 {
			r.fenceMu.Unlock()
			t.Fatalf("intent row %v exists before the fence read lock is acquired", rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.fenceMu.Unlock()

	if err := <-emit; err != nil {
		t.Fatalf("dispatch after the lock released: %v", err)
	}
	rows, err := r.query(context.Background(), `SELECT effect_key FROM uncertain_effects WHERE workspace=?`, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("acknowledged dispatch left %v, want no pending row", rows)
	}
}
