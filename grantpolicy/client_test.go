package grantpolicy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	ExpectedHashes map[string]string `json:"expectedHashes"`
	HashPreimages  struct {
		Body       string `json:"bodyUtf8"`
		Query      string `json:"canonicalQueryUtf8"`
		Headers    string `json:"canonicalForwardedHeadersUtf8"`
		Catalog    string `json:"catalogUtf8"`
		Contract   string `json:"contractUtf8"`
		Credential struct {
			Auth string `json:"authUtf8"`
		} `json:"providerCredential"`
	} `json:"hashPreimages"`
	Wire struct {
		Request  wireRequest                `json:"decideRequest"`
		Response map[string]json.RawMessage `json:"allowResponse"`
		Catalog  map[string]json.RawMessage `json:"catalog"`
	} `json:"wire"`
}

func load(t *testing.T) fixture {
	t.Helper()
	data, e := os.ReadFile("testdata/wire-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	if Digest(data) != "sha256:cbd4f1096c701262b7dd3d8abe603fa636088dc2f8bddb2967262bfbb1ad9696" {
		t.Fatal("fixture artifact changed")
	}
	var f fixture
	if e = json.Unmarshal(data, &f); e != nil {
		t.Fatal(e)
	}
	// This is an owner-specification overlay, not a newly supplied golden.
	overlay, e := os.ReadFile("testdata/wire-v2-spec.json")
	if e != nil {
		t.Fatal(e)
	}
	var spec struct {
		WireVersion uint64 `json:"wireVersion"`
		Revision    int64  `json:"pepPolicyRevision"`
		Mode        string `json:"authorizationMode"`
	}
	if e = json.Unmarshal(overlay, &spec); e != nil || spec.WireVersion != 2 || spec.Revision != 7 || spec.Mode != "user" {
		t.Fatal("invalid owner-specification overlay")
	}
	f.Wire.Request.WireVersion, f.Wire.Request.PEPPolicyRevision = spec.WireVersion, spec.Revision
	f.Wire.Response["wireVersion"], _ = json.Marshal(spec.WireVersion)
	f.Wire.Response["pepPolicyRevision"], _ = json.Marshal(spec.Revision)
	return f
}

