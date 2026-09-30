package auth

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const defaultTimeout = 60 * time.Second

var headerName = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// HeaderClient injects static, connection-scoped credentials into a clone of
// base. It never changes the caller's request or client.
func HeaderClient(base *http.Client, headers http.Header) (*http.Client, error) {
	copyHeaders, err := checkedHeaders(headers)
	if err != nil {
		return nil, err
	}
	client := cloneClient(base)
	client.Transport = headerTransport{base: client.Transport, headers: copyHeaders}
	return client, nil
}

// OAuthClient obtains and persists a connection-scoped token before each
// request. The source owns token caching and refresh; serialization prevents
// refresh-token rotation races within this client.
func OAuthClient(base *http.Client, source func(context.Context) (*oauth2.Token, error), persist func(context.Context, *oauth2.Token) error) (*http.Client, error) {
	if source == nil || persist == nil {
		return nil, errors.New("auth: token source and persistence are required")
	}
	client := cloneClient(base)
	client.Transport = &oauthTransport{base: client.Transport, source: source, persist: persist, gate: make(chan struct{}, 1)}
	return client, nil
}

func cloneClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	clientCopy := *base
	client := &clientCopy
	if client.Timeout <= 0 {
		client.Timeout = defaultTimeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func checkedHeaders(headers http.Header) (http.Header, error) {
	result := make(http.Header, len(headers))
	for name, values := range headers {
		if !headerName.MatchString(name) || forbiddenHeader(name) {
			return nil, errors.New("auth: invalid credential header")
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				return nil, errors.New("auth: invalid credential header")
			}
			result.Add(name, value)
		}
	}
	return result, nil
}

func forbiddenHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "transfer-encoding", "connection", "keep-alive", "trailer", "upgrade", "te", "proxy-connection":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "x-beesuh-")
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copyRequest := request.Clone(request.Context())
	copyRequest.Header = request.Header.Clone()
	if copyRequest.Header == nil {
		copyRequest.Header = make(http.Header)
	}
	for name, values := range t.headers {
		copyRequest.Header.Del(name)
		for _, value := range values {
			copyRequest.Header.Add(name, value)
		}
	}
	return transport(t.base).RoundTrip(copyRequest)
}

type oauthTransport struct {
	base    http.RoundTripper
	source  func(context.Context) (*oauth2.Token, error)
	persist func(context.Context, *oauth2.Token) error
	gate    chan struct{}
	last    *oauth2.Token
	pending *oauth2.Token
}

func (t *oauthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copyToken, err := t.token(request.Context())
	if err != nil {
		return nil, err
	}

	copyRequest := request.Clone(request.Context())
	copyRequest.Header = request.Header.Clone()
	if copyRequest.Header == nil {
		copyRequest.Header = make(http.Header)
	}
	copyRequest.Header.Set("Authorization", copyToken.Type()+" "+copyToken.AccessToken)
	return transport(t.base).RoundTrip(copyRequest)
}

func (t *oauthTransport) token(ctx context.Context) (*oauth2.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case t.gate <- struct{}{}:
		defer func() { <-t.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if t.pending != nil {
		pending, err := t.persistPending(ctx)
		if err != nil {
			return nil, err
		}
		if pending.Valid() {
			return pending, nil
		}
	}

	token, err := t.source(ctx)
	if err != nil {
		return nil, Sanitize(err)
	}
	if token == nil || !token.Valid() || strings.ContainsAny(token.AccessToken, "\r\n\x00") || strings.ContainsAny(token.TokenType, "\r\n\x00") {
		return nil, ErrInvalidCredentials
	}
	copyToken := *token
	if sameToken(t.last, &copyToken) {
		return &copyToken, nil
	}
	t.pending = &copyToken
	return t.persistPending(ctx)
}

func (t *oauthTransport) persistPending(ctx context.Context) (*oauth2.Token, error) {
	persistToken := *t.pending
	if err := t.persist(ctx, &persistToken); err != nil {
		return nil, sanitizePersistence(err)
	}
	committed := *t.pending
	t.last = &committed
	t.pending = nil
	return &committed, nil
}

func sanitizePersistence(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrUnavailable
}

func sameToken(left, right *oauth2.Token) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.AccessToken == right.AccessToken && left.RefreshToken == right.RefreshToken && left.TokenType == right.TokenType && left.Expiry.Equal(right.Expiry)
}

func transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		return http.DefaultTransport
	}
	return base
}
