package beesuh

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/rhiza"
)

type cancellationOutcome struct {
	out Result
	err error
}

func awaitCancellationRun(t *testing.T, done <-chan cancellationOutcome) cancellationOutcome {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
		return cancellationOutcome{}
	}
}

func awaitRunControl(t *testing.T, r *Runtime, req Request) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		r.cancelMu.Lock()
		found := len(r.controls[cancellationKey(req.WorkspaceID, req.RequestID)]) != 0
		r.cancelMu.Unlock()
		if found {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("request was not registered")
		}
	}
}

func TestCancellationQueuedTombstoneRestart(t *testing.T) {
	for _, wait := range []string{"session", "slot", "request"} {
		t.Run(wait, func(t *testing.T) {
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { calls.Add(1); completion(w, "done", "stop") }))
			defer backend.Close()
			opts := nativeOptions(t.TempDir(), t.TempDir(), backend.URL, nil)
			opts.MaxConcurrent = 1
			r := nativeOpen(t, opts)
			defer func() { _ = r.Close() }()
			req := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "queued", Prompt: Prompt{Text: "hello"}}
			var release func()
			switch wait {
			case "session":
				release, _ = r.lockSession(t.Context(), "work\x00session")
			case "request":
				release, _ = r.lockSession(t.Context(), "request\x00work\x00queued")
			case "slot":
				slots := r.slots
				slots <- struct{}{}
				release = func() { <-slots }
			}
			defer release()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan cancellationOutcome, 1)
			go func() { out, err := r.Run(ctx, req); done <- cancellationOutcome{out, err} }()
			awaitRunControl(t, r, req)
			out, err := r.CancelRun(t.Context(), "work", "queued")
			if err != nil || out.State != "cancelled" {
				t.Fatalf("cancel=%+v %v", out, err)
			}
			if got := awaitCancellationRun(t, done); got.err != nil || got.out.State != "cancelled" {
				t.Fatalf("queued outcome=%+v", got)
			}
			if calls.Load() != 0 {
				t.Fatal("cancelled queued request reached provider")
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r = nativeOpen(t, opts)
			for i := 0; i < 2; i++ {
				out, err = r.GetRun(t.Context(), "work", "queued")
				if err != nil || out.State != "cancelled" {
					t.Fatalf("poll=%+v %v", out, err)
				}
				out, err = r.Run(t.Context(), req)
				if err != nil || out.State != "cancelled" {
					t.Fatalf("replay=%+v %v", out, err)
				}
				out, err = r.CancelRun(t.Context(), "work", "queued")
				if err != nil || out.State != "cancelled" {
					t.Fatalf("repeat=%+v %v", out, err)
				}
			}
			if calls.Load() != 0 {
				t.Fatal("restart replay dispatched")
			}
		})
	}
}

func TestCancellationActiveNative(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-req.Context().Done():
		case <-release:
		}
		completion(w, "late", "stop")
	}))
	defer backend.Close()
	defer close(release)
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), backend.URL, nil))
	defer r.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "active", Prompt: Prompt{Text: "hello"}}
	done := make(chan cancellationOutcome, 1)
	go func() { out, err := r.Run(ctx, req); done <- cancellationOutcome{out, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider was not called")
	}
	out, err := r.CancelRun(t.Context(), "work", "active")
	if err != nil || !out.CancellationRequested || out.State != "running" {
		t.Fatalf("cancel=%+v %v", out, err)
	}
	got := awaitCancellationRun(t, done)
	if got.err != nil || got.out.State != "cancelled" {
		t.Fatalf("outcome=%+v", got)
	}
	out, err = r.GetRun(t.Context(), "work", "active")
	if err != nil || out.State != "cancelled" {
		t.Fatalf("poll=%+v %v", out, err)
	}
	if r.runFenceHolds("work") {
		t.Fatal("model-only cancellation unexpectedly fenced workspace")
	}
}