func catalogFixture(t *testing.T, f fixture, revision int64) map[string]json.RawMessage {
	t.Helper()
	catalog := make(map[string]json.RawMessage, len(f.Wire.Catalog)+2)
	for k, v := range f.Wire.Catalog {
		catalog[k] = v
	}
	catalog["authorizationMode"] = json.RawMessage(`"user"`)
	catalog["pepPolicyRevision"], _ = json.Marshal(revision)
	now := time.Now().UTC()
	catalog["issuedAt"], _ = json.Marshal(now)
	catalog["expiresAt"], _ = json.Marshal(now.Add(30 * time.Second))
	return catalog
}
func setup(t *testing.T, base string) (*Client, fixture, Binding, Credential) {
	t.Helper()
	f := load(t)
	w := f.Wire.Request
	b := Binding{CapabilityID: w.CapabilityID, CapabilityVersion: w.CapabilityVersion, ContractDigest: w.ContractDigest, ActionID: "provider.item.read", ResourceType: w.ResourceType, ResourceID: w.ResourceID, ResourceEvidence: w.ResourceEvidence}
	file := filepath.Join(t.TempDir(), "app-key")
	if e := os.WriteFile(file, []byte("fixture-app-key\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := New(Config{BaseURL: base, AllowPrivateHTTP: true, AppKeyFile: file, OrganizationID: "org1", ProviderID: w.ProviderID, PEPClientID: w.PEPClientID, CatalogDigest: w.CatalogDigest, CatalogGeneration: w.CatalogGeneration, Operations: map[string]Binding{"read": b}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.http.CloseIdleConnections)
	return c, f, b, Credential{f.HashPreimages.Credential.Auth, w.ProviderCredential.Subject, w.ProviderCredential.Audience}
}
func rebase(m map[string]json.RawMessage, now time.Time) {
	m["issuedAt"], _ = json.Marshal(now)
	m["expiresAt"], _ = json.Marshal(now.Add(5 * time.Second))
}
func snapshot(f fixture) ActualSnapshot {
	w := f.Wire.Request
	return ActualSnapshot{Method: w.Method, CanonicalPath: w.CanonicalPath, CanonicalQuery: w.CanonicalQuery, ForwardedHeaderDigest: w.ForwardedHeaderDigest, BodyDigest: w.BodyDigest, BodyLength: w.BodyLength}
}
func TestGolden(t *testing.T) {
	c, f, b, cred := setup(t, "http://127.0.0.1:1")
	for key, preimage := range map[string]string{"bodyDigest": f.HashPreimages.Body, "canonicalQueryDigest": f.HashPreimages.Query, "forwardedHeaderDigest": f.HashPreimages.Headers, "catalogDigest": f.HashPreimages.Catalog, "contractDigest": f.HashPreimages.Contract} {
		if Digest([]byte(preimage)) != f.ExpectedHashes[key] {
			t.Fatal(key)
		}
	}
	if CredentialDigest(cred.Authorization) != f.ExpectedHashes["providerCredentialDigest"] {
		t.Fatal("credential domain/length hash")
	}
	request := c.request(b, snapshot(f), cred, f.Wire.Request.RequestBinding)
	request.PEPPolicyRevision = 7 // Check obtains this from the active catalog; DTO test only.
	got, _ := json.Marshal(request)
	want, _ := json.Marshal(f.Wire.Request)
	if !bytes.Equal(got, want) {
		t.Fatalf("wire mismatch: %s", got)
	}
	now := time.Now().UTC()
	rebase(f.Wire.Response, now)
	data, _ := json.Marshal(f.Wire.Response)
	if !validateResponse(data, f.Wire.Request, b.ActionID, now) {
		t.Fatal("golden allow rejected")
	}
}

// Revision authority comes from each active catalog, not a static config pin.
func TestActivePolicyRevisionBindsResponse(t *testing.T) {
	for _, revision := range []int64{7, 11} {
		t.Run(fmt.Sprint(revision), func(t *testing.T) {
			f := load(t)
			now := time.Now().UTC()
			rebase(f.Wire.Response, now)
			requestJSON, err := json.Marshal(f.Wire.Request)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(requestJSON, &fields); err != nil {
				t.Fatal(err)
			}
			fields["pepPolicyRevision"], _ = json.Marshal(revision)
			requestJSON, _ = json.Marshal(fields)
			var request wireRequest
			if err := json.Unmarshal(requestJSON, &request); err != nil {
				t.Fatal(err)
			}
			// Marshal back independently: silently ignoring the new DTO field must fail.
			encoded, _ := json.Marshal(request)
			var roundTrip map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(roundTrip["pepPolicyRevision"], fields["pepPolicyRevision"]) {
				t.Fatal("request DTO lost active policy revision")
			}
			f.Wire.Response["pepPolicyRevision"] = fields["pepPolicyRevision"]
			valid, _ := json.Marshal(f.Wire.Response)
			if !validateResponse(valid, request, "provider.item.read", now) {
				t.Fatal("matching nonconstant active revision rejected")
			}
			for _, bad := range []string{"null", "0", "-1", "1", fmt.Sprint(18 - revision), `"7"`, "7.5", "true", "{}", "[]"} {
				f.Wire.Response["pepPolicyRevision"] = json.RawMessage(bad)
				data, _ := json.Marshal(f.Wire.Response)
				if validateResponse(data, request, "provider.item.read", now) {
					t.Fatalf("stale or malformed revision accepted: %s", bad)
				}
			}
			delete(f.Wire.Response, "pepPolicyRevision")
			missing, _ := json.Marshal(f.Wire.Response)
			if validateResponse(missing, request, "provider.item.read", now) {
				t.Fatal("missing policy revision accepted")
			}
		})
	}
}

func TestEveryResponseFieldFailsClosed(t *testing.T) {
	_, f, b, _ := setup(t, "http://127.0.0.1:1")
	now := time.Now().UTC()
	rebase(f.Wire.Response, now)
	for key := range f.Wire.Response {
		t.Run(key, func(t *testing.T) {
			m := map[string]json.RawMessage{}
			for k, v := range f.Wire.Response {
				m[k] = v
			}
			m[key] = json.RawMessage(`null`)
			data, _ := json.Marshal(m)
			if validateResponse(data, f.Wire.Request, b.ActionID, now) {
				t.Fatal("null accepted")
			}
			delete(m, key)
			data, _ = json.Marshal(m)
			if validateResponse(data, f.Wire.Request, b.ActionID, now) {
				t.Fatal("missing field accepted")
			}
			m[key] = json.RawMessage(`"mismatch"`)
			if key == "reason" {
				m[key] = json.RawMessage(`""`)
			}
			data, _ = json.Marshal(m)
			if validateResponse(data, f.Wire.Request, b.ActionID, now) {
				t.Fatal("mutation accepted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(map[string]json.RawMessage)
	}{
		{"deny", func(m map[string]json.RawMessage) { m["allowed"] = json.RawMessage(`false`) }},
		{"expired", func(m map[string]json.RawMessage) { m["expiresAt"], _ = json.Marshal(now) }},
		{"future", func(m map[string]json.RawMessage) { m["issuedAt"], _ = json.Marshal(now.Add(time.Second)) }},
		{"long lifetime", func(m map[string]json.RawMessage) { m["expiresAt"], _ = json.Marshal(now.Add(6 * time.Second)) }},
		{"unknown", func(m map[string]json.RawMessage) { m["freshnessProof"] = json.RawMessage(`true`) }},
		{"uppercase principal", func(m map[string]json.RawMessage) { m["principalDigest"], _ = json.Marshal(strings.Repeat("A", 64)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := map[string]json.RawMessage{}
			for k, v := range f.Wire.Response {
				m[k] = v
			}
			test.change(m)
			data, _ := json.Marshal(m)
			if validateResponse(data, f.Wire.Request, b.ActionID, now) {
				t.Fatal("accepted")
			}
		})
	}
	data, _ := json.Marshal(f.Wire.Response)
	for _, suffix := range []string{`,"allowed":true}`, `,"Allowed":true}`, `,"ALLOWED":true}`} {
		bad := append(append([]byte{}, data[:len(data)-1]...), []byte(suffix)...)
		if validateResponse(bad, f.Wire.Request, b.ActionID, now) {
			t.Fatal("duplicate accepted")
		}
	}
	for _, bad := range [][]byte{append(data, []byte(` {}`)...), []byte(`[]`), []byte(`{`)} {
		if validateResponse(bad, f.Wire.Request, b.ActionID, now) {
			t.Fatal("malformed accepted")
		}
	}
}
func TestTransport(t *testing.T) {
	f := load(t)
	var nonces = map[string]bool{}
	calls, catalogs := 0, 0
	revisions := []int64{7, 11, 9223372036854775807}
	var active int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/http-capabilities/catalog/provider" {
			if r.Method != "GET" || r.Header.Get("X-Grant-App-Auth-Key") != "fixture-app-key" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Organization-ID") != "" || r.Header.Get("Cookie") != "" || catalogs != calls || catalogs >= len(revisions) {
				t.Error("catalog method/key/original-human leak/order")
				w.WriteHeader(403)
				return
			}
			active = revisions[catalogs]
			catalogs++
			json.NewEncoder(w).Encode(catalogFixture(t, f, active))
			return
		}
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/v2/http-capabilities/decide" || r.Header.Get("Authorization") != f.HashPreimages.Credential.Auth || r.Header.Get("X-Organization-ID") != "org1" || r.Header.Get("X-Grant-App-Auth-Key") != "fixture-app-key" {
			t.Error("transport headers/path")
		}
		data, _ := io.ReadAll(r.Body)
		if bytes.Contains(data, []byte(f.HashPreimages.Credential.Auth)) {
			t.Error("raw credential in body")
		}
		var request wireRequest
		if e := json.Unmarshal(data, &request); e != nil {
			t.Error(e)
		}
		if request.WireVersion != 2 || request.PEPPolicyRevision != active || calls != catalogs {
			t.Error("decide did not use this Check's exact active catalog revision")
		}
		fields, ok := strictObject(data)
		if !ok || len(fields) != 22 || fields["actionId"] != nil || fields["authorizationMode"] != nil {
			t.Error("wire2 request has missing or extra fields")
		}
		n, e := base64.RawURLEncoding.DecodeString(request.RequestBinding)
		if e != nil || len(n) != 24 || nonces[request.RequestBinding] {
			t.Error("nonce")
		}
		nonces[request.RequestBinding] = true
		response := load(t).Wire.Response
		for key := range response {
			if value, ok := fields[key]; ok {
				response[key] = value
			}
		}
		rebase(response, time.Now().UTC())
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	c, _, _, cred := setup(t, server.URL)
	for i := 0; i < 2; i++ {
		if e := c.Check(context.Background(), "read", snapshot(f), cred); e != nil {
			t.Fatal(e)
		}
	}
	dynamic := snapshot(f)
	dynamic.Method = "GET"
	dynamic.CanonicalPath = "/runs/run-2/messages"
	dynamic.CanonicalQuery = "session_key=hello%20world"
	dynamic.BodyDigest = Digest([]byte("new chat message"))
	dynamic.BodyLength = int64(len("new chat message"))
	dynamic.ForwardedHeaderDigest = Digest([]byte("reviewed header preimage"))
	if err := c.Check(context.Background(), "read", dynamic, cred); err != nil {
		t.Fatal("dynamic snapshot: ", err)
	}
	bad := snapshot(f)
	bad.BodyLength = -1
	if c.Check(context.Background(), "read", bad, cred) != ErrDenied {
		t.Fatal("snapshot mismatch")
	}
	if c.Check(context.Background(), "unknown", snapshot(f), cred) != ErrDenied {
		t.Fatal("unknown operation")
	}
	if calls != 3 || catalogs != 3 {
		t.Fatal("invalid input reached PDP")
	}
}
func TestCatalogAndDecisionFailClosed(t *testing.T) {
	for _, mode := range []string{"valid", "future-catalog-issued", "missing-revision", "zero-revision", "negative-revision", "string-revision", "float-revision", "overflow-revision", "expired", "wrong-mode", "wrong-provider", "wrong-pep", "wrong-generation", "wrong-digest", "disabled", "wrong-contract", "wrong-action", "wrong-version", "missing-capability", "stale-response", "catalog-decide-race"} {
		t.Run(mode, func(t *testing.T) {
			f := load(t)
			gets, decides := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/api/v2/http-capabilities/catalog/provider" {
					gets++
					if r.Header.Get("X-Grant-App-Auth-Key") != "fixture-app-key" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Organization-ID") != "" {
						t.Error("catalog must use app key only")
					}
					catalog := catalogFixture(t, f, 11)
					switch mode {
					case "future-catalog-issued":
						catalog["issuedAt"], _ = json.Marshal(time.Now().Add(time.Second))
					case "missing-revision":
						delete(catalog, "pepPolicyRevision")
					case "zero-revision":
						catalog["pepPolicyRevision"] = json.RawMessage(`0`)
					case "negative-revision":
						catalog["pepPolicyRevision"] = json.RawMessage(`-1`)
					case "string-revision":
						catalog["pepPolicyRevision"] = json.RawMessage(`"11"`)
					case "float-revision":
						catalog["pepPolicyRevision"] = json.RawMessage(`11.0`)
					case "overflow-revision":
						catalog["pepPolicyRevision"] = json.RawMessage(`9223372036854775808`)
					case "expired":
						catalog["expiresAt"], _ = json.Marshal(time.Now().Add(-time.Second))
					case "wrong-mode":
						catalog["authorizationMode"] = json.RawMessage(`"app"`)
					case "wrong-provider":
						catalog["providerId"] = json.RawMessage(`"foreign-provider"`)
					case "wrong-pep":
						catalog["pepClientId"] = json.RawMessage(`"foreign-pep"`)
					case "wrong-generation":
						catalog["generation"] = json.RawMessage(`2`)
					case "wrong-digest":
						catalog["catalogDigest"], _ = json.Marshal(Digest([]byte("different catalog")))
					case "missing-capability":
						catalog["capabilities"] = json.RawMessage(`[]`)
					case "disabled", "wrong-contract", "wrong-action", "wrong-version":
						var caps []map[string]json.RawMessage
						if err := json.Unmarshal(catalog["capabilities"], &caps); err != nil || len(caps) != 1 {
							t.Error("invalid test capability")
							w.WriteHeader(403)
							return
						}
						switch mode {
						case "disabled":
							caps[0]["enabled"] = json.RawMessage(`false`)
						case "wrong-contract":
							caps[0]["contractDigest"], _ = json.Marshal(Digest([]byte("different contract")))
						case "wrong-action":
							caps[0]["actionId"] = json.RawMessage(`"foreign.action"`)
						case "wrong-version":
							caps[0]["version"] = json.RawMessage(`2`)
						}
						catalog["capabilities"], _ = json.Marshal(caps)
					}
					json.NewEncoder(w).Encode(catalog)
					return
				}
				decides++
				if r.Method != "POST" || r.URL.Path != "/api/v2/http-capabilities/decide" {
					t.Error("unexpected transport")
				}
				if mode == "catalog-decide-race" {
					w.WriteHeader(403)
					return
				}
				var request wireRequest
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.PEPPolicyRevision != 11 {
					t.Error("active revision not used")
				}
				response := load(t).Wire.Response
				response["pepPolicyRevision"] = json.RawMessage(`11`)
				if mode == "stale-response" {
					response["pepPolicyRevision"] = json.RawMessage(`7`)
				}
				response["requestBinding"], _ = json.Marshal(request.RequestBinding)
				rebase(response, time.Now().UTC())
				json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			c, _, _, cred := setup(t, server.URL)
			err := c.Check(t.Context(), "read", snapshot(f), cred)
			valid := mode == "valid" || mode == "future-catalog-issued"
			if (err == nil) != valid || gets != 1 {
				t.Fatalf("mode=%s error=%v catalog calls=%d", mode, err, gets)
			}
			wantDecides := 0
			if valid || mode == "stale-response" || mode == "catalog-decide-race" {
				wantDecides = 1
			}
			if decides != wantDecides {
				t.Fatalf("decide calls=%d want=%d (no retries or decisions from invalid catalogs)", decides, wantDecides)
			}
		})
	}
}

func TestCatalogNestedObjectsRejectAmbiguousValues(t *testing.T) {
	c, f, b, _ := setup(t, "http://127.0.0.1:1")
	catalog := catalogFixture(t, f, 7)
	valid, _ := json.Marshal(catalog)
	if revision, err := c.validateCatalog(valid, b, "GET", time.Now()); err != nil || revision != 7 {
		t.Fatalf("positive catalog control: revision=%d error=%v", revision, err)
	}
	for _, mode := range []string{"query-duplicate-key", "path-duplicate-key", "request-null-header", "response-null-header", "path-tamper", "query-tamper", "request-tamper", "response-tamper", "extractor-tamper"} {
		t.Run(mode, func(t *testing.T) {
			catalog := catalogFixture(t, f, 7)
			var capabilities []map[string]json.RawMessage
			if err := json.Unmarshal(catalog["capabilities"], &capabilities); err != nil || len(capabilities) != 1 {
				t.Fatal("invalid fixture capability")
			}
			capability := capabilities[0]
			switch mode {
			case "query-duplicate-key":
				capability["query"] = json.RawMessage(`{"mode":"exact","parameters":[{"name":"item","name":"item","format":"asciiToken","required":true,"repeated":false}]}`)
			case "path-duplicate-key":
				capability["pathParameters"] = json.RawMessage(`[{"name":"id","name":"id","format":"asciiToken","maxBytes":256}]`)
			case "request-null-header":
				capability["request"] = json.RawMessage(`{"body":"none","maxBytes":0,"forwardHeaders":[null]}`)
			case "response-null-header":
				capability["response"] = json.RawMessage(`{"maxBytes":32,"forwardHeaders":[null],"redirects":"deny"}`)
			case "path-tamper":
				capability["pathTemplate"] = json.RawMessage(`"/other/{id}"`)
			case "query-tamper":
				capability["query"] = json.RawMessage(`{"mode":"exact","parameters":[{"name":"item","format":"asciiToken","required":true,"repeated":false}]}`)
			case "request-tamper":
				capability["request"] = json.RawMessage(`{"body":"none","maxBytes":1,"forwardHeaders":[]}`)
			case "response-tamper":
				capability["response"] = json.RawMessage(`{"maxBytes":33,"forwardHeaders":[],"redirects":"deny"}`)
			case "extractor-tamper":
				capability["resource"] = json.RawMessage(`{"type":"item","id":{"kind":"pathSegment","name":"other","format":"asciiToken"}}`)
			}
			catalog["capabilities"], _ = json.Marshal(capabilities)
			data, _ := json.Marshal(catalog)
			if _, err := c.validateCatalog(data, b, "GET", time.Now()); err != ErrDenied {
				t.Fatalf("ambiguous nested catalog accepted: %v", err)
			}
		})
	}
}

func TestOwnerInventoryContractVector(t *testing.T) {
	data, err := os.ReadFile("testdata/inventory-contract-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Expected   string          `json:"expectedContractDigest"`
		Preimage   string          `json:"canonicalPreimage"`
		Capability json.RawMessage `json:"capability"`
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}
	const expected = "sha256:dff00647c46093e40e72139bb379208b85bc7572d1371188d1a34b2d69aff1df"
	if vector.Expected != expected || fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(vector.Preimage))) != expected {
		t.Fatal("independent supplier vector/preimage changed")
	}
	if got, err := selectedContractDigest(vector.Capability); err != nil || got != expected {
		t.Fatalf("supplier canonical vector: digest=%s error=%v", got, err)
	}
}

