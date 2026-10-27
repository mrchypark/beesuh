package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/goauthy"
	"github.com/mrchypark/beesuh/httpapi"
)

// Routes are operator-owned provider policy, never user/account manifests.
type serviceDiscovery struct {
	Resource  string            `json:"resource"`
	Providers map[string]string `json:"providers"`
	endpoints map[string]string
}

func (d *serviceDiscovery) valid() bool {
	if !secureEndpoint(d.Resource) || len(d.Providers) == 0 {
		return false
	}
	for upstream, local := range d.Providers {
		if !validServiceText(upstream) || !validServiceText(local) {
			return false
		}
	}
	return true
}
func (d *serviceDiscovery) configure(o *beesuh.Options) error {
	if !d.valid() {
		return errors.New("invalid Goauthy discovery configuration")
	}
	for _, cfg := range o.Native.Providers {
		if !secureEndpoint(cfg.BaseURL) || !strings.HasPrefix(cfg.BaseURL, "https://") || cfg.HTTPClient != nil || (cfg.API != "" && cfg.API != "chat-completions" && cfg.API != "responses") || (cfg.Kind != "" && cfg.Kind != "openai-compatible" && cfg.Kind != "opencode-go" && cfg.Kind != "opencode-zen") {
			return errors.New("Goauthy discovery requires Bearer-compatible HTTP providers")
		}
	}
	d.endpoints = make(map[string]string)
	for upstream, local := range d.Providers {
		cfg, ok := o.Native.Providers[local]
		if !ok {
			return errors.New("Goauthy discovery requires Bearer-compatible HTTP providers")
		}
		operation := "chat/completions"
		if cfg.API == "responses" {
			operation = "responses"
		}
		u, _ := url.Parse(cfg.BaseURL)
		u.Path = path.Join("/", u.Path, operation)
		u.RawPath = ""
		d.endpoints[upstream] = u.String()
	}
	return nil
}

type discoveryRef struct {
	Collection string `json:"collection_id"`
	Connection string `json:"connection_id"`
	Grant      string `json:"grant_id"`
}

func (r discoveryRef) valid() bool {
	return validServiceText(r.Collection) && validServiceText(r.Connection) && validServiceText(r.Grant)
}
func (r discoveryRef) encoded() string {
	b, _ := json.Marshal(r)
	return "ga_" + base64.RawURLEncoding.EncodeToString(b)
}
func decodeDiscoveryRef(ref string) (discoveryRef, error) {
	var r discoveryRef
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ref, "ga_"))
	if !strings.HasPrefix(ref, "ga_") || e != nil || json.Unmarshal(b, &r) != nil || !r.valid() || r.encoded() != ref {
		return r, auth.ErrForbidden
	}
	return r, nil
}

type discoveryConnection struct {
	ID         string `json:"id"`
	Collection string `json:"collection_id"`
	Owner      string `json:"owner_subject"`
	State      string `json:"state"`
}
type discoveryGrant struct {
	Grant struct {
		ID                 string `json:"id"`
		Owner              string `json:"owner_subject"`
		Collection         string `json:"collection_id"`
		Connection         string `json:"connection_id"`
		Consumer           string `json:"consumer_client_id"`
		Mode               string `json:"mode"`
		Resource           string `json:"resource"`
		Generation         string `json:"generation"`
		ConsumerGeneration string `json:"consumer_generation"`
		Provider           string `json:"provider_id"`
		Digest             string `json:"connector_digest"`
		Revision           int64  `json:"revision"`
		ProviderRevision   int64  `json:"provider_revision"`
		Expires            int64  `json:"expires_at_unix_ms"`
		Revoked            bool   `json:"revoked"`
	} `json:"grant"`
	Generation string `json:"connection_generation"`
}

