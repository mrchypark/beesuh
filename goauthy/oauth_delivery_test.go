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

func TestOAuthClientRotatesAndRejectsRollback(t *testing.T) {
	now := time.Unix(100, 0)
	var issuerCalls, providerCalls atomic.Int32
	versions := []int64{1<<62 + 1, 1<<62 + 2, 1<<62 + 1}
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(issuerCalls.Add(1)) - 1
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer human" || r.Header.Get("Cookie") != "" {
			t.Error("unsafe OAuth delivery request")
			return
		}
		json.NewEncoder(w).Encode(oauthFixture(now, versions[index], "account", []string{"read"}))
	}))
	defer issuer.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Error("wrong provider token")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()

	var got OAuthTokenRequest
	binding := oauthBinding(provider.URL)
	client, err := NewOAuthClient(issuer.URL, binding, func(_ context.Context, request OAuthTokenRequest) (string, error) { got = request; return "human", nil }, issuer.Client(), provider.Client(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	binding.Scopes[0] = "write" // The caller must not widen this client's copy.
	call := func() error {
		request, _ := http.NewRequest(http.MethodPost, provider.URL+"/chat", nil)
		request.Header.Set("Authorization", "Bearer caller")
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		return err
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if err := call(); err == nil {
		t.Fatal("credential version rollback accepted")
	}
	if got != (OAuthTokenRequest{Resource: "https://beesuh.local.test", ConsumerClientID: "beesuh"}) {
		t.Fatalf("source request = %#v", got)
	}
	if issuerCalls.Load() != 3 || providerCalls.Load() != 2 {
		t.Fatalf("calls issuer=%d provider=%d", issuerCalls.Load(), providerCalls.Load())
	}
}

func TestOAuthClientRejectsChangedOrMalformedDelivery(t *testing.T) {
	now := time.Unix(100, 0)
	var providerCalls atomic.Int32
	mode := ""
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "status" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if mode == "duplicate" {
			raw, _ := json.Marshal(oauthFixture(now, 1, "account", []string{"read"}))
			w.Write(append(raw[:len(raw)-1], []byte(`,"kind":"oauth2"}`)...))
			return
		}
		value := oauthFixture(now, 1, "account", []string{"read"})
		switch mode {
		case "account":
			value["account_id"] = "other"
		case "grant":
			value["grant_id"] = "other"
		case "provider":
			value["provider_id"] = "other"
		case "generation":
			value["connection_generation"] = "other"
		case "scope":
			value["scopes"] = []string{"write"}
		case "duplicate-scope":
			value["scopes"] = []string{"read", "read"}
		case "expired":
			value["token_expires_at_unix_ms"] = now.UnixMilli()
		case "consent-expired":
			value["consent_expires_at_unix_ms"] = now.UnixMilli()
		case "zero-expiry":
			value["token_expires_at_unix_ms"] = 0
		case "fractional-version":
			value["credential_version"] = 1.5
		case "token-type":
			value["token_type"] = "bearer"
		case "kind":
			value["kind"] = "api_key"
		case "extra":
			value["refresh_token"] = "never-returned"
		case "null":
			value["access_token"] = nil
		case "missing":
			delete(value, "scopes")
		}
		json.NewEncoder(w).Encode(value)
	}))
	defer issuer.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { providerCalls.Add(1) }))
	defer provider.Close()
	client, err := NewOAuthClient(issuer.URL, oauthBinding(provider.URL), func(context.Context, OAuthTokenRequest) (string, error) { return "human", nil }, issuer.Client(), provider.Client(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"account", "grant", "provider", "generation", "scope", "duplicate-scope", "expired", "consent-expired", "zero-expiry", "fractional-version", "token-type", "kind", "extra", "null", "missing", "duplicate", "status"} {
		mode = test
		request, _ := http.NewRequest(http.MethodPost, provider.URL+"/chat", nil)
		_, err := client.Do(request)
		if err == nil || strings.Contains(err.Error(), "access") {
			t.Fatalf("%s: unsafe result %v", test, err)
		}
		if test == "status" {
			var status auth.HTTPError
			if !errors.As(err, &status) || status.StatusCode != http.StatusConflict {
				t.Fatalf("status = %v", err)
			}
		}
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("provider calls = %d", providerCalls.Load())
	}
}

