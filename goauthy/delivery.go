// Package goauthy adapts a GoAuthy credential-delivery grant to a fixed
// provider HTTP endpoint. The binding is trusted service configuration; it is
// never populated from a browser request or a job payload.
package goauthy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mrchypark/beesuh/auth"
)

const maxTimeout = 60 * time.Second

var headerName = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// APIKeyBinding is the service-owned snapshot reviewed with GoAuthy. Its
// fields are metadata, not credentials.
type APIKeyBinding struct {
	GrantID              string `json:"grant_id"`
	ProviderID           string `json:"provider_id"`
	ConnectionGeneration string `json:"connection_generation"`
	ConnectorDigest      string `json:"connector_digest"`
	Header               string `json:"header"`
	Prefix               string `json:"prefix"`
	Endpoint             string `json:"endpoint"`
	CredentialVersion    int64  `json:"credential_version"`
}

// NewAPIKeyClient returns a client that obtains one GoAuthy-delivered API key
// for every provider request. source must acquire a current human token for
// this consumer; it must never accept a browser value or another app's token.
func NewAPIKeyClient(issuer string, binding APIKeyBinding, source func(context.Context) (string, error), issuerClient, providerClient *http.Client) (*http.Client, error) {
	issuerURL, err := absoluteHTTPS(issuer)
	if err != nil {
		return nil, errors.New("goauthy: invalid issuer")
	}
	endpoint, err := absoluteHTTPS(binding.Endpoint)
	if err != nil || binding.GrantID == "" || binding.ProviderID == "" || binding.ConnectionGeneration == "" || binding.ConnectorDigest == "" || binding.CredentialVersion < 1 || source == nil || strings.ContainsAny(binding.Prefix, "\r\n\x00") {
		return nil, errors.New("goauthy: invalid API-key binding")
	}
	header, ok := canonicalHeader(binding.Header)
	if !ok {
		return nil, errors.New("goauthy: invalid API-key binding")
	}
	binding.Header = header
	basePath := strings.TrimRight(issuerURL.Path, "/")
	baseEscapedPath := strings.TrimRight(issuerURL.EscapedPath(), "/")
	issuerURL.Path = basePath + "/auth/v1/connection-grants/" + binding.GrantID + "/credential"
	issuerURL.RawPath = baseEscapedPath + "/auth/v1/connection-grants/" + url.PathEscape(binding.GrantID) + "/credential"

	provider := clone(providerClient)
	provider.Transport = &apiKeyTransport{
		binding: binding, endpoint: endpoint, issuer: issuerURL,
		source: source, issuerClient: clone(issuerClient), provider: provider.Transport,
	}
	return provider, nil
}

type apiKeyTransport struct {
	binding      APIKeyBinding
	endpoint     *url.URL
	issuer       *url.URL
	source       func(context.Context) (string, error)
	issuerClient *http.Client
	provider     http.RoundTripper
}

func (t *apiKeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	dispatched := false
	defer func() {
		if !dispatched && request != nil && request.Body != nil {
			request.Body.Close()
		}
	}()
	if err := t.validDestination(request); err != nil {
		return nil, err
	}
	key, expiresAt, err := t.credential(request.Context())
	if err != nil {
		return nil, err
	}
	if time.Now().UnixMilli() >= expiresAt {
		return nil, auth.ErrUnavailable
	}
	copyRequest := request.Clone(request.Context())
	copyRequest.Header = request.Header.Clone()
	if copyRequest.Header == nil {
		copyRequest.Header = make(http.Header)
	}
	// Never let caller credentials or a provider redirect inherit into this call.
	copyRequest.Header.Del("Authorization")
	copyRequest.Header.Del(t.binding.Header)
	copyRequest.Header.Set(t.binding.Header, t.binding.Prefix+key)
	dispatched = true
	response, err := transport(t.provider).RoundTrip(copyRequest)
	if err != nil {
		return nil, auth.Sanitize(err)
	}
	return response, nil
}

