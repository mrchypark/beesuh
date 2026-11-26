package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/rhiza"
)

type asyncContextKey struct{}

func asyncProvider(t *testing.T, calls *atomic.Int32, text string, started chan<- struct{}, waitForCancel bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if waitForCancel {
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if _, err := w.Write([]byte(" ")); err != nil {
						return
					}
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(server.Close)
	return server
}

func asyncRuntime(t *testing.T, data, providerURL string, prepare func(context.Context, Principal) (context.Context, error)) *Runtime {
	t.Helper()
	r, err := Open(context.Background(), Options{
		DataDir: data, BackendID: "async-test", RequireIdentity: true,
		Authorize: func(context.Context, Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{APIKey: "credential-canary", AccountID: c.UserID}, nil
		},
		PrepareAsync:  prepare,
		MaxConcurrent: 2, MaxWaiting: 4, Timeout: 2 * time.Second,
		Capacity: CapacityLimits{RunsPerWorkspace: 20, RunsGlobal: 20},
		Native: &NativeOptions{
			Providers: map[string]model.Config{"provider": {BaseURL: providerURL, Model: "test"}}, DefaultProvider: "provider",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	for _, user := range []string{"alice", "bob"} {
		if err := r.RegisterConnection(context.Background(), Connection{ID: user + "-model", UserID: user, WorkspaceID: "work", Kind: "provider", TargetID: "provider", CredentialRef: user, State: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func principalContext(user string) context.Context {
	return WithPrincipal(context.Background(), Principal{UserID: user, WorkspaceID: "work"})
}

func asyncRequest(user, request, text string) Request {
	return Request{WorkspaceID: "work", SessionKey: "session", RequestID: request, Provider: "provider", ConnectionID: user + "-model", Prompt: Prompt{Text: text}}
}

func waitForRun(t *testing.T, r *Runtime, user, request, state string) Result {
	t.Helper()
	var got Result
	eventually(t, "run "+request+" reaches "+state, func() bool {
		var err error
		got, err = r.GetRun(principalContext(user), "work", request)
		return err == nil && got.State == state
	})
	return got
}

func TestAsyncEnqueueDeduplicatesAndWorkerUsesRun(t *testing.T) {
	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "queued answer", nil, false)
	var hooks atomic.Int32
	authorized := make(chan Principal, 8)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, p Principal) (context.Context, error) {
		hooks.Add(1)
		authorized <- p
		return context.WithValue(ctx, asyncContextKey{}, "bearer-canary"), nil
	})
	ctx := context.WithValue(principalContext("alice"), asyncContextKey{}, "bearer-canary")
	req := asyncRequest("alice", "one", "hello")
	first, err := r.Enqueue(ctx, req)
	if err != nil || first.State != "queued" {
		t.Fatalf("enqueue = %#v, %v", first, err)
	}
	second, err := r.Enqueue(ctx, req)
	if err != nil || second.State != "queued" || calls.Load() != 0 {
		t.Fatalf("idempotent enqueue = %#v, %v, calls=%d", second, err, calls.Load())
	}
	conflictReq := req
	conflictReq.Prompt.Text = "different"
	if _, err := r.Enqueue(ctx, conflictReq); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}
	queued := waitForRun(t, r, "alice", "one", "queued")
	if queued.RequestID != "one" {
		t.Fatalf("queued = %#v", queued)
	}
	if syncOut, err := r.Run(ctx, req); err != nil || syncOut.State != "queued" || calls.Load() != 0 {
		t.Fatalf("sync/async dedupe = %#v, %v, calls=%d", syncOut, err, calls.Load())
	}
	if err := r.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := waitForRun(t, r, "alice", "one", "succeeded")
	if !strings.Contains(string(done.Output), "queued answer") || calls.Load() != 1 {
		t.Fatalf("result = %#v calls=%d", done, calls.Load())
	}
	eventually(t, "task row cleanup", func() bool {
		rows, err := r.query(context.Background(), "SELECT COUNT(*) FROM agent_tasks")
		return err == nil && rows[0][0].(int64) == 0
	})
	if hooks.Load() < 2 {
		t.Fatalf("prepare hook calls = %d, want enqueue preflight and worker", hooks.Load())
	}
	for len(authorized) != 0 {
		if p := <-authorized; p.UserID != "alice" || p.WorkspaceID != "work" {
			t.Fatalf("prepare principal = %#v", p)
		}
	}
	var stored strings.Builder
	for _, query := range []string{"SELECT principal || request || input_hash FROM agent_tasks", "SELECT result FROM runs", "SELECT payload FROM journal"} {
		rows, err := r.query(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			for _, value := range row {
				if s, ok := value.(string); ok {
					stored.WriteString(s)
				}
			}
		}
	}
	if strings.Contains(stored.String(), "bearer-canary") || strings.Contains(stored.String(), "credential-canary") {
		t.Fatal("task persistence retained request or model credential")
	}
}

func TestAsyncConductorResolvesCredentialsAtExecution(t *testing.T) {
	const (
		providerSecret  = "provider-async-canary"
		conductorSecret = "conductor-async-canary"
		humanCanary     = "human-async-canary"
	)
	var prepareCalls, providerResolutions, conductorResolutions, modelCalls, effects atomic.Int32
	modelHeaders := make(chan http.Header, 2)
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		modelHeaders <- r.Header.Clone()
		if modelCalls.Add(1) == 1 {
			completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
			return
		}
		completion(w, "async conductor done", "stop")
	}))
	defer modelServer.Close()

	type conductorRequest struct {
		header http.Header
		key    string
		name   string
	}
	observed := make(chan conductorRequest, 1)
	conductorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		effects.Add(1)
		var body struct {
			Events []struct {
				Name           string          `json:"name"`
				IdempotencyKey string          `json:"idempotency_key"`
				Data           json.RawMessage `json:"data"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 {
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		event := body.Events[0]
		observed <- conductorRequest{header: r.Header.Clone(), key: event.IdempotencyKey, name: event.Name}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "events": []any{map[string]any{
			"execution_id": "fixture-execution", "execution_request_id": event.IdempotencyKey, "status": "queued",
		}}})
	}))
	defer conductorServer.Close()

	r, err := Open(context.Background(), Options{
		DataDir: t.TempDir(), BackendID: "async-conductor", RequireIdentity: true,
		Authorize: func(context.Context, Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			switch c.Kind {
			case "provider":
				providerResolutions.Add(1)
				return Credential{APIKey: providerSecret, AccountID: c.UserID}, nil
			case "conductor":
				conductorResolutions.Add(1)
				return Credential{APIKey: conductorSecret, AccountID: c.UserID}, nil
			default:
				return Credential{}, ErrForbidden
			}
		},
		PrepareAsync: func(ctx context.Context, p Principal) (context.Context, error) {
			prepareCalls.Add(1)
			if p.UserID != "alice" || p.WorkspaceID != "work" {
				return nil, ErrForbidden
			}
			return context.WithValue(ctx, asyncContextKey{}, humanCanary), nil
		},
		MaxConcurrent: 1, MaxWaiting: 2, Timeout: 5 * time.Second,
		Capacity: CapacityLimits{RunsPerWorkspace: 10, RunsGlobal: 10},
		Native: &NativeOptions{
			Providers:       map[string]model.Config{"provider": {BaseURL: modelServer.URL + "/v1", Model: "test", APIKey: "fixed-model-provider-canary"}},
			DefaultProvider: "provider",
			Conductors: map[string]conductor.Config{
				"workflow": {
					BaseURL: conductorServer.URL, Token: "fixed-gateway-canary", RequireScope: true,
					AllowedEvents: []string{"deploy"},
					Bindings:      map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, c := range []Connection{
		{ID: "alice-model", UserID: "alice", WorkspaceID: "work", Kind: "provider", TargetID: "provider", CredentialRef: "alice-model", State: "ready"},
		{ID: "alice-conductor", UserID: "alice", WorkspaceID: "work", Kind: "conductor", TargetID: "workflow", CredentialRef: "alice-conductor", State: "ready"},
	} {
		if err := r.RegisterConnection(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	providerResolutions.Store(0)
	conductorResolutions.Store(0)

	req := asyncRequest("alice", "async-conductor", "deploy")
	req.ConductorConnectionID = "alice-conductor"
	if out, err := r.Enqueue(context.WithValue(principalContext("alice"), asyncContextKey{}, humanCanary), req); err != nil || out.State != "queued" {
		t.Fatalf("enqueue = %#v, %v", out, err)
	}
	if prepareCalls.Load() != 1 || providerResolutions.Load() != 0 || conductorResolutions.Load() != 0 || modelCalls.Load() != 0 || effects.Load() != 0 {
		t.Fatalf("enqueue side effects: prepare=%d provider=%d conductor=%d model=%d effects=%d", prepareCalls.Load(), providerResolutions.Load(), conductorResolutions.Load(), modelCalls.Load(), effects.Load())
	}
	rows, err := r.query(context.Background(), "SELECT principal || request FROM agent_tasks")
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0][0].(string), "alice-model") || !strings.Contains(rows[0][0].(string), "alice-conductor") {
		t.Fatalf("stored task = %v, %v", rows, err)
	}

	if err := r.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := waitForRun(t, r, "alice", req.RequestID, "succeeded")
	if !strings.Contains(string(done.Output), "async conductor done") {
		t.Fatalf("result = %#v", done)
	}
	if prepareCalls.Load() != 2 || providerResolutions.Load() != 2 || conductorResolutions.Load() != 1 || modelCalls.Load() != 2 || effects.Load() != 1 {
		t.Fatalf("execution calls: prepare=%d provider=%d conductor=%d model=%d effects=%d", prepareCalls.Load(), providerResolutions.Load(), conductorResolutions.Load(), modelCalls.Load(), effects.Load())
	}

	for i := int32(0); i < modelCalls.Load(); i++ {
		header := <-modelHeaders
		if header.Get("Authorization") != "Bearer "+providerSecret || header.Get("X-Beesuh-User-ID") != "" || header.Get("X-Beesuh-Workspace-ID") != "" || header.Get("X-Beesuh-Connection-ID") != "" {
			t.Fatalf("model headers = %v", header)
		}
	}
	effect := <-observed
	scopedWorkspace, err := r.scopedWorkspace(principalContext("alice"), "work")
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "beesuh_" + digest([]string{scopedWorkspace, req.SessionKey, req.RequestID, "emit"})
	if effect.header.Get("Authorization") != "Bearer "+conductorSecret || effect.header.Get("X-Beesuh-User-ID") != "alice" || effect.header.Get("X-Beesuh-Workspace-ID") != "work" || effect.header.Get("X-Beesuh-Connection-ID") != "alice-conductor" || effect.name != "deploy" || effect.key != wantKey {
		t.Fatalf("conductor request = header:%v name:%q key:%q want:%q", effect.header, effect.name, effect.key, wantKey)
	}

	eventually(t, "completed async conductor task cleanup", func() bool {
		rows, err := r.query(context.Background(), "SELECT COUNT(*) FROM agent_tasks")
		return err == nil && rows[0][0].(int64) == 0
	})
	var stored strings.Builder
	for _, query := range []string{"SELECT principal || request || input_hash FROM agent_tasks", "SELECT result FROM runs", "SELECT payload FROM journal"} {
		rows, err := r.query(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			for _, value := range row {
				if s, ok := value.(string); ok {
					stored.WriteString(s)
				}
			}
		}
	}
	for _, canary := range []string{providerSecret, conductorSecret, humanCanary, "fixed-model-provider-canary", "fixed-gateway-canary"} {
		if strings.Contains(stored.String(), canary) {
			t.Fatalf("task, run, or journal retained %q", canary)
		}
	}
}

func TestAsyncIdentityWorkspacesAreIsolated(t *testing.T) {
	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
	for _, user := range []string{"alice", "bob"} {
		if _, err := r.Enqueue(principalContext(user), asyncRequest(user, "same", "hello")); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRun(t, r, "alice", "same", "succeeded")
	waitForRun(t, r, "bob", "same", "succeeded")
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want one per identity", calls.Load())
	}
}

func TestAsyncEnqueueFailsClosedWithoutPrepareHook(t *testing.T) {
	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, nil)
	if _, err := r.Enqueue(principalContext("alice"), asyncRequest("alice", "blocked", "hello")); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("enqueue without hook = %v", err)
	}
	rows, err := r.query(context.Background(), "SELECT COUNT(*) FROM agent_tasks")
	if err != nil || rows[0][0].(int64) != 0 || calls.Load() != 0 {
		t.Fatalf("no-hook storage = %v rows=%v calls=%d", err, rows, calls.Load())
	}
}

func TestEnqueueDedupeSkipsPrepareAsyncPreflight(t *testing.T) {
	t.Run("queued task", func(t *testing.T) {
		var calls, hookCalls atomic.Int32
		provider := asyncProvider(t, &calls, "answer", nil, false)
		r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) {
			hookCalls.Add(1)
			return ctx, nil
		})
		req := asyncRequest("alice", "dedupe-queued", "hello")
		if _, err := r.Enqueue(principalContext("alice"), req); err != nil {
			t.Fatal(err)
		}
		before := hookCalls.Load()
		r.opts.PrepareAsync = nil
		out, err := r.Enqueue(principalContext("alice"), req)
		if err != nil || out.State != "queued" || hookCalls.Load() != before {
			t.Fatalf("queued duplicate = %#v, %v, hooks=%d", out, err, hookCalls.Load())
		}
	})

	t.Run("terminal run", func(t *testing.T) {
		var calls, hookCalls atomic.Int32
		provider := asyncProvider(t, &calls, "answer", nil, false)
		r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) {
			hookCalls.Add(1)
			return ctx, nil
		})
		req := asyncRequest("alice", "dedupe-terminal", "hello")
		if _, err := r.Enqueue(principalContext("alice"), req); err != nil {
			t.Fatal(err)
		}
		if err := r.StartAgentWorker(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitForRun(t, r, "alice", req.RequestID, "succeeded")
		before := hookCalls.Load()
		r.opts.PrepareAsync = nil
		out, err := r.Enqueue(principalContext("alice"), req)
		if err != nil || out.State != "succeeded" || hookCalls.Load() != before {
			t.Fatalf("terminal duplicate = %#v, %v, hooks=%d", out, err, hookCalls.Load())
		}
	})
}

func TestCancelRunDoesNotWaitForPrepareAsyncPreflight(t *testing.T) {
	var calls, hookCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) {
		if hookCalls.Add(1) == 1 {
			return ctx, nil
		}
		close(entered)
		<-release
		return ctx, nil
	})
	ctx := principalContext("alice")
	if _, err := r.Enqueue(ctx, asyncRequest("alice", "already-queued", "hello")); err != nil {
		t.Fatal(err)
	}
	enqueueDone := make(chan struct {
		out Result
		err error
	}, 1)
	go func() {
		out, err := r.Enqueue(ctx, asyncRequest("alice", "new-task", "hello"))
		enqueueDone <- struct {
			out Result
			err error
		}{out: out, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second enqueue did not enter PrepareAsync preflight")
	}
	cancelCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cancelled, err := r.CancelRun(cancelCtx, "work", "already-queued")
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel during preflight = %#v, %v", cancelled, err)
	}
	unblock()
	select {
	case result := <-enqueueDone:
		if result.err != nil || result.out.State != "queued" {
			t.Fatalf("post-preflight enqueue = %#v, %v", result.out, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue did not finish after preflight release")
	}
}

func TestAgentTaskPayloadLimitAndFailedPreflight(t *testing.T) {
	req := asyncRequest("alice", "large", strings.Repeat("x", maxAgentTaskBytes))
	if _, _, err := encodeAgentTask(Principal{UserID: "alice", WorkspaceID: "work"}, req); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized task payload = %v", err)
	}

	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(context.Context, Principal) (context.Context, error) {
		return nil, errors.New("host credential exchange failed")
	})
	if _, err := r.Enqueue(principalContext("alice"), asyncRequest("alice", "preflight", "hello")); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("failed preflight = %v", err)
	}
	rows, err := r.query(context.Background(), "SELECT COUNT(*) FROM agent_tasks")
	if err != nil || rows[0][0].(int64) != 0 || calls.Load() != 0 {
		t.Fatalf("failed preflight stored work: %v rows=%v calls=%d", err, rows, calls.Load())
	}
}

func TestAgentTaskQueueUsesRunCapacityLimits(t *testing.T) {
	r, err := Open(context.Background(), Options{
		DataDir: t.TempDir(), BackendID: "async-capacity", MaxConcurrent: 1, MaxWaiting: 1,
		Timeout: time.Second, Capacity: CapacityLimits{RunsPerWorkspace: 10, RunsGlobal: 1},
		Native: &NativeOptions{Provider: model.Config{BaseURL: "http://127.0.0.1:1", Model: "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "one", Prompt: Prompt{Text: "hello"}}
	if _, err := r.Enqueue(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.RequestID = "two"
	var capacity *CapacityError
	if _, err := r.Enqueue(context.Background(), second); !errors.As(err, &capacity) || capacity.Resource != "agent tasks global" {
		t.Fatalf("second enqueue = %v, want capacity", err)
	}
	rows, err := r.query(context.Background(), "SELECT COUNT(*) FROM agent_tasks")
	if err != nil || rows[0][0].(int64) != 1 {
		t.Fatalf("task count = %v, %v", rows, err)
	}
}

func TestQueuedCancellationPreventsDispatch(t *testing.T) {
	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
	ctx := principalContext("alice")
	if _, err := r.Enqueue(ctx, asyncRequest("alice", "cancel-me", "hello")); err != nil {
		t.Fatal(err)
	}
	waitForRun(t, r, "alice", "cancel-me", "queued")
	cancelled, err := r.CancelRun(ctx, "work", "cancel-me")
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	if err := r.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "queued task deleted", func() bool {
		rows, err := r.query(context.Background(), "SELECT COUNT(*) FROM agent_tasks")
		return err == nil && rows[0][0].(int64) == 0
	})
	if calls.Load() != 0 {
		t.Fatalf("cancelled task dispatched %d model calls", calls.Load())
	}
	if got, err := r.GetRun(ctx, "work", "cancel-me"); err != nil || got.State != "cancelled" {
		t.Fatalf("cancelled run = %#v, %v", got, err)
	}
}

func insertRunningTask(t *testing.T, r *Runtime, req Request) {
	t.Helper()
	p := Principal{UserID: "alice", WorkspaceID: "work"}
	prepared, _, hash, err := r.prepareRun(principalContext("alice"), req)
	if err != nil {
		t.Fatal(err)
	}
	payload, principalJSON, err := encodeAgentTask(p, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mutate(context.Background(), []rhiza.SQLStatement{{SQL: "INSERT INTO agent_tasks(workspace,request_id,principal,request,input_hash,state,enqueued_at) VALUES(?,?,?,?,?,'running',?)", Args: []any{prepared.WorkspaceID, req.RequestID, principalJSON, payload, hash, time.Now().UnixNano()}, ExpectedRowsAffected: one()}}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentWorkerReconcilesCrashStates(t *testing.T) {
	t.Run("running without run requeues", func(t *testing.T) {
		var calls atomic.Int32
		provider := asyncProvider(t, &calls, "recovered", nil, false)
		r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
		insertRunningTask(t, r, asyncRequest("alice", "recover", "hello"))
		if err := r.StartAgentWorker(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitForRun(t, r, "alice", "recover", "succeeded")
		if calls.Load() != 1 {
			t.Fatalf("recovered calls = %d", calls.Load())
		}
	})

	t.Run("any run row deletes task", func(t *testing.T) {
		var calls atomic.Int32
		provider := asyncProvider(t, &calls, "answer", nil, false)
		r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
		ws, err := r.scopedWorkspace(principalContext("alice"), "work")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.mutate(context.Background(), []rhiza.SQLStatement{
			{SQL: "INSERT INTO agent_tasks(workspace,request_id,principal,request,input_hash,state,enqueued_at) VALUES(?,?,?,?,?,'queued',?)", Args: []any{ws, "has-run", "{}", "{}", "hash", time.Now().UnixNano()}},
			{SQL: "INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')", Args: []any{ws, "has-run", "session", "hash"}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.StartAgentWorker(context.Background()); err != nil {
			t.Fatal(err)
		}
		eventually(t, "run-owned task deleted", func() bool {
			rows, err := r.query(context.Background(), "SELECT COUNT(*) FROM agent_tasks")
			return err == nil && rows[0][0].(int64) == 0
		})
		if calls.Load() != 0 {
			t.Fatalf("run-owned task dispatched %d calls", calls.Load())
		}
	})

	t.Run("malformed payload blocks", func(t *testing.T) {
		var calls atomic.Int32
		provider := asyncProvider(t, &calls, "answer", nil, false)
		r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
		ws, err := r.scopedWorkspace(principalContext("alice"), "work")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.mutate(context.Background(), []rhiza.SQLStatement{{SQL: "INSERT INTO agent_tasks(workspace,request_id,principal,request,input_hash,state,enqueued_at) VALUES(?,?,?,?,?,'queued',?)", Args: []any{ws, "malformed", "{}", "not-json", "hash", time.Now().UnixNano()}}}); err != nil {
			t.Fatal(err)
		}
		if err := r.StartAgentWorker(context.Background()); err != nil {
			t.Fatal(err)
		}
		blocked := waitForRun(t, r, "alice", "malformed", "blocked")
		if blocked.FailureCategory != FailureInvalidInput || calls.Load() != 0 {
			t.Fatalf("blocked = %#v calls=%d", blocked, calls.Load())
		}
	})

	t.Run("tampered payload blocks", func(t *testing.T) {
		var calls atomic.Int32
		provider := asyncProvider(t, &calls, "answer", nil, false)
		r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
		ws, err := r.scopedWorkspace(principalContext("alice"), "work")
		if err != nil {
			t.Fatal(err)
		}
		req := asyncRequest("alice", "tampered", "hello")
		prepared, _, hash, err := r.prepareRun(principalContext("alice"), req)
		if err != nil {
			t.Fatal(err)
		}
		payload, principalJSON, err := encodeAgentTask(Principal{UserID: "alice", WorkspaceID: "work"}, req)
		if err != nil {
			t.Fatal(err)
		}
		payload = strings.Replace(payload, "hello", "tampered", 1)
		if err := r.mutate(context.Background(), []rhiza.SQLStatement{{SQL: "INSERT INTO agent_tasks(workspace,request_id,principal,request,input_hash,state,enqueued_at) VALUES(?,?,?,?,?,'queued',?)", Args: []any{prepared.WorkspaceID, req.RequestID, principalJSON, payload, hash, time.Now().UnixNano()}}}); err != nil {
			t.Fatal(err)
		}
		if ws != prepared.WorkspaceID {
			t.Fatal("scoped workspace mismatch")
		}
		if err := r.StartAgentWorker(context.Background()); err != nil {
			t.Fatal(err)
		}
		blocked := waitForRun(t, r, "alice", "tampered", "blocked")
		if blocked.FailureCategory != FailureRequestConflict || calls.Load() != 0 {
			t.Fatalf("tampered = %#v calls=%d", blocked, calls.Load())
		}
	})
}

func TestCloseCancelsAndWaitsForAgentWorker(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	provider := asyncProvider(t, &calls, "", started, true)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
	if _, err := r.Enqueue(principalContext("alice"), asyncRequest("alice", "shutdown", "hello")); err != nil {
		t.Fatal(err)
	}
	if err := r.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not dispatch")
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked with an in-flight worker")
	}
	if err := r.StartAgentWorker(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after close = %v", err)
	}
}

func TestWorkerCancellationDuringPrepareLeavesTaskRunning(t *testing.T) {
	var calls, hookCalls atomic.Int32
	entered := make(chan struct{})
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("PrepareAsync context has no configured deadline")
		}
		if hookCalls.Add(1) == 1 {
			return ctx, nil
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx := principalContext("alice")
	if _, err := r.Enqueue(ctx, asyncRequest("alice", "prepare-cancel", "hello")); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	if err := r.StartAgentWorker(workerCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter PrepareAsync")
	}
	cancel()
	r.workerMu.Lock()
	done := r.workerDone
	r.workerMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	ws, err := r.scopedWorkspace(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	task, found, err := r.agentTask(context.Background(), ws, "prepare-cancel")
	if err != nil || !found || task.state != "running" || calls.Load() != 0 {
		t.Fatalf("cancelled preparation task = %#v found=%v err=%v calls=%d", task, found, err, calls.Load())
	}
}

func TestAgentWorkerRestartsWithFreshContext(t *testing.T) {
	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
	first, cancel := context.WithCancel(context.Background())
	if err := r.StartAgentWorker(first); err != nil {
		t.Fatal(err)
	}
	r.workerMu.Lock()
	oldDone := r.workerDone
	r.workerMu.Unlock()
	cancel()
	<-oldDone
	eventually(t, "worker registration cleared", func() bool {
		r.workerMu.Lock()
		defer r.workerMu.Unlock()
		return r.workerCtx == nil && r.workerDone == nil && r.workerCancel == nil
	})
	if _, err := r.Enqueue(principalContext("alice"), asyncRequest("alice", "restart", "hello")); err != nil {
		t.Fatal(err)
	}
	if err := r.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRun(t, r, "alice", "restart", "succeeded")
	if calls.Load() != 1 {
		t.Fatalf("restarted worker calls = %d", calls.Load())
	}
}

func TestAgentWorkerRestartsAfterAlreadyCanceledContext(t *testing.T) {
	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.StartAgentWorker(dead); err != nil {
		t.Fatal(err)
	}
	// The second call either observes the canceled worker and waits for exit or
	// starts after cleanup; both must leave a live worker for the task below.
	if err := r.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Enqueue(principalContext("alice"), asyncRequest("alice", "already-canceled-restart", "hello")); err != nil {
		t.Fatal(err)
	}
	waitForRun(t, r, "alice", "already-canceled-restart", "succeeded")
	if calls.Load() != 1 {
		t.Fatalf("restarted worker calls = %d", calls.Load())
	}
}

func TestClaimRequeuesRunningTaskWithoutRun(t *testing.T) {
	var calls atomic.Int32
	provider := asyncProvider(t, &calls, "answer", nil, false)
	r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
	insertRunningTask(t, r, asyncRequest("alice", "claim-recover", "hello"))
	task, found, err := r.claimAgentTask(context.Background())
	if err != nil || !found || task.requestID != "claim-recover" || task.state != "running" {
		t.Fatalf("claim requeue = %#v found=%v err=%v", task, found, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("claim dispatched %d model calls", calls.Load())
	}
}

func TestStartAgentWorkerSerializesWithClose(t *testing.T) {
	for i := 0; i < 8; i++ {
		var calls atomic.Int32
		provider := asyncProvider(t, &calls, "answer", nil, false)
		r := asyncRuntime(t, t.TempDir(), provider.URL, func(ctx context.Context, _ Principal) (context.Context, error) { return ctx, nil })
		var starts sync.WaitGroup
		startErr := make(chan error, 1)
		starts.Add(1)
		go func() {
			defer starts.Done()
			startErr <- r.StartAgentWorker(context.Background())
		}()
		closeErr := r.Close()
		starts.Wait()
		if err := <-startErr; err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("start error = %v", err)
		}
		if closeErr != nil {
			t.Fatalf("close error = %v", closeErr)
		}
	}
}
