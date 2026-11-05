package main

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

func TestPersonalFixtureCredentialBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, path, method, token, body, extra string
		want                                   int
	}{
		{"userinfo rejects provider key", "/userinfo", "GET", fakeKey, "", "", 400},
		{"metadata rejects provider key", "/auth/v1/connection-collections", "GET", fakeKey, "", "", 400},
		{"introspection rejects provider key in body", "/introspect", "POST", "", "token=" + fakeKey, "", 400},
		{"provider rejects key in payload", "/inference/go/openai/v1/chat/completions", "POST", fakeKey, `{"model":"` + fakeModel + `","key":"` + fakeKey + `"}`, "", 400},
		{"provider rejects human bearer", "/inference/go/openai/v1/chat/completions", "POST", "smoke-access-token", `{"model":"` + fakeModel + `"}`, "", 400},
		{"provider rejects human token in payload", "/inference/go/openai/v1/chat/completions", "POST", fakeKey, `{"model":"` + fakeModel + `","message":"smoke-access-token"}`, "", 400},
		{"provider rejects client secret header", "/inference/go/openai/v1/chat/completions", "POST", fakeKey, `{"model":"` + fakeModel + `"}`, "smoke-client-secret", 400},
		{"provider rejects extra key header", "/inference/go/openai/v1/chat/completions", "POST", fakeKey, `{"model":"` + fakeModel + `"}`, fakeKey, 400},
		{"provider rejects wrong model", "/inference/go/openai/v1/chat/completions", "POST", fakeKey, `{"model":"wrong"}`, "", 400},
		{"model success", "/inference/go/openai/v1/chat/completions", "POST", fakeKey, `{"model":"` + fakeModel + `"}`, "", 200},
		{"credential delivery", "/auth/v1/connection-grants/approved/credential", "POST", "smoke-access-token", "", "", 200},
		{"credential delivery rejects body", "/auth/v1/connection-grants/approved/credential", "POST", "smoke-access-token", "{}", "", 400},
		{"unapproved grant", "/auth/v1/connections/models/personal/grants/unapproved", "GET", "smoke-access-token", "", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := fixtureHandler()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.extra != "" {
				req.Header.Set("X-Extra", tc.extra)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Fatalf("status=%d want=%d", resp.Code, tc.want)
			}
			if tc.want == 200 && strings.Contains(tc.path, "/credential") {
				var delivery struct {
					Key string `json:"api_key"`
				}
				if err := json.Unmarshal(resp.Body.Bytes(), &delivery); err != nil || delivery.Key != fakeKey {
					t.Fatal("missing fake credential")
				}
			} else if bytes.Contains(resp.Body.Bytes(), []byte(fakeKey)) {
				t.Fatal("key exposed by non-delivery response")
			}
			statsResp := httptest.NewRecorder()
			handler.ServeHTTP(statsResp, httptest.NewRequest("GET", "/smoke/stats", nil))
			var stats fixtureStats
			if err := json.Unmarshal(statsResp.Body.Bytes(), &stats); err != nil {
				t.Fatal(err)
			}
			if (stats.Violations != 0) != (tc.want == 400) {
				t.Fatalf("boundary violations=%d", stats.Violations)
			}
		})
	}
}

func TestGrantFixtureSnapshotStableUntilRevocation(t *testing.T) {
	handler := fixtureHandler()
	read := func() []byte {
		t.Helper()
		request := httptest.NewRequest("GET", "/auth/v1/connections/models/personal/grants/approved", nil)
		request.Header.Set("Authorization", "Bearer smoke-access-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("grant metadata status=%d", response.Code)
		}
		return response.Body.Bytes()
	}
	first := read()
	// Cross the fixture's millisecond timestamp precision before the second read.
	time.Sleep(5 * time.Millisecond)
	if !bytes.Equal(first, read()) {
		t.Fatal("unchanged consent metadata changed between reads; Bee must reject that snapshot before key delivery")
	}
	request := httptest.NewRequest("POST", "/smoke/consent", strings.NewReader(`{"revoked":true}`))
	request.Header.Set("Authorization", "Bearer "+fixtureControl)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("fixture revocation failed")
	}
	var before, after struct {
		Grant struct {
			Expires int64 `json:"expires_at_unix_ms"`
			Revoked bool  `json:"revoked"`
		} `json:"grant"`
	}
	if json.Unmarshal(first, &before) != nil || json.Unmarshal(read(), &after) != nil || before.Grant.Expires == 0 || before.Grant.Expires != after.Grant.Expires || before.Grant.Revoked || !after.Grant.Revoked {
		t.Fatal("revocation must change authority explicitly without changing the recorded expiry")
	}
}

