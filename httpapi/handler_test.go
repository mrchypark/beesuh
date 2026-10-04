package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

func TestNativeChatStatusHistoryAndRun(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"native hello"},"finish_reason":"stop"}]}`))
	}))
	defer backend.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "native-test", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Provider: model.Config{BaseURL: backend.URL, Model: "test"}},
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

	chat := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hello"}`)
	if chat.status != http.StatusOK || !strings.Contains(string(chat.body), "native hello") || calls.Load() != 1 {
		t.Fatalf("chat = %d: %s (calls=%d)", chat.status, chat.body, calls.Load())
	}
	history := api(t, http.MethodGet, server.URL+"/v1/history?workspace_id=work&session_key=user", "secret", "")
	if history.status != http.StatusOK || !strings.Contains(string(history.body), `"kind":"message"`) {
		t.Fatalf("history = %d: %s", history.status, history.body)
	}
	status := api(t, http.MethodGet, server.URL+"/v1/status", "secret", "")
	if status.status != http.StatusOK || !strings.Contains(string(status.body), `"backend":"native"`) {
		t.Fatalf("status = %d: %s", status.status, status.body)
	}
	run := api(t, http.MethodGet, server.URL+"/v1/runs/one?workspace_id=work", "secret", "")
	if run.status != http.StatusOK || !strings.Contains(string(run.body), `"state":"succeeded"`) {
		t.Fatalf("run = %d: %s", run.status, run.body)
	}
}

