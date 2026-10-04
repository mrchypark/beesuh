package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/model"
)

type identityRoundTrip func(*http.Request) (*http.Response, error)

func (f identityRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestServiceRejectsInvalidUTF8IdentityBeforeScopedAccess(t *testing.T) {
	r := &Runtime{opts: Options{RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil }}}
	for _, userID := range []string{"\xff", "\xfe"} {
		ctx := WithPrincipal(context.Background(), Principal{UserID: userID, WorkspaceID: "workspace"})
		if _, err := r.scopedWorkspace(ctx, "workspace"); !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "valid UTF-8") {
			t.Fatalf("scopedWorkspace(%q) = %v", userID, err)
		}
		if _, err := r.History(ctx, "workspace", "session"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("History(%q) = %v", userID, err)
		}
		if _, err := r.GetRun(ctx, "workspace", "request"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("GetRun(%q) = %v", userID, err)
		}
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "사용자", WorkspaceID: "작업공간"})
	got, err := r.scopedWorkspace(ctx, "작업공간")
	if err != nil {
		t.Fatal(err)
	}
	want := "user_" + digest([]string{"사용자", "작업공간"})
	if got != want {
		t.Fatalf("scope = %q, want %q", got, want)
	}
}

func TestResolvedCallerHTTPClientsDiscardSharedCookieJar(t *testing.T) {
	target, err := url.Parse("https://provider.invalid")
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(target, []*http.Cookie{{Name: "shared", Value: "cookie"}})
	var cookies []string
	base := &http.Client{Jar: jar, Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
		cookies = append(cookies, request.Header.Get("Cookie"))
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	r := &Runtime{
		opts: Options{ResolveCredential: func(context.Context, Connection) (Credential, error) {
			return Credential{HTTPClient: base, AccountID: "account"}, nil
		}},
		native: &nativeEngine{opts: NativeOptions{Providers: map[string]model.Config{"provider": {}}}},
	}
	clients := make([]*http.Client, 0, 2)
	for _, id := range []string{"one", "two"} {
		credential, err := r.resolveCredential(context.Background(), Connection{ID: id, UserID: id, WorkspaceID: "workspace", Kind: "provider", TargetID: "provider", CredentialRef: id})
		if err != nil {
			t.Fatal(err)
		}
		if credential.HTTPClient.Jar != nil {
			t.Fatal("resolved client retained the caller cookie jar")
		}
		clients = append(clients, credential.HTTPClient)
		response, err := credential.HTTPClient.Get(target.String())
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if clients[0] == clients[1] {
		t.Fatal("connections reused one HTTP client")
	}
	if strings.Join(cookies, ",") != "," {
		t.Fatalf("caller cookies leaked to connections: %q", cookies)
	}
	if got := jar.Cookies(target); len(got) != 1 || got[0].Name != "shared" {
		t.Fatalf("caller jar changed: %#v", got)
	}
}

func TestCredentialRejectsInvalidUTF8Identifiers(t *testing.T) {
	r := &Runtime{
		opts: Options{ResolveCredential: func(context.Context, Connection) (Credential, error) {
			return Credential{APIKey: "key", AccountID: "\xff"}, nil
		}},
		native: &nativeEngine{opts: NativeOptions{Providers: map[string]model.Config{"provider": {}}}},
	}
	valid := Connection{ID: "connection", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "provider", CredentialRef: "credential"}
	if _, err := r.resolveCredential(context.Background(), valid); !errors.Is(err, ErrConnectionUnavailable) || !strings.Contains(err.Error(), "account ID must be valid UTF-8") {
		t.Fatalf("invalid account ID = %v", err)
	}
	valid.ID = "\xff"
	if _, err := r.resolveCredential(context.Background(), valid); !errors.Is(err, ErrConnectionUnavailable) || !strings.Contains(err.Error(), "connection identifiers must be valid UTF-8") {
		t.Fatalf("invalid connection ID = %v", err)
	}
}

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
	opts := Options{DataDir: t.TempDir(), BackendID: "test", MaxConcurrent: 1, RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil }, Native: &NativeOptions{TokenBudgetPolicy: TokenBudgetAdvisory, Provider: model.Config{Kind: "codex-cli", Model: "test", CodexHome: t.TempDir()}}}
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
