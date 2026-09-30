package beesuh

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

	"github.com/mrchypark/beesuh/goauthy"
)

func TestGoAuthyOAuthProfiles(t *testing.T) {
	now := time.Unix(1800000000, 0)
	var version, calls, deliveries atomic.Int64
	version.Store(1)
	var revoked atomic.Bool
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		profile := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer synthetic-oauth-")
		if profile != fmt.Sprintf("personal-v%d", version.Load()) && profile != fmt.Sprintf("work-v%d", version.Load()) {
			t.Error("wrong credential reached model")
			w.WriteHeader(401)
			return
		}
		completion(w, profile, "stop")
	}))
	defer provider.Close()
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveries.Add(1)
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/v1/connection-grants/"), "/credential")
		if r.Header.Get("Authorization") != "Bearer synthetic-human-"+id {
			t.Error("wrong consumer token reached issuer")
			w.WriteHeader(401)
			return
		}
		if revoked.Load() && id == "work" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind": "oauth2", "access_token": fmt.Sprintf("synthetic-oauth-%s-v%d", id, version.Load()), "token_type": "Bearer",
			"grant_id": id, "provider_id": "fixture", "connection_generation": "gen-" + id,
			"credential_version": version.Load(), "account_id": "account-" + id, "scopes": []string{"model.use"},
			"token_expires_at_unix_ms": now.Add(time.Hour).UnixMilli(), "consent_expires_at_unix_ms": now.Add(time.Hour).UnixMilli(),
		})
	}))
	defer issuer.Close()
	clients := map[string]*http.Client{}
	for _, id := range []string{"personal", "work"} {
		binding := goauthy.OAuthBinding{GrantID: id, ProviderID: "fixture", ConnectionGeneration: "gen-" + id, AccountID: "account-" + id, Resource: "https://fixture.test/connections", ConsumerClientID: "beesuh", Endpoint: provider.URL + "/v1/chat/completions", Scopes: []string{"model.use"}, CredentialVersion: 1}
		c, err := goauthy.NewOAuthClient(issuer.URL, binding, func(_ context.Context, request goauthy.OAuthTokenRequest) (string, error) {
			if request.Resource != binding.Resource || request.ConsumerClientID != binding.ConsumerClientID {
				t.Error("lost consumer binding")
			}
			return "synthetic-human-" + id, nil
		}, issuer.Client(), provider.Client(), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		clients[id] = c
	}
	opts := nativeOptions(t.TempDir(), "", provider.URL, nil)
	opts.RequireIdentity = true
	opts.Authorize = func(context.Context, Principal) error { return nil }
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		// Retain the same client to preserve its observed version high-water mark.
		return Credential{HTTPClient: clients[c.ID], AccountID: "account-" + c.ID}, nil
	}
	r := nativeOpen(t, opts)
	defer r.Close()
	for id := range clients {
		if err := r.RegisterConnection(context.Background(), Connection{ID: id, UserID: "owner", WorkspaceID: "workspace", Kind: "provider", TargetID: "default", CredentialRef: id, State: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []int64{1, 2} {
		version.Store(v)
		for _, id := range []string{"personal", "work"} {
			out, err := deliveryRun(r, id, fmt.Sprintf("v%d", v))
			var response struct {
				Text string `json:"text"`
			}
			if err != nil || json.Unmarshal(out.Output, &response) != nil || response.Text != fmt.Sprintf("%s-v%d", id, v) {
				t.Fatal("selected OAuth profile failed")
			}
		}
	}
	version.Store(1)
	if _, err := deliveryRun(r, "personal", "rollback"); err == nil {
		t.Fatal("version rollback accepted")
	}
	if calls.Load() != 4 {
		t.Fatal("rollback dispatched model")
	}
	version.Store(2)
	revoked.Store(true)
	if _, err := deliveryRun(r, "work", "revoked"); err == nil {
		t.Fatal("revoked grant accepted")
	}
	if calls.Load() != 4 {
		t.Fatal("revoked grant dispatched model")
	}
	if _, err := deliveryRun(r, "personal", "unaffected"); err != nil {
		t.Fatal("other profile affected")
	}
	if calls.Load() != 5 || deliveries.Load() != 7 {
		t.Fatal("unexpected delivery or model count")
	}
	rows, err := r.query(context.Background(), `SELECT payload FROM journal`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row[0].(string), "synthetic-human-") || strings.Contains(row[0].(string), "synthetic-oauth-") {
			t.Fatal("credential persisted")
		}
	}
}
