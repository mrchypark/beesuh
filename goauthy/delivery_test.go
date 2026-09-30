package goauthy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/auth"
)

func TestAPIKeyClientFetchesPerRequestAndDoesNotForwardBearer(t *testing.T) {
	var issuerCalls, providerCalls atomic.Int32
	response := func(key, grant string) map[string]any {
		return map[string]any{"kind": "api_key", "api_key": key, "grant_id": grant, "provider_id": "model", "connection_generation": "gen", "credential_version": int64(1<<62 + 9), "connector_digest": "digest", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()}
	}
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuerCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer human" {
			t.Error("unsafe delivery request")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/one/credential") {
			json.NewEncoder(w).Encode(response("key-one", "one"))
			return
		}
		json.NewEncoder(w).Encode(response("key-two", "two"))
	}))
	defer issuer.Close()
	model := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer key-one" && got != "Bearer key-two" {
			t.Error("credential was not isolated")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer model.Close()

	makeClient := func(grant string) *http.Client {
		client, err := NewAPIKeyClient(issuer.URL, APIKeyBinding{GrantID: grant, ProviderID: "model", ConnectionGeneration: "gen", CredentialVersion: 1<<62 + 9, ConnectorDigest: "digest", Header: "Authorization", Prefix: "Bearer ", Endpoint: model.URL + "/chat"}, func(context.Context) (string, error) { return "human", nil }, issuer.Client(), model.Client())
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	for _, client := range []*http.Client{makeClient("one"), makeClient("two")} {
		req, _ := http.NewRequest(http.MethodPost, model.URL+"/chat", nil)
		req.Header.Set("Authorization", "Bearer caller-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if issuerCalls.Load() != 2 || providerCalls.Load() != 2 {
		t.Fatalf("calls issuer=%d provider=%d", issuerCalls.Load(), providerCalls.Load())
	}
}

func TestAPIKeyClientRejectsDeliveryFailuresBeforeProvider(t *testing.T) {
	var providerCalls atomic.Int32
	bad := ""
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bad == "status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		value := map[string]any{"kind": "api_key", "api_key": "key", "grant_id": "grant", "provider_id": "model", "connection_generation": "gen", "credential_version": 7, "connector_digest": "digest", "header": "Authorization", "prefix": "", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()}
		switch bad {
		case "provider":
			value["provider_id"] = "other"
		case "generation":
			value["connection_generation"] = "other"
		case "digest":
			value["connector_digest"] = "other"
		case "version":
			value["credential_version"] = 8
		case "prefix":
			value["prefix"] = "x"
		case "header":
			value["header"] = "X-API-Key"
		case "expired":
			value["consent_expires_at_unix_ms"] = time.Now().Add(-time.Second).UnixMilli()
		case "kind":
			value["kind"] = "oauth"
		case "missing-prefix":
			delete(value, "prefix")
		case "null-prefix":
			value["prefix"] = nil
		}
		json.NewEncoder(w).Encode(value)
	}))
	defer issuer.Close()
	model := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { providerCalls.Add(1) }))
	defer model.Close()
	client, err := NewAPIKeyClient(issuer.URL, APIKeyBinding{GrantID: "grant", ProviderID: "model", ConnectionGeneration: "gen", CredentialVersion: 7, ConnectorDigest: "digest", Header: "Authorization", Endpoint: model.URL + "/chat"}, func(context.Context) (string, error) { return "human", nil }, issuer.Client(), model.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"provider", "generation", "digest", "version", "prefix", "header", "expired", "kind", "missing-prefix", "null-prefix", "status"} {
		bad = test
		req, _ := http.NewRequest(http.MethodPost, model.URL+"/chat", nil)
		_, err := client.Do(req)
		if err == nil || strings.Contains(err.Error(), "key") {
			t.Fatalf("%s: unsafe error %v", test, err)
		}
		if test == "status" {
			var status auth.HTTPError
			if !errors.As(err, &status) || status.StatusCode != 404 {
				t.Fatalf("status = %v", err)
			}
		}
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("provider calls = %d", providerCalls.Load())
	}
}

