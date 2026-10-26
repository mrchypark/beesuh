package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/goauthy"
)

// Only trusted routing and membership metadata belongs in this file.
type serviceConfig struct {
	Issuer           string              `json:"issuer"`
	IntrospectionURL string              `json:"introspection_url"`
	UserInfoURL      string              `json:"userinfo_url"`
	TokenURL         string              `json:"token_url,omitempty"`
	ClientID         string              `json:"client_id"`
	ClientSecretFile string              `json:"client_secret_file"`
	Audience         string              `json:"audience"`
	RequiredScopes   []string            `json:"required_scopes"`
	Connections      []serviceConnection `json:"connections"`
	Goauthy          *serviceDiscovery   `json:"goauthy,omitempty"`
	client           *http.Client
	providerClient   *http.Client
}
type serviceConnection struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	WorkspaceID string `json:"workspace_id"`
	Provider    string `json:"provider"`
	// Audience is an issuer-configured protected resource accepting OAuth Bearer tokens.
	Audience      string                `json:"audience"`
	Scopes        []string              `json:"scopes"`
	TokenExchange bool                  `json:"token_exchange,omitempty"`
	Resource      string                `json:"resource,omitempty"`
	GoauthyAPIKey *serviceGoauthyAPIKey `json:"goauthy_api_key,omitempty"`
}

// Goauthy delivery is an optional vendor protocol, not an OAuth token exchange.
type serviceGoauthyAPIKey struct {
	Resource string                `json:"resource"`
	Binding  goauthy.APIKeyBinding `json:"binding"`
}

func serviceUserID(issuer, subject string) string {
	b, _ := json.Marshal([]string{issuer, subject})
	sum := sha256.Sum256(b)
	return "oidc_" + hex.EncodeToString(sum[:])
}
func (c serviceConnection) connection(issuer string) beesuh.Connection {
	return beesuh.Connection{ID: c.ID, UserID: serviceUserID(issuer, c.Subject), WorkspaceID: c.WorkspaceID, Kind: "provider", TargetID: c.Provider, CredentialRef: c.ID, State: "ready"}
}
func secureEndpoint(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "https" || (u.Scheme == "http" && ip != nil && ip.IsLoopback())
}
func loadService(filename string) (*serviceConfig, error) {
	f, e := os.Open(filename)
	if e != nil {
		return nil, errors.New("cannot open service configuration")
	}
	defer f.Close()
	var s serviceConfig
	if decodeServiceJSON(f, 1<<20, &s) != nil || !secureEndpoint(s.Issuer) || !secureEndpoint(s.IntrospectionURL) || !secureEndpoint(s.UserInfoURL) || (s.TokenURL != "" && !secureEndpoint(s.TokenURL)) || !validServiceText(s.ClientID) || s.ClientSecretFile == "" || !validServiceText(s.Audience) || !validScopes(s.RequiredScopes) || (len(s.Connections) == 0 && s.Goauthy == nil) {
		return nil, errors.New("invalid OAuth service configuration")
	}
	if s.Goauthy != nil && (len(s.Connections) != 0 || !s.Goauthy.valid() || !strings.HasPrefix(s.Issuer, "https://")) {
		return nil, errors.New("invalid Goauthy discovery configuration")
	}
	seen := map[string]bool{}
	workspaces := map[string]string{}
	for _, c := range s.Connections {
		if seen[c.ID] || !validServiceText(c.ID) || !validServiceText(c.Subject) || !validServiceText(c.WorkspaceID) || !validServiceText(c.Provider) || !s.validCredentialMode(c) {
			return nil, errors.New("invalid OAuth service connection")
		}
		if workspace, ok := workspaces[c.Subject]; ok && workspace != c.WorkspaceID {
			return nil, errors.New("service subject must map to one workspace")
		}
		seen[c.ID] = true
		workspaces[c.Subject] = c.WorkspaceID
	}
	s.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: noServiceRedirect}
	return &s, nil
}

