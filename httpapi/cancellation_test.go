package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/model"
)

func TestCancellationRouteAuthenticatedIsolation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}]}`))
	}))
	defer backend.Close()
	defer close(release)
	runtime, err := beesuh.Open(t.Context(), beesuh.Options{DataDir: t.TempDir(), BackendID: "cancel-http", RequireIdentity: true, Authorize: func(context.Context, beesuh.Principal) error { return nil }, ResolveCredential: func(_ context.Context, c beesuh.Connection) (beesuh.Credential, error) {
		return beesuh.Credential{APIKey: "fixture-key", AccountID: c.UserID}, nil
	}, MaxConcurrent: 1, MaxWaiting: 1, Timeout: 10 * time.Second, Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"team": {BaseURL: backend.URL, Model: "fixture"}}, DefaultProvider: "team"}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.RegisterConnection(t.Context(), beesuh.Connection{ID: "alice-model", UserID: "alice", WorkspaceID: "work", Kind: "provider", TargetID: "team", CredentialRef: "fixture", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	h, err := NewAuthenticated(runtime, func(r *http.Request) (beesuh.Principal, error) {
		switch r.Header.Get("Authorization") {
		case "Bearer alice":
			return beesuh.Principal{UserID: "alice", WorkspaceID: "work"}, nil
		case "Bearer bob":
			return beesuh.Principal{UserID: "bob", WorkspaceID: "work"}, nil
		default:
			return beesuh.Principal{}, beesuh.ErrUnauthenticated
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	target := server.URL + "/v1/runs/one/cancel?workspace_id=work"
	cases := []struct {
		token, url, body string
		want             int
	}{
		{"", target, "", http.StatusUnauthorized},
		{"alice", target, "", http.StatusNotFound},
		{"alice", server.URL + "/v1/runs/one/cancel", "", http.StatusBadRequest},
		{"alice", target + "&workspace_id=work", "", http.StatusBadRequest},
		{"alice", target, "{}", http.StatusBadRequest},
		{"alice", server.URL + "/v1/runs/one/cancel?workspace_id=other", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		got := api(t, http.MethodPost, tc.url, tc.token, tc.body)
		if got.status != tc.want {
			t.Fatalf("cancel %q = %d %s, want %d", tc.url, got.status, got.body, tc.want)
		}
	}
	// Keep the response body owned by the goroutine; all assertions run here.
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat", strings.NewReader(`{"workspace_id":"work","session_key":"s","request_id":"one","connection_id":"alice-model","message":"hello"}`))
		req.Header.Set("Authorization", "Bearer alice")
		res, err := server.Client().Do(req)
		if err != nil {
			done <- 0
			return
		}
		defer res.Body.Close()
		done <- res.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider not called")
	}
	foreign := api(t, http.MethodPost, target, "bob", "")
	if foreign.status != http.StatusNotFound {
		t.Fatalf("foreign=%d %s", foreign.status, foreign.body)
	}
	accepted := api(t, http.MethodPost, target, "alice", "")
	if accepted.status != http.StatusAccepted || !strings.Contains(string(accepted.body), `"cancellation_requested":true`) {
		t.Fatalf("accepted=%d %s", accepted.status, accepted.body)
	}
	assertCancellationMetadata(t, accepted.body, "one", "running", true)
	select {
	case status := <-done:
		if status != http.StatusOK {
			t.Fatalf("cancelled chat=%d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chat did not stop")
	}
	for i := 0; i < 2; i++ {
		got := api(t, http.MethodPost, target, "alice", "")
		if got.status != http.StatusOK || !strings.Contains(string(got.body), `"state":"cancelled"`) {
			t.Fatalf("repeat=%d %s", got.status, got.body)
		}
		assertCancellationMetadata(t, got.body, "one", "cancelled", true)
	}
	polled := api(t, http.MethodGet, server.URL+"/v1/runs/one?workspace_id=work", "alice", "")
	if polled.status != http.StatusOK || !strings.Contains(string(polled.body), `"state":"cancelled"`) {
		t.Fatalf("poll=%d %s", polled.status, polled.body)
	}
}

func assertCancellationMetadata(t *testing.T, body []byte, requestID, state string, requested bool) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"request_id": requestID, "state": state, "cancellation_requested": requested}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cancel metadata=%s, want exactly %+v", body, want)
	}
}

func TestCancellationRouteDoesNotExposeTerminalResult(t *testing.T) {
	for _, state := range []string{"succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if state == "failed" {
					http.Error(w, "private provider detail", http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"private assistant output"},"finish_reason":"stop"}]}`))
			}))
			defer backend.Close()
			runtime, err := beesuh.Open(t.Context(), beesuh.Options{DataDir: t.TempDir(), BackendID: "cancel-metadata", MaxConcurrent: 1, Timeout: 5 * time.Second, Native: &beesuh.NativeOptions{Provider: model.Config{BaseURL: backend.URL, Model: "fixture"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			out, runErr := runtime.Run(t.Context(), beesuh.Request{WorkspaceID: "work", SessionKey: "session", RequestID: "terminal", Prompt: beesuh.Prompt{Text: "private user prompt"}})
			if out.State != state || (state == "succeeded" && runErr != nil) || (state == "failed" && runErr == nil) {
				t.Fatalf("fixture result=%+v %v", out, runErr)
			}
			if state == "succeeded" && len(out.Output) == 0 {
				t.Fatal("fixture has no output to protect")
			}
			if state == "failed" && out.Error == "" {
				t.Fatal("fixture has no failure to protect")
			}
			h, err := New(runtime, "secret")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(h)
			defer server.Close()
			for i := 0; i < 2; i++ {
				got := api(t, http.MethodPost, server.URL+"/v1/runs/terminal/cancel?workspace_id=work", "secret", "")
				if got.status != http.StatusOK {
					t.Fatalf("terminal cancel=%d %s", got.status, got.body)
				}
				assertCancellationMetadata(t, got.body, "terminal", state, false)
			}
			stored, err := runtime.GetRun(t.Context(), "work", "terminal")
			if err != nil || !reflect.DeepEqual(stored, out) {
				t.Fatalf("cancellation changed persisted result: before=%+v after=%+v err=%v", out, stored, err)
			}
		})
	}
}
