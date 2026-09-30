package beesuh

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/goauthy"
)

func deliveryRuntime(t *testing.T, endpoint string, clients map[string]*http.Client, bindings map[string]goauthy.APIKeyBinding) *Runtime {
	t.Helper()
	opts := nativeOptions(t.TempDir(), "", "", nil)
	opts.Native.Provider.BaseURL = strings.TrimSuffix(endpoint, "/chat/completions")
	opts.RequireIdentity = true
	opts.Authorize = func(context.Context, Principal) error { return nil }
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		// Logical binding identity, not an upstream ChatGPT account ID.
		return Credential{HTTPClient: clients[c.ID], AccountID: digest(bindings[c.ID])}, nil
	}
	r := nativeOpen(t, opts)
	t.Cleanup(func() { _ = r.Close() })
	for id := range clients {
		if err := r.RegisterConnection(context.Background(), Connection{ID: id, UserID: "owner", WorkspaceID: "workspace", Kind: "provider", TargetID: "default", CredentialRef: id, State: "ready"}); err != nil {
			t.Fatal("profile registration failed")
		}
	}
	return r
}

func deliveryRun(r *Runtime, id, requestID string) (Result, error) {
	ctx, cancel := context.WithTimeout(WithPrincipal(context.Background(), Principal{UserID: "owner", WorkspaceID: "workspace"}), 15*time.Second)
	defer cancel()
	return r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: id, RequestID: id + "-" + requestID, ConnectionID: id, Prompt: Prompt{Text: "identify fixture profile"}})
}

func TestGoAuthyDeliveryProfiles(t *testing.T) {
	var calls, deliveries atomic.Int32
	var denied atomic.Bool
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer dummy-key-")
		if key != "personal" && key != "work" {
			t.Error("wrong profile credential reached model")
			w.WriteHeader(401)
			return
		}
		completion(w, key, "stop")
	}))
	defer provider.Close()
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries.Add(1)
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/v1/connection-grants/"), "/credential")
		if r.Header.Get("Authorization") != "Bearer dummy-human-"+id {
			t.Error("wrong human token")
			w.WriteHeader(401)
			return
		}
		if denied.Load() && id == "personal" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "api_key", "api_key": "dummy-key-" + id, "grant_id": id, "provider_id": "fixture", "connection_generation": "opaque-" + id, "credential_version": int64(9007199254740993), "connector_digest": "reviewed", "header": "Authorization", "prefix": "Bearer ", "consent_expires_at_unix_ms": time.Now().Add(time.Minute).UnixMilli()})
	}))
	defer issuer.Close()
	clients := map[string]*http.Client{}
	bindings := map[string]goauthy.APIKeyBinding{}
	for _, id := range []string{"personal", "work"} {
		b := goauthy.APIKeyBinding{GrantID: id, ProviderID: "fixture", ConnectionGeneration: "opaque-" + id, CredentialVersion: 9007199254740993, ConnectorDigest: "reviewed", Header: "Authorization", Prefix: "Bearer ", Endpoint: provider.URL + "/v1/chat/completions"}
		client, err := goauthy.NewAPIKeyClient(issuer.URL, b, func(context.Context) (string, error) { return "dummy-human-" + id, nil }, issuer.Client(), provider.Client())
		if err != nil {
			t.Fatal(err)
		}
		clients[id], bindings[id] = client, b
	}
	r := deliveryRuntime(t, bindings["personal"].Endpoint, clients, bindings)
	t.Run("parallel selected accounts", func(t *testing.T) {
		for _, id := range []string{"personal", "work"} {
			t.Run(id, func(t *testing.T) {
				t.Parallel()
				for _, request := range []string{"one", "two"} {
					out, err := deliveryRun(r, id, request)
					if err != nil || out.State != "succeeded" || !strings.Contains(string(out.Output), `"text":"`+id+`"`) {
						t.Fatalf("selected profile did not complete: state=%s err=%v output=%s", out.State, err, out.Output)
					}
				}
			})
		}
	})
	if calls.Load() != 4 || deliveries.Load() != 4 {
		t.Fatal("each model use must fetch a credential")
	}
	denied.Store(true)
	if _, err := deliveryRun(r, "personal", "revoked"); err == nil {
		t.Fatal("revoked grant was used")
	}
	if calls.Load() != 4 || deliveries.Load() != 5 {
		t.Fatal("revocation must stop dispatch without cache fallback")
	}
	if _, err := deliveryRun(r, "work", "still-ready"); err != nil {
		t.Fatal("other account was affected")
	}
	rows, err := r.query(context.Background(), `SELECT payload FROM journal`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row[0].(string), "dummy-key-") || strings.Contains(row[0].(string), "dummy-human-") {
			t.Fatal("credential persisted")
		}
	}
}