func (s *serviceConfig) validCredentialMode(c serviceConnection) bool {
	if c.GoauthyAPIKey != nil {
		g := c.GoauthyAPIKey
		if c.TokenExchange || c.Audience != "" || len(c.Scopes) != 0 || c.Resource != "" || !validServiceText(g.Resource) || !strings.HasPrefix(s.Issuer, "https://") {
			return false
		}
		_, err := goauthy.NewAPIKeyClient(s.Issuer, g.Binding, func(context.Context) (string, error) { return "", auth.ErrUnavailable }, nil, nil)
		return err == nil && g.Binding.Header == "Authorization" && g.Binding.Prefix == "Bearer "
	}
	return validServiceText(c.Audience) && validScopes(c.Scopes) && (!c.TokenExchange || s.TokenURL != "") && (c.TokenExchange || c.Resource == "") && (c.Resource == "" || secureEndpoint(c.Resource))
}
func noServiceRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
func decodeServiceJSON(r io.Reader, limit int64, out any) error {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil || int64(len(b)) > limit {
		return errors.New("invalid service JSON")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid service JSON")
	}
	return nil
}
func validServiceText(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.ContainsAny(s, "\r\n\x00")
}
func validScopes(scopes []string) bool {
	if len(scopes) == 0 {
		return false
	}
	for _, s := range scopes {
		if s == "" {
			return false
		}
		for _, c := range s {
			if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
				return false
			}
		}
	}
	return true
}
func hasScopes(scope string, required []string) bool {
	have := map[string]bool{}
	for _, s := range strings.Fields(scope) {
		have[s] = true
	}
	for _, s := range required {
		if !have[s] {
			return false
		}
	}
	return true
}
func validBearer(s string) bool {
	if s == "" || len(s) > 16384 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~+/=", c)) {
			return false
		}
	}
	return true
}

// OAuth responses are extensible. Configuration remains strict; protocol JSON
// accepts additional claims without accepting oversized or trailing documents.
func (s *serviceConfig) oauth(ctx context.Context, endpoint string, form url.Values, bearer string, out any) error {
	method := "POST"
	var body io.Reader
	if form == nil {
		method = "GET"
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return auth.ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		f, e := os.Open(s.ClientSecretFile)
		if e != nil {
			return auth.ErrUnavailable
		}
		secret, e := io.ReadAll(io.LimitReader(f, (16<<10)+1))
		_ = f.Close()
		v := strings.TrimSpace(string(secret))
		if e != nil || len(secret) > 16<<10 || v == "" || strings.ContainsAny(v, "\r\n\x00") {
			return auth.ErrUnavailable
		}
		// RFC 6749 section 2.3.1 encodes both components before HTTP Basic.
		req.SetBasicAuth(url.QueryEscape(s.ClientID), url.QueryEscape(v))
	} else {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, e := s.client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return auth.ErrUnavailable
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if e != nil || len(b) > 64<<10 {
		return auth.ErrUnavailable
	}
	if resp.StatusCode != 200 {
		if resp.StatusCode == 429 {
			return auth.ErrRateLimited
		}
		// Only standardized codes affect classification; descriptions never escape.
		var failure struct {
			Code string `json:"error"`
		}
		if json.Unmarshal(b, &failure) == nil && (resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403) {
			switch failure.Code {
			case "invalid_token", "invalid_grant":
				return beesuh.ErrUnauthenticated
			case "insufficient_scope", "invalid_scope", "invalid_target", "access_denied":
				return auth.ErrForbidden
			}
		}
		if form == nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return beesuh.ErrUnauthenticated
			case http.StatusForbidden, http.StatusNotFound:
				return auth.ErrForbidden
			}
		}
		return auth.ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return auth.ErrUnavailable
	}
	return nil
}

type serviceClaims struct {
	ClientID  string          `json:"client_id"`
	Active    bool            `json:"active"`
	Subject   string          `json:"sub"`
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	Expiry    int64           `json:"exp"`
	NotBefore int64           `json:"nbf"`
	Scope     string          `json:"scope"`
	TokenType string          `json:"token_type"`
}

