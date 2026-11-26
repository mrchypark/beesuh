package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/model"
)

func TestPreferRespondAsyncParsing(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "bare", values: []string{"respond-async"}, want: true},
		{name: "case and list", values: []string{"wait=10, RESPOND-ASYNC"}, want: true},
		{name: "multiple fields", values: []string{"unknown", "respond-async"}, want: true},
		{name: "quoted comma", values: []string{"example=\"a,b\", respond-async"}, want: true},
		{name: "quoted false positive", values: []string{"example=\"respond-async,other\""}},
		{name: "parameterized", values: []string{"respond-async; foo=bar"}},
		{name: "value", values: []string{"respond-async=false"}},
		{name: "unknown", values: []string{"wait=10"}},
		{name: "unterminated quote", values: []string{"example=\"respond-async"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat", nil)
			for _, value := range test.values {
				r.Header.Add("Prefer", value)
			}
			if got := wantsAsync(r); got != test.want {
				t.Fatalf("wantsAsync = %v, want %v", got, test.want)
			}
		})
	}
}

type asyncHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

func asyncHTTP(t *testing.T, method, endpoint, token, body string, prefer bool) asyncHTTPResponse {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if prefer {
		req.Header.Set("Prefer", "respond-async")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return asyncHTTPResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: b}
}

type ResultForTest struct {
	State  string
	Output json.RawMessage
}