func TestSelectedContractCanonicalPreimages(t *testing.T) {
	c, f, b, _ := setup(t, "http://127.0.0.1:1")
	base := f.HashPreimages.Contract // Retained independent literal, not encoder output.
	for _, tc := range []struct {
		name     string
		preimage string
	}{
		{"original", base},
		{"array-order", strings.Replace(base, `"response":{"forwardHeaders":[]`, `"response":{"forwardHeaders":["x-b","x-a"]`, 1)},
		{"large-exact-integer", strings.Replace(base, `"maxBytes":32`, `"maxBytes":9007199254740993`, 1)},
		{"uint64-maximum", strings.Replace(base, `"maxBytes":32`, `"maxBytes":18446744073709551615`, 1)},
		{"request-null-headers", strings.Replace(base, `"request":{"body":"none","forwardHeaders":[]`, `"request":{"body":"none","forwardHeaders":null`, 1)},
		{"response-null-headers", strings.Replace(base, `"response":{"forwardHeaders":[]`, `"response":{"forwardHeaders":null`, 1)},
		{"unicode-separators", strings.Replace(base, `"pathTemplate":`, "\"organizationContext\":\"\u2028\u2029\",\"pathTemplate\":", 1)},
		{"literal-unicode-escape", strings.Replace(base, `"pathTemplate":`, `"organizationContext":"\\u2028\\u2029","pathTemplate":`, 1)},
		{"no-html-escape", strings.Replace(base, `"pathTemplate":`, `"organizationContext":"<>&","pathTemplate":`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Only SHA-256 is computed here; canonical bytes are independently literal.
			pin := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(tc.preimage)))
			if tc.name == "original" && pin != "sha256:e24b65bb4c4e37a170ed8fabcf94bab2111861b1b6d220395d5274f22f0bdf42" {
				t.Fatal("retained canonical vector changed")
			}
			if tc.name != "original" && pin == "sha256:e24b65bb4c4e37a170ed8fabcf94bab2111861b1b6d220395d5274f22f0bdf42" {
				t.Fatal("different canonical values must retain distinct digests")
			}
			binding := b
			binding.ContractDigest = pin
			// Reorder nested keys in input without changing the literal expected pin.
			input := strings.Replace(tc.preimage, `{"format":"asciiToken","kind":"pathSegment","name":"id"}`, `{"name":"id","kind":"pathSegment","format":"asciiToken"}`, 1)
			capability := input[:len(input)-1] + `,"contractDigest":"` + pin + `"}`
			catalog := catalogFixture(t, f, 7)
			catalog["capabilities"] = json.RawMessage("[" + capability + "]")
			data, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			if revision, err := c.validateCatalog(data, binding, "GET", time.Now()); err != nil || revision != 7 {
				t.Fatalf("independent preimage rejected: revision=%d error=%v", revision, err)
			}
			// Valid JSON representation escapes must decode to the same UTF-8 string.
			if tc.name == "unicode-separators" {
				escaped := strings.ReplaceAll(strings.ReplaceAll(capability, "\u2028", `\u2028`), "\u2029", `\u2029`)
				catalog["capabilities"] = json.RawMessage("[" + escaped + "]")
				data, _ := json.Marshal(catalog)
				if _, err := c.validateCatalog(data, binding, "GET", time.Now()); err != nil {
					t.Fatal("equivalent Unicode JSON escape changed digest")
				}
			}
			// Semantically valid changes must fail with the original reviewed hash.
			changed := strings.Replace(capability, `"maxBytes":0`, `"maxBytes":1`, 1)
			catalog["capabilities"] = json.RawMessage("[" + changed + "]")
			data, _ = json.Marshal(catalog)
			if _, err := c.validateCatalog(data, binding, "GET", time.Now()); err != ErrDenied {
				t.Fatal("modified content accepted under unchanged contract digest")
			}
		})
	}
}