func TestCancellationLostAcknowledgement(t *testing.T) {
	opts := nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil)
	r := nativeOpen(t, opts)
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := r.registerRun("work", "queued", cancel)
	r.db = &faultingRuntimeDB{DB: r.db, executeFault: func(req rhiza.ExecuteRequest) bool {
		return strings.Contains(req.Statements[0].SQL, "INSERT INTO run_cancellations")
	}}
	if _, err := r.CancelRun(t.Context(), "work", "queued"); !errors.Is(err, errPostCommitAcknowledgementLoss) {
		t.Fatalf("lost ack=%v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("live execution was not signalled")
	}
	unregister()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	out, err := r.CancelRun(t.Context(), "work", "queued")
	if err != nil || out.State != "cancelled" {
		t.Fatalf("retry=%+v %v", out, err)
	}
}

func TestCancellationUnknownTerminalAndCompletionOrder(t *testing.T) {
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil))
	defer r.Close()
	if _, err := r.CancelRun(t.Context(), "work", "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown=%v", err)
	}
	for _, cancelFirst := range []bool{false, true} {
		id := "finished"
		if cancelFirst {
			id = "cancelled"
		}
		req := Request{WorkspaceID: "work", SessionKey: id, RequestID: id, Prompt: Prompt{Text: "hello"}}
		session := reserveRunningRun(t, r, req)
		if cancelFirst {
			if _, err := r.CancelRun(t.Context(), "work", id); err != nil {
				t.Fatal(err)
			}
		}
		out := Result{RequestID: id, SessionID: session, State: "succeeded"}
		if err := r.finishResult(t.Context(), "work", id, id, &out); err != nil {
			t.Fatal(err)
		}
		want := "succeeded"
		if cancelFirst {
			want = "cancelled"
		}
		if out.State != want {
			t.Fatalf("finish=%+v", out)
		}
		for i := 0; i < 2; i++ {
			got, err := r.CancelRun(t.Context(), "work", id)
			if err != nil || got.State != want {
				t.Fatalf("terminal=%+v %v", got, err)
			}
		}
	}
}

func TestCancellationRestartRetainsRunningFenceAndEffects(t *testing.T) {
	opts := nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil)
	r := nativeOpen(t, opts)
	defer func() { _ = r.Close() }()
	req := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "running", Prompt: Prompt{Text: "hello"}}
	reserveRunningRun(t, r, req)
	if err := r.recordUncertainIntent(t.Context(), req, "effect", "deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CancelRun(t.Context(), "work", "running"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = nativeOpen(t, opts)
	if !r.runFenceHolds("work") {
		t.Fatal("restart lost running fence")
	}
	if out, err := r.Run(t.Context(), req); !errors.Is(err, ErrUncertain) {
		t.Fatalf("replay=%+v %v", out, err)
	}
	if err := r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, err := r.GetRun(t.Context(), "work", "running")
	if err != nil || out.State != "cancelled" {
		t.Fatalf("recovered=%+v %v", out, err)
	}
	effects, err := r.UnresolvedEffects(t.Context(), "work", "", 0)
	if err != nil || len(effects.Effects) != 1 {
		t.Fatalf("effects=%+v %v", effects, err)
	}
	if err := r.assertWorkspaceRecoverable(t.Context(), "work"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("cancellation or recovery resolved an external effect: %v", err)
	}
}

func TestCancellationUncertainConductorStaysFenced(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		select {
		case <-req.Context().Done():
		case <-release:
		}
	}))
	defer conduit.Close()
	defer close(release)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
	}))
	defer provider.Close()
	cfg := &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), provider.URL, cfg))
	defer r.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "effect", Prompt: Prompt{Text: "deploy"}}
	done := make(chan cancellationOutcome, 1)
	go func() { out, err := r.Run(ctx, req); done <- cancellationOutcome{out, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("effect not dispatched")
	}
	if _, err := r.CancelRun(t.Context(), "work", "effect"); err != nil {
		t.Fatal(err)
	}
	got := awaitCancellationRun(t, done)
	if !errors.Is(got.err, ErrUncertain) || got.out.State != "uncertain" {
		t.Fatalf("outcome=%+v", got)
	}
	if !r.runFenceHolds("work") {
		t.Fatal("cancelled effect lost fence")
	}
	out, err := r.GetRun(t.Context(), "work", "effect")
	if err != nil || out.State != "running" || !out.CancellationRequested {
		t.Fatalf("poll=%+v %v", out, err)
	}
}

