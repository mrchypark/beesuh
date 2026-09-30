package model

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
	protocols := []string{"chat-completions", "responses", "anthropic"}
	for _, protocol := range protocols {
		for _, tc := range cases {
			t.Run(protocol+"/"+http.StatusText(tc.status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte("provider-secret-must-not-escape"))
				}))
				defer server.Close()
				client, err := New(Config{BaseURL: server.URL, API: protocol, Model: "m"})
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
				if !errors.Is(err, tc.want) {
					t.Fatalf("errors.Is(%v, %v) = false", err, tc.want)
				}
				if strings.Contains(err.Error(), "provider-secret") {
					t.Fatalf("provider response leaked: %v", err)
				}
			})
		}
	}
}

func TestTransportAuthenticationErrorsArePreserved(t *testing.T) {
	client, err := New(Config{
		BaseURL: "https://provider.invalid",
		Model:   "m",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, auth.ErrForbidden
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("errors.Is(%v, forbidden) = false", err)
	}
	if strings.Contains(err.Error(), "provider.invalid") {
		t.Fatalf("transport diagnostic leaked: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
