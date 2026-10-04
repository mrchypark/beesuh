package goauthy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mrchypark/beesuh/auth"
)

// OAuthBinding is the trusted, service-owned snapshot for one GoAuthy OAuth
// connection. AccountID is a required expected identity and is compared exactly
// when delivered.
type OAuthBinding struct {
	GrantID              string   `json:"grant_id"`
	ProviderID           string   `json:"provider_id"`
	ConnectionGeneration string   `json:"connection_generation"`
	AccountID            string   `json:"account_id"`
	Resource             string   `json:"resource"`
	ConsumerClientID     string   `json:"consumer_client_id"`
	Endpoint             string   `json:"endpoint"`
	Scopes               []string `json:"scopes"`
	CredentialVersion    int64    `json:"credential_version"`
}

// OAuthTokenRequest tells the trusted host which execution consumer needs a
// current human token. It is not proof of that token's owner or authority.
type OAuthTokenRequest struct {
	Resource         string
	ConsumerClientID string
}

// NewOAuthClient creates a fixed-destination HTTP client. It retrieves an
// OAuth access token for every request and never persists or refreshes it.
func NewOAuthClient(issuer string, binding OAuthBinding, source func(context.Context, OAuthTokenRequest) (string, error), issuerClient, providerClient *http.Client, now func() time.Time) (*http.Client, error) {
	issuerURL, err := absoluteHTTPS(issuer)
	if err != nil {
		return nil, errors.New("goauthy: invalid issuer")
	}
	endpoint, err := absoluteHTTPS(binding.Endpoint)
	if err != nil || source == nil || !validOAuthBinding(binding) {
		return nil, errors.New("goauthy: invalid OAuth binding")
	}
	if now == nil {
		now = time.Now
	}
	// The caller may retain its configuration slice; do not let a later edit
	// widen this client's trusted scope allowlist.
	binding.Scopes = append([]string(nil), binding.Scopes...)
	basePath := strings.TrimRight(issuerURL.Path, "/")
	baseEscapedPath := strings.TrimRight(issuerURL.EscapedPath(), "/")
	issuerURL.Path = basePath + "/auth/v1/connection-grants/" + binding.GrantID + "/credential"
	issuerURL.RawPath = baseEscapedPath + "/auth/v1/connection-grants/" + url.PathEscape(binding.GrantID) + "/credential"

	provider := clone(providerClient)
	provider.Transport = &oauthTransport{
		binding: binding, endpoint: endpoint, issuer: issuerURL, source: source,
		issuerClient: clone(issuerClient), provider: provider.Transport, now: now,
		gate: make(chan struct{}, 1), lastVersion: binding.CredentialVersion,
	}
	return provider, nil
}

type oauthTransport struct {
	binding      OAuthBinding
	endpoint     *url.URL
	issuer       *url.URL
	source       func(context.Context, OAuthTokenRequest) (string, error)
	issuerClient *http.Client
	provider     http.RoundTripper
	now          func() time.Time
	gate         chan struct{}
	lastVersion  int64
}

func (t *oauthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	dispatched := false
	defer func() {
		if !dispatched && request != nil && request.Body != nil {
			request.Body.Close()
		}
	}()
	if err := t.validDestination(request); err != nil {
		return nil, err
	}
	select {
	case t.gate <- struct{}{}:
		defer func() { <-t.gate }()
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	accessToken, expiresAt, version, err := t.credential(request.Context())
	if err != nil {
		return nil, err
	}
	if version < t.lastVersion || !expiresAt.After(t.now()) {
		return nil, auth.ErrUnavailable
	}
	t.lastVersion = version
	copyRequest := request.Clone(request.Context())
	copyRequest.Header = request.Header.Clone()
	if copyRequest.Header == nil {
		copyRequest.Header = make(http.Header)
	}
	copyRequest.Header.Del("Authorization")
	copyRequest.Header.Set("Authorization", "Bearer "+accessToken)
	dispatched = true
	response, err := transport(t.provider).RoundTrip(copyRequest)
	if err != nil {
		return nil, auth.Sanitize(err)
	}
	return response, nil
}

func (t *oauthTransport) validDestination(request *http.Request) error {
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.User != nil || (request.Host != "" && request.Host != t.endpoint.Host) || request.URL.Scheme != t.endpoint.Scheme || request.URL.Host != t.endpoint.Host || request.URL.EscapedPath() != t.endpoint.EscapedPath() || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		return errors.New("goauthy: provider request is not allowed")
	}
	return nil
}

