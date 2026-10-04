package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/model"
)

func TestComposRequiresAuthorizedProducer(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}]}`))
	}))
	defer backend.Close()

	runtime, err := beesuh.Open(context.Background(), beesuh.Options{
		DataDir: t.TempDir(), BackendID: "compos-producer", RequireIdentity: true,
		Authorize: func(context.Context, beesuh.Principal) error { return nil },
		ResolveCredential: func(_ context.Context, c beesuh.Connection) (beesuh.Credential, error) {
			return beesuh.Credential{APIKey: "test-key", AccountID: c.UserID}, nil
		},
		MaxConcurrent: 1, MaxWaiting: 1, Timeout: time.Second,
		Native: &beesuh.NativeOptions{Providers: map[string]model.Config{"team": {BaseURL: backend.URL, Model: "test"}}, DefaultProvider: "team"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.RegisterConnection(context.Background(), beesuh.Connection{ID: "producer-model", UserID: "producer", WorkspaceID: "work", Kind: "provider", TargetID: "team", CredentialRef: "producer-account", State: "ready"}); err != nil {
		t.Fatal(err)
	}

	authenticate := func(r *http.Request) (beesuh.Principal, error) {
		switch r.Header.Get("Authorization") {
		case "Bearer producer":
			return beesuh.Principal{UserID: "producer", WorkspaceID: "work"}, nil
		case "Bearer consumer":
			return beesuh.Principal{UserID: "consumer", WorkspaceID: "work"}, nil
		default:
			return beesuh.Principal{}, beesuh.ErrUnauthenticated
		}
	}
	h, err := NewAuthenticatedWithComposProducer(runtime, authenticate, func(_ context.Context, p beesuh.Principal) error {
		if p.UserID != "producer" || p.WorkspaceID != "work" {
			return errors.New("producer permission denied")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()

	request := func(workspace, subject string) string {
		return `{"agent_definition":{"instructions":"Return JSON."},"task_definition":{"input_schema":{"type":"object"},"output_schema":{"type":"object","properties":{"answer":{"type":"string"}}}},"input":{},"subject_ref":` + subject + `,"_compos":{"workspace_id":"` + workspace + `","execution_request_id":"request-1"}}`
	}
	for _, test := range []struct {
		name, token, workspace, subject string
		want                            int
	}{
		{name: "no producer permission", token: "consumer", workspace: "work", subject: `{"producer":true,"workspace_id":"work"}`, want: http.StatusForbidden},
		{name: "workspace is not owned", token: "producer", workspace: "other", subject: `{"workspace_id":"work"}`, want: http.StatusForbidden},
		{name: "subject assertion does not grant authority", token: "producer", workspace: "work", subject: `{"user_id":"other","workspace_id":"other","producer":false}`, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := api(t, http.MethodPost, server.URL+"/v1/compos?session_key=compos&connection_id=producer-model", test.token, request(test.workspace, test.subject))
			if response.status != test.want {
				t.Fatalf("status = %d: %s", response.status, response.body)
			}
			if strings.Contains(string(response.body), "producer permission denied") {
				t.Fatalf("internal producer error leaked: %s", response.body)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d", calls.Load())
	}
}