func (c serviceClaims) hasAudience(want string) bool {
	var one string
	if json.Unmarshal(c.Audience, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(c.Audience, &many) != nil {
		return false
	}
	for _, a := range many {
		if a == want {
			return true
		}
	}
	return false
}
func (s *serviceConfig) inspect(ctx context.Context, token, audience string, scopes []string) (serviceClaims, error) {
	var c serviceClaims
	if !validBearer(token) {
		return c, beesuh.ErrUnauthenticated
	}
	if e := s.oauth(ctx, s.IntrospectionURL, url.Values{"token": {token}, "token_type_hint": {"access_token"}}, "", &c); e != nil {
		return c, e
	}
	now := time.Now().Unix()
	if !c.Active || !validServiceText(c.Subject) || (c.Issuer != "" && c.Issuer != s.Issuer) || !c.hasAudience(audience) || c.Expiry <= now || c.NotBefore > now || !hasScopes(c.Scope, scopes) || (c.TokenType != "" && !strings.EqualFold(c.TokenType, "Bearer")) {
		return c, beesuh.ErrUnauthenticated
	}
	return c, nil
}

type serviceAccessKey struct{}

// Request-scoped only: no token cache indexed by user, no persistence, and no
// restart/background execution without a fresh authenticated request.
func (s *serviceConfig) authenticate(r *http.Request) (beesuh.Principal, error) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !validBearer(parts[1]) {
		return beesuh.Principal{}, beesuh.ErrUnauthenticated
	}
	p, e := s.verify(r.Context(), parts[1])
	if e != nil {
		return beesuh.Principal{}, e
	}
	*r = *r.WithContext(context.WithValue(r.Context(), serviceAccessKey{}, parts[1]))
	return p, nil
}
func (s *serviceConfig) verify(ctx context.Context, token string) (beesuh.Principal, error) {
	c, e := s.inspect(ctx, token, s.Audience, s.RequiredScopes)
	if e != nil {
		return beesuh.Principal{}, e
	}
	// Introspection hints and Bearer usage do not prove access-token type.
	// UserInfo must validate an access token, never a refresh token.
	if s.UserInfoURL == "" {
		return beesuh.Principal{}, beesuh.ErrUnauthenticated
	}
	if s.UserInfoURL != "" {
		if !hasScopes(c.Scope, []string{"openid"}) {
			return beesuh.Principal{}, beesuh.ErrUnauthenticated
		}
		var info struct {
			Subject string `json:"sub"`
		}
		if e := s.oauth(ctx, s.UserInfoURL, nil, token, &info); e != nil {
			return beesuh.Principal{}, e
		}
		if info.Subject != c.Subject {
			return beesuh.Principal{}, beesuh.ErrUnauthenticated
		}
	}
	for _, b := range s.Connections {
		if b.Subject == c.Subject {
			return beesuh.Principal{UserID: serviceUserID(s.Issuer, c.Subject), WorkspaceID: b.WorkspaceID}, nil
		}
	}
	if s.Goauthy != nil {
		id := serviceUserID(s.Issuer, c.Subject)
		return beesuh.Principal{UserID: id, WorkspaceID: id}, nil
	}
	return beesuh.Principal{}, beesuh.ErrForbidden
}
func (s *serviceConfig) authorize(ctx context.Context, p beesuh.Principal) error {
	token, _ := ctx.Value(serviceAccessKey{}).(string)
	verified, e := s.verify(ctx, token)
	if e != nil {
		return e
	}
	if verified != p {
		return beesuh.ErrForbidden
	}
	return nil
}
func (s *serviceConfig) resolve(_ context.Context, c beesuh.Connection) (beesuh.Credential, error) {
	for _, b := range s.Connections {
		if b.ID == c.ID && serviceUserID(s.Issuer, b.Subject) == c.UserID && b.WorkspaceID == c.WorkspaceID && b.Provider == c.TargetID && c.CredentialRef == b.ID && c.Kind == "provider" {
			accountID := serviceUserID(s.Issuer, b.Subject)
			if b.GoauthyAPIKey != nil {
				metadata, _ := json.Marshal([]any{s.Issuer, b.Subject, b.GoauthyAPIKey})
				digest := sha256.Sum256(metadata)
				accountID = "goauthy_" + hex.EncodeToString(digest[:])
			}
			return beesuh.Credential{AccountID: accountID, HTTPClient: &http.Client{Transport: serviceTransport{s: s, binding: b}, Timeout: 60 * time.Second, CheckRedirect: noServiceRedirect}}, nil
		}
	}
	return beesuh.Credential{}, beesuh.ErrForbidden
}

type serviceTransport struct {
	s            *serviceConfig
	binding      serviceConnection
	endpoint     string
	apiKeyClient *http.Client
}

