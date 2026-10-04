package beesuh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/model"
)

// Child fixture uses only dummy credentials; no installed CLI or external account.
func TestCodexProfileRPCProcess(t *testing.T) {
	child := false
	for _, arg := range os.Args {
		if arg == "--" {
			child = true
		}
	}
	if !child {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	account := ""
	for scanner.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			os.Exit(3)
		}
		reply := func(value any) { _ = enc.Encode(map[string]any{"id": msg.ID, "result": value}) }
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fixture"})
		case "initialized":
		case "account/login/start":
			var p struct {
				Account string `json:"chatgptAccountId"`
				Token   string `json:"accessToken"`
			}
			if json.Unmarshal(msg.Params, &p) != nil || p.Token != "secret-"+p.Account {
				os.Exit(4)
			}
			account = p.Account
			reply(map[string]string{"type": "chatgptAuthTokens"})
		case "thread/start":
			reply(map[string]any{"thread": map[string]string{"id": "thread"}})
		case "turn/start":
			if strings.Contains(string(msg.Params), "secret-") {
				os.Exit(5)
			}
			reply(map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress", "items": []any{}}})
			_ = enc.Encode(map[string]any{"id": 100, "method": "account/chatgptAuthTokens/refresh", "params": map[string]string{"reason": "unauthorized", "previousAccountId": account}})
		default:
			if string(msg.ID) != "100" {
				continue
			}
			var p struct {
				Account string `json:"chatgptAccountId"`
				Token   string `json:"accessToken"`
			}
			if json.Unmarshal(msg.Result, &p) != nil || p.Account != account || p.Token != "secret-"+account {
				os.Exit(6)
			}
			text, _ := json.Marshal(map[string]any{"content": account, "tool_calls": []any{}})
			item := map[string]any{"type": "agentMessage", "id": "message", "text": string(text), "phase": "final_answer"}
			_ = enc.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": item}})
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "completed", "items": []any{item}, "error": nil}}})
		}
	}
	os.Exit(0)
}

func TestCodexProfilesSelectAndRefreshOnlyOwnedAccount(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "codex-fixture")
	// Shell quoting is necessary even for the temporary test executable path.
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexec "+quoted+" -test.run='^TestCodexProfileRPCProcess$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var wrongAccount atomic.Bool
	refresh := map[string]int{}
	opts := Options{DataDir: t.TempDir(), BackendID: "codex-profile-test", RequireIdentity: true, MaxConcurrent: 2, MaxWaiting: 2, Timeout: 10 * time.Second,
		Authorize: func(context.Context, Principal) error { return nil },
		Native:    &NativeOptions{Providers: map[string]model.Config{"codex": {Kind: "codex-app-server", Command: command, Model: "fixture"}}, DefaultProvider: "codex", TokenBudgetPolicy: TokenBudgetAdvisory},
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{AccountID: c.ID, CodexAuth: func(_ context.Context, request model.CodexAuthRequest) (model.CodexAuthTokens, error) {
				if request.PreviousAccountID != "" && request.PreviousAccountID != c.ID {
					return model.CodexAuthTokens{}, auth.ErrInvalidCredentials
				}
				if request.Reason == "unauthorized" {
					mu.Lock()
					refresh[c.ID]++
					mu.Unlock()
				}
				account := c.ID
				if c.ID == "personal" && wrongAccount.Load() {
					account = "work"
				}
				return model.CodexAuthTokens{AccessToken: "secret-" + account, AccountID: account}, nil
			}}, nil
		},
	}
	r := nativeOpen(t, opts)
	defer r.Close()
	for _, id := range []string{"personal", "work"} {
		if err := r.RegisterConnection(context.Background(), Connection{ID: id, UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "codex", CredentialRef: id, State: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := WithPrincipal(context.Background(), Principal{UserID: "user", WorkspaceID: "workspace"})
	results := make(chan error, 2)
	for _, id := range []string{"personal", "work"} {
		go func(id string) {
			out, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: id, RequestID: id, ConnectionID: id, Prompt: Prompt{Text: "hello"}})
			if err == nil && string(out.Output) != `{"text":"`+id+`"}` {
				err = fmt.Errorf("wrong selected profile result")
			}
			results <- err
		}(id)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	if refresh["personal"] != 1 || refresh["work"] != 1 {
		t.Errorf("refresh was not profile-specific: %v", refresh)
	}
	mu.Unlock()
	for _, id := range []string{"personal", "work"} {
		history, err := r.History(ctx, "workspace", id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(history)
		if strings.Contains(string(raw), "secret-") {
			t.Fatal("credential leaked to journal")
		}
	}
	foreign := WithPrincipal(context.Background(), Principal{UserID: "other", WorkspaceID: "workspace"})
	if _, err := r.Run(foreign, Request{WorkspaceID: "workspace", SessionKey: "foreign", RequestID: "foreign", ConnectionID: "personal", Prompt: Prompt{Text: "hello"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign profile not rejected: %v", err)
	}
	wrongAccount.Store(true)
	if _, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "personal", RequestID: "wrong-account", ConnectionID: "personal", Prompt: Prompt{Text: "hello"}}); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("wrong account source accepted: %v", err)
	}
	personal, err := r.ownedConnection(ctx, "personal")
	if err != nil || personal.State != "needs_login" {
		t.Fatal("only rejected profile should need login")
	}
	workProfile, err := r.ownedConnection(ctx, "work")
	if err != nil || workProfile.State != "ready" {
		t.Fatal("wrong account invalidated another profile")
	}
	if err := r.SetConnectionState(ctx, "personal", "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "personal", RequestID: "after-revoke", ConnectionID: "personal", Prompt: Prompt{Text: "hello"}}); !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("revoked profile accepted: %v", err)
	}
	if _, err := r.Run(ctx, Request{WorkspaceID: "workspace", SessionKey: "work", RequestID: "work-again", ConnectionID: "work", Prompt: Prompt{Text: "hello"}}); err != nil {
		t.Fatal("revoking personal affected work profile")
	}
}
