package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mrchypark/beesuh/auth"
	"golang.org/x/oauth2"
)

func TestOAuthClientsAreResolvedPerUserAndPersistBeforeCallingModel(t *testing.T) {
	var mu sync.Mutex
	persisted := map[string]int{}
	var seen []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer token-")
		mu.Lock()
		seen = append(seen, user)
		if persisted[user] != 1 {
			t.Errorf("unpersisted account reached model: %q", user)
		}
		mu.Unlock()
		completion(w, "ok", "stop")
	}))
	defer backend.Close()
	clients := map[string]*http.Client{}
	for _, user := range []string{"alice", "bob"} {
		client, err := auth.OAuthClient(nil, func(context.Context) (*oauth2.Token, error) {
			return &oauth2.Token{AccessToken: "token-" + user, RefreshToken: "refresh-" + user}, nil
		}, func(_ context.Context, token *oauth2.Token) error {
			mu.Lock()
			defer mu.Unlock()
			if token.RefreshToken != "refresh-"+user {
				t.Error("wrong refresh account")
			}
			persisted[user]++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		clients[user] = client
	}
	opts := nativeOptions(t.TempDir(), "", backend.URL, nil)
	opts.RequireIdentity = true
	opts.Authorize = func(context.Context, Principal) error { return nil }
	opts.Native.Provider.APIKey = "static-key-must-not-be-used"
	opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		return Credential{HTTPClient: clients[c.UserID], AccountID: "account-" + c.UserID}, nil
	}
	r := nativeOpen(t, opts)
	defer r.Close()
	for _, user := range []string{"alice", "bob"} {
		if err := r.RegisterConnection(context.Background(), Connection{ID: user, UserID: user, WorkspaceID: "w", Kind: "provider", TargetID: "default", CredentialRef: "ref-" + user, State: "ready"}); err != nil {
			t.Fatal(err)
		}
		ctx := WithPrincipal(context.Background(), Principal{UserID: user, WorkspaceID: "w"})
		for _, id := range []string{"1", "2"} {
			if _, err := r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "same", RequestID: id, ConnectionID: user, Prompt: Prompt{Text: "hello"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mu.Lock()
	if strings.Join(seen, ",") != "alice,alice,bob,bob" {
		t.Fatalf("account routing = %v", seen)
	}
	mu.Unlock()
	rows, err := r.query(context.Background(), `SELECT payload FROM journal`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row[0].(string), "token-alice") || strings.Contains(row[0].(string), "refresh-bob") {
			t.Fatal("credential reached journal")
		}
	}
	data, err := json.Marshal(Credential{HTTPClient: clients["alice"], AccountID: "private-account"})
	if err != nil || string(data) != "{}" {
		t.Fatalf("credential JSON=%s err=%v", data, err)
	}
}

func TestOnlyAuthenticationRejectionInvalidatesConnection(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		resolverError error
		want          error
		state         string
	}{
		{name: "invalid", status: 401, want: ErrConnectionUnavailable, state: "needs_login"},
		{name: "forbidden", status: 403, want: ErrForbidden, state: "ready"},
		{name: "rate", status: 429, want: auth.ErrRateLimited, state: "ready"},
		{name: "server", status: 503, want: auth.ErrUnavailable, state: "ready"},
		{name: "store", resolverError: errors.New("secret-store-password-must-not-escape"), want: auth.ErrUnavailable, state: "ready"},
		{name: "cancel", resolverError: context.Canceled, want: context.Canceled, state: "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("upstream-secret"))
			}))
			defer backend.Close()
			opts := nativeOptions(t.TempDir(), "", backend.URL, nil)
			opts.RequireIdentity = true
			opts.Authorize = func(context.Context, Principal) error { return nil }
			executing := false
			opts.ResolveCredential = func(context.Context, Connection) (Credential, error) {
				if executing && tc.resolverError != nil {
					return Credential{}, tc.resolverError
				}
				return Credential{APIKey: "key", AccountID: "account"}, nil
			}
			r := nativeOpen(t, opts)
			defer r.Close()
			if err := r.RegisterConnection(context.Background(), Connection{ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "default", CredentialRef: "ref", State: "ready"}); err != nil {
				t.Fatal(err)
			}
			executing = true
			ctx := WithPrincipal(context.Background(), Principal{UserID: "u", WorkspaceID: "w"})
			_, err := r.Run(ctx, Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", ConnectionID: "c", Prompt: Prompt{Text: "hello"}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("secret leaked: %v", err)
			}
			list, err := r.Connections(ctx)
			if err != nil || len(list) != 1 || list[0].State != tc.state {
				t.Fatalf("state=%+v err=%v", list, err)
			}
			if tc.resolverError != nil && calls != 0 {
				t.Fatal("resolver failure reached upstream")
			}
		})
	}
}
