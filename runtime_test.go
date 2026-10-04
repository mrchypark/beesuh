package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/opencode"
)

type fakeOpenCode struct {
	server    *httptest.Server
	mu        sync.Mutex
	next      int
	creates   int
	deletes   int
	prompts   int
	aborts    int
	active    int
	maxActive int

	promptGate chan struct{}
	prompted   chan struct{}
	abortGate  chan struct{}
	aborted    chan struct{}
	abortOK    bool
	// abortFail names session IDs whose abort must be refused, which reproduces
	// a recovery pass that stops on one workspace after persisting another.
	abortFail   map[string]bool
	promptCode  int
	promptReply string
	deleteCode  int
	deleteOK    bool
	// createCode, when non-zero, makes session creation fail with that status
	// after recording the session, which reproduces a lost create response.
	createCode int

	// createGate, when non-nil, blocks the session create response until it is
	// closed. It lets a test drive the request context to cancellation between
	// the remote session existing and the local mapping insert running. The
	// response headers are flushed before the wait, so the client already has a
	// fully formed response and only the body read is interrupted.
	createGate chan struct{}
	created    chan struct{}
	// createAfterHeader, when non-nil, is closed by the fake after the create
	// response headers are flushed. A request context cancelled after that point
	// makes the client lose the response body while the server has already
	// created the session, which is the worst-case orphan shape.
	createAfterHeader chan struct{}
	// deleted receives one value per DELETE the fake accepted, so tests can wait
	// for the reclaim instead of sleeping.
	deleted chan struct{}

	// sessionTitles mirrors the sessions that exist on the fake server so the
	// list endpoint can answer title lookups the way OpenCode does.
	sessionTitles map[string]string
	// promptTargets records which session each prompt was sent to, so a test can
	// prove a session was never written into rather than inferring it from counts.
	promptTargets []string
}