func TestOAuthClientBlocksSourceFailureAndRedirects(t *testing.T) {
	now := time.Unix(100, 0)
	var providerCalls, redirectCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectCalls.Add(1) }))
	defer target.Close()
	redirect := atomic.Bool{}
	redirect.Store(true)
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			http.Redirect(w, r, target.URL, http.StatusFound)
			return
		}
		json.NewEncoder(w).Encode(oauthFixture(now, 1, "account", []string{"read"}))
	}))
	defer issuer.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer provider.Close()
	sourceFailure := atomic.Bool{}
	client, err := NewOAuthClient(issuer.URL, oauthBinding(provider.URL), func(context.Context, OAuthTokenRequest) (string, error) {
		if sourceFailure.Load() {
			return "", errors.New("token failure")
		}
		return "human", nil
	}, issuer.Client(), provider.Client(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	call := func() (*http.Response, error) {
		request, _ := http.NewRequest(http.MethodPost, provider.URL+"/chat", nil)
		return client.Do(request)
	}
	if _, err := call(); err == nil {
		t.Fatal("issuer redirect accepted")
	}
	if providerCalls.Load() != 0 || redirectCalls.Load() != 0 {
		t.Fatal("issuer redirect dispatched")
	}
	redirect.Store(false)
	sourceFailure.Store(true)
	if _, err := call(); err == nil {
		t.Fatal("source failure accepted")
	}
	if providerCalls.Load() != 0 {
		t.Fatal("source failure dispatched")
	}
	sourceFailure.Store(false)
	response, err := call()
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if providerCalls.Load() != 1 || redirectCalls.Load() != 0 {
		t.Fatal("provider redirect followed")
	}
	sourceFailure.Store(true)
	if _, err := call(); err == nil {
		t.Fatal("source failure reused a cached access token")
	}
	if providerCalls.Load() != 1 {
		t.Fatal("source failure dispatched with a cached access token")
	}
}

func TestOAuthClientHonorsContextWhileWaitingForGate(t *testing.T) {
	now := time.Unix(100, 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode(oauthFixture(now, 1, "account", []string{"read"}))
	}))
	defer issuer.Close()
	provider := httptest.NewTLSServer(http.NotFoundHandler())
	defer provider.Close()
	client, err := NewOAuthClient(issuer.URL, oauthBinding(provider.URL), func(context.Context, OAuthTokenRequest) (string, error) { return "human", nil }, issuer.Client(), provider.Client(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, provider.URL+"/chat", nil)
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		firstDone <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL+"/chat", nil)
	if _, err := client.Do(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("gate cancellation = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestOAuthClientRechecksConsentBeforeDispatch(t *testing.T) {
	now := time.Unix(100, 0)
	var clockCalls, providerCalls atomic.Int32
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := oauthFixture(now, 1, "account", []string{"read"})
		value["token_expires_at_unix_ms"] = now.Add(time.Hour).UnixMilli()
		json.NewEncoder(w).Encode(value)
	}))
	defer issuer.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { providerCalls.Add(1) }))
	defer provider.Close()
	client, err := NewOAuthClient(issuer.URL, oauthBinding(provider.URL), func(context.Context, OAuthTokenRequest) (string, error) { return "human", nil }, issuer.Client(), provider.Client(), func() time.Time {
		if clockCalls.Add(1) >= 3 {
			return now.Add(time.Minute)
		}
		return now
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, provider.URL+"/chat", nil)
	if _, err := client.Do(request); err == nil || providerCalls.Load() != 0 {
		t.Fatal("expired consent dispatched model")
	}
}

func oauthBinding(endpoint string) OAuthBinding {
	return OAuthBinding{GrantID: "grant", ProviderID: "provider", ConnectionGeneration: "generation", AccountID: "account", Resource: "https://beesuh.local.test", ConsumerClientID: "beesuh", Endpoint: endpoint + "/chat", Scopes: []string{"read"}, CredentialVersion: 1}
}

func oauthFixture(now time.Time, version int64, account string, scopes []string) map[string]any {
	return map[string]any{"kind": "oauth2", "access_token": "access", "token_type": "Bearer", "grant_id": "grant", "provider_id": "provider", "connection_generation": "generation", "credential_version": version, "account_id": account, "scopes": scopes, "token_expires_at_unix_ms": now.Add(time.Minute).UnixMilli(), "consent_expires_at_unix_ms": now.Add(time.Minute).UnixMilli()}
}
