package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/oauth2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackingReadCloser struct {
	io.Reader
	closed atomic.Bool
}

func (r *trackingReadCloser) Close() error {
	r.closed.Store(true)
	return nil
}

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

func TestAuthenticatedClientsDiscardBaseCookieJar(t *testing.T) {
	target, err := url.Parse("https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(target, []*http.Cookie{{Name: "base", Value: "cookie"}})
	base := &http.Client{Jar: jar, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Cookie"); got != "" {
			t.Errorf("cookie = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Set-Cookie": {"derived=cookie"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}
	headerClient, err := HeaderClient(base, http.Header{"Authorization": {"Bearer header"}})
	if err != nil {
		t.Fatal(err)
	}
	oauthClient, err := OAuthClient(base, func(context.Context) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "oauth"}, nil
	}, func(context.Context, *oauth2.Token) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for name, client := range map[string]*http.Client{"header": headerClient, "oauth": oauthClient} {
		t.Run(name, func(t *testing.T) {
			if client.Jar != nil {
				t.Fatal("client inherited base cookie jar")
			}
			for range 2 {
				response, err := client.Get(target.String())
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
			}
		})
	}
	if got := jar.Cookies(target); len(got) != 1 || got[0].Name != "base" {
		t.Fatalf("base cookies = %#v", got)
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

func TestOAuthTransportClosesBodyOnPreDispatchFailure(t *testing.T) {
	var dispatched atomic.Int32
	client, err := OAuthClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("must not dispatch")
	})}, func(context.Context) (*oauth2.Token, error) {
		return nil, errors.New("source failed")
	}, func(context.Context, *oauth2.Token) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	directBody := &trackingReadCloser{Reader: strings.NewReader("body")}
	directRequest, err := http.NewRequest(http.MethodPost, "https://example.test", directBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Transport.RoundTrip(directRequest); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("direct RoundTrip error = %v", err)
	}
	if !directBody.closed.Load() {
		t.Fatal("direct RoundTrip did not close body")
	}
	clientBody := &trackingReadCloser{Reader: strings.NewReader("body")}
	clientRequest, err := http.NewRequest(http.MethodPost, "https://example.test", clientBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(clientRequest); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("client Do error = %v", err)
	}
	if !clientBody.closed.Load() {
		t.Fatal("client Do did not close body")
	}
	if got := dispatched.Load(); got != 0 {
		t.Fatalf("dispatches = %d", got)
	}
}

func TestOAuthClientRejectsTokenExpiredDuringPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dispatched, persisted atomic.Int32
		expiry := time.Now().Add(12 * time.Second)
		client, err := OAuthClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			dispatched.Add(1)
			return nil, errors.New("must not dispatch")
		})}, func(context.Context) (*oauth2.Token, error) {
			return &oauth2.Token{AccessToken: "token", Expiry: expiry}, nil
		}, func(context.Context, *oauth2.Token) error {
			persisted.Add(1)
			time.Sleep(3 * time.Second)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "https://example.test", nil)
		if _, err := client.Transport.RoundTrip(request); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("RoundTrip error = %v", err)
		}
		if got := persisted.Load(); got != 1 {
			t.Fatalf("persist calls = %d", got)
		}
		if got := dispatched.Load(); got != 0 {
			t.Fatalf("dispatches = %d", got)
		}
	})
}

func TestOAuthClientRetryPersistenceAtExpiryPersistsRotatedRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dispatched, persistCalls, sourceCalls atomic.Int32
		var stored *oauth2.Token
		expiry := time.Now().Add(10 * time.Second)
		client, err := OAuthClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			dispatched.Add(1)
			return nil, errors.New("must not dispatch")
		})}, func(context.Context) (*oauth2.Token, error) {
			switch sourceCalls.Add(1) {
			case 1:
				return &oauth2.Token{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", Expiry: expiry}, nil
			case 2:
				return &oauth2.Token{AccessToken: "expired-refresh", Expiry: time.Now()}, nil
			default:
				return nil, errors.New("source called too many times")
			}
		}, func(_ context.Context, token *oauth2.Token) error {
			copyToken := *token
			if persistCalls.Add(1) == 1 {
				return errors.New("temporary storage failure")
			}
			stored = &copyToken
			// oauth2 accepts the equality boundary, then rejects immediately after it.
			time.Sleep(time.Nanosecond)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		first := &trackingReadCloser{Reader: strings.NewReader("first")}
		firstRequest := httptest.NewRequest(http.MethodPost, "https://example.test", first)
		if _, err := client.Transport.RoundTrip(firstRequest); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("first RoundTrip error = %v", err)
		}
		if !first.closed.Load() {
			t.Fatal("first request body was not closed")
		}

		retry := &trackingReadCloser{Reader: strings.NewReader("retry")}
		retryRequest := httptest.NewRequest(http.MethodPost, "https://example.test", retry)
		if _, err := client.Transport.RoundTrip(retryRequest); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("retry RoundTrip error = %v", err)
		}
		if !retry.closed.Load() {
			t.Fatal("retry request body was not closed")
		}
		if got := dispatched.Load(); got != 0 {
			t.Fatalf("dispatches = %d", got)
		}
		if got := sourceCalls.Load(); got != 2 {
			t.Fatalf("source calls = %d", got)
		}
		if got := persistCalls.Load(); got != 2 {
			t.Fatalf("persist calls = %d", got)
		}
		if stored == nil || stored.AccessToken != "rotated-access" || stored.RefreshToken != "rotated-refresh" || !stored.Expiry.Equal(expiry) {
			t.Fatalf("persisted retry token = %#v", stored)
		}
		transport := client.Transport.(*oauthTransport)
		if transport.pending != nil || transport.last == nil || transport.last.RefreshToken != "rotated-refresh" {
			t.Fatalf("retry state: pending=%#v last=%#v", transport.pending, transport.last)
		}

		// OAuthClient has no credential store of its own. The source and persist
		// callbacks form that boundary, so a new client models close/reopen.
		expiredReopened, err := OAuthClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("reloaded expired access token was dispatched")
			return nil, errors.New("must not dispatch")
		})}, func(context.Context) (*oauth2.Token, error) { copyToken := *stored; return &copyToken, nil },
			func(context.Context, *oauth2.Token) error {
				t.Error("expired reloaded token was persisted again")
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := expiredReopened.Get("https://example.test"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("expired reopened token error = %v", err)
		}
		if !stored.Expiry.Equal(expiry) {
			t.Fatal("reload renewed the expiry")
		}
		var reopenedDispatched atomic.Int32
		reopened, err := OAuthClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			reopenedDispatched.Add(1)
			if got := request.Header.Get("Authorization"); got != "Bearer refreshed-access" {
				t.Errorf("authorization = %q", got)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		})}, func(context.Context) (*oauth2.Token, error) {
			if stored == nil || stored.RefreshToken != "rotated-refresh" {
				return nil, errors.New("rotated refresh was not stored")
			}
			return &oauth2.Token{AccessToken: "refreshed-access", RefreshToken: stored.RefreshToken, Expiry: time.Now().Add(time.Hour)}, nil
		}, func(_ context.Context, token *oauth2.Token) error {
			copyToken := *token
			stored = &copyToken
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		response, err := reopened.Get("https://example.test")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if got := reopenedDispatched.Load(); got != 1 {
			t.Fatalf("reopened dispatches = %d", got)
		}
	})
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
