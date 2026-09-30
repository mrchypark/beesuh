package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/auth"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "app-server" {
		fakeAppServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCodexAppServerExternalAuth(t *testing.T) {
	var requests []CodexAuthRequest
	client, err := newAppServer(Config{Command: os.Args[0], Model: "test", CodexAccountID: "account-a", CodexAuth: func(_ context.Context, request CodexAuthRequest) (CodexAuthTokens, error) {
		requests = append(requests, request)
		return CodexAuthTokens{AccessToken: "secret-token", AccountID: "account-a"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.completeAppServer(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" || len(requests) != 2 || requests[0].Reason != "initial" || requests[1].Reason != "unauthorized" || requests[1].PreviousAccountID != "account-a" {
		t.Fatalf("unexpected response or refresh: %#v %#v", response, requests)
	}
}

func TestCodexAppServerRejectsWrongAccount(t *testing.T) {
	client, err := newAppServer(Config{Command: os.Args[0], Model: "test", CodexAccountID: "account-a", CodexAuth: func(context.Context, CodexAuthRequest) (CodexAuthTokens, error) {
		return CodexAuthTokens{AccessToken: "secret", AccountID: "account-b"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.completeAppServer(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
}

func TestCodexAppServerTemplateNeedsAuthAtCompletion(t *testing.T) {
	client, err := newAppServer(Config{Command: os.Args[0], Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.completeAppServer(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
}

func TestCodexAppServerProfilesDoNotShareState(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, account := range []string{"account-a", "account-b"} {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := newAppServer(Config{Command: os.Args[0], Model: "test", CodexAccountID: account, CodexAuth: func(_ context.Context, request CodexAuthRequest) (CodexAuthTokens, error) {
				if request.PreviousAccountID != account {
					return CodexAuthTokens{}, auth.ErrInvalidCredentials
				}
				return CodexAuthTokens{AccessToken: "token-" + account, AccountID: account}, nil
			}})
			if err == nil {
				_, err = client.completeAppServer(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexAuthTokensValidation(t *testing.T) {
	if err := (CodexAuthTokens{AccessToken: "x", AccountID: "a"}).Validate("b"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if err := (CodexAuthTokens{AccessToken: "x\n", AccountID: "a"}).Validate("a"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
}

func TestCodexAppServerDoesNotExposeRPCError(t *testing.T) {
	rpc := &appServerRPC{in: io.Discard, scan: bufio.NewScanner(bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"error":{"message":"secret-token"}}` + "\n"))}
	_, err := rpc.call(context.Background(), "initialize", nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestCodexAppServerWaitsForSuccessfulTerminal(t *testing.T) {
	item := `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","phase":"final_answer","text":"{\"content\":\"ok\",\"tool_calls\":[]}"}}}` + "\n"
	failed := `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"failed","items":[]}}}` + "\n"
	rpc := &appServerRPC{in: io.Discard, scan: bufio.NewScanner(bytes.NewBufferString(item + failed))}
	if _, err := rpc.waitTurn(context.Background(), "thread", "turn"); err == nil {
		t.Fatal("accepted item before failed terminal")
	}
}

func TestCodexAppServerUsesCompletedFinalItem(t *testing.T) {
	item := `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","phase":"final_answer","text":"{\"content\":\"ok\",\"tool_calls\":[]}"}}}` + "\n"
	done := `{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed","items":[]}}}` + "\n"
	rpc := &appServerRPC{in: io.Discard, scan: bufio.NewScanner(bytes.NewBufferString(item + done))}
	text, err := rpc.waitTurn(context.Background(), "thread", "turn")
	if err != nil || text == "" {
		t.Fatalf("got %q, %v", text, err)
	}
}

func TestCodexAppServerRejectsInvalidRefreshAccountHint(t *testing.T) {
	called := false
	rpc := &appServerRPC{in: io.Discard, accountID: "account-a", source: func(context.Context, CodexAuthRequest) (CodexAuthTokens, error) {
		called = true
		return CodexAuthTokens{}, nil
	}}
	err := rpc.serverRequest(context.Background(), map[string]any{"id": 1, "method": "account/chatgptAuthTokens/refresh", "params": map[string]any{"reason": "unauthorized", "previousAccountId": true}})
	if !errors.Is(err, auth.ErrInvalidCredentials) || called {
		t.Fatalf("got %v, called=%v", err, called)
	}
}

func fakeAppServer() {
	s := bufio.NewScanner(os.Stdin)
	s.Buffer(make([]byte, 1024), maxResponseBytes+1)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	write := func(v any) { b, _ := json.Marshal(v); _, _ = w.Write(append(b, '\n')); _ = w.Flush() }
	badEnv := os.Getenv("HOME") == "" || os.Getenv("CODEX_HOME") != os.Getenv("HOME")+"/.codex" || strings.Contains(strings.Join(os.Args, "\x00"), "secret-token") || strings.Contains(strings.Join(os.Environ(), "\x00"), "secret-token")
	account, plan := "", ""
	for s.Scan() {
		var request map[string]any
		_ = json.Unmarshal(s.Bytes(), &request)
		id := request["id"]
		if badEnv {
			write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"message": "unsafe process setup"}})
			continue
		}
		switch request["method"] {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "account/login/start":
			params, _ := request["params"].(map[string]any)
			account, _ = params["chatgptAccountId"].(string)
			plan, _ = params["chatgptPlanType"].(string)
			write(map[string]any{"jsonrpc": "2.0", "method": "account/updated", "params": map[string]any{}})
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		case "thread/start":
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"thread": map[string]any{"id": "thread"}}})
		case "turn/start":
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"turn": map[string]any{"id": "turn"}}})
			if plan == "slow" {
				continue
			}
			write(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "account/chatgptAuthTokens/refresh", "params": map[string]any{"reason": "unauthorized", "previousAccountId": account}})
			if !s.Scan() {
				return
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "completed", "items": []any{map[string]any{"type": "agentMessage", "text": "{\"content\":\"ok\",\"tool_calls\":[]}"}}}}})
		}
	}
}

func TestCodexAppServerCancellationBeforeLaunch(t *testing.T) {
	client, err := newAppServer(Config{Command: os.Args[0], Model: "test", CodexAccountID: "account-a", CodexAuth: func(ctx context.Context, _ CodexAuthRequest) (CodexAuthTokens, error) {
		<-ctx.Done()
		return CodexAuthTokens{}, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err = client.completeAppServer(ctx, Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestCodexAppServerCancellationKillsRunningProcess(t *testing.T) {
	client, err := newAppServer(Config{Command: os.Args[0], Model: "test", CodexAccountID: "account-a", CodexAuth: func(context.Context, CodexAuthRequest) (CodexAuthTokens, error) {
		return CodexAuthTokens{AccessToken: "x", AccountID: "account-a", PlanType: "slow"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.completeAppServer(ctx, Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