func TestAPIKeyClientHasNoCachedKeyFallback(t *testing.T) {
	var providerCalls atomic.Int32
	state := "ok"
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state == "unavailable" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": "key", "grant_id": "grant", "provider_id": "model", "connection_generation": "gen", "credential_version": 1, "connector_digest": "digest", "header": "Authorization", "prefix": "", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()})
	}))
	defer issuer.Close()
	model := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { providerCalls.Add(1) }))
	defer model.Close()
	client, err := NewAPIKeyClient(issuer.URL, APIKeyBinding{GrantID: "grant", ProviderID: "model", ConnectionGeneration: "gen", CredentialVersion: 1, ConnectorDigest: "digest", Header: "Authorization", Endpoint: model.URL + "/chat"}, func(context.Context) (string, error) {
		if state == "source-error" {
			return "", errors.New("token unavailable")
		}
		return "human", nil
	}, issuer.Client(), model.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := func() error {
		req, _ := http.NewRequest(http.MethodPost, model.URL+"/chat", nil)
		response, err := client.Do(req)
		if response != nil {
			response.Body.Close()
		}
		return err
	}
	if err := request(); err != nil {
		t.Fatal(err)
	}
	state = "unavailable"
	if err := request(); err == nil {
		t.Fatal("delivery 503 used a cached key")
	}
	state = "source-error"
	if err := request(); err == nil {
		t.Fatal("source error used a cached key")
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider calls = %d", providerCalls.Load())
	}
}

func TestAPIKeyClientDoesNotFollowIssuerOrProviderRedirects(t *testing.T) {
	var redirectCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectCalls.Add(1) }))
	defer target.Close()
	var issuerRedirect atomic.Bool
	issuerRedirect.Store(true)
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if issuerRedirect.Load() {
			http.Redirect(w, r, target.URL, http.StatusFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": "key", "grant_id": "grant", "provider_id": "model", "connection_generation": "gen", "credential_version": 1, "connector_digest": "digest", "header": "Authorization", "prefix": "", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()})
	}))
	defer issuer.Close()
	modelCalls := atomic.Int32{}
	model := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer model.Close()
	binding := APIKeyBinding{GrantID: "grant", ProviderID: "model", ConnectionGeneration: "gen", CredentialVersion: 1, ConnectorDigest: "digest", Header: "Authorization", Endpoint: model.URL + "/chat"}
	client, err := NewAPIKeyClient(issuer.URL, binding, func(context.Context) (string, error) { return "human", nil }, issuer.Client(), model.Client())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, model.URL+"/chat", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("issuer redirect accepted")
	}
	if modelCalls.Load() != 0 || redirectCalls.Load() != 0 {
		t.Fatalf("issuer redirect dispatched model=%d target=%d", modelCalls.Load(), redirectCalls.Load())
	}

	issuerRedirect.Store(false)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if modelCalls.Load() != 1 || redirectCalls.Load() != 0 {
		t.Fatalf("provider redirect followed model=%d target=%d", modelCalls.Load(), redirectCalls.Load())
	}
}

func TestAPIKeyClientRejectsUnsafeDestination(t *testing.T) {
	issuer := httptest.NewTLSServer(http.NotFoundHandler())
	defer issuer.Close()
	model := httptest.NewTLSServer(http.NotFoundHandler())
	defer model.Close()
	client, err := NewAPIKeyClient(issuer.URL, APIKeyBinding{GrantID: "g", ProviderID: "p", ConnectionGeneration: "gen", CredentialVersion: 1, ConnectorDigest: "d", Header: "Authorization", Endpoint: model.URL + "/fixed"}, func(context.Context) (string, error) { return "human", nil }, issuer.Client(), model.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{model.URL + "/other", model.URL + "/fixed?q=1"} {
		req, _ := http.NewRequest(http.MethodPost, raw, nil)
		if _, err := client.Do(req); err == nil {
			t.Fatal("unsafe destination accepted")
		}
	}
}
