package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/model"
)

func TestStoredRunFailureKeepsCategoryOnPoll(t *testing.T) {
	for _, tc := range []struct {
		name, category string
		status         int
	}{
		{name: "rate limited", category: beesuh.FailureRateLimited, status: http.StatusTooManyRequests},
		{name: "service unavailable", category: beesuh.FailureServiceUnavailable, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(upstream.Close)

			runtime, err := beesuh.Open(context.Background(), beesuh.Options{
				DataDir: t.TempDir(), BackendID: tc.name, MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
				Native: &beesuh.NativeOptions{Provider: model.Config{BaseURL: upstream.URL, Model: "test"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			h, err := New(runtime, "secret")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(h)
			t.Cleanup(server.Close)

			first := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hello"}`)
			if first.status != tc.status {
				t.Fatalf("first status = %d: %s", first.status, first.body)
			}
			polled := api(t, http.MethodGet, server.URL+"/v1/runs/one?workspace_id=work", "secret", "")
			if polled.status != tc.status {
				t.Fatalf("poll status = %d, want %d: %s", polled.status, tc.status, polled.body)
			}
			var result struct {
				FailureCategory string `json:"failure_category"`
			}
			if err := json.Unmarshal(polled.body, &result); err != nil {
				t.Fatal(err)
			}
			if result.FailureCategory != tc.category {
				t.Fatalf("poll failure category = %q, want %q: %s", result.FailureCategory, tc.category, polled.body)
			}
		})
	}
}

func TestHTTPStorageCapacityPreservesReplayAndBlocksDispatch(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "capacity-http", MaxConcurrent: 1,
		Capacity: beesuh.CapacityLimits{RunsPerWorkspace: 1},
		Native:   &beesuh.NativeOptions{Provider: model.Config{BaseURL: upstream.URL, Model: "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	h, err := New(runtime, "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	firstBody := `{"workspace_id":"work","session_key":"user","request_id":"one","message":"hello"}`
	first := chat(t, server.URL, "secret", firstBody)
	if first.status != http.StatusServiceUnavailable {
		t.Fatalf("first=%d %s", first.status, first.body)
	}
	denied := chat(t, server.URL, "secret", `{"workspace_id":"work","session_key":"user","request_id":"two","message":"hello"}`)
	var out struct {
		Code string `json:"error_code"`
	}
	if err := json.Unmarshal(denied.body, &out); err != nil {
		t.Fatal(err)
	}
	if denied.status != http.StatusServiceUnavailable || out.Code != beesuh.FailureStorageCapacity {
		t.Fatalf("capacity=%d %s", denied.status, denied.body)
	}
	replay := chat(t, server.URL, "secret", firstBody)
	if replay.status != first.status || calls != 1 {
		t.Fatalf("replay=%d calls=%d", replay.status, calls)
	}
}