// The producer owns a disposable actual issuer and synthetic model, never SaaS.
func TestGoAuthyDeliveryLiveE2E(t *testing.T) {
	if os.Getenv("BEESUH_E2E_GOAUTHY_DELIVERY") != "1" {
		t.Skip("actual GoAuthy delivery fixture not configured; NOT VERIFIED")
	}
	var fixture struct {
		Issuer     string `json:"issuer"`
		IssuerCA   string `json:"issuer_ca_file"`
		ProviderCA string `json:"provider_ca_file"`
		Profiles   []struct {
			ID            string                `json:"connection_id"`
			TokenFile     string                `json:"token_file"`
			Binding       goauthy.APIKeyBinding `json:"binding"`
			Expected      string                `json:"expected_text"`
			ExpectedError bool                  `json:"expected_error"`
		} `json:"profiles"`
	}
	if json.Unmarshal(deliveryPrivateFile(t, os.Getenv("BEESUH_E2E_GOAUTHY_DELIVERY_FIXTURE_FILE")), &fixture) != nil || len(fixture.Profiles) < 2 {
		t.Fatal("invalid delivery fixture")
	}
	loopback := func(raw string) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
			t.Fatal("pilot requires loopback HTTPS")
		}
	}
	loopback(fixture.Issuer)
	client := func(path string) *http.Client {
		pem, err := os.ReadFile(path)
		roots := x509.NewCertPool()
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			t.Fatal("invalid fixture CA")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: 15 * time.Second}
	}
	issuerClient, providerClient := client(fixture.IssuerCA), client(fixture.ProviderCA)
	clients := map[string]*http.Client{}
	bindings := map[string]goauthy.APIKeyBinding{}
	endpoint := fixture.Profiles[0].Binding.Endpoint
	for _, p := range fixture.Profiles {
		loopback(p.Binding.Endpoint)
		if p.ID == "" || clients[p.ID] != nil || (!p.ExpectedError && p.Expected == "") || p.Binding.Endpoint != endpoint || !strings.HasSuffix(endpoint, "/chat/completions") {
			t.Fatal("invalid selected profile fixture")
		}
		c, err := goauthy.NewAPIKeyClient(fixture.Issuer, p.Binding, func(context.Context) (string, error) {
			return strings.TrimSpace(string(deliveryPrivateFile(t, p.TokenFile))), nil
		}, issuerClient, providerClient)
		if err != nil {
			t.Fatal("invalid fixture binding")
		}
		clients[p.ID], bindings[p.ID] = c, p.Binding
	}
	r := deliveryRuntime(t, endpoint, clients, bindings)
	for _, p := range fixture.Profiles {
		for _, request := range []string{"first", "fresh-delivery"} {
			out, err := deliveryRun(r, p.ID, request)
			if p.ExpectedError {
				if err == nil || out.State == "succeeded" {
					t.Fatal("rejected profile unexpectedly completed")
				}
				continue
			}
			var response struct {
				Text string `json:"text"`
			}
			if err != nil || json.Unmarshal(out.Output, &response) != nil || response.Text != p.Expected {
				t.Fatal("actual issuer selected-profile model completion failed")
			}
		}
	}
}

func deliveryPrivateFile(t *testing.T, path string) []byte {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		t.Fatal("invalid private fixture file")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) || info.Mode().Perm() != 0600 {
		t.Fatal("fixture changed")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(after, info) {
		t.Fatal("fixture changed")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		t.Fatal("fixture too large or unreadable")
	}
	return raw
}
