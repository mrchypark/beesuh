package conductor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh/auth"
)

func TestHTTPAuthenticationErrorsAreTypedAndSanitized(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, auth.ErrInvalidCredentials},
		{http.StatusForbidden, auth.ErrForbidden},
		{http.StatusTooManyRequests, auth.ErrRateLimited},
		{http.StatusServiceUnavailable, auth.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("provider-secret-must-not-escape"))
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, AllowedEvents: []string{"ok"}, Bindings: map[string]Binding{"ok": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Emit(context.Background(), "workspace", "ok", []byte(`{}`), "key")
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.want)
			}
			if strings.Contains(err.Error(), "provider-secret") {
				t.Fatalf("provider response leaked: %v", err)
			}
		})
	}
}

func TestTransportAuthenticationErrorsArePreserved(t *testing.T) {
	client, err := New(Config{
		BaseURL:       "https://conductor.invalid",
		AllowedEvents: []string{"ok"}, Bindings: map[string]Binding{"ok": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}},
		HTTPClient: &http.Client{Transport: conductorRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, auth.ErrRateLimited
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Emit(context.Background(), "workspace", "ok", []byte(`{}`), "key")
	if !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("errors.Is(%v, rate limited) = false", err)
	}
	if strings.Contains(err.Error(), "conductor.invalid") {
		t.Fatalf("transport diagnostic leaked: %v", err)
	}
}

type conductorRoundTripFunc func(*http.Request) (*http.Response, error)

func (f conductorRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
