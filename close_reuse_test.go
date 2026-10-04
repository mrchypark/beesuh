package beesuh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/model"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "close-reuse-app-server":
			closeReuseAppServer()
			return
		case "close-reuse-hold-stdout":
			time.Sleep(time.Second)
			return
		}
	}
	os.Exit(m.Run())
}

func closeReuseAppServer() {
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()
	write := func(id int, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		_, _ = writer.Write(append(b, '\n'))
		_ = writer.Flush()
	}
	for scanner.Scan() {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &request)
		switch request.Method {
		case "initialize", "account/login/start":
			write(request.ID, map[string]any{})
		case "thread/start":
			write(request.ID, map[string]any{"thread": map[string]any{"id": "thread"}})
		case "turn/start":
			write(request.ID, map[string]any{"turn": map[string]any{"id": "turn"}})
			child := exec.Command(os.Args[0], "close-reuse-hold-stdout")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if child.Start() == nil && len(os.Args) > 2 {
				_ = os.WriteFile(os.Args[2], []byte("started"), 0o600)
			}
		}
	}
}

func TestRuntimeCloseAfterDetachedCodexAppServerCancellation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "detached-started")
	command := filepath.Join(t.TempDir(), "app-server")
	if err := os.WriteFile(command, []byte(fmt.Sprintf("#!/bin/sh\nexec %q close-reuse-app-server %q\n", os.Args[0], marker)), 0o700); err != nil {
		t.Fatal(err)
	}

	r := nativeOpen(t, Options{
		DataDir:       t.TempDir(),
		BackendID:     "close-reuse",
		MaxConcurrent: 1,
		MaxWaiting:    1,
		// The run timeout must outlast the app-server handshake and turn start,
		// otherwise a loaded machine times out before the detached descendant
		// exists and the test measures scheduling rather than Close. The
		// cancellation and Close bounds below are what this test actually asserts.
		Timeout:         30 * time.Second,
		RequireIdentity: true,
		Authorize:       func(context.Context, Principal) error { return nil },
		ResolveCredential: func(context.Context, Connection) (Credential, error) {
			return Credential{AccountID: "account", CodexAuth: func(context.Context, model.CodexAuthRequest) (model.CodexAuthTokens, error) {
				return model.CodexAuthTokens{AccessToken: "token", AccountID: "account"}, nil
			}}, nil
		},
		Native: &NativeOptions{
			Providers:         map[string]model.Config{"codex": {Kind: "codex-app-server", Command: command, Model: "test"}},
			DefaultProvider:   "codex",
			TokenBudgetPolicy: TokenBudgetAdvisory,
		},
	})
	principal := WithPrincipal(context.Background(), Principal{UserID: "user", WorkspaceID: "workspace"})
	if err := r.RegisterConnection(principal, Connection{ID: "conn", UserID: "user", WorkspaceID: "workspace", Kind: "provider", TargetID: "codex", CredentialRef: "credential", State: "ready"}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(principal)
	runDone := make(chan error, 1)
	go func() {
		_, err := r.Run(runCtx, Request{WorkspaceID: "workspace", SessionKey: "session", RequestID: "request", ConnectionID: "conn", Prompt: Prompt{Text: "wait"}})
		runDone <- err
	}()
	waitForCloseReuseFile(t, marker, runDone, "detached descendant did not start")
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after detached stdout cleanup")
	}

	revokeDone := make(chan error, 1)
	go func() { revokeDone <- r.SetConnectionState(principal, "conn", "revoked") }()
	select {
	case err := <-revokeDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("post-Close revoke error = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-Close revocation did not return")
	}
}

func waitForCloseReuseFile(t *testing.T, name string, runDone <-chan error, message string) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if _, err := os.Stat(name); err == nil {
			return
		}
		select {
		case err := <-runDone:
			t.Fatalf("Run returned before the detached descendant started: %v", err)
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal(message)
		}
	}
}