func waitAsyncState(t *testing.T, endpoint, token, state string) ResultForTest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		response := asyncHTTP(t, http.MethodGet, endpoint, token, "", false)
		if response.status == http.StatusOK {
			var out ResultForTest
			if err := json.Unmarshal(response.body, &out); err != nil {
				t.Fatal(err)
			}
			if out.State == state {
				return out
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach %s: %d %s", state, response.status, response.body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAsyncChatReturnsAcceptedAndPollsToCompletion(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte("{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"async ok\"},\"finish_reason\":\"stop\"}]}"))
	}))
	defer backend.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "async-http", MaxConcurrent: 1, MaxWaiting: 2, Timeout: 5 * time.Second,
		Native: &beesuh.NativeOptions{Provider: model.Config{BaseURL: backend.URL, Model: "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err := New(runtime, "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	body := "{\"workspace_id\":\"work\",\"session_key\":\"chat\",\"request_id\":\"request-1\",\"message\":\"hello\"}"
	response := asyncHTTP(t, http.MethodPost, server.URL+"/v1/chat", "secret", body, true)
	if response.status != http.StatusAccepted || response.header.Get("Preference-Applied") != "respond-async" || response.header.Get("Vary") != "Prefer" {
		t.Fatalf("accepted response = %d %#v %s", response.status, response.header, response.body)
	}
	location := response.header.Get("Location")
	if location != "/v1/runs/request-1?workspace_id=work" {
		t.Fatalf("location = %q", location)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not dispatch")
	}
	runningDuplicate := asyncHTTP(t, http.MethodPost, server.URL+"/v1/chat", "secret", body, true)
	var runningResult struct {
		State string
	}
	if err := json.Unmarshal(runningDuplicate.body, &runningResult); err != nil || runningDuplicate.status != http.StatusAccepted || runningResult.State != "running" {
		t.Fatalf("running duplicate = %d %s, %v", runningDuplicate.status, runningDuplicate.body, err)
	}
	waitAsyncState(t, server.URL+location, "secret", "running")
	close(release)
	out := waitAsyncState(t, server.URL+location, "secret", "succeeded")
	if calls.Load() != 1 || !strings.Contains(string(out.Output), "async ok") {
		t.Fatalf("calls=%d output=%s", calls.Load(), out.Output)
	}
	duplicate := asyncHTTP(t, http.MethodPost, server.URL+"/v1/chat", "secret", body, true)
	var duplicateResult struct {
		State string
	}
	if err := json.Unmarshal(duplicate.body, &duplicateResult); err != nil || duplicate.status != http.StatusOK || duplicateResult.State != "succeeded" {
		t.Fatalf("duplicate result = %d %s, %v", duplicate.status, duplicate.body, err)
	}
}

func TestAsyncComposReturnsAcceptedAndPollsToCompletion(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "{\"answer\":\"Beesuh\"}"}, "finish_reason": "stop"}}})
	}))
	defer backend.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "async-compos", RequireIdentity: true,
		Authorize: func(context.Context, beesuh.Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c beesuh.Connection) (beesuh.Credential, error) {
			return beesuh.Credential{APIKey: "fixture", AccountID: c.UserID}, nil
		},
		PrepareAsync: func(ctx context.Context, p beesuh.Principal) (context.Context, error) {
			return beesuh.WithPrincipal(ctx, p), nil
		},
		MaxConcurrent: 1, MaxWaiting: 2, Timeout: 5 * time.Second,
		Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"team": {BaseURL: backend.URL, Model: "test"}}, DefaultProvider: "team"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.RegisterConnection(context.Background(), beesuh.Connection{ID: "producer-model", UserID: "producer", WorkspaceID: "work", Kind: "provider", TargetID: "team", CredentialRef: "producer", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.StartAgentWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err := NewAuthenticatedWithComposProducer(runtime, func(*http.Request) (beesuh.Principal, error) {
		return beesuh.Principal{UserID: "producer", WorkspaceID: "work"}, nil
	}, func(_ context.Context, p beesuh.Principal) error {
		if p.UserID != "producer" {
			return beesuh.ErrForbidden
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	body, err := json.Marshal(map[string]any{
		"agent_definition": map[string]any{"instructions": "Return JSON."},
		"task_definition": map[string]any{
			"input_schema":  map[string]any{"type": "object"},
			"output_schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}},
		},
		"input": map[string]any{}, "subject_ref": map[string]any{},
		"_compos": map[string]any{"workspace_id": "work", "execution_request_id": "compos-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := asyncHTTP(t, http.MethodPost, server.URL+"/v1/compos?session_key=compos&provider=team&connection_id=producer-model", "producer", string(body), true)
	if response.status != http.StatusAccepted || response.header.Get("Preference-Applied") != "respond-async" || response.header.Get("Vary") != "Prefer" || response.header.Get("Location") != "/v1/runs/compos-1?workspace_id=work" {
		t.Fatalf("compos accepted = %d %#v %s", response.status, response.header, response.body)
	}
	out := waitAsyncState(t, server.URL+response.header.Get("Location"), "producer", "succeeded")
	if !strings.Contains(string(out.Output), "Beesuh") {
		t.Fatalf("output = %s", out.Output)
	}
}

func TestAsyncWithoutPrepareHookReturns503(t *testing.T) {
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "async-no-hook", RequireIdentity: true,
		Authorize: func(context.Context, beesuh.Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c beesuh.Connection) (beesuh.Credential, error) {
			return beesuh.Credential{APIKey: "fixture", AccountID: c.UserID}, nil
		},
		MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"team": {BaseURL: "http://127.0.0.1:1", Model: "test"}}, DefaultProvider: "team"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.RegisterConnection(context.Background(), beesuh.Connection{ID: "alice-model", UserID: "alice", WorkspaceID: "work", Kind: "provider", TargetID: "team", CredentialRef: "alice", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	h, err := NewAuthenticated(runtime, func(*http.Request) (beesuh.Principal, error) {
		return beesuh.Principal{UserID: "alice", WorkspaceID: "work"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	body, err := json.Marshal(map[string]any{"workspace_id": "work", "session_key": "chat", "request_id": "no-hook", "connection_id": "alice-model", "message": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	response := asyncHTTP(t, http.MethodPost, server.URL+"/v1/chat", "alice", string(body), true)
	if response.status != http.StatusServiceUnavailable || response.header.Get("Preference-Applied") != "" || response.header.Get("Location") != "" {
		t.Fatalf("no-hook response = %d %#v %s", response.status, response.header, response.body)
	}
}

func TestAsyncLocationEscapesRequestAndWorkspace(t *testing.T) {
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "async-location", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Provider: model.Config{BaseURL: "http://127.0.0.1:1", Model: "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	h, err := New(runtime, "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	body, err := json.Marshal(map[string]any{"workspace_id": "work space", "session_key": "chat", "request_id": "a/b", "message": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	response := asyncHTTP(t, http.MethodPost, server.URL+"/v1/chat", "secret", string(body), true)
	if response.status != http.StatusAccepted || response.header.Get("Location") != "/v1/runs/a%2Fb?workspace_id=work+space" {
		t.Fatalf("escaped location = %d %#v %s", response.status, response.header, response.body)
	}
}
