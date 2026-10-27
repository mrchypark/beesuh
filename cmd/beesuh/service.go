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
	"reflect"
	"strings"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/auth"
)

// Only trusted issuer and provider routing policy belongs in this file.
type serviceConfig struct {
	Issuer           string            `json:"issuer"`
	IntrospectionURL string            `json:"introspection_url"`
	UserInfoURL      string            `json:"userinfo_url"`
	ClientID         string            `json:"client_id"`
	ClientSecretFile string            `json:"client_secret_file"`
	Audience         string            `json:"audience"`
	RequiredScopes   []string          `json:"required_scopes"`
	Goauthy          *serviceDiscovery `json:"goauthy"`
	client           *http.Client
	providerClient   *http.Client
}

func serviceUserID(issuer, subject string) string {
	b, _ := json.Marshal([]string{issuer, subject})
	sum := sha256.Sum256(b)
	return "oidc_" + hex.EncodeToString(sum[:])
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
	if decodeServiceJSON(f, 1<<20, &s) != nil || !secureEndpoint(s.Issuer) || !secureEndpoint(s.IntrospectionURL) || !secureEndpoint(s.UserInfoURL) || !validServiceText(s.ClientID) || s.ClientSecretFile == "" || !validServiceText(s.Audience) || !validScopes(s.RequiredScopes) {
		return nil, errors.New("invalid OAuth service configuration")
	}
	if s.Goauthy == nil || !s.Goauthy.valid() || !strings.HasPrefix(s.Issuer, "https://") {
		return nil, errors.New("invalid Goauthy discovery configuration")
	}
	s.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: noServiceRedirect}
	return &s, nil
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
	id := serviceUserID(s.Issuer, c.Subject)
	return beesuh.Principal{UserID: id, WorkspaceID: id}, nil
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
func (s *serviceConfig) configure(o *beesuh.Options) error {
	if o.Native == nil || o.Native.Conductor != nil || len(o.Native.Conductors) != 0 {
		return errors.New("service mode requires native providers without operator Conductor configuration")
	}
	if !reflect.ValueOf(o.Native.Provider).IsZero() || len(o.Native.Providers) == 0 {
		return errors.New("service mode requires named providers without a singleton provider")
	}
	for _, cfg := range o.Native.Providers {
		if cfg.APIKey != "" || cfg.APIKeyEnv != "" || cfg.CodexAuth != nil || cfg.CodexAccountID != "" || cfg.Command != "" || cfg.CodexHome != "" || cfg.HTTPClient != nil {
			return errors.New("service mode forbids shared provider credentials and login stores")
		}
	}
	if s.Goauthy == nil {
		return errors.New("service mode requires Goauthy discovery")
	}
	if err := s.Goauthy.configure(o); err != nil {
		return err
	}
	o.RequireIdentity = true
	o.Authorize = s.authorize
	o.ResolveCredential = s.resolveDiscovered
	return nil
}