func (t serviceTransport) RoundTrip(r *http.Request) (response *http.Response, err error) {
	defer func() {
		if err != nil && r.Body != nil {
			_ = r.Body.Close()
		}
	}()
	if r.Method != "POST" || r.URL.String() != t.endpoint || (r.Host != "" && r.Host != r.URL.Host) {
		return nil, auth.ErrForbidden
	}
	p := beesuh.Principal{UserID: serviceUserID(t.s.Issuer, t.binding.Subject), WorkspaceID: t.binding.WorkspaceID}
	if e := t.s.authorize(r.Context(), p); e != nil {
		return nil, e
	}
	if t.binding.GoauthyAPIKey != nil {
		if t.apiKeyClient == nil {
			return nil, auth.ErrUnavailable
		}
		resp, e := t.apiKeyClient.Do(r)
		if e != nil {
			return nil, auth.Sanitize(e)
		}
		resp.Request = r
		return resp, nil
	}
	token, _ := r.Context().Value(serviceAccessKey{}).(string)
	if t.binding.TokenExchange {
		form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "subject_token": {token}, "subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"}, "requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"}, "audience": {t.binding.Audience}, "scope": {strings.Join(t.binding.Scopes, " ")}}
		if t.binding.Resource != "" {
			form.Del("audience")
			form.Set("resource", t.binding.Resource)
		}
		var out struct {
			AccessToken     string `json:"access_token"`
			TokenType       string `json:"token_type"`
			IssuedTokenType string `json:"issued_token_type"`
			ExpiresIn       *int64 `json:"expires_in"`
		}
		if e := t.s.oauth(r.Context(), t.s.TokenURL, form, "", &out); e != nil {
			return nil, e
		}
		if !validBearer(out.AccessToken) || !strings.EqualFold(out.TokenType, "Bearer") || out.IssuedTokenType != "urn:ietf:params:oauth:token-type:access_token" || (out.ExpiresIn != nil && *out.ExpiresIn <= 0) {
			return nil, auth.ErrForbidden
		}
		token = out.AccessToken
	}
	claims, e := t.s.inspect(r.Context(), token, t.binding.Audience, t.binding.Scopes)
	if e != nil {
		return nil, e
	}
	if claims.Subject != t.binding.Subject {
		return nil, auth.ErrForbidden
	}
	if e := t.s.authorize(r.Context(), p); e != nil {
		return nil, e
	}
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token)
	resp, e := http.DefaultTransport.RoundTrip(clone)
	if e != nil {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		return nil, auth.ErrUnavailable
	}
	resp.Request = r
	return resp, nil
}
func (s *serviceConfig) configure(o *beesuh.Options) error {
	if o.Native == nil || o.Native.Conductor != nil {
		return errors.New("service mode requires native providers without operator Conductor configuration")
	}
	for _, cfg := range o.Native.Providers {
		if cfg.APIKey != "" || cfg.APIKeyEnv != "" {
			return errors.New("service mode forbids shared provider credentials")
		}
	}
	if s.Goauthy != nil {
		if err := s.Goauthy.configure(o); err != nil {
			return err
		}
	}
	for _, c := range s.Connections {
		if !s.validCredentialMode(c) {
			return errors.New("invalid service credential mode")
		}
		cfg, ok := o.Native.Providers[c.Provider]
		if !ok {
			return errors.New("service connection provider is not configured")
		}
		if !secureEndpoint(cfg.BaseURL) || (cfg.Kind != "" && cfg.Kind != "openai-compatible" && cfg.Kind != "opencode-go" && cfg.Kind != "opencode-zen") || cfg.HTTPClient != nil || cfg.API == "anthropic" {
			return errors.New("OAuth service provider requires a Bearer-compatible HTTP endpoint")
		}
	}
	o.RequireIdentity = true
	o.Authorize = s.authorize
	o.ResolveCredential = func(ctx context.Context, c beesuh.Connection) (beesuh.Credential, error) {
		if s.Goauthy != nil {
			return s.resolveDiscovered(ctx, c)
		}
		credential, e := s.resolve(ctx, c)
		if e != nil {
			return credential, e
		}
		cfg := o.Native.Providers[c.TargetID]
		u, _ := url.Parse(cfg.BaseURL)
		operation := "chat/completions"
		if cfg.API == "responses" {
			operation = "responses"
		}
		u.Path = path.Join("/", u.Path, operation)
		u.RawPath = ""
		binding := findBinding(s, c.ID)
		transport := serviceTransport{s: s, binding: binding, endpoint: u.String()}
		if binding.GoauthyAPIKey != nil {
			g := binding.GoauthyAPIKey
			if g.Binding.Endpoint != u.String() {
				return beesuh.Credential{}, auth.ErrForbidden
			}
			source := func(ctx context.Context) (string, error) {
				p := beesuh.Principal{UserID: serviceUserID(s.Issuer, binding.Subject), WorkspaceID: binding.WorkspaceID}
				if e := s.authorize(ctx, p); e != nil {
					return "", e
				}
				token, _ := ctx.Value(serviceAccessKey{}).(string)
				claims, e := s.inspect(ctx, token, g.Resource, []string{"goauthy.connections.use"})
				if e != nil {
					return "", e
				}
				if claims.Subject != binding.Subject || claims.ClientID != s.ClientID {
					return "", auth.ErrForbidden
				}
				return token, nil
			}
			transport.apiKeyClient, e = goauthy.NewAPIKeyClient(s.Issuer, g.Binding, source, s.client, s.providerClient)
			if e != nil {
				return beesuh.Credential{}, auth.ErrUnavailable
			}
		}
		credential.HTTPClient.Transport = transport
		return credential, nil
	}
	return nil
}
func findBinding(s *serviceConfig, id string) serviceConnection {
	for _, c := range s.Connections {
		if c.ID == id {
			return c
		}
	}
	return serviceConnection{}
}