func (s *serviceConfig) discoveryClaims(ctx context.Context, use bool) (serviceClaims, string, error) {
	token, _ := ctx.Value(serviceAccessKey{}).(string)
	scopes := []string{"goauthy.connections.read"}
	if use {
		scopes = append(scopes, "goauthy.connections.use")
	}
	claims, e := s.inspect(ctx, token, s.Goauthy.Resource, scopes)
	if e != nil {
		return claims, "", e
	}
	if use && claims.ClientID != s.ClientID {
		return claims, "", auth.ErrForbidden
	}
	return claims, token, nil
}
func (s *serviceConfig) metadata(ctx context.Context, token string, out any, segments ...string) error {
	endpoint := strings.TrimRight(s.Issuer, "/") + "/auth/v1"
	for _, v := range segments {
		if v == "." || v == ".." || strings.ContainsAny(v, "/\\") {
			return auth.ErrForbidden
		}
		endpoint += "/" + url.PathEscape(v)
	}
	return s.oauth(ctx, endpoint, nil, token, out)
}
func (s *serviceConfig) checkedGrant(ctx context.Context, ref discoveryRef) (discoveryGrant, serviceClaims, error) {
	var status discoveryGrant
	claims, token, e := s.discoveryClaims(ctx, true)
	if e != nil {
		return status, claims, e
	}
	var conn discoveryConnection
	if e = s.metadata(ctx, token, &conn, "connections", ref.Collection, ref.Connection); e != nil {
		return status, claims, e
	}
	if conn.ID != ref.Connection || conn.Collection != ref.Collection || conn.Owner != claims.Subject {
		return status, claims, auth.ErrForbidden
	}
	var raw json.RawMessage
	if e = s.metadata(ctx, token, &raw, "connections", ref.Collection, ref.Connection, "grants", ref.Grant); e != nil {
		return status, claims, e
	}
	// Consent cannot be inferred from Go zero values: null/missing revoked is
	// unknown authority, and an omitted digest must not select OAuth delivery.
	var envelope struct {
		Grant map[string]json.RawMessage `json:"grant"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return status, claims, auth.ErrForbidden
	}
	for name, target := range map[string]any{"revoked": new(bool), "connector_digest": new(string)} {
		value, ok := envelope.Grant[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, target) != nil {
			return status, claims, auth.ErrForbidden
		}
	}
	if json.Unmarshal(raw, &status) != nil {
		return status, claims, auth.ErrForbidden
	}
	g := status.Grant
	if g.ID != ref.Grant || g.Owner != claims.Subject || g.Collection != ref.Collection || g.Connection != ref.Connection || g.Consumer != s.ClientID || g.Mode != "credential_delivery" || g.Resource != s.Goauthy.Resource || g.Revoked || g.Expires <= time.Now().UnixMilli() || g.Generation == "" || g.Generation != status.Generation || g.ConsumerGeneration == "" || g.Revision < 1 || g.ProviderRevision < 1 || s.Goauthy.Providers[g.Provider] == "" {
		return status, claims, auth.ErrForbidden
	}
	return status, claims, nil
}
func (s *serviceConfig) resolveDiscovered(ctx context.Context, c beesuh.Connection) (beesuh.Credential, error) {
	ref, e := decodeDiscoveryRef(c.CredentialRef)
	if e != nil {
		return beesuh.Credential{}, e
	}
	status, claims, e := s.checkedGrant(ctx, ref)
	if e != nil {
		return beesuh.Credential{}, e
	}
	p := beesuh.Principal{UserID: serviceUserID(s.Issuer, claims.Subject), WorkspaceID: serviceUserID(s.Issuer, claims.Subject)}
	if c.Kind != "provider" || c.UserID != p.UserID || c.WorkspaceID != p.WorkspaceID || c.TargetID != s.Goauthy.Providers[status.Grant.Provider] || c.ID != discoveredID(p, c.CredentialRef) {
		return beesuh.Credential{}, auth.ErrForbidden
	}
	if e = s.authorize(ctx, p); e != nil {
		return beesuh.Credential{}, e
	}
	binding := goauthy.APIKeyBinding{GrantID: ref.Grant, ProviderID: status.Grant.Provider, ConnectionGeneration: status.Generation, ConnectorDigest: status.Grant.Digest, Header: "Authorization", Prefix: "Bearer ", Endpoint: s.Goauthy.endpoints[status.Grant.Provider]}
	source := func(ctx context.Context) (string, error) {
		if e := s.authorize(ctx, p); e != nil {
			return "", e
		}
		current, _, e := s.checkedGrant(ctx, ref)
		if e != nil {
			return "", e
		}
		if current.Grant != status.Grant || current.Generation != status.Generation {
			return "", auth.ErrForbidden
		}
		token, _ := ctx.Value(serviceAccessKey{}).(string)
		return token, nil
	}
	var client *http.Client
	account := ""
	if status.Grant.Digest != "" {
		if c.State == "ready" {
			var version int64
			client, version, e = goauthy.ResolveCurrentAPIKey(ctx, s.Issuer, binding, source, s.client, s.providerClient)
			account = strconv.FormatInt(version, 10)
		} else {
			client = &http.Client{Transport: discoveryUnavailableTransport{}, Timeout: time.Minute, CheckRedirect: noServiceRedirect}
		}
	} else {
		var public struct {
			Connected bool     `json:"connected"`
			State     string   `json:"state"`
			Version   int64    `json:"version"`
			Provider  string   `json:"provider_id"`
			Account   string   `json:"account_id"`
			Scopes    []string `json:"scopes"`
		}
		token, _ := ctx.Value(serviceAccessKey{}).(string)
		if e = s.metadata(ctx, token, &public, "connection-grants", ref.Grant, "credential-status"); e != nil {
			return beesuh.Credential{}, e
		}
		if !public.Connected || public.State != "ready" || public.Version < 1 || public.Provider != status.Grant.Provider || !validServiceText(public.Account) {
			return beesuh.Credential{}, auth.ErrForbidden
		}
		account = public.Account
		client, e = goauthy.NewOAuthClient(s.Issuer, goauthy.OAuthBinding{GrantID: ref.Grant, ProviderID: public.Provider, ConnectionGeneration: status.Generation, AccountID: public.Account, Resource: s.Goauthy.Resource, ConsumerClientID: s.ClientID, Endpoint: binding.Endpoint, Scopes: public.Scopes, CredentialVersion: public.Version}, func(ctx context.Context, _ goauthy.OAuthTokenRequest) (string, error) { return source(ctx) }, s.client, s.providerClient, nil)
	}
	if e != nil {
		return beesuh.Credential{}, e
	}
	metadata, _ := json.Marshal([]string{c.CredentialRef, status.Generation, status.Grant.Digest, account})
	digest := sha256.Sum256(metadata)
	return beesuh.Credential{AccountID: "goauthy_" + hex.EncodeToString(digest[:]), HTTPClient: client}, nil
}

type discoveryUnavailableTransport struct{}

func (discoveryUnavailableTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return nil, auth.ErrUnavailable
}
func discoveredID(p beesuh.Principal, ref string) string {
	b, _ := json.Marshal([]string{p.UserID, p.WorkspaceID, ref})
	h := sha256.Sum256(b)
	return "ga_" + hex.EncodeToString(h[:])
}

// Discovery exposes only known public fields, never arbitrary connection metadata.
func (s *serviceConfig) discoveryHandler(runtime *beesuh.Runtime, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !(r.Method == "GET" && r.URL.Path == "/v1/connection-options") && !(r.Method == "POST" && r.URL.Path == "/v1/connections") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		write := func(code int, value any) {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(httpapi.WriteBudget))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(value)
		}
		fail := func(err error) {
			code := http.StatusServiceUnavailable
			switch {
			case errors.Is(err, beesuh.ErrUnauthenticated), errors.Is(err, auth.ErrInvalidCredentials):
				code = http.StatusUnauthorized
			case errors.Is(err, beesuh.ErrInvalidInput):
				code = http.StatusBadRequest
			case errors.Is(err, beesuh.ErrForbidden), errors.Is(err, auth.ErrForbidden):
				code = http.StatusForbidden
			}
			write(code, map[string]string{"error": "connection unavailable"})
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			fail(beesuh.ErrInvalidInput)
			return
		}
		p, e := s.authenticate(r)
		if e != nil {
			fail(e)
			return
		}
		if r.Method == "GET" {
			claims, token, e := s.discoveryClaims(r.Context(), false)
			if e != nil {
				fail(e)
				return
			}
			var collections []struct {
				ID      string `json:"id"`
				Enabled bool   `json:"enabled"`
				Method  string `json:"auth_method"`
			}
			if e = s.metadata(r.Context(), token, &collections, "connection-collections"); e != nil {
				fail(e)
				return
			}
			if len(collections) > 128 {
				fail(auth.ErrUnavailable)
				return
			}
			options := []discoveryConnection{}
			for _, collection := range collections {
				if !collection.Enabled || (collection.Method != "api_key" && collection.Method != "oauth2") {
					continue
				}
				var connections []discoveryConnection
				if e = s.metadata(r.Context(), token, &connections, "connections", collection.ID); e != nil {
					fail(e)
					return
				}
				if len(connections) > 1000 || len(options)+len(connections) > 1000 {
					fail(auth.ErrUnavailable)
					return
				}
				for _, conn := range connections {
					if conn.Owner == claims.Subject && conn.Collection == collection.ID {
						options = append(options, conn)
					}
				}
			}
			write(http.StatusOK, options)
			return
		}
		var ref discoveryRef
		if decodeServiceJSON(r.Body, 4096, &ref) != nil || !ref.valid() || len(ref.encoded()) > 256 {
			fail(beesuh.ErrInvalidInput)
			return
		}
		status, claims, e := s.checkedGrant(r.Context(), ref)
		if e != nil {
			fail(e)
			return
		}
		if p.UserID != serviceUserID(s.Issuer, claims.Subject) {
			fail(auth.ErrForbidden)
			return
		}
		c := beesuh.Connection{ID: discoveredID(p, ref.encoded()), UserID: p.UserID, WorkspaceID: p.WorkspaceID, Kind: "provider", TargetID: s.Goauthy.Providers[status.Grant.Provider], CredentialRef: ref.encoded(), State: "ready"}
		if e = runtime.RegisterConnection(r.Context(), c); e != nil {
			fail(e)
			return
		}
		connections, e := runtime.Connections(beesuh.WithPrincipal(r.Context(), p))
		if e != nil {
			fail(e)
			return
		}
		for _, current := range connections {
			if current.ID == c.ID {
				if current.State != "ready" {
					fail(auth.ErrForbidden)
					return
				}
				write(http.StatusCreated, current)
				return
			}
		}
		fail(auth.ErrUnavailable)
	})
}
