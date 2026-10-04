package beesuh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

func capacityRuntime(t *testing.T, f *fakeOpenCode, data string, capacity CapacityLimits) *Runtime {
	t.Helper()
	r, err := Open(t.Context(), Options{
		DataDir: data, BackendID: "capacity", MaxConcurrent: 2, MaxWaiting: 2,
		Timeout: 5 * time.Second, Capacity: capacity,
		OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCapacityIntegrationRunSlotsGlobalReplayAndReopen(t *testing.T) {
	t.Run("one workspace slot admits one concurrent run", func(t *testing.T) {
		f := newFakeOpenCode(t)
		f.promptGate = make(chan struct{})
		r := capacityRuntime(t, f, t.TempDir(), CapacityLimits{RunsPerWorkspace: 1, RunsGlobal: 10})
		defer r.Close()
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, req := range []Request{testRequest("a", "one", "one"), testRequest("b", "two", "two")} {
			wg.Add(1)
			go func(req Request) {
				defer wg.Done()
				<-start
				_, err := r.Run(context.Background(), req)
				results <- err
			}(req)
		}
		close(start)
		receive(t, f.prompted, "first prompt")
		close(f.promptGate)
		wg.Wait()
		close(results)
		var ok, capacity int
		for err := range results {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCapacity):
				capacity++
			default:
				t.Fatalf("run error = %v", err)
			}
		}
		if ok != 1 || capacity != 1 {
			t.Fatalf("success=%d capacity=%d, want one each", ok, capacity)
		}
		creates, prompts, _ := f.counts()
		if creates != 1 || prompts != 1 {
			t.Fatalf("remote creates=%d prompts=%d, want one each", creates, prompts)
		}
	})

	t.Run("global cap spans workspaces", func(t *testing.T) {
		f := newFakeOpenCode(t)
		r := capacityRuntime(t, f, t.TempDir(), CapacityLimits{RunsPerWorkspace: 10, RunsGlobal: 1})
		defer r.Close()
		if _, err := r.Run(context.Background(), testRequest("a", "one", "one")); err != nil {
			t.Fatal(err)
		}
		second := testRequest("b", "two", "two")
		second.WorkspaceID = "other"
		if _, err := r.Run(context.Background(), second); !errors.Is(err, ErrCapacity) {
			t.Fatalf("second error=%v, want capacity", err)
		}
		_, prompts, _ := f.counts()
		if prompts != 1 {
			t.Fatalf("prompts=%d, want one", prompts)
		}
	})

	t.Run("replay at capacity and conflict do not grow history", func(t *testing.T) {
		f := newFakeOpenCode(t)
		data := t.TempDir()
		r := capacityRuntime(t, f, data, CapacityLimits{RunsPerWorkspace: 2, RunsGlobal: 2})
		req := testRequest("a", "one", "one")
		if _, err := r.Run(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		secondReq := testRequest("b", "two", "two")
		if _, err := r.Run(context.Background(), secondReq); err != nil {
			t.Fatal(err)
		}
		before, err := r.History(context.Background(), req.WorkspaceID, req.SessionKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Run(context.Background(), req); err != nil {
			t.Fatalf("replay at capacity: %v", err)
		}
		conflict := req
		conflict.Prompt.Text = "changed"
		if _, err := r.Run(context.Background(), conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflict=%v", err)
		}
		after, err := r.History(context.Background(), req.WorkspaceID, req.SessionKey)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Fatalf("history grew from %d to %d", len(before), len(after))
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		r = capacityRuntime(t, f, data, CapacityLimits{RunsPerWorkspace: 1, RunsGlobal: 1})
		defer r.Close()
		if _, err := r.Run(context.Background(), req); err != nil {
			t.Fatalf("reopen replay: %v", err)
		}
		if _, err := r.Run(context.Background(), secondReq); err != nil {
			t.Fatalf("reopen second replay: %v", err)
		}
		third := testRequest("c", "three", "three")
		if _, err := r.Run(context.Background(), third); !errors.Is(err, ErrCapacity) {
			t.Fatalf("reopen third=%v, want capacity", err)
		}
		if _, err := r.History(context.Background(), req.WorkspaceID, req.SessionKey); err != nil {
			t.Fatalf("reopen inspection: %v", err)
		}
		_, prompts, _ := f.counts()
		if prompts != 2 {
			t.Fatalf("prompts=%d, want two original calls", prompts)
		}
	})
}

func TestCapacityIntegrationPendingCreateAndOversizedInputFailBeforeRemote(t *testing.T) {
	t.Run("pending create cap", func(t *testing.T) {
		f := newFakeOpenCode(t)
		r := capacityRuntime(t, f, t.TempDir(), CapacityLimits{PendingCreatesPerWorkspace: 1, PendingCreatesGlobal: 1})
		defer r.Close()
		seed := testRequest("seed", "seed", "seed")
		if err := r.reservePendingCreate(context.Background(), seed, "capacity-seed"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Run(context.Background(), testRequest("a", "one", "one")); !errors.Is(err, ErrCapacity) {
			t.Fatalf("error=%v, want capacity", err)
		}
		creates, prompts, _ := f.counts()
		if creates != 0 || prompts != 0 {
			t.Fatalf("remote creates=%d prompts=%d", creates, prompts)
		}
	})
	t.Run("oversized input", func(t *testing.T) {
		f := newFakeOpenCode(t)
		r := capacityRuntime(t, f, t.TempDir(), CapacityLimits{})
		defer r.Close()
		req := testRequest("a", "large", strings.Repeat("x", 256<<10+1))
		if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("error=%v, want invalid input", err)
		}
		creates, prompts, _ := f.counts()
		if creates != 0 || prompts != 0 {
			t.Fatalf("remote creates=%d prompts=%d", creates, prompts)
		}
	})
}

func TestCapacityIntegrationJournalRoomBeforeExternalPost(t *testing.T) {
	for _, tc := range []struct{ seed, posts int }{{251, 0}, {250, 1}} {
		t.Run(fmt.Sprintf("seed=%d", tc.seed), func(t *testing.T) {
			var providerCalls, conductorCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{"v":"x"}}`}})
			}))
			t.Cleanup(provider.Close)
			conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conductorCalls.Add(1)
				conductorFixtureAck(t, w, r)
			}))
			t.Cleanup(conduit.Close)
			cond := &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
			opts := nativeOptions(t.TempDir(), t.TempDir(), provider.URL, cond)
			r := nativeOpen(t, opts)
			t.Cleanup(func() { _ = r.Close() })
			req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "room", Prompt: Prompt{Text: "deploy"}}
			for i := 0; i < tc.seed; i++ {
				if err := r.appendJournal(context.Background(), req, "message", fmt.Sprintf(`{"n":%d}`, i)); err != nil {
					t.Fatal(err)
				}
			}
			out, err := r.Run(context.Background(), req)
			if !errors.Is(err, ErrCapacity) || out.State != "failed" || out.FailureCategory != FailureStorageCapacity {
				t.Fatalf("result=%+v error=%v", out, err)
			}
			if providerCalls.Load() != 1 || conductorCalls.Load() != int32(tc.posts) {
				t.Fatalf("provider=%d conductor=%d want1/%d", providerCalls.Load(), conductorCalls.Load(), tc.posts)
			}
			stored, err := r.GetRun(context.Background(), "w", "room")
			if err != nil || stored.State != "failed" {
				t.Fatalf("settlement=%+v error=%v", stored, err)
			}
			if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrCapacity) {
				t.Fatalf("replay=%v", err)
			}
			if providerCalls.Load() != 1 || conductorCalls.Load() != int32(tc.posts) {
				t.Fatal("capacity failure replay dispatched again")
			}
		})
	}
}

func TestCapacityIntegrationUnknownEffectSettlementAtRunCapacity(t *testing.T) {
	var posts atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
	}))
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		h, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := h.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer provider.Close()
	defer conduit.Close()
	cond := &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	opts := nativeOptions(t.TempDir(), t.TempDir(), provider.URL, cond)
	opts.Capacity = CapacityLimits{RunsPerWorkspace: 1, RunsGlobal: 1}
	r := nativeOpen(t, opts)
	defer r.Close()
	req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "unknown", Prompt: Prompt{Text: "deploy"}}
	if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrUncertain) {
		t.Fatalf("run error=%v, want uncertain", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("conductor posts=%d, want one", posts.Load())
	}
	effect := "beesuh_" + digest([]string{"w", "s", "unknown", "emit"})
	if err := r.ResolveUncertainEffect(context.Background(), "w", effect, dispositionResolved); err != nil {
		t.Fatalf("resolve at cap: %v", err)
	}
	if err := r.Recover(context.Background()); err != nil {
		t.Fatalf("recover at cap: %v", err)
	}
	status, err := r.Status(context.Background())
	if err != nil || status.Blocked {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestCapacityIntegrationAdmissionFailureDoesNotCreateRemoteSession(t *testing.T) {
	f := newFakeOpenCode(t)
	r := capacityRuntime(t, f, t.TempDir(), CapacityLimits{})
	defer r.Close()
	if err := r.mutate(context.Background(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_capacity_reservation BEFORE INSERT ON capacity_run_reservations BEGIN SELECT RAISE(ABORT, 'injected admission failure'); END`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), testRequest("a", "injected", "one")); err == nil {
		t.Fatal("run succeeded despite injected reservation failure")
	}
	creates, prompts, _ := f.counts()
	if creates != 0 || prompts != 0 {
		t.Fatalf("remote creates=%d prompts=%d, want zero", creates, prompts)
	}
}
