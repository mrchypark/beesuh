package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// All values are deliberately fake, and the Docker network has no external route.
const fakeKey = "smoke-provider-secret-key-not-real"
const fakeModel = "minimax-m3.1-flash"
const resource = issuer + "/connections"
const blockMarker = "smoke-block-until-cancel"

type fixtureStats struct {
	Deliveries int64 `json:"deliveries"`
	Models     int64 `json:"models"`
	Active     bool  `json:"active"`
	Violations int64 `json:"violations"`
}

func personalFixture(auth http.Handler) http.Handler {
	var deliveries, models, violations atomic.Int64
	var active atomic.Bool
	var revoked atomic.Bool
	// Consent is one stable record; reads must not manufacture a new revision.
	expires := time.Now().Add(time.Hour).UnixMilli()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if err != nil || len(body) > 1<<20 {
			http.Error(w, "invalid body", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		reject := func() { violations.Add(1); http.Error(w, "fixture boundary violation", 400) }
		provider := r.URL.Path == "/inference/go/openai/v1/chat/completions"
		// The model key may occur only in the provider Authorization header.
		headers := r.Header.Clone()
		if provider {
			headers.Del("Authorization")
		}
		if strings.Contains(fmt.Sprint(headers)+r.URL.String()+string(body), fakeKey) {
			reject()
			return
		}
		if !provider && strings.Contains(r.Header.Get("Authorization"), fakeKey) {
			reject()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/smoke/consent" {
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+fixtureControl {
				reject()
				return
			}
			var state struct {
				Revoked bool `json:"revoked"`
			}
			if json.Unmarshal(body, &state) != nil {
				reject()
				return
			}
			revoked.Store(state.Revoked)
			fmt.Fprintln(w, `{}`)
			return
		}
		if r.URL.Path == "/smoke/stats" && r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(fixtureStats{deliveries.Load(), models.Load(), active.Load(), violations.Load()})
			return
		}
		if provider {
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+fakeKey || r.Header.Get("Cookie") != "" {
				reject()
				return
			}
			for _, secret := range fixtureSecrets()[1:] {
				if strings.Contains(fmt.Sprint(r.Header)+string(body), secret) {
					reject()
					return
				}
			}
			var request struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if json.Unmarshal(body, &request) != nil || request.Model != fakeModel || request.Stream {
				reject()
				return
			}
			models.Add(1)
			if bytes.Contains(body, []byte(blockMarker)) {
				active.Store(true)
				defer active.Store(false)
				// Never finish first: CancelRun must interrupt the actual provider request.
				select {
				case <-r.Context().Done():
				case <-time.After(60 * time.Second):
					violations.Add(1)
				}
				return
			}
			fmt.Fprintln(w, `{"choices":[{"message":{"role":"assistant","content":"smoke model reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/auth/v1/") {
			auth.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer smoke-access-token" || r.Header.Get("Cookie") != "" {
			reject()
			return
		}
		credential := r.URL.Path == "/auth/v1/connection-grants/approved/credential"
		if (!credential && r.Method != "GET") || (credential && (r.Method != "POST" || len(body) != 0)) {
			reject()
			return
		}
		connection := map[string]any{"id": "personal", "collection_id": "models", "owner_subject": "smoke-user", "state": "ready"}
		switch r.URL.Path {
		case "/auth/v1/connection-collections":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": "models", "enabled": true, "auth_method": "api_key", "provider_ids": []string{"smoke-provider"}}})
		case "/auth/v1/connections/models":
			_ = json.NewEncoder(w).Encode([]any{connection, map[string]any{"id": "foreign", "collection_id": "models", "owner_subject": "other", "state": "ready"}})
		case "/auth/v1/connections/models/personal":
			_ = json.NewEncoder(w).Encode(connection)
		case "/auth/v1/connections/models/personal/grants/approved":
			_ = json.NewEncoder(w).Encode(map[string]any{"connection_generation": "generation", "grant": map[string]any{"id": "approved", "owner_subject": "smoke-user", "collection_id": "models", "connection_id": "personal", "consumer_client_id": "beesuh-smoke", "mode": "credential_delivery", "resource": resource, "generation": "generation", "consumer_generation": "consumer-generation", "provider_id": "smoke-provider", "connector_digest": "digest", "revision": 1, "provider_revision": 1, "expires_at_unix_ms": expires, "revoked": revoked.Load()}})
		case "/auth/v1/connections/models/personal/grants/unapproved":
			http.Error(w, `{"error":"access_denied"}`, 403)
		case "/auth/v1/connection-grants/approved/credential":
			deliveries.Add(1)
			if revoked.Load() {
				http.Error(w, `{"error":"access_denied"}`, 403)
				return
			}
			log.Print("validated fake credential delivery")
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": fakeKey, "grant_id": "approved", "provider_id": "smoke-provider", "connection_generation": "generation", "credential_version": 1, "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": expires})
		default:
			reject()
		}
	})
}

const fixtureAppKey = "smoke-grant-app-key-not-real"
const fixtureControl = "smoke-fixture-control-not-real"
const sharedOrganization = "smoke-org"
const sharedWorkspace = "smoke-shared"
const grantHintAudience = "urn:grant:smoke-hint"

// Historical pins plus the owner-confirmed wire2 shape are fixture-only.
// No supplier wire2 golden exists; these are not live catalog hash proofs.
const fixtureCatalog = "sha256:833587bd54c6af1c79f7c56955fc5289d1b01a4e69d8462463afed85cc9caa0b"

// Reviewed ASCII canonical preimages, authored from the owner specification.
// This is deliberately separate from the production selected-contract encoder.
func fixtureContractPreimage(operation string) string {
	route, ok := fixtureOperations()[operation]
	if !ok {
		panic("unknown fixture operation")
	}
	pathParameters := ""
	if strings.Contains(route[1], "{id}") {
		pathParameters = `"pathParameters":[{"format":"asciiToken","maxBytes":256,"name":"id"}],`
	}
	query := `{"mode":"none"}`
	if operation == "run_read" || operation == "history_read" || operation == "run_cancel" {
		parameters := `{"format":"asciiToken","maxBytes":256,"name":"workspace_id","repeated":false,"required":true}`
		if operation == "history_read" {
			parameters += `,{"format":"asciiToken","maxBytes":256,"name":"session_key","repeated":false,"required":true}`
		}
		query = `{"mode":"exact","parameters":[` + parameters + `]}`
	}
	request := `{"authorization":"forward-user","body":"none","forwardHeaders":["x-organization-id"],"maxBytes":0}`
	if operation == "run_execute" || operation == "connection_select" {
		request = `{"authorization":"forward-user","body":"opaque","contentTypes":["application/json"],"forwardHeaders":["content-type","x-organization-id"],"maxBytes":262144}`
	}
	return fmt.Sprintf(`{"accessMode":"authorized","actionId":%q,"capabilityId":%q,"enabled":true,"method":%q,%s"pathTemplate":%q,"query":%s,"request":%s,"resource":{"id":{"kind":"constant","value":"smoke-shared"},"type":"workspace"},"response":{"forwardHeaders":[],"maxBytes":1048576,"redirects":"deny"},"version":1}`, fixtureAction(operation), "bee."+operation, route[0], pathParameters, route[1], query, request)
}

func fixtureContractDigest(operation string) string {
	return fixtureDigest([]byte(fixtureContractPreimage(operation)))
}

// Owner-supported constant resource for this single-workspace fixture. This
// does not register a catalog or grant membership from caller selectors.
const fixtureEvidenceKind = "constant"

func fixtureSecrets() []string {
	return []string{fakeKey, "smoke-access-token", "smoke-client-secret", fixtureAppKey, fixtureControl, "smoke-nonmember-token", "smoke-wrong-user-token", "smoke-wrong-audience-token", "smoke-wrong-client-token", "smoke-no-use-token", "smoke-no-connections-audience-token"}
}

func fixtureDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fixtureCredentialDigest(authorization string) string {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(authorization)))
	data := append([]byte("grant.http-capability.provider-credential.v1"), size[:]...)
	return fixtureDigest(append(data, []byte(authorization)...))
}

func fixtureHeaderDigest(operation string, contentTypes []string) (string, bool) {
	preimage := ""
	if operation == "run_execute" || operation == "connection_select" {
		if len(contentTypes) != 1 {
			return "", false
		}
		mediaType, _, err := mime.ParseMediaType(contentTypes[0])
		if err != nil || mediaType != "application/json" {
			return "", false
		}
		preimage = "content-type:" + contentTypes[0] + "\n"
	}
	return fixtureDigest([]byte(preimage + "x-organization-id:" + sharedOrganization + "\n")), true
}

func fixtureAction(operation string) string {
	switch operation {
	case "run_execute", "connection_select", "connection_revoke":
		return "beesuh.run.create"
	case "run_cancel":
		return "beesuh.run.cancel"
	default:
		return "beesuh.run.read"
	}
}

func fixtureOperations() map[string][2]string {
	return map[string][2]string{
		"run_execute": {"POST", "/v1/chat"}, "run_read": {"GET", "/v1/runs/{id}"},
		"history_read": {"GET", "/v1/history"}, "providers_list": {"GET", "/v1/providers"},
		"connection_options": {"GET", "/v1/connection-options"}, "connection_select": {"POST", "/v1/connections"},
		"connections_list": {"GET", "/v1/connections"}, "connection_revoke": {"DELETE", "/v1/connections/{id}"},
		"run_cancel": {"POST", "/v1/runs/{id}/cancel"},
	}
}

func sharedFixtureConfig(base []byte, partial bool) ([]byte, error) {
	var config map[string]any
	if err := json.Unmarshal(base, &config); err != nil {
		return nil, err
	}
	operations := map[string]any{}
	for operation := range fixtureOperations() {
		if partial && operation == "run_execute" {
			continue
		}
		headers := []string{"x-organization-id"}
		if operation == "run_execute" || operation == "connection_select" {
			headers = []string{"content-type", "x-organization-id"}
		}
		operations[operation] = map[string]any{"capability_id": "bee." + operation, "capability_version": 1, "contract_digest": fixtureContractDigest(operation), "action_id": fixtureAction(operation), "forward_headers": headers}
	}
	config["shared"] = map[string]any{
		"grant_url": issuer, "app_key_file": "/config/grant-app-key", "grant_credential_hint_audience": grantHintAudience,
		"contexts": []any{map[string]any{
			"organization_id": sharedOrganization, "workspace_id": sharedWorkspace, "provider_id": "bee-smoke-provider", "pep_client_id": "bee-smoke-pep",
			"catalog_generation": 1, "catalog_digest": fixtureCatalog, "resource_type": "workspace", "resource_id": sharedWorkspace,
			"resource_evidence": map[string]string{"kind": fixtureEvidenceKind, "name": "", "canonicalValue": sharedWorkspace}, "operations": operations,
		}},
	}
	return json.Marshal(config)
}

type fixtureExpectation struct {
	Method       string   `json:"method"`
	Path         string   `json:"path"`
	Body         string   `json:"body"`
	Token        string   `json:"token"`
	Mode         string   `json:"mode"`
	ContentTypes []string `json:"contentTypes"`
}

type fixturePolicyStats struct {
	Catalogs   int   `json:"catalogs"`
	Revision   int64 `json:"revision"`
	Checks     int   `json:"checks"`
	Allows     int   `json:"allows"`
	Violations int   `json:"violations"`
}

type fixturePolicy struct {
	mu       sync.Mutex
	expected fixtureExpectation
	nonces   map[string]bool
	observed fixturePolicyStats
	revision int64
	fetched  bool
}

func newFixturePolicy() *fixturePolicy {
	return &fixturePolicy{nonces: map[string]bool{}, revision: 7}
}

// Catalog bytes follow the owner's schema, with configured fixture pins only.
// This fixture does not compile catalogs or recompute pending canonical hashes.
func (p *fixturePolicy) catalog(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method != "GET" || r.URL.Path != "/api/v2/http-capabilities/catalog/bee-smoke-provider" || len(r.Header.Values("X-Grant-App-Auth-Key")) != 1 || r.Header.Get("X-Grant-App-Auth-Key") != fixtureAppKey || len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("X-Organization-ID")) != 0 || r.Header.Get("Cookie") != "" || r.URL.RawQuery != "" {
		p.observed.Violations++
		http.Error(w, "fixture catalog requires app key only", 403)
		return
	}
	p.observed.Catalogs++
	p.fetched = true
	capabilities := []any{}
	for operation, route := range fixtureOperations() {
		headers := []string{"x-organization-id"}
		body, maxBytes := "none", uint64(0)
		if operation == "run_execute" || operation == "connection_select" {
			headers = []string{"content-type", "x-organization-id"}
			body, maxBytes = "opaque", 262144
		}
		query := map[string]any{"mode": "none"}
		if operation == "run_read" || operation == "history_read" || operation == "run_cancel" {
			parameters := []any{map[string]any{"name": "workspace_id", "format": "asciiToken", "required": true, "repeated": false, "maxBytes": uint64(256)}}
			if operation == "history_read" {
				parameters = append(parameters, map[string]any{"name": "session_key", "format": "asciiToken", "required": true, "repeated": false, "maxBytes": uint64(256)})
			}
			query = map[string]any{"mode": "exact", "parameters": parameters}
		}
		request := map[string]any{"body": body, "maxBytes": maxBytes, "forwardHeaders": headers, "authorization": "forward-user"}
		if body == "opaque" {
			request["contentTypes"] = []string{"application/json"}
		}
		capability := map[string]any{
			"capabilityId": "bee." + operation, "version": uint64(1), "enabled": true, "accessMode": "authorized", "method": route[0], "pathTemplate": route[1], "actionId": fixtureAction(operation),
			"resource": map[string]any{"type": "workspace", "id": map[string]string{"kind": "constant", "value": sharedWorkspace}},
			"query":    query, "request": request, "response": map[string]any{"maxBytes": uint64(1048576), "forwardHeaders": []string{}, "redirects": "deny"}, "contractDigest": fixtureContractDigest(operation),
		}
		if strings.Contains(route[1], "{id}") {
			capability["pathParameters"] = []any{map[string]any{"name": "id", "format": "asciiToken", "maxBytes": uint64(256)}}
		}
		capabilities = append(capabilities, capability)
	}
	now := time.Now().UTC()
	catalog := map[string]any{"schemaVersion": uint64(1), "authorizationMode": "user", "pepPolicyRevision": p.revision, "providerId": "bee-smoke-provider", "pepClientId": "bee-smoke-pep", "generation": uint64(1), "catalogDigest": fixtureCatalog, "capabilities": capabilities, "issuedAt": now, "expiresAt": now.Add(30 * time.Second)}
	switch p.expected.Mode {
	case "catalog-unavailable":
		http.Error(w, "fixture catalog unavailable", 503)
		return
	case "catalog-decide-race":
		p.revision += 4
	}
	_ = json.NewEncoder(w).Encode(catalog)
}

func (p *fixturePolicy) expect(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+fixtureControl {
		http.Error(w, "fixture control required", 403)
		return
	}
	var expected fixtureExpectation
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&expected) != nil || expected.Method == "" || expected.Path == "" {
		http.Error(w, "invalid fixture expectation", 400)
		return
	}
	p.mu.Lock()
	p.expected = expected
	if expected.Mode == "revision-11" {
		p.revision = 11
	}
	p.mu.Unlock()
	fmt.Fprintln(w, `{}`)
}

func (p *fixturePolicy) stats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+fixtureControl {
		http.Error(w, "fixture control required", 403)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = json.NewEncoder(w).Encode(p.observed)
}

func (p *fixturePolicy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observed.Checks++
	var request map[string]json.RawMessage
	d := json.NewDecoder(io.LimitReader(r.Body, 32769))
	if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF {
		p.observed.Violations++
		http.Error(w, "invalid fixture wire", 403)
		return
	}
	expected := p.expected
	u, err := url.ParseRequestURI(expected.Path)
	if err != nil {
		p.observed.Violations++
		http.Error(w, "missing expected business request", 403)
		return
	}
	operation := ""
	for name, route := range fixtureOperations() {
		path := route[1]
		if strings.Contains(path, "{id}") {
			parts, actual := strings.Split(path, "/"), strings.Split(u.Path, "/")
			if len(parts) == len(actual) {
				for i := range parts {
					if parts[i] == "{id}" && actual[i] != "" {
						parts[i] = actual[i]
					}
				}
				path = strings.Join(parts, "/")
			}
		}
		if route[0] == expected.Method && path == u.Path {
			operation = name
		}
	}
	query := strings.ReplaceAll(u.Query().Encode(), "+", "%20")
	credential := "Bearer " + expected.Token
	headerDigest, validContentType := fixtureHeaderDigest(operation, expected.ContentTypes)
	values := map[string]any{
		"wireVersion": uint64(2), "pepPolicyRevision": p.revision,
		"bodyDigest": fixtureDigest([]byte(expected.Body)), "bodyLength": len(expected.Body), "canonicalPath": u.Path,
		"canonicalQuery": query, "canonicalQueryDigest": fixtureDigest([]byte(query)), "forwardedHeaderDigest": headerDigest,
		"capabilityId": "bee." + operation, "capabilityVersion": 1, "catalogDigest": fixtureCatalog, "catalogGeneration": 1,
		"contractDigest": fixtureContractDigest(operation), "method": expected.Method, "pepClientId": "bee-smoke-pep", "providerId": "bee-smoke-provider",
		"providerCredential": map[string]string{"subject": "goauthy:" + issuer + "/smoke-user", "audience": grantHintAudience}, "providerCredentialDigest": fixtureCredentialDigest(credential),
		"resourceEvidence": map[string]string{"canonicalValue": sharedWorkspace, "kind": fixtureEvidenceKind, "name": ""}, "resourceId": sharedWorkspace, "resourceType": "workspace",
	}
	if expected.Token == "smoke-nonmember-token" {
		values["providerCredential"] = map[string]string{"subject": "goauthy:" + issuer + "/nonmember-user", "audience": grantHintAudience}
	}
	valid := validContentType && p.fetched && operation != "" && r.Method == "POST" && r.URL.Path == "/api/v2/http-capabilities/decide" && len(r.Header.Values("Authorization")) == 1 && r.Header.Get("Authorization") == credential && len(r.Header.Values("X-Organization-ID")) == 1 && r.Header.Get("X-Organization-ID") == sharedOrganization && len(r.Header.Values("X-Grant-App-Auth-Key")) == 1 && r.Header.Get("X-Grant-App-Auth-Key") == fixtureAppKey && len(request) == len(values)+1
	p.fetched = false
	if expected.Mode == "catalog-decide-race" {
		http.Error(w, "fixture stale policy", 403)
		return
	}
	for name, value := range values {
		var actual any
		encoded, _ := json.Marshal(value)
		var want any
		if json.Unmarshal(request[name], &actual) != nil || json.Unmarshal(encoded, &want) != nil || !reflect.DeepEqual(actual, want) {
			valid = false
		}
	}
	var nonce string
	if json.Unmarshal(request["requestBinding"], &nonce) != nil {
		valid = false
	}
	nonceBytes, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(nonceBytes) != 24 || p.nonces[nonce] {
		valid = false
	}
	if !valid {
		p.observed.Violations++
		http.Error(w, "fixture snapshot or binding mismatch", 403)
		return
	}
	p.nonces[nonce] = true
	p.observed.Revision = p.revision
	if expected.Mode == "outage" {
		http.Error(w, "fixture PDP unavailable", 503)
		return
	}
	member := expected.Token == "smoke-access-token" || expected.Token == "smoke-no-use-token" || expected.Token == "smoke-no-connections-audience-token"
	if !member || expected.Mode == "deny" {
		http.Error(w, "fixture membership/policy denial", 403)
		return
	}
	// Construct the result from independently expected values, never an allow echo.
	delete(values, "canonicalQuery")
	delete(values, "resourceEvidence")
	delete(values, "providerCredential")
	values["requestBinding"], values["allowed"], values["reason"] = nonce, true, "fixture_only_allow"
	values["actionId"], values["providerSubject"], values["providerAudience"] = fixtureAction(operation), "goauthy:"+issuer+"/smoke-user", grantHintAudience
	values["principalDigest"] = strings.TrimPrefix(fixtureDigest([]byte("fixture-principal:smoke-user")), "sha256:")
	now := time.Now().UTC()
	values["issuedAt"], values["expiresAt"] = now, now.Add(3*time.Second)
	switch expected.Mode {
	case "bad-binding":
		values["requestBinding"] = "different-binding"
	case "bad-digest":
		values["bodyDigest"] = fixtureDigest([]byte("different-body"))
	case "bad-headers":
		values["forwardedHeaderDigest"] = fixtureDigest(nil)
	case "bad-hint":
		values["providerAudience"] = serviceAudience
	case "stale-revision":
		values["pepPolicyRevision"] = int64(7)
	case "malformed-revision":
		values["pepPolicyRevision"] = "11"
	}
	p.observed.Allows++
	_ = json.NewEncoder(w).Encode(values)
}