func (t *apiKeyTransport) validDestination(request *http.Request) error {
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.User != nil || (request.Host != "" && request.Host != t.endpoint.Host) || request.URL.Scheme != t.endpoint.Scheme || request.URL.Host != t.endpoint.Host || request.URL.EscapedPath() != t.endpoint.EscapedPath() || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		return errors.New("goauthy: provider request is not allowed")
	}
	return nil
}

func (t *apiKeyTransport) credential(ctx context.Context) (string, int64, error) {
	token, err := t.source(ctx)
	if err != nil {
		return "", 0, auth.Sanitize(err)
	}
	if token == "" || len(token) > 8192 || strings.ContainsAny(token, "\r\n\x00") {
		return "", 0, auth.ErrInvalidCredentials
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.issuer.String(), bytes.NewReader(nil))
	if err != nil {
		return "", 0, auth.ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// The cloned client has no Jar and no redirects; do not copy any caller headers.
	response, err := t.issuerClient.Do(req)
	if err != nil {
		return "", 0, auth.Sanitize(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", 0, auth.HTTPError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<10+1))
	if err != nil || len(body) > 16<<10 {
		return "", 0, auth.ErrUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return "", 0, auth.ErrUnavailable
	}
	var delivery apiKeyDelivery
	if !decodeDelivery(fields, &delivery) || !t.matches(delivery) || delivery.APIKey == "" || len(delivery.APIKey) > 2048 || strings.ContainsAny(delivery.APIKey, "\r\n\x00") || delivery.ConsentExpiresAt <= time.Now().UnixMilli() {
		return "", 0, auth.ErrUnavailable
	}
	return delivery.APIKey, delivery.ConsentExpiresAt, nil
}

type apiKeyDelivery struct {
	Kind                 string
	APIKey               string
	GrantID              string
	ProviderID           string
	ConnectionGeneration string
	CredentialVersion    int64
	ConnectorDigest      string
	Header               string
	Prefix               string
	ConsentExpiresAt     int64
}

func decodeDelivery(fields map[string]json.RawMessage, target *apiKeyDelivery) bool {
	// Decode each required member independently so a missing empty prefix is not
	// confused with a present empty prefix. json.Decoder preserves large int64s.
	need := []struct {
		name string
		out  any
	}{
		{"kind", &target.Kind}, {"api_key", &target.APIKey}, {"grant_id", &target.GrantID}, {"provider_id", &target.ProviderID},
		{"connection_generation", &target.ConnectionGeneration}, {"credential_version", &target.CredentialVersion}, {"connector_digest", &target.ConnectorDigest},
		{"header", &target.Header}, {"prefix", &target.Prefix}, {"consent_expires_at_unix_ms", &target.ConsentExpiresAt},
	}
	for _, required := range need {
		raw, ok := fields[required.name]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, required.out) != nil {
			return false
		}
	}
	return true
}

func (t *apiKeyTransport) matches(value apiKeyDelivery) bool {
	header, ok := canonicalHeader(value.Header)
	return ok && value.Kind == "api_key" && value.GrantID == t.binding.GrantID && value.ProviderID == t.binding.ProviderID && value.ConnectionGeneration == t.binding.ConnectionGeneration && value.CredentialVersion == t.binding.CredentialVersion && value.CredentialVersion > 0 && value.ConnectorDigest == t.binding.ConnectorDigest && header == t.binding.Header && value.Prefix == t.binding.Prefix
}

func absoluteHTTPS(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("invalid URL")
	}
	return u, nil
}

func canonicalHeader(value string) (string, bool) {
	canonical := http.CanonicalHeaderKey(value)
	if canonical == "" || canonical != value || !headerName.MatchString(value) || strings.ContainsAny(value, "\r\n\x00") {
		return "", false
	}
	switch strings.ToLower(value) {
	case "host", "content-length", "transfer-encoding", "connection", "keep-alive", "trailer", "upgrade", "te", "proxy-connection":
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(value), "x-beesuh-") {
		return "", false
	}
	return canonical, true
}

func clone(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	copyClient := *base
	copyClient.Jar = nil
	if copyClient.Timeout <= 0 || copyClient.Timeout > maxTimeout {
		copyClient.Timeout = maxTimeout
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copyClient
}

func transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		return http.DefaultTransport
	}
	return base
}