type fakeSession struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func newFakeOpenCode(t *testing.T) *fakeOpenCode {
	t.Helper()
	f := &fakeOpenCode{prompted: make(chan struct{}, 32), aborted: make(chan struct{}, 32), created: make(chan struct{}, 32), deleted: make(chan struct{}, 32), sessionTitles: map[string]string{}, abortOK: true, deleteOK: true}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOpenCode) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("directory") == "" && r.URL.Path != "/global/health" {
		http.Error(w, "directory required", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/session" {
		f.mu.Lock()
		f.next++
		f.creates++
		id := fmt.Sprintf("ses_%d", f.next)
		gate := f.createGate
		afterHeader := f.createAfterHeader
		if afterHeader != nil {
			// Fire only on the first create; later creates behave normally.
			f.createAfterHeader = nil
		}
		var title string
		if strings.HasPrefix(r.URL.Path, "/session") {
			var body struct {
				Title string `json:"title"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			title = body.Title
		}
		f.sessionTitles[id] = title
		code := f.createCode
		if code != 0 {
			f.createCode = 0
			f.mu.Unlock()
			f.created <- struct{}{}
			http.Error(w, "create failed", code)
			return
		}
		f.mu.Unlock()
		f.created <- struct{}{}
		if gate != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-gate
		}
		if afterHeader != nil {
			select {
			case <-afterHeader:
			default:
				close(afterHeader)
			}
			<-r.Context().Done()
			// The client is gone, but the session exists on the server. This is
			// the untracked-orphan shape the runtime has to be able to rule out.
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/session/status" {
		_, _ = w.Write([]byte(`{}`)) // OpenCode omits idle sessions.
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/session" {
		f.mu.Lock()
		titles := make([]fakeSession, 0, len(f.sessionTitles))
		for id, title := range f.sessionTitles {
			titles = append(titles, fakeSession{ID: id, Title: title})
		}
		f.mu.Unlock()
		body, _ := json.Marshal(titles)
		_, _ = w.Write(body)
		return
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/session/") {
		deletedID := strings.TrimPrefix(r.URL.Path, "/session/")
		f.mu.Lock()
		f.deletes++
		code := f.deleteCode
		ok := f.deleteOK
		if ok {
			delete(f.sessionTitles, deletedID)
		}
		f.mu.Unlock()
		f.deleted <- struct{}{}
		if code != 0 {
			http.Error(w, "delete failed", code)
			return
		}
		_, _ = fmt.Fprintf(w, `%t`, ok)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/session/") {
		id := strings.TrimPrefix(r.URL.Path, "/session/")
		_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
		target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/session/"), "/message")
		f.mu.Lock()
		f.prompts++
		f.promptTargets = append(f.promptTargets, target)
		f.active++
		if f.active > f.maxActive {
			f.maxActive = f.active
		}
		gate, code := f.promptGate, f.promptCode
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.active--
			f.mu.Unlock()
		}()
		f.prompted <- struct{}{}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if code != 0 {
			http.Error(w, "agent failed", code)
			return
		}
		if f.promptReply != "" {
			_, _ = w.Write([]byte(f.promptReply))
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/session/"), "/message")
		_, _ = fmt.Fprintf(w, `{"info":{"id":"msg_1","sessionID":%q,"role":"assistant","time":{"completed":1},"finish":"stop"},"parts":[{"type":"text","text":"ok"}]}`, id)
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/abort") {
		target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/session/"), "/abort")
		f.mu.Lock()
		f.aborts++
		gate, ok := f.abortGate, f.abortOK
		fail := f.abortFail[target]
		f.mu.Unlock()
		f.aborted <- struct{}{}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = fmt.Fprintf(w, "%t", ok && !fail)
		return
	}
	http.NotFound(w, r)
}

func (f *fakeOpenCode) counts() (creates, prompts, aborts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.prompts, f.aborts
}

func (f *fakeOpenCode) promptStats() (active, maxActive int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, f.maxActive
}

func openTestRuntime(t *testing.T, f *fakeOpenCode, dataDir string, max int) *Runtime {
	return openTestRuntimeWithPermissions(t, f, dataDir, max, nil)
}

func openTestRuntimeWithPermissions(t *testing.T, f *fakeOpenCode, dataDir string, max int, permissions []opencode.PermissionRule) *Runtime {
	t.Helper()
	r, err := Open(context.Background(), Options{
		DataDir: dataDir, BackendID: "test", MaxConcurrent: max, MaxWaiting: 4,
		Timeout:     2 * time.Second,
		OpenCode:    opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"},
		Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testRequest(session, request, text string) Request {
	return Request{WorkspaceID: "workspace", SessionKey: session, RequestID: request, Prompt: opencode.Prompt{Text: text}}
}

// runFenced reports whether this runtime holds a volatile run fence. It reads
// the map directly because Status is refused in service mode, and these call
// sites assert only that a failed operation left no run fence behind.
func runFencedNow(r *Runtime) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runFenced) != 0
}

func receive(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func eventually(t *testing.T, name string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", name)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func lockRefs(r *Runtime, key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lock := r.locks[key]; lock != nil {
		return lock.refs
	}
	return 0
}

func lockCount(r *Runtime) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.locks)
}

func TestRuntimeReusesDurableSessionAndReplay(t *testing.T) {
	f := newFakeOpenCode(t)
	data := t.TempDir()
	r := openTestRuntime(t, f, data, 2)
	first, err := r.Run(context.Background(), testRequest("agent", "request-1", "one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Run(context.Background(), testRequest("agent", "request-2", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID != second.SessionID {
		t.Fatalf("sessions differ: %q, %q", first.SessionID, second.SessionID)
	}
	if _, err := r.Run(context.Background(), testRequest("agent", "request-1", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), testRequest("agent", "request-1", "changed")); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}
	if creates, prompts, _ := f.counts(); creates != 1 || prompts != 2 {
		t.Fatalf("before reopen creates=%d prompts=%d", creates, prompts)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openTestRuntime(t, f, data, 2)
	t.Cleanup(func() { _ = r.Close() })
	third, err := r.Run(context.Background(), testRequest("agent", "request-3", "three"))
	if err != nil {
		t.Fatal(err)
	}
	if third.SessionID != first.SessionID {
		t.Fatalf("reopened session = %q, want %q", third.SessionID, first.SessionID)
	}
	if creates, prompts, _ := f.counts(); creates != 1 || prompts != 3 {
		t.Fatalf("after reopen creates=%d prompts=%d", creates, prompts)
	}
}

func TestRuntimeSessionLocksAndPool(t *testing.T) {
	t.Run("same session serializes even when two pool slots exist", func(t *testing.T) {
		f := newFakeOpenCode(t)
		f.promptGate = make(chan struct{})
		r := openTestRuntime(t, f, t.TempDir(), 2)
		t.Cleanup(func() { _ = r.Close() })
		first := make(chan error, 1)
		second := make(chan error, 1)
		go func() { _, err := r.Run(context.Background(), testRequest("same", "one", "one")); first <- err }()
		receive(t, f.prompted, "first prompt")
		go func() { _, err := r.Run(context.Background(), testRequest("same", "two", "two")); second <- err }()
		eventually(t, "second request waiting on its session", func() bool {
			return lockRefs(r, "workspace\x00same") == 2
		})
		if _, prompts, _ := f.counts(); prompts != 1 {
			t.Fatalf("same session prompts=%d, want 1 while lock held", prompts)
		}
		if active, maxActive := f.promptStats(); active != 1 || maxActive != 1 {
			t.Fatalf("same session active=%d max=%d, want 1", active, maxActive)
		}
		close(f.promptGate)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		receive(t, f.prompted, "second same-session prompt")
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if _, maxActive := f.promptStats(); maxActive != 1 {
			t.Fatalf("same session max active=%d, want 1", maxActive)
		}
	})

	t.Run("different sessions use both pool slots", func(t *testing.T) {
		f := newFakeOpenCode(t)
		f.promptGate = make(chan struct{})
		r := openTestRuntime(t, f, t.TempDir(), 2)
		t.Cleanup(func() { _ = r.Close() })
		left := make(chan error, 1)
		right := make(chan error, 1)
		go func() { _, err := r.Run(context.Background(), testRequest("left", "one", "one")); left <- err }()
		go func() { _, err := r.Run(context.Background(), testRequest("right", "two", "two")); right <- err }()
		receive(t, f.prompted, "left or right prompt")
		receive(t, f.prompted, "other session prompt")
		eventually(t, "two active prompts", func() bool {
			active, _ := f.promptStats()
			return active == 2
		})
		if _, maxActive := f.promptStats(); maxActive != 2 {
			t.Fatalf("cross-session max active=%d, want 2", maxActive)
		}
		close(f.promptGate)
		if err := <-left; err != nil {
			t.Fatal(err)
		}
		if err := <-right; err != nil {
			t.Fatal(err)
		}
	})
}

func TestRuntimeCancellationAbortsBeforePoolRelease(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptGate = make(chan struct{})
	f.abortGate = make(chan struct{})
	r := openTestRuntime(t, f, t.TempDir(), 1)
	t.Cleanup(func() { _ = r.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := r.Run(ctx, testRequest("one", "one", "one")); first <- err }()
	receive(t, f.prompted, "first prompt")
	cancel()
	receive(t, f.aborted, "abort")
	second := make(chan error, 1)
	go func() { _, err := r.Run(context.Background(), testRequest("two", "two", "two")); second <- err }()
	eventually(t, "second request admitted behind occupied pool", func() bool { return len(r.admission) == 2 })
	if _, prompts, _ := f.counts(); prompts != 1 {
		t.Fatalf("pool released before abort returned: prompts=%d", prompts)
	}
	close(f.abortGate)
	if err := <-first; err == nil {
		t.Fatal("cancelled request succeeded")
	}
	if _, _, aborts := f.counts(); aborts != 1 {
		t.Fatalf("aborts=%d", aborts)
	}
	close(f.promptGate)
	receive(t, f.prompted, "second prompt")
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeOverloadAndLockCleanup(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptGate = make(chan struct{})
	r := openTestRuntime(t, f, t.TempDir(), 1)
	t.Cleanup(func() { _ = r.Close() })
	done := make(chan error, 5)
	for i := range 5 {
		go func(i int) {
			_, err := r.Run(context.Background(), testRequest(fmt.Sprintf("session-%d", i), fmt.Sprintf("request-%d", i), "wait"))
			done <- err
		}(i)
	}
	receive(t, f.prompted, "first pool prompt")
	eventually(t, "all configured waiting capacity", func() bool { return len(r.admission) == cap(r.admission) })
	overflowCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := r.Run(overflowCtx, testRequest("overflow", "overflow", "overflow")); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("overflow error = %v, want ErrOverloaded", err)
	}
	close(f.promptGate)
	for range 5 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "session lock cleanup", func() bool { return lockCount(r) == 0 })
}

func TestRuntimeUncertainRequiresRecovery(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptCode = http.StatusInternalServerError
	f.abortOK = false
	data := t.TempDir()
	r := openTestRuntime(t, f, data, 1)
	_, err := r.Run(context.Background(), testRequest("agent", "original", "one"))
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("first error = %v", err)
	}
	if _, err := r.Run(context.Background(), testRequest("agent", "later", "two")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("later error = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openTestRuntime(t, f, data, 1)
	t.Cleanup(func() { _ = r.Close() })
	if _, err := r.Run(context.Background(), testRequest("agent", "later-restart", "three")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart error = %v", err)
	}
	f.mu.Lock()
	f.abortOK = true
	f.promptCode = 0
	f.mu.Unlock()
	if err := r.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), testRequest("agent", "original", "one")); err == nil {
		t.Fatal("original execution was retried")
	}
	if _, err := r.Run(context.Background(), testRequest("agent", "later", "two")); err != nil {
		t.Fatal(err)
	}
	if creates, prompts, aborts := f.counts(); creates != 1 || prompts != 2 || aborts != 2 {
		t.Fatalf("creates=%d prompts=%d aborts=%d", creates, prompts, aborts)
	}
}

// An unresolved run must not deny service to an unrelated workspace. The old
// runtime-wide flag made workspace A's stuck run refuse every request in
// workspace B, which is a cross-tenant denial-of-service primitive.
func TestRuntimeUncertainWorkspaceDoesNotBlockAnotherWorkspace(t *testing.T) {
	f := newFakeOpenCode(t)
	f.promptCode = http.StatusInternalServerError
	f.abortOK = false
	r := openTestRuntime(t, f, t.TempDir(), 1)
	t.Cleanup(func() { _ = r.Close() })

	stuck := testRequest("agent", "original", "one")
	if _, err := r.Run(context.Background(), stuck); !errors.Is(err, ErrUncertain) {
		t.Fatalf("first error = %v", err)
	}

	// A different workspace must still be served while the first one awaits
	// recovery. Its request succeeds because the fence is workspace-scoped.
	f.mu.Lock()
	f.promptCode = 0
	f.mu.Unlock()
	other := Request{WorkspaceID: "other-workspace", SessionKey: "agent", RequestID: "other", Prompt: opencode.Prompt{Text: "two"}}
	if _, err := r.Run(context.Background(), other); err != nil {
		t.Fatalf("unrelated workspace was blocked: %v", err)
	}
	// The fenced workspace is still refused, so the fix did not weaken it.
	if _, err := r.Run(context.Background(), testRequest("agent", "later", "three")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("fenced workspace error = %v", err)
	}
}

// A create intent records the configuration that was live when the attempt
// started. Adopting that session under different OpenCode permissions would
// prompt an orphan created with the old permissions as if the new profile
// owned it, so the intent must be refused before adoption.
func TestPendingCreateIntentIsRefusedAfterProfileChange(t *testing.T) {
	f := newFakeOpenCode(t)
	data := t.TempDir()
	open := func(permissions []opencode.PermissionRule) *Runtime {
		return openTestRuntimeWithPermissions(t, f, data, 1, permissions)
	}
	r := open(nil)
	if _, _, _, _, err := r.pendingCreateToken(context.Background(), testRequest("session", "request", "text")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with different permissions. The surviving intent describes a
	// session created under the old profile, so adoption must refuse it.
	r = open([]opencode.PermissionRule{{Permission: "bash", Pattern: "deploy", Action: "allow"}})
	t.Cleanup(func() { _ = r.Close() })
	if _, _, _, resumed, err := r.pendingCreateToken(context.Background(), testRequest("session", "request", "text")); !errors.Is(err, ErrConflict) || resumed {
		t.Fatalf("resumed=%v err=%v, want ErrConflict", resumed, err)
	}
	// The intent survives the refusal: it is the only evidence of a session
	// that may exist, so a rejected lookup must not discard it.
	if _, err := r.query(context.Background(), `SELECT ownership_token FROM pending_session_creates WHERE workspace=? AND session_key=?`, "workspace", "session"); err != nil {
		t.Fatal(err)
	}
	// A directory change is equally incompatible, and must not be adopted.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(context.Background(), Options{DataDir: data, BackendID: "test", MaxConcurrent: 1, MaxWaiting: 4, Timeout: 2 * time.Second,
		OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/somewhere-else"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if _, _, _, resumed, err := r2.pendingCreateToken(context.Background(), testRequest("session", "request", "text")); !errors.Is(err, ErrConflict) || resumed {
		t.Fatalf("directory change: resumed=%v err=%v, want ErrConflict", resumed, err)
	}
}

func TestRuntimeLegacyOpenCodeFailureRequiresRecoveryAfterSuccessfulAbort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeOpenCode)
	}{
		{
			name:  "prompt error",
			setup: func(f *fakeOpenCode) { f.promptCode = http.StatusInternalServerError },
		},
		{
			name: "invalid assistant response",
			setup: func(f *fakeOpenCode) {
				f.promptReply = `{"info":{"id":"msg_1","sessionID":"wrong","role":"assistant","time":{"completed":1},"finish":"stop"}}`
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOpenCode(t)
			tc.setup(f)
			data := t.TempDir()
			permissions := []opencode.PermissionRule{{Permission: "conductor_emit", Pattern: "*", Action: "allow"}}
			r := openTestRuntimeWithPermissions(t, f, data, 1, permissions)

			if _, err := r.Run(context.Background(), testRequest("agent", "original", "one")); !errors.Is(err, ErrUncertain) {
				t.Fatalf("first error = %v, want ErrUncertain", err)
			}
			if _, _, aborts := f.counts(); aborts != 1 {
				t.Fatalf("aborts=%d, want 1", aborts)
			}
			if _, err := r.Run(context.Background(), testRequest("agent", "later", "two")); !errors.Is(err, ErrUncertain) {
				t.Fatalf("later error = %v, want ErrUncertain", err)
			}
			if _, prompts, _ := f.counts(); prompts != 1 {
				t.Fatalf("blocked runtime prompted %d times, want 1", prompts)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}

			r = openTestRuntimeWithPermissions(t, f, data, 1, permissions)
			t.Cleanup(func() { _ = r.Close() })
			if _, err := r.Run(context.Background(), testRequest("agent", "after-restart", "three")); !errors.Is(err, ErrUncertain) {
				t.Fatalf("restart error = %v, want ErrUncertain", err)
			}
			f.mu.Lock()
			f.promptCode, f.promptReply = 0, ""
			f.mu.Unlock()
			if err := r.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(context.Background(), testRequest("agent", "original", "one")); err == nil {
				t.Fatal("recovery retried original execution")
			}
			if _, err := r.Run(context.Background(), testRequest("agent", "later", "two")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
