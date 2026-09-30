package beesuh

import (
	"context"
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
	prompts   int
	aborts    int
	active    int
	maxActive int

	promptGate  chan struct{}
	prompted    chan struct{}
	abortGate   chan struct{}
	aborted     chan struct{}
	abortOK     bool
	promptCode  int
	promptReply string
}

func newFakeOpenCode(t *testing.T) *fakeOpenCode {
	t.Helper()
	f := &fakeOpenCode{prompted: make(chan struct{}, 32), aborted: make(chan struct{}, 32), abortOK: true}
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
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/session/status" {
		_, _ = w.Write([]byte(`{}`)) // OpenCode omits idle sessions.
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/session/") {
		id := strings.TrimPrefix(r.URL.Path, "/session/")
		_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
		f.mu.Lock()
		f.prompts++
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
		f.mu.Lock()
		f.aborts++
		gate, ok := f.abortGate, f.abortOK
		f.mu.Unlock()
		f.aborted <- struct{}{}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = fmt.Fprintf(w, "%t", ok)
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
