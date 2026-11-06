// Package grantpolicy checks reviewed pins against the current USER catalog
// before requesting a wire-v2 decision. It does not compile contracts or grant
// permissions. Callers must keep Goauthy consent separate.
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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
	WireVersion           uint64 `json:"wireVersion"`
	PEPPolicyRevision     int64  `json:"pepPolicyRevision"`
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
	w.WireVersion = 2
	return w
}
func (c *Client) Check(ctx context.Context, operation string, s ActualSnapshot, cred Credential) error {
	b, ok := c.config.Operations[operation]
	if !ok || !validSnapshot(s) || !strings.HasPrefix(cred.Authorization, "Bearer ") || len(cred.Authorization) <= 7 || strings.ContainsAny(cred.Authorization, "\r\n") || cred.Subject == "" || cred.Audience == "" {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	revision, e := c.catalogRevision(ctx, b, s.Method)
	if e != nil {
		return ErrDenied
	}
	var nonce [24]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return ErrDenied
	}
	w := c.request(b, s, cred, base64.RawURLEncoding.EncodeToString(nonce[:]))
	w.PEPPolicyRevision = revision
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

func (c *Client) catalogRevision(ctx context.Context, b Binding, method string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.config.BaseURL, "/")+"/api/v2/http-capabilities/catalog/"+c.config.ProviderID, nil)
	if err != nil {
		return 0, ErrDenied
	}
	req.Header.Set("X-Grant-App-Auth-Key", c.appKey)
	res, err := c.http.Do(req)
	if err != nil {
		return 0, ErrDenied
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, ErrDenied
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return 0, ErrDenied
	}
	return c.validateCatalog(data, b, method, time.Now())
}