func TestTransportFailuresNoSecrets(t *testing.T) {
	for _, mode := range []string{"deny", "redirect", "oversize", "malformed", "timeout", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			redirectCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectCalls++ }))
			defer target.Close()
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, 302)
				case "oversize":
					io.WriteString(w, strings.Repeat(" ", 32769))
				case "malformed":
					io.WriteString(w, `{"allowed":true}`)
				case "deny":
					io.WriteString(w, `{"allowed":false,"reason":"denied"}`)
				case "timeout":
					select {
					case <-r.Context().Done():
					case <-release:
					}
				default:
					w.WriteHeader(503)
					io.WriteString(w, "fixture-app-key")
				}
			}))
			defer server.Close()
			defer close(release)
			c, f, _, cred := setup(t, server.URL)
			if mode == "unavailable" {
				server.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			e := c.Check(ctx, "read", snapshot(f), cred)
			if e != ErrDenied || strings.Contains(e.Error(), cred.Authorization) || strings.Contains(e.Error(), "fixture-app-key") {
				t.Fatal("failure or secret leakage")
			}
			if redirectCalls != 0 {
				t.Fatal("redirect followed")
			}
		})
	}
}
func TestConfigValidationAndCopy(t *testing.T) {
	c, _, b, _ := setup(t, "http://127.0.0.1:1")
	base := c.config
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"remote HTTP", func(c *Config) { c.BaseURL = "http://example.com" }},
		{"HTTP opt in", func(c *Config) { c.AllowPrivateHTTP = false }},
		{"URL user", func(c *Config) { c.BaseURL = "https://user@example.com" }},
		{"URL path", func(c *Config) { c.BaseURL = "https://example.com/path" }},
		{"URL query", func(c *Config) { c.BaseURL = "https://example.com?x=y" }},
		{"missing generation", func(c *Config) { c.CatalogGeneration = 0 }},
		{"bad digest", func(c *Config) { c.CatalogDigest = "sha256:" + strings.Repeat("A", 64) }},
		{"missing operations", func(c *Config) { c.Operations = nil }},
		{"bad version", func(c *Config) { x := b; x.CapabilityVersion = 0; c.Operations = map[string]Binding{"read": x} }},
		{"inconsistent evidence", func(c *Config) {
			x := b
			x.ResourceEvidence.CanonicalValue = "other"
			c.Operations = map[string]Binding{"read": x}
		}},
		{"bad key", func(c *Config) { c.PEPClientID = "bad\nkey" }},
		{"bad app file", func(c *Config) { c.AppKeyFile = "/nonexistent/app-key" }},
		{"timeout", func(c *Config) { c.Timeout = 6 * time.Second }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			if _, e := New(cfg); e == nil {
				t.Fatal("bad config accepted")
			}
		})
	}
	cfg := base
	client, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	delete(cfg.Operations, "read")
	if len(client.config.Operations) != 1 {
		t.Fatal("bindings were not copied")
	}
}