func (t *oauthTransport) credential(ctx context.Context) (string, time.Time, int64, error) {
	humanToken, err := t.source(ctx, OAuthTokenRequest{Resource: t.binding.Resource, ConsumerClientID: t.binding.ConsumerClientID})
	if err != nil {
		return "", time.Time{}, 0, auth.Sanitize(err)
	}
	if humanToken == "" || len(humanToken) > 8192 || strings.ContainsAny(humanToken, "\r\n\x00") {
		return "", time.Time{}, 0, auth.ErrInvalidCredentials
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.issuer.String(), bytes.NewReader(nil))
	if err != nil {
		return "", time.Time{}, 0, auth.ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+humanToken)
	response, err := t.issuerClient.Do(req)
	if err != nil {
		return "", time.Time{}, 0, auth.Sanitize(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", time.Time{}, 0, auth.HTTPError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<10+1))
	if err != nil || len(body) > 16<<10 {
		return "", time.Time{}, 0, auth.ErrUnavailable
	}
	delivery, ok := decodeOAuthDelivery(body)
	if !ok || !t.matches(delivery) || delivery.AccessToken == "" || len(delivery.AccessToken) > 8192 || strings.ContainsAny(delivery.AccessToken, "\r\n\x00") {
		return "", time.Time{}, 0, auth.ErrUnavailable
	}
	tokenExpiry := time.UnixMilli(delivery.TokenExpiresAt)
	consentExpiry := time.UnixMilli(delivery.ConsentExpiresAt)
	if !tokenExpiry.After(t.now()) || !consentExpiry.After(t.now()) {
		return "", time.Time{}, 0, auth.ErrUnavailable
	}
	if consentExpiry.Before(tokenExpiry) {
		tokenExpiry = consentExpiry
	}
	return delivery.AccessToken, tokenExpiry, delivery.CredentialVersion, nil
}

// oauthDelivery is deliberately private because it includes an access token.
type oauthDelivery struct {
	Kind                 string   `json:"-"`
	AccessToken          string   `json:"-"`
	TokenType            string   `json:"-"`
	GrantID              string   `json:"-"`
	ProviderID           string   `json:"-"`
	ConnectionGeneration string   `json:"-"`
	CredentialVersion    int64    `json:"-"`
	AccountID            string   `json:"-"`
	Scopes               []string `json:"-"`
	TokenExpiresAt       int64    `json:"-"`
	ConsentExpiresAt     int64    `json:"-"`
}

func (oauthDelivery) String() string   { return "[redacted OAuth credential delivery]" }
func (oauthDelivery) GoString() string { return "[redacted OAuth credential delivery]" }

func decodeOAuthDelivery(body []byte) (oauthDelivery, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return oauthDelivery{}, false
	}
	fields := make(map[string]json.RawMessage, 11)
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return oauthDelivery{}, false
		}
		key, ok := name.(string)
		if !ok {
			return oauthDelivery{}, false
		}
		if _, exists := fields[key]; exists {
			return oauthDelivery{}, false
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return oauthDelivery{}, false
		}
		fields[key] = raw
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || decoder.More() {
		return oauthDelivery{}, false
	}
	var value oauthDelivery
	required := []struct {
		name string
		out  any
	}{
		{"kind", &value.Kind}, {"access_token", &value.AccessToken}, {"token_type", &value.TokenType}, {"grant_id", &value.GrantID},
		{"provider_id", &value.ProviderID}, {"connection_generation", &value.ConnectionGeneration}, {"credential_version", &value.CredentialVersion},
		{"account_id", &value.AccountID}, {"scopes", &value.Scopes}, {"token_expires_at_unix_ms", &value.TokenExpiresAt}, {"consent_expires_at_unix_ms", &value.ConsentExpiresAt},
	}
	if len(fields) != len(required) {
		return oauthDelivery{}, false
	}
	for _, field := range required {
		raw, ok := fields[field.name]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, field.out) != nil {
			return oauthDelivery{}, false
		}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return oauthDelivery{}, false
	}
	return value, true
}

func (t *oauthTransport) matches(value oauthDelivery) bool {
	return value.Kind == "oauth2" && value.TokenType == "Bearer" && value.GrantID == t.binding.GrantID && value.ProviderID == t.binding.ProviderID && value.ConnectionGeneration == t.binding.ConnectionGeneration && value.AccountID == t.binding.AccountID && value.CredentialVersion >= t.binding.CredentialVersion && value.CredentialVersion > 0 && validScopes(value.Scopes) && scopeSubset(value.Scopes, t.binding.Scopes)
}

func validOAuthBinding(binding OAuthBinding) bool {
	if binding.GrantID == "" || binding.ProviderID == "" || binding.ConnectionGeneration == "" || binding.AccountID == "" || binding.Resource == "" || binding.ConsumerClientID == "" || binding.CredentialVersion < 1 {
		return false
	}
	for _, value := range []string{binding.GrantID, binding.ProviderID, binding.ConnectionGeneration, binding.AccountID, binding.Resource, binding.ConsumerClientID} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return false
		}
	}
	return validScopes(binding.Scopes)
}

func validScopes(scopes []string) bool {
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n\x00") {
			return false
		}
		if _, ok := seen[scope]; ok {
			return false
		}
		seen[scope] = struct{}{}
	}
	return true
}

func scopeSubset(got, allowed []string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, scope := range allowed {
		set[scope] = struct{}{}
	}
	for _, scope := range got {
		if _, ok := set[scope]; !ok {
			return false
		}
	}
	return true
}
