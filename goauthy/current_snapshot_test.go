package goauthy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCurrentAPIKeySnapshotPinsDeliveredVersion(t *testing.T) {
	var calls atomic.Int32
	var version atomic.Int64
	version.Store(7)
	var mismatched atomic.Bool
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		provider := "model"
		if mismatched.Load() {
			provider = "other"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": fmt.Sprintf("secret-%d", version.Load()), "grant_id": "grant", "provider_id": provider, "connection_generation": "gen", "credential_version": version.Load(), "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()})
	}))
	defer issuer.Close()
	var got string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization"); w.WriteHeader(204) }))
	defer provider.Close()
	binding := APIKeyBinding{GrantID: "grant", ProviderID: "model", ConnectionGeneration: "gen", ConnectorDigest: "digest", Header: "Authorization", Prefix: "Bearer ", Endpoint: provider.URL + "/chat"}
	source := func(context.Context) (string, error) { return "human", nil }
	client, v, e := ResolveCurrentAPIKey(context.Background(), issuer.URL, binding, source, issuer.Client(), provider.Client())
	if e != nil || v != 7 || calls.Load() != 1 {
		t.Fatalf("snapshot version=%d calls=%d err=%v", v, calls.Load(), e)
	}
	version.Store(8)
	req, _ := http.NewRequest("POST", binding.Endpoint, nil)
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if got != "Bearer secret-7" || calls.Load() != 1 {
		t.Fatal("snapshot changed after model boundary")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", client.Transport, client.Transport), "secret-7") {
		t.Fatal("snapshot formats a secret")
	}
	req, _ = http.NewRequest("POST", binding.Endpoint+"?escape=true", nil)
	if _, e = client.Do(req); e == nil {
		t.Fatal("unsafe destination allowed")
	}
	for _, bad := range []int64{0, -1} {
		version.Store(bad)
		if _, _, e = ResolveCurrentAPIKey(context.Background(), issuer.URL, binding, source, issuer.Client(), provider.Client()); e == nil {
			t.Fatal("invalid version accepted")
		}
	}
	version.Store(8)
	mismatched.Store(true)
	if _, _, e = ResolveCurrentAPIKey(context.Background(), issuer.URL, binding, source, issuer.Client(), provider.Client()); e == nil {
		t.Fatal("wrong binding accepted")
	}
	binding.CredentialVersion = 7
	if _, _, e = ResolveCurrentAPIKey(context.Background(), issuer.URL, binding, source, issuer.Client(), provider.Client()); e == nil {
		t.Fatal("ambiguous pinned/current mode")
	}
}

func TestSnapshotUsesDefaultTransportWhenBaseClientIsNil(t *testing.T) {
	// Exercise the nil transport produced by clone(nil), with a local HTTP peer
	// to avoid installing test trust in the process-wide default HTTPS transport.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing snapshot credential")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL+"/chat", nil)
	base := clone(nil)
	client := &http.Client{Transport: &apiKeySnapshotTransport{destination: &apiKeyTransport{endpoint: req.URL, binding: APIKeyBinding{Header: "Authorization", Prefix: "Bearer "}, provider: base.Transport}, key: "secret", expires: time.Now().Add(time.Minute).UnixMilli()}}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}
