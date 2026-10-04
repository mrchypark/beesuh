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
)

func TestScopedConductorUsesOwnedConnection(t *testing.T) {
	var modelCalls, effects atomic.Int32
	headers := make(chan http.Header, 1)
	conductorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		headers <- r.Header.Clone()
		conductorFixtureAck(t, w, r)
	}))
	defer conductorServer.Close()
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []model.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if in.Messages[len(in.Messages)-1].Role == "tool" {
			completion(w, "done", "stop")
			return
		}
		modelCalls.Add(1)
		completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
	}))
	defer modelServer.Close()
	r, err := Open(context.Background(), Options{
		DataDir: t.TempDir(), BackendID: "scoped-conductor", MaxConcurrent: 1, MaxWaiting: 1,
		RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{APIKey: "service-token", AccountID: c.UserID}, nil
		},
		Native: &NativeOptions{Providers: map[string]model.Config{"work": {BaseURL: modelServer.URL + "/v1", Model: "test"}}, DefaultProvider: "work", Conductors: map[string]conductor.Config{
			"workflow": {BaseURL: conductorServer.URL, Token: "service-token", RequireScope: true, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, c := range []Connection{
		{ID: "provider-u1", UserID: "u1", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "provider-u1", State: "ready"},
		{ID: "conductor-u1", UserID: "u1", WorkspaceID: "w", Kind: "conductor", TargetID: "workflow", CredentialRef: "conductor-u1", State: "ready"},
		{ID: "provider-u2", UserID: "u2", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "provider-u2", State: "ready"},
	} {
		if err := r.RegisterConnection(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "u1", WorkspaceID: "w"})
	_, err = r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "one", ConnectionID: "provider-u1", ConductorConnectionID: "conductor-u1", Prompt: Prompt{Text: "deploy"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-headers:
		if got.Get("Authorization") != "Bearer service-token" || got.Get("X-Beesuh-User-ID") != "u1" || got.Get("X-Beesuh-Workspace-ID") != "w" || got.Get("X-Beesuh-Connection-ID") != "conductor-u1" {
			t.Fatalf("gateway headers = %v", got)
		}
	default:
		t.Fatal("no Conductor effect")
	}
	if _, err := r.Run(WithPrincipal(context.Background(), Principal{UserID: "u2", WorkspaceID: "w"}), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "two", ConnectionID: "provider-u2", ConductorConnectionID: "conductor-u1", Prompt: Prompt{Text: "deploy"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign Conductor connection error = %v", err)
	}
	if effects.Load() != 1 || modelCalls.Load() != 1 {
		t.Fatalf("foreign connection reached execution: effects=%d model=%d", effects.Load(), modelCalls.Load())
	}
}

func TestScopedConductorRechecksAuthorizationBeforeEffect(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	var effects atomic.Int32
	conductorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		conductorFixtureAck(t, w, r)
	}))
	defer conductorServer.Close()
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Store(false) // The service membership changed after model admission.
		completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
	}))
	defer modelServer.Close()
	r, err := Open(context.Background(), Options{
		DataDir: t.TempDir(), BackendID: "recheck-conductor", MaxConcurrent: 1, MaxWaiting: 1,
		RequireIdentity: true, Authorize: func(context.Context, Principal) error {
			if !active.Load() {
				return ErrForbidden
			}
			return nil
		},
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{APIKey: "service-token", AccountID: c.UserID}, nil
		},
		Native: &NativeOptions{Providers: map[string]model.Config{"work": {BaseURL: modelServer.URL + "/v1", Model: "test"}}, DefaultProvider: "work", Conductors: map[string]conductor.Config{
			"workflow": {BaseURL: conductorServer.URL, Token: "service-token", RequireScope: true, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, c := range []Connection{
		{ID: "provider", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "provider", State: "ready"},
		{ID: "conductor", UserID: "u", WorkspaceID: "w", Kind: "conductor", TargetID: "workflow", CredentialRef: "conductor", State: "ready"},
	} {
		if err := r.RegisterConnection(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "u", WorkspaceID: "w"})
	if _, err := r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "one", ConnectionID: "provider", ConductorConnectionID: "conductor", Prompt: Prompt{Text: "deploy"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked user effect error = %v", err)
	}
	if effects.Load() != 0 {
		t.Fatalf("deactivated user reached Conductor: %d", effects.Load())
	}
}

func TestScopedConductorRejectsOtherTargetEventWithoutBlocking(t *testing.T) {
	var effects atomic.Int32
	conductorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		conductorFixtureAck(t, w, r)
	}))
	defer conductorServer.Close()
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []model.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if in.Messages[len(in.Messages)-1].Role == "tool" {
			completion(w, "safe", "stop")
			return
		}
		// The tool schema contains this event because another configured target
		// permits it, but this request's selected connection does not.
		completion(w, "", "tool_calls", model.ToolCall{ID: "foreign", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"other","data":{}}`}})
	}))
	defer modelServer.Close()
	r, err := Open(context.Background(), Options{
		DataDir: t.TempDir(), BackendID: "target-events", MaxConcurrent: 1, MaxWaiting: 1,
		RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{APIKey: "service-token", AccountID: c.UserID}, nil
		},
		Native: &NativeOptions{Providers: map[string]model.Config{"work": {BaseURL: modelServer.URL + "/v1", Model: "test"}}, DefaultProvider: "work", Conductors: map[string]conductor.Config{
			"deploy-target": {BaseURL: conductorServer.URL, Token: "service-token", RequireScope: true, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}},
			"other-target":  {BaseURL: conductorServer.URL, Token: "service-token", RequireScope: true, AllowedEvents: []string{"other"}, Bindings: map[string]conductor.Binding{"other": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, c := range []Connection{
		{ID: "provider", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "provider", State: "ready"},
		{ID: "deploy-connection", UserID: "u", WorkspaceID: "w", Kind: "conductor", TargetID: "deploy-target", CredentialRef: "deploy", State: "ready"},
	} {
		if err := r.RegisterConnection(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "u", WorkspaceID: "w"})
	out, err := r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "one", ConnectionID: "provider", ConductorConnectionID: "deploy-connection", Prompt: Prompt{Text: "deploy"}})
	if !errors.Is(err, ErrForbidden) || out.State != "failed" {
		t.Fatalf("run = %+v, %v", out, err)
	}
	if effects.Load() != 0 || runFencedNow(r) {
		t.Fatalf("foreign event caused external effect/block: effects=%d blocked=%v", effects.Load(), runFencedNow(r))
	}
}

func TestScopedConductorEffectFinishesBeforeRevocation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	conductorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		conductorFixtureAck(t, w, r)
	}))
	defer conductorServer.Close()
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []model.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if in.Messages[len(in.Messages)-1].Role == "tool" {
			completion(w, "done", "stop")
			return
		}
		completion(w, "", "tool_calls", model.ToolCall{ID: "emit", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"deploy","data":{}}`}})
	}))
	defer modelServer.Close()
	r, err := Open(context.Background(), Options{
		DataDir: t.TempDir(), BackendID: "effect-revoke", MaxConcurrent: 1, MaxWaiting: 1,
		RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{APIKey: "service-token", AccountID: c.UserID}, nil
		},
		Native: &NativeOptions{Providers: map[string]model.Config{"work": {BaseURL: modelServer.URL + "/v1", Model: "test"}}, DefaultProvider: "work", Conductors: map[string]conductor.Config{
			"workflow": {BaseURL: conductorServer.URL, Token: "ignored", RequireScope: true, AllowedEvents: []string{"deploy"}, Bindings: map[string]conductor.Binding{"deploy": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, c := range []Connection{
		{ID: "provider", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "provider", State: "ready"},
		{ID: "conductor", UserID: "u", WorkspaceID: "w", Kind: "conductor", TargetID: "workflow", CredentialRef: "conductor", State: "ready"},
	} {
		if err := r.RegisterConnection(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "u", WorkspaceID: "w"})
	run := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "one", ConnectionID: "provider", ConductorConnectionID: "conductor", Prompt: Prompt{Text: "deploy"}})
		run <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Conductor effect did not begin")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- r.SetConnectionState(ctx, "conductor", "revoked") }()
	select {
	case err := <-revoked:
		t.Fatalf("revocation raced admitted effect: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-run; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if _, err := r.Connection(ctx, "conductor"); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("revoked connection = %v", err)
	}
}
