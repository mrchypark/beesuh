package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/mrchypark/beesuh/auth"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "cli-helper" {
		// The marker is deliberately written only after a delay that is far
		// longer than any parent-side timing, so the test proves the process
		// group was killed rather than that the helper was merely slow.
		time.Sleep(5 * time.Second)
		_ = os.WriteFile(os.Args[2], []byte("survived"), 0o600)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		fakeCodexCLI()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "hold-stdout" {
		time.Sleep(time.Second)
		os.Exit(0)
	}
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
	if response.Usage.Available() {
		t.Fatalf("usage must be unknown: %+v", response.Usage)
	}
}

func TestCodexAppServerRequiresAdvisoryBudgetOptIn(t *testing.T) {
	called := false
	client, err := newAppServer(Config{Command: os.Args[0], Model: "test", CodexAccountID: "account-a", CodexAuth: func(context.Context, CodexAuthRequest) (CodexAuthTokens, error) {
		called = true
		return CodexAuthTokens{AccessToken: "x", AccountID: "account-a"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	in := Request{Messages: []Message{{Role: "user", Content: "hello"}}, MaxTokens: 12}
	if _, err := client.Complete(context.Background(), in); !errors.Is(err, ErrTokenLimitNotEnforced) || called {
		t.Fatalf("err=%v auth_called=%v", err, called)
	}
	response, err := client.Complete(AllowAdvisoryTokenBudget(context.Background()), in)
	if err != nil || response.Message.Content != "ok" || response.Usage.Available() {
		t.Fatalf("response: %+v, %v", response, err)
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
			if plan == "detached" {
				child := exec.Command(os.Args[0], "hold-stdout")
				child.Stdout = os.Stdout
				child.Stderr = os.Stderr
				child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				_ = child.Start()
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

func TestCodexAppServerCancellationClosesDetachedStdout(t *testing.T) {
	client, err := newAppServer(Config{Command: os.Args[0], Model: "test", CodexAccountID: "account-a", CodexAuth: func(context.Context, CodexAuthRequest) (CodexAuthTokens, error) {
		return CodexAuthTokens{AccessToken: "x", AccountID: "account-a", PlanType: "detached"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = client.completeAppServer(ctx, Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("call did not terminate on cancellation: %v", err)
	}
}

// Match only complete frames and retain bytes not consumed by the scanner.
type cancellationGatedReader struct {
	reader  *bufio.Reader
	marker  []byte
	trigger func()
	pending []byte
	err     error
	gated   bool
}

func (r *cancellationGatedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		r.pending, r.err = r.reader.ReadBytes('\n')
		if r.err == nil && !r.gated && bytes.Contains(r.pending, r.marker) {
			r.gated = true
			r.trigger()
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Synthetic deadlines exercise normalization, not the runtime's timer implementation.
type cancellationCauseContext struct{ context.Context }

func (c cancellationCauseContext) Err() error {
	if c.Context.Err() != nil {
		return context.Cause(c.Context)
	}
	return nil
}

type sessionTestWriter struct{ closed bool }

func (w *sessionTestWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

const sessionTestWire = `{"jsonrpc":"2.0","id":1,"result":{}}
{"jsonrpc":"2.0","id":2,"result":{}}
{"jsonrpc":"2.0","id":3,"result":{"thread":{"id":"thread"}}}
{"jsonrpc":"2.0","id":4,"result":{"turn":{"id":"turn"}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed","items":[{"type":"agentMessage","text":"{\"content\":\"ok\",\"tool_calls\":[]}"}]}}}
`

func TestCodexAppServerSessionCancellation(t *testing.T) {
	for _, phase := range []string{"initialize", "terminal"} {
		for _, want := range []error{context.DeadlineExceeded, context.Canceled} {
			t.Run(phase+"/"+want.Error(), func(t *testing.T) {
				base, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				ctx := cancellationCauseContext{base}
				writer := &sessionTestWriter{}
				marker := []byte(`"id":1`)
				if phase == "terminal" {
					marker = []byte(`"method":"turn/completed"`)
				}
				reader := &cancellationGatedReader{
					reader: bufio.NewReader(iotest.OneByteReader(strings.NewReader(sessionTestWire))), marker: marker,
					trigger: func() { cancel(want); writer.closed = true },
				}
				rpc := &appServerRPC{in: writer, scan: bufio.NewScanner(reader)}
				response, err := (&Client{model: "test"}).completeAppServerSession(ctx, Request{Messages: []Message{{Role: "user", Content: "x"}}}, rpc, CodexAuthTokens{}, "")
				if !reader.gated || ctx.Err() != want {
					t.Fatal("selected cancellation boundary was not exercised")
				}
				if !errors.Is(err, want) || response.Message.Content != "" {
					t.Fatalf("response=%+v err=%v; want empty response and %v", response, err, want)
				}
			})
		}
	}
}

func TestCancellationGatedReaderSplitMarkerAndUnreadBytes(t *testing.T) {
	const before = "{\"id\":1,\"result\":{}}\n"
	const selected = "{\"method\":\"turn/completed\"}\n"
	const after = "{\"id\":9,\"result\":{}}\n"
	calls := 0
	reader := &cancellationGatedReader{
		reader: bufio.NewReader(io.MultiReader(strings.NewReader(before+`{"method":"tur`), strings.NewReader("n/completed\"}\n"+after))),
		marker: []byte(`"method":"turn/completed"`), trigger: func() { calls++ },
	}
	prefix := make([]byte, len(before))
	if _, err := io.ReadFull(reader, prefix); err != nil {
		t.Fatal(err)
	}
	if string(prefix) != before || calls != 0 {
		t.Fatal("gate triggered before selected frame")
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(reader, one); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("split marker did not trigger at complete frame")
	}
	rest, err := io.ReadAll(reader)
	if err != nil || string(one)+string(rest) != selected+after || calls != 1 {
		t.Fatalf("unread bytes lost or gate repeated: %q, %v, calls=%d", string(one)+string(rest), err, calls)
	}
}

func TestCodexAppServerSessionUncanceledControls(t *testing.T) {
	for _, tc := range []struct {
		name, wire, want string
		closed           bool
	}{
		{name: "invalid", wire: "not-json\n", want: "model: invalid Codex app-server response"},
		{name: "unavailable", want: "model: Codex app-server unavailable"},
		{name: "closed-write", wire: sessionTestWire, closed: true, want: "model: Codex app-server unavailable"},
		{name: "success", wire: sessionTestWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &appServerRPC{in: &sessionTestWriter{closed: tc.closed}, scan: bufio.NewScanner(strings.NewReader(tc.wire))}
			response, err := (&Client{}).completeAppServerSession(context.Background(), Request{}, rpc, CodexAuthTokens{}, "")
			if tc.want == "" {
				if err != nil || response.Message.Content != "ok" {
					t.Fatalf("response=%+v err=%v", response, err)
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v; want %s", err, tc.want)
			}
		})
	}
}