func TestBlockingModelStopsOnClientCancellation(t *testing.T) {
	server := httptest.NewServer(fixtureHandler())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/inference/go/openai/v1/chat/completions", strings.NewReader(`{"model":"`+fakeModel+`","messages":[{"content":"`+blockMarker+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+fakeKey)
	done := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	stats := func() fixtureStats {
		t.Helper()
		resp, err := server.Client().Get(server.URL + "/smoke/stats")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out fixtureStats
		if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for !stats().Active {
		if time.Now().After(deadline) {
			t.Fatal("model never active")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("provider returned before cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("client did not cancel")
	}
	for stats().Active {
		if time.Now().After(deadline) {
			t.Fatal("provider did not observe cancellation")
		}
		time.Sleep(time.Millisecond)
	}
	if out := stats(); out.Models != 1 || out.Violations != 0 {
		t.Fatalf("stats=%+v", out)
	}
}

func TestCancelResponseHasNoReadData(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"request_id":"r","state":"running","cancellation_requested":true}`, true},
		{`{"request_id":"r","state":"running","cancellation_requested":true,"output":"private"}`, false},
		{`{"request_id":"r","state":"running","cancellation_requested":true,"error":"private"}`, false},
		{`{"request_id":"r","state":"running","cancellation_requested":false}`, false},
		{`{"request_id":"r","state":"cancelled","cancellation_requested":true}`, false},
	} {
		if err := checkCancel([]byte(tc.body), "r", "running", true); (err == nil) != tc.valid {
			t.Fatalf("valid=%t err=%v", tc.valid, err)
		}
	}
	if err := checkCancel([]byte(`{"request_id":"r","state":"succeeded","cancellation_requested":false}`), "r", "succeeded", false); err != nil {
		t.Fatal(err)
	}
	if err := checkCancel([]byte(`{"request_id":"r","state":"succeeded"}`), "r", "succeeded", false); err == nil {
		t.Fatal("accepted absent cancellation_requested boolean")
	}
	if err := checkCancel([]byte(`{"request_id":"r","state":"succeeded","cancellation_requested":null}`), "r", "succeeded", false); err == nil {
		t.Fatal("accepted null cancellation_requested")
	}
	if err := checkCancel([]byte(`{"request_id":"r","state":"succeeded","output":"private"}`), "r", "succeeded", false); err == nil {
		t.Fatal("accepted output instead of cancellation_requested")
	}
}

