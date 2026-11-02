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
		expires := time.Now().Add(time.Hour).UnixMilli()
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

// Existing Grant wire-v1 golden pins are wire-contract fixtures only. They are
// not a registered Bee/Exposure catalog, learner grant, or live-service proof.
const fixtureCatalog = "sha256:833587bd54c6af1c79f7c56955fc5289d1b01a4e69d8462463afed85cc9caa0b"
const fixtureContract = "sha256:e24b65bb4c4e37a170ed8fabcf94bab2111861b1b6d220395d5274f22f0bdf42"

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

func fixtureHeaderDigest() string {
	// Candidate two-selector preimage; Content-Type forward-list confirmation is pending.
	return fixtureDigest([]byte("x-organization-id:" + sharedOrganization + "\nx-workspace-id:" + sharedWorkspace + "\n"))
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
		operations[operation] = map[string]any{"capability_id": "bee." + operation, "capability_version": 1, "contract_digest": fixtureContract, "action_id": fixtureAction(operation)}
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
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
	Token  string `json:"token"`
	Mode   string `json:"mode"`
}

type fixturePolicyStats struct {
	Checks     int `json:"checks"`
	Allows     int `json:"allows"`
	Violations int `json:"violations"`
}

type fixturePolicy struct {
	mu       sync.Mutex
	expected fixtureExpectation
	nonces   map[string]bool
	observed fixturePolicyStats
}

func newFixturePolicy() *fixturePolicy {
	return &fixturePolicy{nonces: map[string]bool{}}
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
	values := map[string]any{
		"bodyDigest": fixtureDigest([]byte(expected.Body)), "bodyLength": len(expected.Body), "canonicalPath": u.Path,
		"canonicalQuery": query, "canonicalQueryDigest": fixtureDigest([]byte(query)), "forwardedHeaderDigest": fixtureHeaderDigest(),
		"capabilityId": "bee." + operation, "capabilityVersion": 1, "catalogDigest": fixtureCatalog, "catalogGeneration": 1,
		"contractDigest": fixtureContract, "method": expected.Method, "pepClientId": "bee-smoke-pep", "providerId": "bee-smoke-provider",
		"providerCredential": map[string]string{"subject": "smoke-user", "audience": grantHintAudience}, "providerCredentialDigest": fixtureCredentialDigest(credential),
		"resourceEvidence": map[string]string{"canonicalValue": sharedWorkspace, "kind": fixtureEvidenceKind, "name": ""}, "resourceId": sharedWorkspace, "resourceType": "workspace",
	}
	if expected.Token == "smoke-nonmember-token" {
		values["providerCredential"] = map[string]string{"subject": "nonmember-user", "audience": grantHintAudience}
	}
	valid := operation != "" && r.Method == "POST" && r.URL.Path == "/api/v2/http-capabilities/decide" && r.Header.Get("Authorization") == credential && r.Header.Get("X-Organization-ID") == sharedOrganization && r.Header.Get("X-Grant-App-Auth-Key") == fixtureAppKey && len(request) == len(values)+1
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
	values["actionId"], values["providerSubject"], values["providerAudience"] = fixtureAction(operation), "smoke-user", grantHintAudience
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
	}
	p.observed.Allows++
	_ = json.NewEncoder(w).Encode(values)
}