func TestSnapshotFieldsAndTLS(t *testing.T) {
	c, f, b, cred := setup(t, "http://127.0.0.1:1")
	snapshots := []ActualSnapshot{snapshot(f), snapshot(f), snapshot(f), snapshot(f), snapshot(f), snapshot(f)}
	snapshots[0].Method = "POST"
	snapshots[1].CanonicalPath = "/items/other"
	snapshots[2].CanonicalQuery = "x=1"
	snapshots[3].BodyDigest = Digest([]byte("x"))
	snapshots[4].ForwardedHeaderDigest = Digest([]byte("x"))
	snapshots[5].BodyLength = 1
	for _, s := range snapshots {
		if !validSnapshot(s) {
			t.Fatal("dynamic snapshot rejected")
		}
		request := c.request(b, s, cred, f.Wire.Request.RequestBinding)
		request.PEPPolicyRevision = 7
		now := time.Now().UTC()
		rebase(f.Wire.Response, now)
		data, _ := json.Marshal(f.Wire.Response)
		if validateResponse(data, request, b.ActionID, now) {
			t.Fatal("stale snapshot echo allowed")
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS reached handler") }))
	defer server.Close()
	c, f, _, cred = setup(t, server.URL)
	if c.Check(context.Background(), "read", snapshot(f), cred) != ErrDenied {
		t.Fatal("untrusted certificate accepted")
	}
}
func TestSingleCaseAliasRejected(t *testing.T) {
	_, f, b, _ := setup(t, "http://127.0.0.1:1")
	now := time.Now().UTC()
	rebase(f.Wire.Response, now)
	for key := range f.Wire.Response {
		m := map[string]json.RawMessage{}
		for k, v := range f.Wire.Response {
			m[k] = v
		}
		delete(m, key)
		m[strings.ToUpper(key)] = f.Wire.Response[key]
		data, _ := json.Marshal(m)
		if validateResponse(data, f.Wire.Request, b.ActionID, now) {
			t.Fatalf("case alias %s", key)
		}
	}
}

func TestCanonicalQuerySpaces(t *testing.T) {
	s := snapshot(load(t))
	for query, want := range map[string]bool{"session_key=hello%20world": true, "session_key=hello+world": false, "session_key=a%2Bb": true, "session_key=a%20%2B%20b": true, "session_key=%ZZ": false} {
		s.CanonicalQuery = query
		if validSnapshot(s) != want {
			t.Errorf("query %q: want valid=%v", query, want)
		}
	}
}
func TestConstantEvidence(t *testing.T) {
	c, _, b, _ := setup(t, "http://127.0.0.1:1")
	b.ResourceEvidence.Kind = "constant"
	b.ResourceEvidence.Name = ""
	cfg := c.config
	cfg.Operations = map[string]Binding{"read": b}
	client, err := New(cfg)
	if err != nil {
		t.Fatal("registered constant with empty name: ", err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	raw, err := json.Marshal(b.ResourceEvidence)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["name"]; present {
		t.Fatalf("constant resourceEvidence must omit empty name, got %s", raw)
	}
	b.ResourceEvidence.Kind = "pathSegment"
	cfg.Operations = map[string]Binding{"read": b}
	if _, err := New(cfg); err == nil {
		t.Fatal("empty pathSegment name accepted")
	}
	b.ResourceEvidence.Kind = "constant"
	b.ResourceEvidence.CanonicalValue = "mismatch"
	cfg.Operations = map[string]Binding{"read": b}
	if _, err := New(cfg); err == nil {
		t.Fatal("constant resource mismatch accepted")
	}
}