func TestNamedProvidersSelectOnlyTheRequestedConnection(t *testing.T) {
	serverFor := func(text string, calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/chat/completions" {
				http.NotFound(w, r)
				return
			}
			calls.Add(1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}]}`))
		}))
	}
	var firstCalls, secondCalls atomic.Int32
	first, second := serverFor("first", &firstCalls), serverFor("second", &secondCalls)
	defer first.Close()
	defer second.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{DataDir: t.TempDir(), BackendID: "named-test", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second, Native: &beesuh.NativeOptions{
		Providers: map[string]model.Config{
			"first":  {BaseURL: first.URL, Model: "model-a"},
			"second": {BaseURL: second.URL, Model: "model-b"},
		},
		DefaultProvider: "first",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	h, err := New(runtime, "secret")
	if err != nil {
		t.Fatal(err)
	}
	apiServer := httptest.NewServer(h)
	defer apiServer.Close()

	chosen := chat(t, apiServer.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","provider":"second","message":"hello"}`)
	if chosen.status != http.StatusOK || !strings.Contains(string(chosen.body), "second") || firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("chosen = %d: %s (first=%d second=%d)", chosen.status, chosen.body, firstCalls.Load(), secondCalls.Load())
	}
	unknown := chat(t, apiServer.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"two","provider":"missing","message":"hello"}`)
	if unknown.status != http.StatusBadRequest {
		t.Fatalf("unknown = %d: %s", unknown.status, unknown.body)
	}
	providers := api(t, http.MethodGet, apiServer.URL+"/v1/providers", "secret", "")
	if providers.status != http.StatusOK || !strings.Contains(string(providers.body), `"id":"first"`) || !strings.Contains(string(providers.body), `"id":"second"`) || strings.Contains(string(providers.body), first.URL) || strings.Contains(string(providers.body), second.URL) {
		t.Fatalf("providers = %d: %s", providers.status, providers.body)
	}
	if unauthorized := api(t, http.MethodGet, apiServer.URL+"/v1/providers", "wrong", ""); unauthorized.status != http.StatusUnauthorized {
		t.Fatalf("providers unauthorized = %d: %s", unauthorized.status, unauthorized.body)
	}
}

func TestComposEndpointMapsAuthenticatedInput(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"Beesuh\"}"},"finish_reason":"stop"}]}`))
	}))
	defer backend.Close()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "native-test", RequireIdentity: true,
		Authorize: func(context.Context, beesuh.Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c beesuh.Connection) (beesuh.Credential, error) {
			return beesuh.Credential{APIKey: "test-key", AccountID: c.UserID}, nil
		},
		MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"selected": {BaseURL: backend.URL, Model: "test"}}, DefaultProvider: "selected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.RegisterConnection(context.Background(), beesuh.Connection{ID: "producer-model", UserID: "producer", WorkspaceID: "demo", Kind: "provider", TargetID: "selected", CredentialRef: "producer-account", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	h, err := NewAuthenticatedWithComposProducer(runtime, func(r *http.Request) (beesuh.Principal, error) {
		if r.Header.Get("Authorization") != "Bearer producer" {
			return beesuh.Principal{}, beesuh.ErrUnauthenticated
		}
		return beesuh.Principal{UserID: "producer", WorkspaceID: "demo"}, nil
	}, func(_ context.Context, p beesuh.Principal) error {
		if p.UserID != "producer" || p.WorkspaceID != "demo" {
			return beesuh.ErrForbidden
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	body, err := os.ReadFile("../examples/compos-input.json")
	if err != nil {
		t.Fatal(err)
	}
	response := api(t, http.MethodPost, server.URL+"/v1/compos?session_key=user&provider=selected&connection_id=producer-model", "producer", string(body))
	if response.status != http.StatusOK || !strings.Contains(string(response.body), `"session_id"`) || !strings.Contains(string(response.body), `"answer"`) {
		t.Fatalf("compos = %d: %s", response.status, response.body)
	}
	unauthorized := api(t, http.MethodPost, server.URL+"/v1/compos?session_key=user", "wrong", string(body))
	if unauthorized.status != http.StatusUnauthorized {
		t.Fatalf("unauthorized = %d: %s", unauthorized.status, unauthorized.body)
	}
}

func TestChatReusesSessionAndReplaysRequest(t *testing.T) {
	var creates, prompts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			creates.Add(1)
			_, _ = w.Write([]byte(`{"id":"ses_direct"}`))
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			// Session listing is only reached when a create response was lost.
			// This backend has no session to rediscover, so it answers empty.
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_direct", r.URL.Path == "/session/status":
			if r.URL.Path == "/session/status" {
				_, _ = w.Write([]byte(`{"ses_direct":{"type":"idle"}}`))
			} else {
				_, _ = w.Write([]byte(`{"id":"ses_direct"}`))
			}
		case r.URL.Path == "/session/ses_direct/message":
			prompts.Add(1)
			_, _ = w.Write([]byte(`{"info":{"id":"msg_direct","sessionID":"ses_direct","role":"assistant","time":{"completed":1}},"parts":[{"type":"text","text":"hello"}]}`))
		default:
			t.Errorf("unexpected backend path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	runtime := newRuntime(t, backend.URL)
	defer runtime.Close()
	h, err := New(runtime, "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()

	first := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"first"}`)
	if first.status != http.StatusOK {
		t.Fatalf("first status = %d: %s", first.status, first.body)
	}
	second := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"two","message":"second"}`)
	if second.status != http.StatusOK {
		t.Fatalf("second status = %d: %s", second.status, second.body)
	}
	replay := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"first"}`)
	if replay.status != http.StatusOK {
		t.Fatalf("replay status = %d: %s", replay.status, replay.body)
	}
	if creates.Load() != 1 || prompts.Load() != 2 {
		t.Fatalf("creates=%d prompts=%d", creates.Load(), prompts.Load())
	}
	var out beesuh.Result
	if err := json.Unmarshal(replay.body, &out); err != nil || out.SessionID != "ses_direct" || out.State != "succeeded" {
		t.Fatalf("replay result = %#v, %v", out, err)
	}
	status := api(t, http.MethodGet, server.URL+"/v1/status", "secret", "")
	if status.status != http.StatusOK || !strings.Contains(string(status.body), `"backend":"opencode"`) {
		t.Fatalf("status = %d: %s", status.status, status.body)
	}
	history := api(t, http.MethodGet, server.URL+"/v1/history?workspace_id=work&session_key=user", "secret", "")
	if history.status != http.StatusOK || string(history.body) != "[]\n" {
		t.Fatalf("history = %d: %s", history.status, history.body)
	}
	run := api(t, http.MethodGet, server.URL+"/v1/runs/one?workspace_id=work", "secret", "")
	if run.status != http.StatusOK || !strings.Contains(string(run.body), `"state":"succeeded"`) {
		t.Fatalf("run = %d: %s", run.status, run.body)
	}
}

func TestChatRejectsUnauthorizedMalformedAndLargeInput(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer backend.Close()
	runtime := newRuntime(t, backend.URL)
	defer runtime.Close()
	h, err := New(runtime, "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()

	unauthorized := chat(t, server.URL, "wrong", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hi"}`)
	if unauthorized.status != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.status)
	}
	malformed := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hi","model":"x"}`)
	if malformed.status != http.StatusBadRequest {
		t.Fatalf("malformed status = %d", malformed.status)
	}
	large := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"`+strings.Repeat("x", maxBodyBytes)+`"}`)
	if large.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("large status = %d", large.status)
	}
	if calls.Load() != 0 {
		t.Fatalf("backend calls = %d", calls.Load())
	}
}

func TestNativeInputLimitIsBadRequest(t *testing.T) {
	if got := errorStatus(errors.Join(beesuh.ErrInvalidInput, errors.New("native input exceeds 12 KiB"))); got != http.StatusBadRequest {
		t.Fatalf("status = %d", got)
	}
	if got := errorStatus(beesuh.ErrNotFound); got != http.StatusNotFound {
		t.Fatalf("not found status = %d", got)
	}
}

func TestRuntimeFailureDoesNotExposeTransportAddress(t *testing.T) {
	const internalAddress = "http://127.0.0.1:9999/internal"
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir:   t.TempDir(),
		BackendID: "transport-error",
		OpenCode: opencode.Config{BaseURL: "http://provider.invalid", Directory: "/private/workspace", HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/session" && r.Method == http.MethodPost {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"ses_internal"}`))}, nil
			}
			if r.URL.Path == "/session" && r.Method == http.MethodGet {
				// Session listing is only reached when a create response was lost.
				// Answer empty so this test keeps exercising the prompt transport
				// failure it was written for.
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`[]`))}, nil
			}
			return nil, errors.New("dial " + internalAddress)
		})}},
		MaxConcurrent: 1,
		MaxWaiting:    1,
		Timeout:       time.Second,
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

	first := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hello"}`)
	if first.status != http.StatusConflict || strings.Contains(string(first.body), internalAddress) || strings.Contains(string(first.body), "/private/workspace") {
		t.Fatalf("chat = %d: %s", first.status, first.body)
	}
	replayed := api(t, http.MethodGet, server.URL+"/v1/runs/one?workspace_id=work", "secret", "")
	if replayed.status != http.StatusOK || strings.Contains(string(replayed.body), internalAddress) || strings.Contains(string(replayed.body), "/private/workspace") {
		t.Fatalf("run = %d: %s", replayed.status, replayed.body)
	}
}