func TestCancellationRejectedPersistenceDoesNotSignalOrAcknowledge(t *testing.T) {
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil))
	defer r.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := r.registerRun("work", "queued", cancel)
	defer unregister()
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_cancellation BEFORE INSERT ON run_cancellations BEGIN SELECT RAISE(ABORT,'injected cancellation failure'); END`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CancelRun(t.Context(), "work", "queued"); err == nil {
		t.Fatal("rejected persistence was acknowledged")
	}
	if ctx.Err() != nil {
		t.Fatal("rejected persistence signalled execution")
	}
	if _, err := r.GetRun(t.Context(), "work", "queued"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected tombstone poll=%v", err)
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `DROP TRIGGER reject_cancellation`}}); err != nil {
		t.Fatal(err)
	}
	if out, err := r.CancelRun(t.Context(), "work", "queued"); err != nil || out.State != "cancelled" {
		t.Fatalf("retry=%+v %v", out, err)
	}
	if ctx.Err() == nil {
		t.Fatal("committed intent did not signal execution")
	}
}

func TestCancellationCompletionRace(t *testing.T) {
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil))
	defer r.Close()
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		req := Request{WorkspaceID: "work", SessionKey: id, RequestID: id, Prompt: Prompt{Text: "hello"}}
		session := reserveRunningRun(t, r, req)
		start := make(chan struct{})
		finished := make(chan cancellationOutcome, 1)
		cancelled := make(chan cancellationOutcome, 1)
		go func() {
			<-start
			out := Result{SessionID: session, RequestID: id, State: "succeeded"}
			err := r.finishResult(t.Context(), "work", id, id, &out)
			finished <- cancellationOutcome{out, err}
		}()
		go func() {
			<-start
			out, err := r.CancelRun(t.Context(), "work", id)
			cancelled <- cancellationOutcome{out, err}
		}()
		close(start)
		finish, cancellation := awaitCancellationRun(t, finished), awaitCancellationRun(t, cancelled)
		if finish.err != nil || cancellation.err != nil {
			t.Fatalf("finish=%+v cancel=%+v", finish, cancellation)
		}
		stored, err := r.GetRun(t.Context(), "work", id)
		if err != nil {
			t.Fatal(err)
		}
		if cancellation.out.CancellationRequested && stored.State != "cancelled" {
			t.Fatalf("accepted cancellation became %+v", stored)
		}
		if !cancellation.out.CancellationRequested && stored.State != "succeeded" {
			t.Fatalf("completed result changed: %+v", stored)
		}
	}
}

func TestCancellationTombstoneCapacityAndIdempotency(t *testing.T) {
	opts := nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil)
	opts.Capacity = CapacityLimits{RunsPerWorkspace: 1, RunsGlobal: 1}
	r := nativeOpen(t, opts)
	defer r.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := r.registerRun("work", "one", cancel)
	defer unregister()
	if _, err := r.CancelRun(t.Context(), "work", "one"); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("accepted cancellation did not signal queued request")
	}
	if _, err := r.CancelRun(t.Context(), "work", "one"); err != nil {
		t.Fatalf("idempotency at capacity: %v", err)
	}
	other, stop := context.WithCancel(t.Context())
	defer stop()
	unregisterOther := r.registerRun("work", "two", stop)
	defer unregisterOther()
	if _, err := r.CancelRun(t.Context(), "work", "two"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity=%v", err)
	}
	if other.Err() != nil {
		t.Fatal("capacity rejection signalled other request")
	}
}

func TestCancellationLegacyLibraryStopsWithoutNewEngine(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptGate = make(chan struct{})
	r := openTestRuntime(t, f, t.TempDir(), 1)
	defer r.Close()
	defer close(f.promptGate)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := testRequest("session", "cancel", "hello")
	done := make(chan cancellationOutcome, 1)
	go func() { out, err := r.Run(ctx, req); done <- cancellationOutcome{out, err} }()
	select {
	case <-f.prompted:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy prompt not dispatched")
	}
	if _, err := r.CancelRun(t.Context(), req.WorkspaceID, req.RequestID); err != nil {
		t.Fatal(err)
	}
	got := awaitCancellationRun(t, done)
	if got.err != nil || got.out.State != "cancelled" {
		t.Fatalf("legacy outcome=%+v", got)
	}
	_, _, aborts := f.counts()
	if aborts != 1 {
		t.Fatalf("legacy aborts=%d", aborts)
	}
}

func TestCancellationPreservesExistingHashAndFailureReplay(t *testing.T) {
	opts := nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil)
	r := nativeOpen(t, opts)
	defer func() { _ = r.Close() }()
	for _, state := range []string{"cancelled", "failed"} {
		req := Request{WorkspaceID: "work", SessionKey: state, RequestID: state, Prompt: Prompt{Text: "hello"}}
		session := reserveRunningRun(t, r, req)
		out := Result{RequestID: state, SessionID: session, State: "failed"}
		setFailure(&out, ErrForbidden)
		if state == "cancelled" {
			if _, err := r.CancelRun(t.Context(), "work", state); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.finishResult(t.Context(), "work", state, state, &out); err != nil {
			t.Fatal(err)
		}
		got, err := r.CancelRun(t.Context(), "work", state)
		if err != nil || got.State != state {
			t.Fatalf("terminal cancel=%+v %v", got, err)
		}
		if state == "failed" && got.FailureCategory != FailureForbidden {
			t.Fatalf("lost failure category: %+v", got)
		}
		replay, err := r.Run(t.Context(), req)
		if state == "failed" && (!errors.Is(err, ErrForbidden) || replay.FailureCategory != FailureForbidden) {
			t.Fatalf("failure replay=%+v %v", replay, err)
		}
		if state == "cancelled" && (err != nil || replay.State != "cancelled") {
			t.Fatalf("cancel replay=%+v %v", replay, err)
		}
		req.Prompt.Text = "different"
		if _, err := r.Run(t.Context(), req); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed input lost original conflict: %v", err)
		}
	}
	for restart := 0; restart < 2; restart++ {
		if restart != 0 {
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r = nativeOpen(t, opts)
		}
		for _, state := range []string{"cancelled", "failed"} {
			req := Request{WorkspaceID: "work", SessionKey: "different-session", RequestID: state, Prompt: Prompt{Text: "hello"}}
			if _, err := r.Run(t.Context(), req); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed session (restart=%d) lost conflict: %v", restart, err)
			}
			req.SessionKey, req.Prompt.Text = state, "different"
			if _, err := r.Run(t.Context(), req); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed prompt (restart=%d) lost conflict: %v", restart, err)
			}
		}
	}
}

func TestCancellationSignalFollowsDurableCommit(t *testing.T) {
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://127.0.0.1:1", nil))
	defer r.Close()
	signalled := false
	unregister := r.registerRun("work", "queued", func() {
		requested, err := r.cancellationRequested(t.Context(), "work", "queued")
		if err != nil || !requested {
			t.Errorf("execution signalled before durable intent: requested=%v err=%v", requested, err)
		}
		signalled = true
	})
	defer unregister()
	out, err := r.CancelRun(t.Context(), "work", "queued")
	if err != nil || out.State != "cancelled" || !signalled {
		t.Fatalf("cancel=%+v %v signalled=%v", out, err, signalled)
	}
}

// Fail both acknowledgement and confirmation after the real Rhiza commit.
// Atomics keep fault injection safe while the model owner is still running.
type ambiguousCancellationDB struct {
	runtimeDB
	unreadable atomic.Bool
}

func (db *ambiguousCancellationDB) Execute(ctx context.Context, req rhiza.ExecuteRequest) (rhiza.ExecuteResponse, error) {
	response, err := db.runtimeDB.Execute(ctx, req)
	if err == nil && response.Status == rhiza.MutationCommitted && strings.Contains(req.Statements[0].SQL, "INSERT INTO run_cancellations") {
		db.unreadable.Store(true)
		return response, errPostCommitAcknowledgementLoss
	}
	return response, err
}
func (db *ambiguousCancellationDB) Query(ctx context.Context, req rhiza.QueryRequest) (rhiza.QueryResponse, error) {
	if db.unreadable.Load() && strings.Contains(req.SQL, "run_cancellations") {
		return rhiza.QueryResponse{}, errors.New("injected cancellation confirmation read failure")
	}
	return db.runtimeDB.Query(ctx, req)
}

type cancellationRoundTrip func(*http.Request) (*http.Response, error)

func (f cancellationRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCancellationAmbiguousCommitStopsHeldModelToolDispatch(t *testing.T) {
	var effects atomic.Int32
	conduit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { effects.Add(1); w.WriteHeader(http.StatusAccepted) }))
	defer conduit.Close()
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	cfg := &conductor.Config{BaseURL: conduit.URL, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	opts := nativeOptions(t.TempDir(), t.TempDir(), "http://fixture.invalid", cfg)
	opts.Native.Provider.HTTPClient = &http.Client{Transport: cancellationRoundTrip(func(req *http.Request) (*http.Response, error) {
		entered <- req.Context()
		<-release
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"emit","type":"function","function":{"name":"conductor_emit","arguments":"{\"name\":\"deploy\",\"data\":{}}"}}]},"finish_reason":"tool_calls"}]}`)), Request: req}, nil
	})}
	r := nativeOpen(t, opts)
	defer r.Close()
	defer releaseOnce.Do(func() { close(release) })
	db := &ambiguousCancellationDB{runtimeDB: r.db}
	r.db = db
	req := Request{WorkspaceID: "work", SessionKey: "s", RequestID: "ambiguous", Prompt: Prompt{Text: "deploy"}}
	done := make(chan cancellationOutcome, 1)
	go func() { out, err := r.Run(t.Context(), req); done <- cancellationOutcome{out, err} }()
	var modelCtx context.Context
	select {
	case modelCtx = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model not entered")
	}
	if _, err := r.CancelRun(t.Context(), "work", "ambiguous"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("ambiguous cancel=%v", err)
	}
	if modelCtx.Err() == nil {
		t.Fatal("ambiguous committed cancellation did not signal model")
	}
	if !r.runFenceHolds("work") {
		t.Fatal("ambiguous cancellation did not install recovery barrier")
	}
	releaseOnce.Do(func() { close(release) })
	got := awaitCancellationRun(t, done)
	if !errors.Is(got.err, ErrUncertain) || got.out.State != "uncertain" {
		t.Fatalf("ambiguous outcome=%+v", got)
	}
	if effects.Load() != 0 {
		t.Fatal("late model result dispatched an external effect")
	}
	db.unreadable.Store(false)
	requested, err := r.cancellationRequested(t.Context(), "work", "ambiguous")
	if err != nil || !requested {
		t.Fatalf("intent not durable: %v %v", requested, err)
	}
}

