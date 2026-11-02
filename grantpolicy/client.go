// Package grantpolicy implements a static, reviewed HTTP-capability PEP binding.
// It does not compile catalogs, grant permissions, or establish catalog freshness.
// Grant's owner-side freshness proof remains pending. Callers must enforce Check
// immediately before the exact operation and keep Goauthy consent separate.
package grantpolicy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var ErrDenied = errors.New("grantpolicy: authorization denied or unavailable")
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var hexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func digestOK(s string) bool {
	return strings.HasPrefix(s, "sha256:") && hexPattern.MatchString(strings.TrimPrefix(s, "sha256:"))
}

// Config is trusted server configuration, copied by New. No fields may come
// from browser headers or backend permission/filter interpretation.
type Config struct {
	BaseURL, AppKeyFile, OrganizationID, ProviderID, PEPClientID string
	CatalogGeneration                                            int64
	CatalogDigest                                                string
	Operations                                                   map[string]Binding
	Timeout                                                      time.Duration
	AllowPrivateHTTP                                             bool
}
type ResourceEvidence struct {
	CanonicalValue string `json:"canonicalValue"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
}
type ActualSnapshot struct {
	Method                string `json:"method"`
	CanonicalPath         string `json:"canonicalPath"`
	CanonicalQuery        string `json:"canonicalQuery"`
	ForwardedHeaderDigest string `json:"forwardedHeaderDigest"`
	BodyDigest            string `json:"bodyDigest"`
	BodyLength            int64  `json:"bodyLength"`
}
type Binding struct {
	CapabilityID                                       string
	CapabilityVersion                                  int64
	ContractDigest, ActionID, ResourceType, ResourceID string
	ResourceEvidence                                   ResourceEvidence
}

// Credential is supplied per check, never retained. Subject is an already
// verified OIDC claim. Audience is an explicit registered, verified provider
// hint, never inferred from OIDC aud/client_id or copied from request headers.
type Credential struct{ Authorization, Subject, Audience string }
type Client struct {
	config Config
	http   *http.Client
	appKey string
}

func New(cfg Config) (*Client, error) {
	bad := func() (*Client, error) { return nil, errors.New("grantpolicy: invalid static configuration") }
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return bad()
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !cfg.AllowPrivateHTTP || (u.Hostname() != "localhost" && (ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()))) {
			return bad()
		}
	}
	if !keyPattern.MatchString(cfg.ProviderID) || !keyPattern.MatchString(cfg.PEPClientID) || !keyPattern.MatchString(cfg.OrganizationID) || cfg.CatalogGeneration <= 0 || !digestOK(cfg.CatalogDigest) || len(cfg.Operations) == 0 {
		return bad()
	}
	ops := make(map[string]Binding, len(cfg.Operations))
	for name, b := range cfg.Operations {
		if !keyPattern.MatchString(name) || !keyPattern.MatchString(b.CapabilityID) || b.CapabilityVersion <= 0 || !digestOK(b.ContractDigest) || !keyPattern.MatchString(b.ActionID) || !keyPattern.MatchString(b.ResourceType) || !keyPattern.MatchString(b.ResourceID) || b.ResourceEvidence.CanonicalValue != b.ResourceID || !keyPattern.MatchString(b.ResourceEvidence.Kind) || (b.ResourceEvidence.Name == "" && b.ResourceEvidence.Kind != "constant") || (b.ResourceEvidence.Name != "" && !keyPattern.MatchString(b.ResourceEvidence.Name)) {
			return bad()
		}
		ops[name] = b
	}
	cfg.Operations = ops
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Timeout < 0 || cfg.Timeout > 5*time.Second {
		return bad()
	}
	f, err := os.Open(cfg.AppKeyFile)
	if err != nil {
		return bad()
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return bad()
	}
	key := strings.TrimSpace(string(data))
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return bad()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{cfg, &http.Client{Timeout: cfg.Timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, key}, nil
}
func validSnapshot(s ActualSnapshot) bool {
	if s.Method == "" || s.Method != strings.ToUpper(s.Method) || !keyPattern.MatchString(s.Method) || s.BodyLength < 0 || !digestOK(s.BodyDigest) || !digestOK(s.ForwardedHeaderDigest) {
		return false
	}
	u, e := url.ParseRequestURI(s.CanonicalPath)
	if e != nil || u.Path == "" || !strings.HasPrefix(s.CanonicalPath, "/") || strings.HasPrefix(s.CanonicalPath, "//") || u.RawQuery != "" || strings.ContainsAny(s.CanonicalPath, "?#\r\n") {
		return false
	}
	q, e := url.ParseQuery(s.CanonicalQuery)
	return e == nil && strings.ReplaceAll(q.Encode(), "+", "%20") == s.CanonicalQuery
}

// Digest hashes exact bytes. Canonical query/header preimages must be built by
// the trusted server according to its reviewed contract; this client does not
// invent canonicalization rules absent from the delivered wire artifact.
func Digest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}
func CredentialDigest(authorization string) string {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(authorization)))
	h := sha256.New()
	h.Write([]byte("grant.http-capability.provider-credential.v1"))
	h.Write(n[:])
	h.Write([]byte(authorization))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

type wireRequest struct {
	BodyDigest            string `json:"bodyDigest"`
	BodyLength            int64  `json:"bodyLength"`
	CanonicalPath         string `json:"canonicalPath"`
	CanonicalQuery        string `json:"canonicalQuery"`
	CanonicalQueryDigest  string `json:"canonicalQueryDigest"`
	CapabilityID          string `json:"capabilityId"`
	CapabilityVersion     int64  `json:"capabilityVersion"`
	CatalogDigest         string `json:"catalogDigest"`
	CatalogGeneration     int64  `json:"catalogGeneration"`
	ContractDigest        string `json:"contractDigest"`
	ForwardedHeaderDigest string `json:"forwardedHeaderDigest"`
	Method                string `json:"method"`
	PEPClientID           string `json:"pepClientId"`
	ProviderCredential    struct {
		Audience string `json:"audience"`
		Subject  string `json:"subject"`
	} `json:"providerCredential"`
	ProviderCredentialDigest string           `json:"providerCredentialDigest"`
	ProviderID               string           `json:"providerId"`
	RequestBinding           string           `json:"requestBinding"`
	ResourceEvidence         ResourceEvidence `json:"resourceEvidence"`
	ResourceID               string           `json:"resourceId"`
	ResourceType             string           `json:"resourceType"`
}

func (c *Client) request(b Binding, s ActualSnapshot, cred Credential, nonce string) wireRequest {
	w := wireRequest{BodyDigest: s.BodyDigest, BodyLength: s.BodyLength, CanonicalPath: s.CanonicalPath, CanonicalQuery: s.CanonicalQuery, CanonicalQueryDigest: Digest([]byte(s.CanonicalQuery)), CapabilityID: b.CapabilityID, CapabilityVersion: b.CapabilityVersion, CatalogDigest: c.config.CatalogDigest, CatalogGeneration: c.config.CatalogGeneration, ContractDigest: b.ContractDigest, ForwardedHeaderDigest: s.ForwardedHeaderDigest, Method: s.Method, PEPClientID: c.config.PEPClientID, ProviderCredentialDigest: CredentialDigest(cred.Authorization), ProviderID: c.config.ProviderID, RequestBinding: nonce, ResourceEvidence: b.ResourceEvidence, ResourceID: b.ResourceID, ResourceType: b.ResourceType}
	w.ProviderCredential.Audience = cred.Audience
	w.ProviderCredential.Subject = cred.Subject
	return w
}
func (c *Client) Check(ctx context.Context, operation string, s ActualSnapshot, cred Credential) error {
	b, ok := c.config.Operations[operation]
	if !ok || !validSnapshot(s) || !strings.HasPrefix(cred.Authorization, "Bearer ") || len(cred.Authorization) <= 7 || strings.ContainsAny(cred.Authorization, "\r\n") || cred.Subject == "" || cred.Audience == "" {
		return ErrDenied
	}
	var nonce [24]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return ErrDenied
	}
	w := c.request(b, s, cred, base64.RawURLEncoding.EncodeToString(nonce[:]))
	data, e := json.Marshal(w)
	if e != nil {
		return ErrDenied
	}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.config.BaseURL, "/")+"/api/v2/http-capabilities/decide", bytes.NewReader(data))
	if e != nil {
		return ErrDenied
	}
	req.Header.Set("Authorization", cred.Authorization)
	req.Header.Set("X-Organization-ID", c.config.OrganizationID)
	req.Header.Set("X-Grant-App-Auth-Key", c.appKey)
	req.Header.Set("Content-Type", "application/json")
	res, e := c.http.Do(req)
	if e != nil {
		return ErrDenied
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ErrDenied
	}
	data, e = io.ReadAll(io.LimitReader(res.Body, 32769))
	if e != nil || len(data) > 32768 {
		return ErrDenied
	}
	if !validateResponse(data, w, b.ActionID, time.Now()) {
		return ErrDenied
	}
	return nil
}

// Decode exact field names and reject duplicates, including case aliases. This
// is consumer hardening, not a claim about the owner's decoder behavior.
func strictObject(data []byte) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return nil, false
	}
	m := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return nil, false
		}
		k, ok := t.(string)
		if !ok || seen[strings.ToLower(k)] {
			return nil, false
		}
		seen[strings.ToLower(k)] = true
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, false
		}
		m[k] = v
	}
	if t, e = d.Token(); e != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, false
	}
	return m, true
}
func validateResponse(data []byte, w wireRequest, action string, now time.Time) bool {
	m, ok := strictObject(data)
	if !ok {
		return false
	}
	expectedData, _ := json.Marshal(w)
	expected, _ := strictObject(expectedData)
	delete(expected, "canonicalQuery")
	delete(expected, "resourceEvidence")
	delete(expected, "providerCredential")
	for k, v := range map[string]string{"actionId": action, "providerSubject": w.ProviderCredential.Subject, "providerAudience": w.ProviderCredential.Audience} {
		expected[k], _ = json.Marshal(v)
	}
	for k, v := range expected {
		if !bytes.Equal(bytes.TrimSpace(m[k]), v) {
			return false
		}
		delete(m, k)
	}
	if len(m) != 5 {
		return false
	}
	var allowed bool
	var reason, principal string
	var issued, expires time.Time
	if json.Unmarshal(m["allowed"], &allowed) != nil || !allowed || json.Unmarshal(m["reason"], &reason) != nil || reason == "" || json.Unmarshal(m["principalDigest"], &principal) != nil || !hexPattern.MatchString(principal) || json.Unmarshal(m["issuedAt"], &issued) != nil || json.Unmarshal(m["expiresAt"], &expires) != nil {
		return false
	}
	return !issued.IsZero() && !issued.After(now) && expires.After(now) && expires.After(issued) && expires.Sub(issued) <= 5*time.Second
}