func TestPersistedFailureDoesNotExposeTransportAddress(t *testing.T) {
	const internalAddress = "http://127.0.0.1:9999/internal"
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "persisted-transport-error", MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Provider: model.Config{BaseURL: "http://provider.invalid", Model: "test", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial " + internalAddress)
		})}}},
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

	first := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hello"}`)
	if first.status != http.StatusServiceUnavailable || strings.Contains(string(first.body), internalAddress) {
		t.Fatalf("chat = %d: %s", first.status, first.body)
	}
	replayed := api(t, http.MethodGet, server.URL+"/v1/runs/one?workspace_id=work", "secret", "")
	// The replayed run must report the status the original request reported.
	// Reclassifying a stored failure as a generic gateway error would contradict
	// the failure category still carried in the response body.
	if replayed.status != http.StatusServiceUnavailable || strings.Contains(string(replayed.body), internalAddress) {
		t.Fatalf("run = %d: %s", replayed.status, replayed.body)
	}
}

func TestStorageFailureDoesNotExposeDiagnostic(t *testing.T) {
	const diagnostic = "/private/beesuh/rhiza.db table=sessions"
	recorder := httptest.NewRecorder()
	(handler{}).fail(recorder, errors.New("rhiza: "+diagnostic), beesuh.Result{})
	if body := recorder.Body.String(); recorder.Code != http.StatusBadGateway || strings.Contains(body, diagnostic) {
		t.Fatalf("storage failure = %d: %s", recorder.Code, body)
	}
}

