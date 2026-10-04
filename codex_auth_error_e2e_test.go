package beesuh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/model"
)

// This child is an app-server fixture that reports a structured terminal
// unauthorized error. It uses dummy credentials only.
func TestCodexAppServerUnauthorizedRPCProcess(t *testing.T) {
	child := false
	for _, arg := range os.Args {
		if arg == "--" {
			child = true
			break
		}
	}
	if !child {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			os.Exit(3)
		}
		reply := func(value any) { _ = enc.Encode(map[string]any{"id": message.ID, "result": value}) }
		switch message.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fixture"})
		case "initialized":
		case "account/login/start":
			reply(map[string]string{"type": "chatgptAuthTokens"})
		case "thread/start":
			reply(map[string]any{"thread": map[string]string{"id": "thread"}})
		case "turn/start":
			reply(map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}})
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "failed", "error": map[string]any{"codexErrorInfo": "unauthorized", "message": "private fixture detail"}}}})
		}
	}
}

func TestRuntimeCodexAppServerUnauthorizedInvalidatesOnlyConnection(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "codex-unauthorized-fixture")
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexec "+quoted+" -test.run='^TestCodexAppServerUnauthorizedRPCProcess$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var tokenCalls atomic.Int32
	r := nativeOpen(t, Options{
		DataDir: t.TempDir(), BackendID: "codex-unauthorized", MaxConcurrent: 1, MaxWaiting: 1, Timeout: 10 * time.Second,
		RequireIdentity: true,
		Authorize:       func(context.Context, Principal) error { return nil },
		Native: &NativeOptions{
			Providers:         map[string]model.Config{"codex": {Kind: "codex-app-server", Command: command, Model: "fixture"}},
			DefaultProvider:   "codex",
			TokenBudgetPolicy: TokenBudgetAdvisory,
		},
		ResolveCredential: func(_ context.Context, c Connection) (Credential, error) {
			return Credential{AccountID: c.ID, CodexAuth: func(context.Context, model.CodexAuthRequest) (model.CodexAuthTokens, error) {
				tokenCalls.Add(1)
				return model.CodexAuthTokens{AccessToken: "fixture-token", AccountID: c.ID}, nil
			}}, nil
		},
	})
	defer r.Close()
	ctx := WithPrincipal(context.Background(), Principal{UserID: "user", WorkspaceID: "workspace"})
	if err := r.RegisterConnection(ctx, Connection{ID: "conn", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "codex", CredentialRef: "credential", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterConnection(ctx, Connection{ID: "other", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "codex", CredentialRef: "other-credential", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	req := Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "request", ConnectionID: "conn", Prompt: Prompt{Text: "hello"}}
	first, err := r.Run(ctx, req)
	if !errors.Is(err, ErrConnectionUnavailable) {
		t.Fatalf("Run error = %v, want %v", err, ErrConnectionUnavailable)
	}
	if first.State != "failed" || first.FailureCategory != FailureConnectionUnavailable || first.Error != PublicFailureMessage(FailureConnectionUnavailable) {
		t.Fatalf("initial result = %+v", first)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("Run leaked app-server detail: %v", err)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token source calls = %d, want one app-server process/auth attempt", got)
	}
	stored, storedErr := r.GetRun(ctx, "workspace", "request")
	if storedErr != nil || stored.State != "failed" || stored.FailureCategory != FailureConnectionUnavailable || stored.Error != PublicFailureMessage(FailureConnectionUnavailable) {
		t.Fatalf("stored run=%+v error=%v", stored, storedErr)
	}
	connection, err := r.ownedConnection(ctx, "conn")
	if err != nil || connection.State != "needs_login" {
		t.Fatalf("failed connection=%+v error=%v", connection, err)
	}
	other, err := r.ownedConnection(ctx, "other")
	if err != nil || other.State != "ready" {
		t.Fatalf("unrelated connection=%+v error=%v", other, err)
	}

	replayed, replayErr := r.Run(ctx, req)
	if !errors.Is(replayErr, ErrConnectionUnavailable) {
		t.Fatalf("repeat error = %v, want typed connection-unavailable sentinel", replayErr)
	}
	// bindRequest rejects the now-needs-login connection before previous can
	// replay the stored run. GetRun above is the public durable-result path.
	if replayed.State != "" || replayed.Error != "" || replayed.FailureCategory != "" || len(replayed.Output) != 0 {
		t.Fatalf("repeat result = %+v, want no new run result", replayed)
	}
	if strings.Contains(replayErr.Error(), "private") {
		t.Fatalf("repeat leaked app-server detail: %v", replayErr)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("repeat started another token/model attempt: calls=%d", got)
	}
}
