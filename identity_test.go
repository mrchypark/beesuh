package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/model"
)

func TestServiceQueueRechecksRevokedConnection(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
		}
		completion(w, "ok", "stop")
	}))
	defer backend.Close()
	opts := nativeOptions(t.TempDir(), "", backend.URL, nil)
	opts.RequireIdentity = true
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		return Credential{APIKey: "key", AccountID: c.UserID}, nil
	}
	opts.Authorize = func(context.Context, Principal) error { return nil }
	opts.MaxConcurrent = 1
	r := nativeOpen(t, opts)
	defer r.Close()
	for _, id := range []string{"first", "queued"} {
		if err := r.RegisterConnection(context.Background(), Connection{ID: id, UserID: id, WorkspaceID: "w", Kind: "provider", TargetID: "default", State: "ready", CredentialRef: "test-ref"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run := func(id string, done chan error) {
		_, err := r.Run(WithPrincipal(ctx, Principal{UserID: id, WorkspaceID: "w"}), Request{WorkspaceID: "w", SessionKey: "same", RequestID: "same", ConnectionID: id, Prompt: Prompt{Text: "hello"}})
		done <- err
	}
	first, queued := make(chan error, 1), make(chan error, 1)
	go run("first", first)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first model did not start")
	}
	go run("queued", queued)
	// Observe admission, not a timing assumption about when the goroutine runs.
	for len(r.admission) != 2 {
		select {
		case <-ctx.Done():
			t.Fatal("second request did not queue")
		case <-time.After(time.Millisecond):
		}
	}
	if err := r.SetConnectionState(WithPrincipal(ctx, Principal{UserID: "queued", WorkspaceID: "w"}), "queued", "revoked"); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("queued result = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("revoked queued request reached provider: %d calls", calls.Load())
	}
}

func TestIdentityModeCannotDowngradeOrClaimOperatorSessions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { completion(w, "ok", "stop") }))
	defer backend.Close()
	opts := nativeOptions(t.TempDir(), "", backend.URL, nil)
	opts.RequireIdentity = true
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		return Credential{APIKey: "key", AccountID: c.UserID}, nil
	}
	opts.Authorize = func(context.Context, Principal) error { return nil }
	r := nativeOpen(t, opts)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	opts.RequireIdentity = false
	opts.Authorize = nil
	opts.ResolveCredential = nil
	if leaked, err := Open(context.Background(), opts); err == nil {
		leaked.Close()
		t.Fatal("service database reopened as operator")
	}
	opts.DataDir = t.TempDir()
	r = nativeOpen(t, opts)
	if _, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", Prompt: Prompt{Text: "operator"}}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	opts.RequireIdentity = true
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		return Credential{APIKey: "key", AccountID: c.UserID}, nil
	}
	opts.Authorize = func(context.Context, Principal) error { return nil }
	if claimed, err := Open(context.Background(), opts); err == nil {
		claimed.Close()
		t.Fatal("operator sessions silently claimed by service")
	}
}

func TestServiceRejectsUnscopedEntryAndAccountHome(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	ctx := context.Background()
	if _, err := r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", Prompt: Prompt{Text: "hello"}}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous Run: %v", err)
	}
	if _, err := r.History(ctx, "w", "s"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous History: %v", err)
	}
	if _, err := r.GetRun(ctx, "w", "r"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous GetRun: %v", err)
	}
	if _, err := r.Providers(ctx); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous Providers: %v", err)
	}
	if _, err := r.Status(ctx); !errors.Is(err, ErrForbidden) {
		t.Fatalf("global Status: %v", err)
	}
	opts := Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil }, Native: &NativeOptions{Provider: model.Config{Kind: "codex-cli", Model: "test"}}}
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) { return Credential{AccountID: c.UserID}, nil }
	secured := nativeOpen(t, opts)
	defer secured.Close()
	if err := secured.RegisterConnection(ctx, Connection{ID: "cli", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "default", CredentialRef: "cli-ref", State: "ready"}); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("unresolved CLI account registered ready: %v", err)
	}
	if err := secured.RegisterConnection(ctx, Connection{ID: "cli", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "default", CredentialRef: "cli-ref", State: "needs_login"}); err != nil {
		t.Fatal(err)
	}
	if _, err := secured.Run(WithPrincipal(ctx, Principal{UserID: "u", WorkspaceID: "w"}), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", ConnectionID: "cli", Prompt: Prompt{Text: "hi"}}); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("inherited CLI account error = %v", err)
	}
}

func TestServiceCredentialsAndOpaqueStateFollowResolvedAccount(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	var replayed []bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		b, _ := json.Marshal(body)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		replayed = append(replayed, strings.Contains(string(b), "opaque-secret"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "reasoning", "id": "rs_test", "summary": []any{}, "encrypted_content": "opaque-secret"},
			map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}},
		}})
	}))
	defer backend.Close()
	var account atomic.Value
	account.Store("account-a")
	opts := nativeOptions(t.TempDir(), "", backend.URL, nil)
	opts.RequireIdentity = true
	opts.Authorize = func(context.Context, Principal) error { return nil }
	opts.Native.Provider = model.Config{Kind: "codex-api", BaseURL: backend.URL + "/v1", Model: "test", APIKey: "global-must-not-be-used"}
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		id := account.Load().(string)
		if c.UserID == "bob" {
			id = "bob-account"
		}
		return Credential{APIKey: "key-" + id, AccountID: id}, nil
	}
	r := nativeOpen(t, opts)
	defer r.Close()
	for _, user := range []string{"alice", "bob"} {
		if err := r.RegisterConnection(context.Background(), Connection{ID: user, UserID: user, WorkspaceID: "w", Kind: "provider", TargetID: "default", CredentialRef: "ref-" + user, State: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	call := func(user string, i int) {
		_, err := r.Run(WithPrincipal(context.Background(), Principal{UserID: user, WorkspaceID: "w"}), Request{WorkspaceID: "w", SessionKey: "s", RequestID: fmt.Sprint(i), ConnectionID: user, Prompt: Prompt{Text: "hello"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	call("alice", 1)
	call("alice", 2) // same account may retain opaque state
	account.Store("account-b")
	call("alice", 3) // changed remote account must reconstruct portable history
	call("bob", 1)   // identical target/session/request, different user credential
	ctx := WithPrincipal(context.Background(), Principal{UserID: "alice", WorkspaceID: "w"})
	if err := r.SetConnectionState(ctx, "alice", "needs_login"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetConnectionState(ctx, "alice", "ready"); err != nil {
		t.Fatal(err)
	}
	call("alice", 4) // reauthentication invalidates state even for same account
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(replayed) != "[false true false false false]" {
		t.Fatalf("state isolation = %v", replayed)
	}
	if strings.Join(auths, ",") != "Bearer key-account-a,Bearer key-account-a,Bearer key-account-b,Bearer key-bob-account,Bearer key-account-b" {
		t.Fatalf("wrong resolved account: %v", auths)
	}
	rows, err := r.query(context.Background(), `SELECT payload FROM journal`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row[0].(string), "key-account") || strings.Contains(row[0].(string), "global-must") {
			t.Fatal("credential persisted")
		}
	}
}