func (c *Client) validateCatalog(data []byte, b Binding, method string, now time.Time) (int64, error) {
	m, ok := strictObject(data)
	if !ok {
		return 0, ErrDenied
	}
	for key := range m {
		switch key {
		case "schemaVersion", "authorizationMode", "pepPolicyRevision", "providerId", "pepClientId", "generation", "catalogDigest", "capabilities", "issuedAt", "expiresAt", "responseResources":
		default:
			return 0, ErrDenied
		}
	}
	for key, value := range map[string]any{"schemaVersion": uint64(1), "authorizationMode": "user", "providerId": c.config.ProviderID, "pepClientId": c.config.PEPClientID, "generation": uint64(c.config.CatalogGeneration), "catalogDigest": c.config.CatalogDigest} {
		want, _ := json.Marshal(value)
		if !bytes.Equal(bytes.TrimSpace(m[key]), want) {
			return 0, ErrDenied
		}
	}
	var revision int64
	var issued, expires time.Time
	if json.Unmarshal(m["pepPolicyRevision"], &revision) != nil || revision <= 0 || json.Unmarshal(m["issuedAt"], &issued) != nil || issued.IsZero() || json.Unmarshal(m["expiresAt"], &expires) != nil || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > 30*time.Second {
		return 0, ErrDenied
	}
	var capabilities []json.RawMessage
	if json.Unmarshal(m["capabilities"], &capabilities) != nil || capabilities == nil {
		return 0, ErrDenied
	}
	found := false
	for _, raw := range capabilities {
		capability, ok := strictObject(raw)
		if !ok {
			return 0, ErrDenied
		}
		var id string
		if json.Unmarshal(capability["capabilityId"], &id) != nil || id == "" {
			return 0, ErrDenied
		}
		if id != b.CapabilityID {
			continue
		}
		if found {
			return 0, ErrDenied
		}
		found = true
		for key := range capability {
			switch key {
			case "capabilityId", "version", "enabled", "accessMode", "method", "pathTemplate", "actionId", "resource", "query", "request", "response", "contractDigest", "pathParameters", "recordScope", "batch", "responseAuthorization", "organizationContext":
			default:
				return 0, ErrDenied
			}
		}
		computed, err := selectedContractDigest(raw)
		if err != nil || computed != b.ContractDigest {
			return 0, ErrDenied
		}
		// These contracts require follow-up enforcement this consumer does not implement.
		for _, key := range []string{"recordScope", "batch", "responseAuthorization"} {
			if _, present := capability[key]; present {
				return 0, ErrDenied
			}
		}
		for key, value := range map[string]any{"version": uint64(b.CapabilityVersion), "enabled": true, "accessMode": "authorized", "method": method, "actionId": b.ActionID, "contractDigest": b.ContractDigest} {
			want, _ := json.Marshal(value)
			if !bytes.Equal(bytes.TrimSpace(capability[key]), want) {
				return 0, ErrDenied
			}
		}
		var path string
		if json.Unmarshal(capability["pathTemplate"], &path) != nil || !strings.HasPrefix(path, "/") {
			return 0, ErrDenied
		}
		resource, ok := strictObject(capability["resource"])
		wantType, _ := json.Marshal(b.ResourceType)
		if !ok || !bytes.Equal(bytes.TrimSpace(resource["type"]), wantType) {
			return 0, ErrDenied
		}
		extractor, ok := strictObject(resource["id"])
		if !ok {
			return 0, ErrDenied
		}
		var kind string
		if json.Unmarshal(extractor["kind"], &kind) != nil {
			return 0, ErrDenied
		}
		switch kind {
		case "constant":
			var value string
			if len(extractor) != 2 || json.Unmarshal(extractor["value"], &value) != nil || value != b.ResourceID || b.ResourceEvidence.Kind != "constant" {
				return 0, ErrDenied
			}
		case "pathSegment", "queryScalar", "jsonPointer":
			field := "name"
			if kind == "jsonPointer" {
				field = "pointer"
			}
			var name, format string
			if len(extractor) != 3 || json.Unmarshal(extractor[field], &name) != nil || name == "" || json.Unmarshal(extractor["format"], &format) != nil || format == "" {
				return 0, ErrDenied
			}
		default:
			return 0, ErrDenied
		}
		if raw, present := capability["organizationContext"]; present {
			var organizationContext string
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &organizationContext) != nil {
				return 0, ErrDenied
			}
		}
		// Validate supported shapes without compiling route or extractor behavior.
		for _, key := range []string{"query", "request", "response"} {
			if _, ok := strictObject(capability[key]); !ok {
				return 0, ErrDenied
			}
		}
		query, _ := strictObject(capability["query"])
		var mode string
		if json.Unmarshal(query["mode"], &mode) != nil || (mode != "none" && mode != "exact") {
			return 0, ErrDenied
		}
		if mode == "exact" {
			var parameters []json.RawMessage
			if json.Unmarshal(query["parameters"], &parameters) != nil || parameters == nil {
				return 0, ErrDenied
			}
			for _, raw := range parameters {
				fields, ok := strictObject(raw)
				var parameter struct {
					Name     string  `json:"name"`
					Format   string  `json:"format"`
					Required *bool   `json:"required"`
					MaxBytes *uint64 `json:"maxBytes"`
					Repeated *bool   `json:"repeated"`
				}
				if !ok || fields["name"] == nil || fields["format"] == nil || fields["required"] == nil || json.Unmarshal(raw, &parameter) != nil || parameter.Name == "" || parameter.Format == "" || parameter.Required == nil {
					return 0, ErrDenied
				}
			}
		}
		if raw, present := capability["pathParameters"]; present {
			var parameters []json.RawMessage
			if json.Unmarshal(raw, &parameters) != nil || parameters == nil {
				return 0, ErrDenied
			}
			for _, raw := range parameters {
				fields, ok := strictObject(raw)
				var parameter struct {
					Name     string  `json:"name"`
					Format   string  `json:"format"`
					MaxBytes *uint64 `json:"maxBytes"`
				}
				if !ok || fields["name"] == nil || fields["format"] == nil || fields["maxBytes"] == nil || json.Unmarshal(raw, &parameter) != nil || parameter.Name == "" || parameter.Format == "" || parameter.MaxBytes == nil {
					return 0, ErrDenied
				}
			}
		}
		for _, key := range []string{"request", "response"} {
			contract, ok := strictObject(capability[key])
			if !ok {
				return 0, ErrDenied
			}
			var maxBytes uint64
			var headers []json.RawMessage
			if bytes.Equal(bytes.TrimSpace(contract["maxBytes"]), []byte("null")) || json.Unmarshal(contract["maxBytes"], &maxBytes) != nil || contract["forwardHeaders"] == nil || json.Unmarshal(contract["forwardHeaders"], &headers) != nil {
				return 0, ErrDenied
			}
			for _, raw := range headers {
				var name string
				if json.Unmarshal(raw, &name) != nil || name == "" {
					return 0, ErrDenied
				}
			}
			field := "body"
			if key == "response" {
				field = "redirects"
			}
			var value string
			if bytes.Equal(bytes.TrimSpace(contract[field]), []byte("null")) || json.Unmarshal(contract[field], &value) != nil {
				return 0, ErrDenied
			}
		}
	}
	if !found {
		return 0, ErrDenied
	}
	return revision, nil
}

