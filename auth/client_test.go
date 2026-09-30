package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestHeaderClientInjectsWithoutMutatingAndDoesNotFollowRedirects(t *testing.T) {
	var target atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			target.Store(true)
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
	}))
	defer server.Close()

	client, err := HeaderClient(&http.Client{}, http.Header{"Authorization": {"Bearer secret"}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "caller")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := req.Header.Get("Authorization"); got != "caller" {
		t.Fatalf("caller request mutated: %q", got)
	}
	response, err = client.Get(server.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound || target.Load() {
		t.Fatalf("redirect status=%d followed=%t", response.StatusCode, target.Load())
	}
}

func TestHeaderClientRejectsScopedAndInjectedHeaders(t *testing.T) {
	for _, headers := range []http.Header{
		{"Host": {"example.com"}},
		{"X-Beesuh-Actor": {"x"}},
		{"Authorization": {"Bearer bad\r\nX: y"}},
	} {
		if _, err := HeaderClient(nil, headers); err == nil {
			t.Fatal("HeaderClient accepted forbidden header")
		}
	}
}

func TestClientsInitializeNilHeadersAndBoundTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "key" {
			t.Errorf("X-API-Key = %q", got)
		}
	}))
	defer server.Close()
	client, err := HeaderClient(&http.Client{Timeout: -time.Second}, http.Header{"X-API-Key": {"key"}})
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != defaultTimeout {
		t.Fatalf("timeout = %v", client.Timeout)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = nil
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if request.Header != nil {
		t.Fatal("caller nil header was changed")
	}
}

func TestOAuthClientPersistsChangedTokenBeforeRequest(t *testing.T) {
	var persisted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if persisted.Load() != 1 {
			t.Error("request happened before persistence")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer one" {
			t.Errorf("authorization = %q", got)
		}
	}))
	defer server.Close()
	client, err := OAuthClient(nil, func(context.Context) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "one", TokenType: "Bearer"}, nil
	}, func(context.Context, *oauth2.Token) error { persisted.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if got := persisted.Load(); got != 1 {
		t.Fatalf("persist calls = %d", got)
	}
}

func TestOAuthClientConcurrentRequestsPersistOneUnchangedToken(t *testing.T) {
	var persisted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	client, err := OAuthClient(nil, func(context.Context) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "one"}, nil
	}, func(context.Context, *oauth2.Token) error { persisted.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := client.Get(server.URL)
			if err == nil {
				response.Body.Close()
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if got := persisted.Load(); got != 1 {
		t.Fatalf("persist calls = %d", got)
	}
}

func TestOAuthClientStorageFailureBlocksAndRetries(t *testing.T) {
	var requests, persistCalls, sourceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client, err := OAuthClient(nil, func(context.Context) (*oauth2.Token, error) {
		if sourceCalls.Add(1) != 1 {
			return nil, errors.New("source called again with stale refresh token")
		}
		return &oauth2.Token{AccessToken: "one"}, nil
	}, func(context.Context, *oauth2.Token) error {
		if persistCalls.Add(1) == 1 {
			return errors.New("storage token=one")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(server.URL); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "one") {
		t.Fatalf("first error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("request was sent despite storage failure")
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if requests.Load() != 1 || persistCalls.Load() != 2 || sourceCalls.Load() != 1 {
		t.Fatalf("requests=%d persists=%d source=%d", requests.Load(), persistCalls.Load(), sourceCalls.Load())
	}
}

func TestOAuthClientRejectsInjectedTokenBeforePersistence(t *testing.T) {
	var persisted atomic.Int32
	client, err := OAuthClient(nil, func(context.Context) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "one\r\ntwo"}, nil
	}, func(context.Context, *oauth2.Token) error { persisted.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
	if _, err := client.Transport.RoundTrip(request); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v", err)
	}
	if persisted.Load() != 0 {
		t.Fatal("invalid token was persisted")
	}
}

func TestOAuthClientSerializesPersistenceAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	client, err := OAuthClient(nil, func(context.Context) (*oauth2.Token, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return &oauth2.Token{AccessToken: "one"}, nil
	}, func(context.Context, *oauth2.Token) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	firstDone := make(chan error, 1)
	go func() {
		response, err := client.Get(server.URL)
		if response != nil {
			response.Body.Close()
		}
		firstDone <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	_, err = client.Do(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting error = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeAndHTTPError(t *testing.T) {
	for _, test := range []struct {
		err  error
		want error
	}{
		{HTTPError{StatusCode: 401}, ErrInvalidCredentials},
		{HTTPError{StatusCode: 403}, ErrForbidden},
		{HTTPError{StatusCode: 429}, ErrRateLimited},
		{HTTPError{StatusCode: 503}, ErrUnavailable},
		{errors.New("provider said token=secret"), ErrUnavailable},
	} {
		if got := Sanitize(test.err); !errors.Is(got, test.want) || strings.Contains(got.Error(), "secret") {
			t.Fatalf("Sanitize(%v) = %v", test.err, got)
		}
	}
}