func TestReplayedRuntimeFailureKeepsHTTPStatusAfterReopen(t *testing.T) {
	for _, tc := range []struct {
		name, category string
		status         int
		wait           bool
	}{
		{"rate limited", beesuh.FailureRateLimited, http.StatusTooManyRequests, false},
		{"unavailable", beesuh.FailureServiceUnavailable, http.StatusServiceUnavailable, false},
		{"deadline", beesuh.FailureDeadlineExceeded, http.StatusGatewayTimeout, true},
		{"upstream", beesuh.FailureUpstream, http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tc.wait {
					time.Sleep(500 * time.Millisecond)
					return
				}
				if tc.category == beesuh.FailureUpstream {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(upstream.Close)

			dataDir := t.TempDir()
			opts := beesuh.Options{DataDir: dataDir, BackendID: "replay-" + tc.category, MaxConcurrent: 1, MaxWaiting: 1, Timeout: 100 * time.Millisecond,
				Native: &beesuh.NativeOptions{Provider: model.Config{BaseURL: upstream.URL, Model: "test"}}}
			runtime, err := beesuh.Open(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			h, err := New(runtime, "secret")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(h)
			first := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hello"}`)
			server.Close()
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			if first.status != tc.status {
				t.Fatalf("first status = %d: %s", first.status, first.body)
			}

			runtime, err = beesuh.Open(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			h, err = New(runtime, "secret")
			if err != nil {
				t.Fatal(err)
			}
			server = httptest.NewServer(h)
			t.Cleanup(server.Close)
			replayed := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hello"}`)
			if replayed.status != first.status || calls.Load() != 1 {
				t.Fatalf("replay status = %d, calls = %d; first = %d", replayed.status, calls.Load(), first.status)
			}
			var result struct {
				FailureCategory string `json:"failure_category"`
				Error           string `json:"error"`
			}
			if err := json.Unmarshal(replayed.body, &result); err != nil || result.FailureCategory != tc.category || result.Error != beesuh.PublicFailureMessage(tc.category) {
				t.Fatalf("replay result = %s, error = %v", replayed.body, err)
			}
		})
	}
}

func TestHistoryResponseWriteDeadlineWithTCPClient(t *testing.T) {
	// The stalled reader below does not read at all, so the 8 MiB response
	// cannot drain while it sits in kernel buffers. The write deadline must
	// therefore fire, the handler must return, and the server must release the
	// connection. The client-observed EOF is deliberately not asserted: whether
	// a socket-level write failure reaches the peer as EOF or as RST depends on
	// kernel TCP behaviour that differs between a local run and a CI runner, and
	// that is not the property under test. Connection release is observed through
	// http.Server.ConnState instead.
	const writeBudget = 2 * time.Second
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handlerDone := make(chan struct{})
	closed := make(chan struct{})
	server := &http.Server{
		Handler: responseWriteDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			if r.URL.Path != "/v1/history" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode([]string{strings.Repeat("x", 8<<20)})
		}), writeBudget),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				select {
				case <-closed:
				default:
					close(closed)
				}
			}
		},
	}
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Close()

	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("GET /v1/history?workspace_id=work&session_key=user HTTP/1.1\r\nHost: beesuh\r\nConnection: close\r\nAuthorization: Bearer secret\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(2 * writeBudget):
		t.Fatal("large history response did not finish within the write budget")
	case <-handlerDone:
	}
	select {
	case <-closed:
	case <-time.After(2 * writeBudget):
		t.Fatal("stalled history connection was not released after the write deadline")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newRuntime(t *testing.T, baseURL string) *beesuh.Runtime {
	t.Helper()
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir:       t.TempDir(),
		BackendID:     "test",
		OpenCode:      opencode.Config{BaseURL: baseURL, Directory: "/work"},
		MaxConcurrent: 1,
		MaxWaiting:    1,
		Timeout:       time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

type chatResponse struct {
	status int
	body   []byte
}

func chat(t *testing.T, url, token, body string) chatResponse {
	t.Helper()
	return api(t, http.MethodPost, url+"/v1/chat", token, body)
}

func api(t *testing.T, method, url, token, body string) chatResponse {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return chatResponse{status: resp.StatusCode, body: b}
}