func TestCancellationBypassesQueuedLifecycleWriter(t *testing.T) {
	for _, writer := range []string{"recover", "close"} {
		t.Run(writer, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			opts := nativeOptions(t.TempDir(), t.TempDir(), "http://fixture.invalid", nil)
			opts.Timeout = 30 * time.Second
			opts.Native.Provider.HTTPClient = &http.Client{Transport: cancellationRoundTrip(func(req *http.Request) (*http.Response, error) {
				close(entered)
				<-release
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}]}`)), Request: req}, nil
			})}
			r := nativeOpen(t, opts)
			defer r.Close()
			defer once.Do(func() { close(release) })
			done := make(chan cancellationOutcome, 1)
			go func() {
				out, err := r.Run(t.Context(), Request{WorkspaceID: "work", SessionKey: "s", RequestID: "active", Prompt: Prompt{Text: "hello"}})
				done <- cancellationOutcome{out, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("model not entered")
			}
			writerDone := make(chan error, 1)
			go func() {
				if writer == "recover" {
					writerDone <- r.Recover(t.Context())
				} else {
					writerDone <- r.Close()
				}
			}()
			deadline := time.After(5 * time.Second)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for r.life.TryRLock() {
				r.life.RUnlock()
				select {
				case <-deadline:
					t.Fatal("lifecycle writer did not queue")
				case <-ticker.C:
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			out, err := r.CancelRun(ctx, "work", "active")
			if err != nil || !out.CancellationRequested {
				t.Fatalf("cancel behind %s=%+v %v", writer, out, err)
			}
			// The model still owns the lifecycle read lock: cancellation must complete
			// before it releases, rather than waiting for the original run timeout.
			select {
			case <-writerDone:
				t.Fatal("writer unexpectedly acquired lifecycle lock")
			default:
			}
			once.Do(func() { close(release) })
			awaitCancellationRun(t, done)
			select {
			case err := <-writerDone:
				if err != nil {
					t.Fatalf("writer=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("writer did not finish")
			}
		})
	}
}

func TestCancellationControlLockHonorsContext(t *testing.T) {
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://fixture.invalid", nil))
	defer r.Close()
	r.cancelMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := r.CancelRun(ctx, "work", "unknown")
	r.cancelMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("control lock wait=%v", err)
	}
}

// Pause inside the actual DB close, after Runtime.Close has taken cancelMu.
type heldCancellationCloseDB struct {
	runtimeDB
	entered, release chan struct{}
	queries          atomic.Int32
}

func (db *heldCancellationCloseDB) Close() error {
	close(db.entered)
	<-db.release
	return db.runtimeDB.Close()
}

func (db *heldCancellationCloseDB) Query(ctx context.Context, req rhiza.QueryRequest) (rhiza.QueryResponse, error) {
	db.queries.Add(1)
	return db.runtimeDB.Query(ctx, req)
}

func TestCancellationDuringDatabaseCloseHonorsDeadlineAndClosedState(t *testing.T) {
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://fixture.invalid", nil))
	db := &heldCancellationCloseDB{runtimeDB: r.db, entered: make(chan struct{}), release: make(chan struct{})}
	r.db = db
	var releaseOnce sync.Once
	closed, closeResult := make(chan struct{}), make(chan error, 1)
	go func() { closeResult <- r.Close(); close(closed) }()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(db.release) })
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Error("held DB close did not clean up")
		}
	})
	select {
	case <-db.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("DB close was not entered")
	}
	if r.cancelMu.TryLock() {
		r.cancelMu.Unlock()
		t.Fatal("DB close did not hold cancellation lock")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	cancelled := make(chan cancellationOutcome, 1)
	go func() { out, err := r.CancelRun(ctx, "work", "request"); cancelled <- cancellationOutcome{out, err} }()
	select {
	case got := <-cancelled:
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("cancel during DB close=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel ignored its context while DB close was held")
	}
	if db.queries.Load() != 0 {
		t.Fatal("cancellation queried DB while close held its lifetime lock")
	}
	releaseOnce.Do(func() { close(db.release) })
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DB close did not finish")
	}
	after, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if _, err := r.CancelRun(after, "work", "request"); !errors.Is(err, ErrClosed) {
		t.Fatalf("cancel after close=%v", err)
	}
	if db.queries.Load() != 0 {
		t.Fatal("cancellation queried the closed DB instead of returning ErrClosed")
	}
}

// Pause recovery either before it takes cancelMu or while it owns cancelMu
// immediately before the terminal mutation reaches the real Rhiza database.
type heldCancellationRecoveryDB struct {
	runtimeDB
	cancelFirst      bool
	entered, release chan struct{}
	once             sync.Once
}

func (db *heldCancellationRecoveryDB) pause(ctx context.Context) error {
	db.once.Do(func() { close(db.entered) })
	select {
	case <-db.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (db *heldCancellationRecoveryDB) Query(ctx context.Context, req rhiza.QueryRequest) (rhiza.QueryResponse, error) {
	response, err := db.runtimeDB.Query(ctx, req)
	if err == nil && db.cancelFirst && strings.Contains(req.SQL, "UNION SELECT workspace FROM runs WHERE state='running'") {
		err = db.pause(ctx)
	}
	return response, err
}

func (db *heldCancellationRecoveryDB) Execute(ctx context.Context, req rhiza.ExecuteRequest) (rhiza.ExecuteResponse, error) {
	if !db.cancelFirst && strings.HasPrefix(req.Statements[0].SQL, "UPDATE runs SET state=") {
		if err := db.pause(ctx); err != nil {
			return rhiza.ExecuteResponse{}, err
		}
	}
	return db.runtimeDB.Execute(ctx, req)
}

func TestCancellationInterleavesRunningRecoveryBeforeTerminalPersistence(t *testing.T) {
	for _, order := range []string{"cancel-first", "recover-first"} {
		t.Run(order, func(t *testing.T) {
			r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), "http://fixture.invalid", nil))
			req := Request{WorkspaceID: "work", SessionKey: "session", RequestID: "recovering", Prompt: Prompt{Text: "hello"}}
			reserveRunningRun(t, r, req)
			if err := r.recordUncertainIntent(t.Context(), req, "existing-effect", "deploy"); err != nil {
				r.Close()
				t.Fatal(err)
			}
			db := &heldCancellationRecoveryDB{runtimeDB: r.db, cancelFirst: order == "cancel-first", entered: make(chan struct{}), release: make(chan struct{})}
			r.db = db
			var releaseOnce sync.Once
			recovered, recoveryResult := make(chan struct{}), make(chan error, 1)
			go func() { recoveryResult <- r.Recover(t.Context()); close(recovered) }()
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(db.release) })
				select {
				case <-recovered:
					if err := r.Close(); err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("paused recovery did not clean up")
				}
			})
			select {
			case <-db.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("running recovery did not reach persistence barrier")
			}
			if r.life.TryRLock() {
				r.life.RUnlock()
				t.Fatal("recovery was not already holding its lifecycle write lock")
			}
			rows, err := r.query(t.Context(), `SELECT state FROM runs WHERE workspace=? AND request_id=?`, "work", "recovering")
			if err != nil || len(rows) != 1 || rows[0][0] != "running" {
				t.Fatalf("run was already terminal before interleaving: %v %v", rows, err)
			}
			timeout := time.Second
			if !db.cancelFirst {
				timeout = 30 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			cancelled := make(chan cancellationOutcome, 1)
			go func() { out, err := r.CancelRun(ctx, "work", "recovering"); cancelled <- cancellationOutcome{out, err} }()
			var got cancellationOutcome
			select {
			case got = <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation deadlocked against running recovery")
			}
			if db.cancelFirst {
				if got.err != nil || got.out.State != "running" || !got.out.CancellationRequested {
					t.Fatalf("cancel before recovery persistence=%+v", got)
				}
			} else if !errors.Is(got.err, context.DeadlineExceeded) {
				t.Fatalf("cancel while recovery holds persistence lock=%+v", got)
			}
			requested, err := r.cancellationRequested(t.Context(), "work", "recovering")
			if err != nil || requested != db.cancelFirst {
				t.Fatalf("intent ordering: requested=%v err=%v cancelFirst=%v", requested, err, db.cancelFirst)
			}
			if !db.cancelFirst && !r.runFenceHolds("work") {
				t.Fatal("running recovery lost its unsettled run fence")
			}
			releaseOnce.Do(func() { close(db.release) })
			select {
			case err := <-recoveryResult:
				if err != nil {
					t.Fatalf("recovery=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("recovery did not finish after persistence release")
			}
			want := "interrupted"
			if db.cancelFirst {
				want = "cancelled"
			}
			stored, err := r.GetRun(t.Context(), "work", "recovering")
			if err != nil || stored.State != want {
				t.Fatalf("recovery outcome=%+v %v, want %s", stored, err, want)
			}
			terminal, err := r.CancelRun(t.Context(), "work", "recovering")
			if err != nil || terminal.State != want || terminal.CancellationRequested != db.cancelFirst {
				t.Fatalf("terminal ordering=%+v %v", terminal, err)
			}
			if r.runFenceHolds("work") {
				t.Fatal("settled recovery retained its run fence")
			}
			if err := r.assertWorkspaceRecoverable(t.Context(), "work"); !errors.Is(err, ErrUncertain) {
				t.Fatalf("interleaving cleared the durable effect fence: %v", err)
			}
			effects, err := r.UnresolvedEffects(t.Context(), "work", "", 0)
			if err != nil || len(effects.Effects) != 1 || effects.Effects[0].EffectKey != "existing-effect" || effects.Effects[0].Disposition != dispositionPending {
				t.Fatalf("existing effect changed: %+v %v", effects, err)
			}
		})
	}
}