func TestScanFindsSecretsAcrossChunks(t *testing.T) {
	for _, secret := range fixtureSecrets() {
		dir := t.TempDir()
		path := filepath.Join(dir, "binary-db")
		data := append(bytes.Repeat([]byte{0}, (64<<10)-4), []byte(secret)...)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := scan([]string{dir}); err == nil {
			t.Fatal("persisted credential not detected")
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte("validated fake credential delivery"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := scan([]string{dir}); err != nil {
		t.Fatal(err)
	}
}

func TestGrantCredentialGoldenHash(t *testing.T) {
	// Exact original Grant wire-v1 preimage, not a real signed credential.
	authorization := "Bearer eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyLTEiLCJjbGllbnRfaWQiOiJjbGllbnQtMSJ9.c2lnbmF0dXJl"
	if fixtureCredentialDigest(authorization) != "sha256:328d64177ede33a5cf466892b8c77fe095c7f64c35175dc765757a04d6265b84" {
		t.Fatal("fixture drifted from the domain/length-prefixed Grant credential digest")
	}
	if serviceAudience == grantHintAudience || serviceAudience == resource || grantHintAudience == resource || serviceAudience == "beesuh-smoke" || grantHintAudience == "beesuh-smoke" {
		t.Fatal("fixture collapsed independent client/audience/resource/hint values")
	}
}

func TestOperationForwardedHeaderDigests(t *testing.T) {
	// Hashes independently computed from the exact lower-sorted LF preimages.
	const selectors = "sha256:63b6196fc43c2567e4e3105d288c284ff24f4109146a3322f2b8d8d2279a9774"
	const jsonHeaders = "sha256:093bc306da9e5a608f43c3ecf837898fb96159a87a29f31c9c3b34334069c9d4"
	const charsetHeaders = "sha256:a1d2b89749f3f14a7a44d8a1bbba664c5946c0df36b55d60750d2f86e06cb8aa"
	for _, tc := range []struct {
		operation    string
		contentTypes []string
		want         string
	}{
		{"run_execute", []string{"application/json"}, jsonHeaders},
		{"connection_select", []string{"application/json; charset=utf-8"}, charsetHeaders},
		{"run_read", nil, selectors},
		{"history_read", []string{"text/plain"}, selectors},
		{"run_cancel", []string{"application/json; charset=utf-8"}, selectors},
		{"connection_revoke", []string{"application/json"}, selectors},
		{"run_execute", nil, ""},
		{"connection_select", []string{"application/json", "application/json"}, ""},
		{"run_execute", []string{"text/plain"}, ""},
		{"connection_select", []string{"application/json; broken"}, ""},
	} {
		got, valid := fixtureHeaderDigest(tc.operation, tc.contentTypes)
		if got != tc.want || valid != (tc.want != "") {
			t.Fatalf("%s forward-list digest/validation mismatch", tc.operation)
		}
	}
	spaced, _ := fixtureHeaderDigest("run_execute", []string{"application/json; charset=utf-8"})
	compact, valid := fixtureHeaderDigest("run_execute", []string{"application/json;charset=utf-8"})
	if !valid || spaced == compact {
		t.Fatal("raw Content-Type spacing was normalized before hashing")
	}
}

type fixtureRoundTrip func(*http.Request) (*http.Response, error)

func (fn fixtureRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestSharedExpectationUsesOriginalContentType(t *testing.T) {
	const raw = "application/json; Charset=\"utf-8\""
	calls := 0
	client := &http.Client{Transport: fixtureRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if r.URL.Path != "/smoke/expect" || r.Header.Get("Content-Type") != "application/json" {
				t.Fatal("wrong fixture control transport")
			}
			var expectation fixtureExpectation
			if err := json.NewDecoder(r.Body).Decode(&expectation); err != nil || len(expectation.ContentTypes) != 1 || expectation.ContentTypes[0] != raw {
				t.Fatal("control expectation lost the original Bee HTTP Content-Type")
			}
		} else if calls != 2 || r.URL.Path != "/v1/chat" || r.Header.Get("Content-Type") != raw {
			t.Fatal("business transport changed the original raw Content-Type")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	f := flow{context.Background(), client, "http://fixture-bee.invalid"}
	_, err := f.sharedRequestContentTypes("POST", "/v1/chat", `{"workspace_id":"smoke-shared"}`, "smoke-access-token", sharedOrganization, sharedWorkspace, "allow", []string{raw}, 200)
	if err != nil || calls != 2 {
		t.Fatalf("original Content-Type binding check: calls=%d error=%v", calls, err)
	}
}

func TestFixturePDPRejectsUnboundSnapshots(t *testing.T) {
	for _, mutate := range []string{"valid", "bodyDigest", "bodyLength", "canonicalPath", "canonicalQueryDigest", "forwardedHeaderDigest", "providerCredentialDigest", "catalogDigest", "contractDigest", "resourceEvidence", "unsupported-header-kind", "wrong-resource", "nonmember", "requestBinding", "original-bearer", "hint"} {
		t.Run(mutate, func(t *testing.T) {
			policy := newFixturePolicy()
			body := `{"workspace_id":"smoke-shared","message":"independent bytes"}`
			policy.expected = fixtureExpectation{Method: "POST", Path: "/v1/chat", Body: body, Token: "smoke-access-token", Mode: "allow", ContentTypes: []string{"application/json; charset=utf-8"}}
			wire := map[string]any{
				"bodyDigest": fixtureDigest([]byte(body)), "bodyLength": len(body), "canonicalPath": "/v1/chat", "canonicalQuery": "",
				"canonicalQueryDigest": fixtureDigest(nil), "forwardedHeaderDigest": "sha256:a1d2b89749f3f14a7a44d8a1bbba664c5946c0df36b55d60750d2f86e06cb8aa", "method": "POST",
				"capabilityId": "bee.run_execute", "capabilityVersion": 1, "catalogGeneration": 1, "catalogDigest": fixtureCatalog, "contractDigest": fixtureContract,
				"pepClientId": "bee-smoke-pep", "providerId": "bee-smoke-provider", "providerCredentialDigest": fixtureCredentialDigest("Bearer smoke-access-token"),
				"providerCredential": map[string]string{"subject": "smoke-user", "audience": grantHintAudience},
				"resourceId":         sharedWorkspace, "resourceType": "workspace", "resourceEvidence": map[string]string{"canonicalValue": sharedWorkspace, "kind": "constant", "name": ""},
				"requestBinding": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 24)),
			}
			switch mutate {
			case "valid", "original-bearer":
			case "hint":
				wire["providerCredential"] = map[string]string{"subject": "smoke-user", "audience": serviceAudience}
			case "unsupported-header-kind":
				wire["resourceEvidence"] = map[string]string{"canonicalValue": sharedWorkspace, "kind": "header", "name": "x-workspace-id"}
			case "wrong-resource":
				wire["resourceId"] = "another-workspace"
				wire["resourceEvidence"] = map[string]string{"canonicalValue": "another-workspace", "kind": "constant", "name": ""}
			case "nonmember":
				policy.expected.Token = "smoke-nonmember-token"
				wire["providerCredential"] = map[string]string{"subject": "nonmember-user", "audience": grantHintAudience}
				wire["providerCredentialDigest"] = fixtureCredentialDigest("Bearer smoke-nonmember-token")
			default:
				wire[mutate] = "wrong"
			}
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/api/v2/http-capabilities/decide", bytes.NewReader(data))
			// PDP transport JSON differs from the original Bee request's raw charset value.
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer smoke-access-token")
			if mutate == "nonmember" {
				request.Header.Set("Authorization", "Bearer smoke-nonmember-token")
			}
			request.Header.Set("X-Organization-ID", sharedOrganization)
			request.Header.Set("X-Grant-App-Auth-Key", fixtureAppKey)
			if mutate == "original-bearer" {
				request.Header.Set("Authorization", "Bearer another-token")
			}
			response := httptest.NewRecorder()
			policy.ServeHTTP(response, request)
			if (response.Code == 200) != (mutate == "valid") {
				t.Fatalf("snapshot %s status=%d", mutate, response.Code)
			}
			if mutate == "valid" {
				// A replayed request binding is rejected even when every digest matches.
				request.Body = io.NopCloser(bytes.NewReader(data))
				response = httptest.NewRecorder()
				policy.ServeHTTP(response, request)
				if response.Code != 403 {
					t.Fatal("reused PDP request binding accepted")
				}
			}
		})
	}
}

func TestSharedFixtureOperationMetadata(t *testing.T) {
	for _, partial := range []bool{false, true} {
		data, err := sharedFixtureConfig([]byte(`{"client_id":"beesuh-smoke"}`), partial)
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Shared struct {
				Hint     string `json:"grant_credential_hint_audience"`
				Contexts []struct {
					Workspace    string            `json:"workspace_id"`
					ResourceID   string            `json:"resource_id"`
					ResourceType string            `json:"resource_type"`
					Evidence     map[string]string `json:"resource_evidence"`
					Operations   map[string]struct {
						Action string `json:"action_id"`
					} `json:"operations"`
				} `json:"contexts"`
			} `json:"shared"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		if len(config.Shared.Contexts) != 1 || config.Shared.Hint != grantHintAudience {
			t.Fatal("shared context/hint lost")
		}
		context := config.Shared.Contexts[0]
		if context.ResourceType != "workspace" || context.ResourceID != sharedWorkspace || context.Workspace != sharedWorkspace || context.Evidence["kind"] != "constant" || context.Evidence["name"] != "" || context.Evidence["canonicalValue"] != sharedWorkspace {
			t.Fatal("single-workspace constant resource evidence lost")
		}
		_, execute := context.Operations["run_execute"]
		wantCount := 9
		if partial {
			wantCount = 8
		}
		if execute == partial || len(context.Operations) != wantCount {
			t.Fatal("partial mapping does not omit only run_execute")
		}
		for operation, pin := range context.Operations {
			want := "beesuh.run.read"
			switch operation {
			case "run_execute", "connection_select", "connection_revoke":
				want = "beesuh.run.create"
			case "run_cancel":
				want = "beesuh.run.cancel"
			}
			if pin.Action != want {
				t.Fatal("fixture action mapping drifted")
			}
		}
	}
}

func TestOwnerConfirmedCancelAction(t *testing.T) {
	// This literal is the owner's registered action, independent of fixture config/PDP generation.
	if fixtureAction("run_cancel") != "beesuh.run.cancel" {
		t.Fatal("cancel action differs from the owner's registered contract")
	}
}

func TestGeneratedOpenCodeProfileHasNoKey(t *testing.T) {
	dir := t.TempDir()
	if err := initialize(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile struct {
		Default   string `json:"default"`
		Providers []struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			Model     string `json:"model"`
			BaseURL   string `json:"base_url"`
			APIKey    string `json:"api_key"`
			APIKeyEnv string `json:"api_key_env"`
		} `json:"providers"`
	}
	if json.Unmarshal(data, &profile) != nil || len(profile.Providers) != 1 || profile.Default != "opencode" {
		t.Fatal("invalid provisional OpenCode supplier profile")
	}
	p := profile.Providers[0]
	if p.ID != "opencode" || p.Kind != "openai-compatible" || p.Model != "minimax-m3.1-flash" || p.BaseURL != issuer+"/inference/go/openai/v1" || p.APIKey != "" || p.APIKeyEnv != "" || bytes.Contains(data, []byte(fakeKey)) {
		t.Fatal("fixture changed model/profile/POST base or embedded a key")
	}
}

func TestFixtureRejectsModelGET(t *testing.T) {
	req := httptest.NewRequest("GET", "/inference/go/openai/v1/chat/completions", strings.NewReader(`{"model":"`+fakeModel+`"}`))
	req.Header.Set("Authorization", "Bearer "+fakeKey)
	response := httptest.NewRecorder()
	fixtureHandler().ServeHTTP(response, req)
	if response.Code != 400 {
		t.Fatal("model GET accepted; credential delivery must support the configured consumer POST")
	}
}

func TestEscapedResponseSecretFails(t *testing.T) {
	if err := checkFixtureSecrets([]byte(`{"value":"smoke-provider-secret-key-not-rea\u006c"}`)); err == nil {
		t.Fatal("escaped model key bypassed response check")
	}
}

func TestFlowResponseRejectsCredentialLeakAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{200, fakeKey}, {302, ""}, {200, strings.Repeat("x", (1<<20)+1)}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/other")
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		client := server.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		f := flow{context.Background(), client, server.URL}
		if _, err := f.auth("GET", "/v1/history", "", 200); err == nil {
			server.Close()
			t.Fatal("accepted unsafe response")
		}
		server.Close()
	}
}
