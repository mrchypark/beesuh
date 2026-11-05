package grantpolicy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	return f
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
	got, _ := json.Marshal(c.request(b, snapshot(f), cred, f.Wire.Request.RequestBinding))
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
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		n, e := base64.RawURLEncoding.DecodeString(request.RequestBinding)
		if e != nil || len(n) != 24 || nonces[request.RequestBinding] {
			t.Error("nonce")
		}
		nonces[request.RequestBinding] = true
		response := load(t).Wire.Response
		fields, _ := strictObject(data)
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
	dynamic.Method = "POST"
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
	if calls != 3 {
		t.Fatal("invalid input reached PDP")
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