// Only the selected server-serialized capability is hashed, not the catalog envelope.
func selectedContractDigest(data []byte) (string, error) {
	if !utf8.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return "", ErrDenied
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	canonical, err := canonicalContractValue(d, true)
	if err != nil {
		return "", ErrDenied
	}
	if _, err := d.Token(); err != io.EOF {
		return "", ErrDenied
	}
	return Digest(canonical), nil
}

func canonicalContractValue(d *json.Decoder, root bool) ([]byte, error) {
	token, err := d.Token()
	if err != nil {
		return nil, ErrDenied
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			fields := map[string][]byte{}
			seen := map[string]bool{}
			for d.More() {
				keyToken, err := d.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok || seen[strings.ToLower(key)] {
					return nil, ErrDenied
				}
				seen[strings.ToLower(key)] = true
				encoded, err := canonicalContractValue(d, false)
				if err != nil {
					return nil, ErrDenied
				}
				if !root || key != "contractDigest" {
					fields[key] = encoded
				}
			}
			if end, err := d.Token(); err != nil || end != json.Delim('}') {
				return nil, ErrDenied
			}
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			out := []byte{'{'}
			for i, key := range keys {
				if i != 0 {
					out = append(out, ',')
				}
				out = appendContractString(out, key)
				out = append(out, ':')
				out = append(out, fields[key]...)
			}
			return append(out, '}'), nil
		case '[':
			out := []byte{'['}
			for i := 0; d.More(); i++ {
				encoded, err := canonicalContractValue(d, false)
				if err != nil {
					return nil, ErrDenied
				}
				if i != 0 {
					out = append(out, ',')
				}
				out = append(out, encoded...)
			}
			if end, err := d.Token(); err != nil || end != json.Delim(']') {
				return nil, ErrDenied
			}
			return append(out, ']'), nil
		}
	case string:
		return appendContractString(nil, value), nil
	case json.Number:
		number := string(value)
		if strings.ContainsAny(number, ".eE") {
			return nil, ErrDenied
		}
		if strings.HasPrefix(number, "-") {
			if _, err := strconv.ParseInt(number, 10, 64); err != nil || number == "-0" {
				return nil, ErrDenied
			}
		} else if _, err := strconv.ParseUint(number, 10, 64); err != nil {
			return nil, ErrDenied
		}
		return []byte(number), nil
	case bool:
		return []byte(strconv.FormatBool(value)), nil
	case nil:
		return []byte("null"), nil
	}
	return nil, ErrDenied
}

func appendContractString(out []byte, value string) []byte {
	out = append(out, '"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			out = append(out, '\\', byte(r))
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				out = append(out, '\\', 'u', '0', '0', hex[byte(r)>>4], hex[byte(r)&15])
			} else {
				out = utf8.AppendRune(out, r)
			}
		}
	}
	return append(out, '"')
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
	if w.WireVersion != 2 || w.PEPPolicyRevision <= 0 {
		return false
	}
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
	if bytes.Equal(bytes.TrimSpace(m["allowed"]), []byte("null")) || json.Unmarshal(m["allowed"], &allowed) != nil || json.Unmarshal(m["reason"], &reason) != nil || reason == "" || json.Unmarshal(m["principalDigest"], &principal) != nil || !hexPattern.MatchString(principal) || json.Unmarshal(m["issuedAt"], &issued) != nil || json.Unmarshal(m["expiresAt"], &expires) != nil {
		return false
	}
	if issued.IsZero() || issued.After(now) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > 5*time.Second {
		return false
	}
	return allowed
}
