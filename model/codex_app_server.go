package model

// This bridge deliberately uses the app-server's external-token mode.  It is
// experimental upstream, so each completion gets a short-lived, isolated
// process; no account selection or auth state is shared with the operator.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mrchypark/beesuh/auth"
)

type CodexAuthTokens struct {
	AccessToken string `json:"-"`
	AccountID   string `json:"-"`
	PlanType    string `json:"-"`
}

type CodexAuthRequest struct {
	Reason            string
	PreviousAccountID string
}

// CodexAuthSource returns a token for the selected, authorized profile. It
// must honor context cancellation and must not log the token.
type CodexAuthSource func(context.Context, CodexAuthRequest) (CodexAuthTokens, error)

func (t CodexAuthTokens) Validate(expectedAccountID string) error {
	if strings.TrimSpace(expectedAccountID) == "" || strings.TrimSpace(t.AccessToken) == "" || len(t.AccessToken) > 64<<10 || invalidText(t.AccessToken) || strings.TrimSpace(t.AccountID) == "" || len(t.AccountID) > 256 || invalidText(t.AccountID) || len(t.PlanType) > 128 || invalidText(t.PlanType) || t.AccountID != expectedAccountID {
		return auth.ErrInvalidCredentials
	}
	return nil
}

func newAppServer(cfg Config) (*Client, error) {
	if cfg.BaseURL != "" || cfg.API != "" || cfg.APIKey != "" || cfg.APIKeyEnv != "" || cfg.HTTPClient != nil || cfg.CodexHome != "" {
		return nil, errors.New("model: codex-app-server does not accept HTTP, API key, or Codex home configuration")
	}
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		command = "codex"
	}
	if invalidText(command) || invalidText(cfg.Model) || strings.TrimSpace(cfg.Model) == "" || invalidText(cfg.CodexAccountID) {
		return nil, errors.New("model: Codex app-server command and model are required and must not contain newlines")
	}
	if cfg.CodexAuth != nil && strings.TrimSpace(cfg.CodexAccountID) == "" {
		return nil, errors.New("model: Codex app-server auth source requires an account ID")
	}
	return &Client{kind: "codex-app-server", command: command, model: cfg.Model, codexAuth: cfg.CodexAuth, codexAccountID: cfg.CodexAccountID}, nil
}

func (c *Client) completeAppServer(ctx context.Context, in Request) (Response, error) {
	if err := c.validateTokenLimit(ctx, in); err != nil {
		return Response{}, err
	}
	if c.codexAuth == nil || strings.TrimSpace(c.codexAccountID) == "" {
		return Response{}, auth.ErrInvalidCredentials
	}
	ctx, cancel := cliContext(ctx)
	defer cancel()
	tokens, err := c.codexAuth(ctx, CodexAuthRequest{Reason: "initial", PreviousAccountID: c.codexAccountID})
	if err != nil {
		return Response{}, auth.Sanitize(err)
	}
	if err := tokens.Validate(c.codexAccountID); err != nil {
		return Response{}, err
	}
	work, err := os.MkdirTemp("", "beesuh-codex-app-server-")
	if err != nil {
		return Response{}, errors.New("model: create Codex app-server directory failed")
	}
	defer os.RemoveAll(work)
	home := filepath.Join(work, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return Response{}, errors.New("model: create isolated Codex home failed")
	}
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o700); err != nil {
		return Response{}, errors.New("model: create isolated Codex state directory failed")
	}
	cmd := exec.CommandContext(context.Background(), c.command, appServerArgs()...)
	cmd.Dir = work
	cmd.Env = appServerEnv(home)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = codexWaitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	inPipe, err := cmd.StdinPipe()
	if err != nil {
		return Response{}, errors.New("model: start Codex app-server failed")
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Response{}, errors.New("model: start Codex app-server failed")
	}
	if err := cmd.Start(); err != nil {
		return Response{}, errors.New("model: start Codex app-server failed")
	}
	var cleanupOnce sync.Once
	stop := func() {
		cleanupOnce.Do(func() {
			_ = inPipe.Close()
			_ = outPipe.Close()
			_ = cmd.Cancel()
			if cmd.Process != nil {
				_, _ = cmd.Process.Wait()
			}
		})
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	defer func() { close(done); stop() }()
	rpc := &appServerRPC{in: inPipe, scan: bufio.NewScanner(outPipe), source: c.codexAuth, accountID: c.codexAccountID}
	rpc.scan.Buffer(make([]byte, 4096), maxResponseBytes+1)
	return c.completeAppServerSession(ctx, in, rpc, tokens, work)
}

func (c *Client) completeAppServerSession(ctx context.Context, in Request, rpc *appServerRPC, tokens CodexAuthTokens, work string) (response Response, err error) {
	defer func() {
		// Cancellation can close pipes while a buffered response is still being decoded.
		if ctx.Err() != nil {
			response, err = Response{}, ctx.Err()
		}
	}()
	if _, err := rpc.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "beesuh", "version": "0.1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return Response{}, err
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return Response{}, err
	}
	if _, err := rpc.call(ctx, "account/login/start", appServerTokens(tokens)); err != nil {
		return Response{}, err
	}
	thread, err := rpc.call(ctx, "thread/start", map[string]any{"cwd": work, "model": c.model, "ephemeral": true, "approvalPolicy": "never", "sandbox": "read-only", "environments": []any{}, "dynamicTools": []any{}})
	if err != nil {
		return Response{}, err
	}
	threadID := nestedString(thread, "thread", "id")
	if threadID == "" {
		return Response{}, errors.New("model: invalid Codex app-server thread response")
	}
	prompt, err := codexPrompt(in)
	if err != nil {
		return Response{}, errors.New("model: encode Codex request failed")
	}
	turn, err := rpc.call(ctx, "turn/start", map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": prompt}}, "outputSchema": json.RawMessage(codexOutputSchema), "approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "readOnly"}, "environments": []any{}})
	if err != nil {
		return Response{}, err
	}
	turnID := nestedString(turn, "turn", "id")
	if turnID == "" {
		return Response{}, errors.New("model: invalid Codex app-server turn response")
	}
	content, err := rpc.waitTurn(ctx, threadID, turnID)
	if err != nil {
		return Response{}, err
	}
	out, err := decodeCodexOutput([]byte(content), in.Tools)
	if err != nil {
		return Response{}, err
	}
	out.Usage = unknownUsage()
	return out, nil
}

func appServerArgs() []string {
	return []string{"app-server", "--strict-config", "-c", "cli_auth_credentials_store='ephemeral'", "-c", "features.shell_tool=false", "-c", "features.unified_exec=false", "-c", "features.multi_agent=false", "-c", "features.apps=false", "-c", "features.plugins=false", "-c", "features.hooks=false", "-c", "features.browser_use=false", "-c", "features.computer_use=false", "-c", "features.image_generation=false", "-c", "features.browser_use_external=false", "-c", "features.in_app_browser=false", "-c", "features.in_app_chat=false", "-c", "features.in_app_local_automation=false", "-c", "features.remote_plugin=false", "-c", "features.skill_search=false", "-c", "features.skill_mcp_dependency_install=false", "-c", "features.code_mode_host=false", "-c", "features.view_image=false", "-c", "features.goals=false", "-c", "web_search='disabled'", "-c", "mcp_servers={}", "-c", "project_doc_max_bytes=0"}
}

func appServerEnv(home string) []string {
	keys := map[string]bool{"PATH": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if keys[key] {
			env = append(env, entry)
		}
	}
	return append(env, "HOME="+home, "CODEX_HOME="+filepath.Join(home, ".codex"))
}

func appServerTokens(t CodexAuthTokens) map[string]any {
	p := map[string]any{"type": "chatgptAuthTokens", "accessToken": t.AccessToken, "chatgptAccountId": t.AccountID}
	if t.PlanType != "" {
		p["chatgptPlanType"] = t.PlanType
	}
	return p
}

type appServerRPC struct {
	in        io.Writer
	scan      *bufio.Scanner
	source    CodexAuthSource
	accountID string
	refreshes int
	next      int
	pending   []map[string]any
	seen      int
}

func (r *appServerRPC) call(ctx context.Context, method string, params any) (map[string]any, error) {
	r.next++
	id := r.next
	if err := r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		m, err := r.readWire(ctx)
		if err != nil {
			return nil, err
		}
		if err := r.serverRequest(ctx, m); err != nil {
			return nil, err
		}
		if m["method"] != nil {
			if m["id"] == nil {
				if err := r.queue(m); err != nil {
					return nil, err
				}
			}
			continue
		}
		if numberID(m["id"]) != id {
			continue
		}
		if m["error"] != nil {
			return nil, errors.New("model: Codex app-server request failed")
		}
		result, ok := m["result"].(map[string]any)
		if !ok {
			return nil, errors.New("model: invalid Codex app-server response")
		}
		return result, nil
	}
}

func (r *appServerRPC) waitTurn(ctx context.Context, threadID, turnID string) (string, error) {
	finalText := ""
	for {
		m, err := r.read(ctx)
		if err != nil {
			return "", err
		}
		if err := r.serverRequest(ctx, m); err != nil {
			return "", err
		}
		method, _ := m["method"].(string)
		if method != "" && m["id"] == nil {
			if err := r.note(); err != nil {
				return "", err
			}
		}
		if method == "item/completed" && nestedString(m, "params", "threadId") == threadID && nestedString(m, "params", "turnId") == turnID {
			if text := itemText(nested(m, "params", "item")); text != "" {
				finalText = text
			}
		}
		if method == "turn/completed" && nestedString(m, "params", "threadId") == threadID && nestedString(m, "params", "turn", "id") == turnID {
			if nestedString(m, "params", "turn", "status") != "completed" {
				return "", appServerTurnError(nested(m, "params", "turn", "error", "codexErrorInfo"))
			}
			if text := turnText(nested(m, "params", "turn", "items")); text != "" {
				return text, nil
			}
			if finalText != "" {
				return finalText, nil
			}
			return "", errors.New("model: Codex app-server produced no response")
		}
	}
}

// CodexErrorInfo uses camel-case variants on the wire. Classify only the
// structured error, never its message or additionalDetails, which can contain
// credentials. Unknown protocol errors remain ordinary model failures.
func appServerTurnError(info any) error {
	if kind, ok := info.(string); ok {
		switch kind {
		case "unauthorized":
			return auth.ErrInvalidCredentials
		case "rateLimitExceeded", "usageLimitExceeded":
			return auth.ErrRateLimited
		case "serverOverloaded", "internalServerError":
			return auth.ErrUnavailable
		}
	}
	for _, variant := range []string{"httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts"} {
		if status, ok := nested(info, variant, "httpStatusCode").(float64); ok && status == float64(int(status)) && status >= 400 && status <= 599 {
			return auth.HTTPError{StatusCode: int(status)}
		}
	}
	return errors.New("model: Codex app-server turn failed")
}

func (r *appServerRPC) serverRequest(ctx context.Context, m map[string]any) error {
	method, _ := m["method"].(string)
	if method == "" {
		return nil
	}
	if strings.Contains(method, "approval") || strings.Contains(method, "tool/") || strings.Contains(method, "permissions") {
		if m["id"] != nil {
			_ = r.write(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32601, "message": "denied"}})
		}
		return errors.New("model: Codex app-server requested unsupported action")
	}
	if m["id"] == nil {
		return nil
	}
	if method != "account/chatgptAuthTokens/refresh" {
		_ = r.write(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32601, "message": "denied"}})
		return errors.New("model: Codex app-server requested unsupported action")
	}
	params, ok := nested(m, "params").(map[string]any)
	if !ok {
		return auth.ErrInvalidCredentials
	}
	reason := nestedString(params, "reason")
	previous := ""
	if raw, exists := params["previousAccountId"]; exists && raw != nil {
		var ok bool
		previous, ok = raw.(string)
		if !ok {
			return auth.ErrInvalidCredentials
		}
	}
	if reason != "unauthorized" || (previous != "" && previous != r.accountID) {
		return auth.ErrInvalidCredentials
	}
	r.refreshes++
	if r.refreshes > 3 {
		return auth.ErrInvalidCredentials
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	t, err := r.source(refreshCtx, CodexAuthRequest{Reason: reason, PreviousAccountID: previous})
	if err != nil {
		return auth.Sanitize(err)
	}
	if err := refreshCtx.Err(); err != nil {
		return err
	}
	if err := t.Validate(r.accountID); err != nil {
		return err
	}
	return r.write(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": appServerTokens(t)})
}

func (r *appServerRPC) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil || len(b) > maxResponseBytes {
		return errors.New("model: encode Codex app-server request failed")
	}
	b = append(b, '\n')
	if _, err := r.in.Write(b); err != nil {
		return errors.New("model: Codex app-server unavailable")
	}
	return nil
}
func (r *appServerRPC) notify(method string, params any) error {
	return r.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (r *appServerRPC) read(ctx context.Context) (map[string]any, error) {
	if len(r.pending) != 0 {
		m := r.pending[0]
		r.pending = r.pending[1:]
		return m, nil
	}
	return r.readWire(ctx)
}
func (r *appServerRPC) readWire(ctx context.Context) (map[string]any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !r.scan.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("model: Codex app-server unavailable")
	}
	var m map[string]any
	if err := json.Unmarshal(r.scan.Bytes(), &m); err != nil {
		return nil, errors.New("model: invalid Codex app-server response")
	}
	return m, nil
}
func (r *appServerRPC) queue(m map[string]any) error {
	if err := r.note(); err != nil {
		return err
	}
	if len(r.pending) >= 64 {
		return errors.New("model: excessive Codex app-server notifications")
	}
	r.pending = append(r.pending, m)
	return nil
}
func (r *appServerRPC) note() error {
	r.seen++
	if r.seen > 1024 {
		return errors.New("model: excessive Codex app-server notifications")
	}
	return nil
}
func numberID(v any) int { f, _ := v.(float64); return int(f) }
func nested(v any, keys ...string) any {
	current := v
	for _, k := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[k]
	}
	return current
}
func nestedString(v any, keys ...string) string { s, _ := nested(v, keys...).(string); return s }
func itemText(v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	if typeName, _ := m["type"].(string); typeName != "agentMessage" {
		return ""
	}
	if phase, exists := m["phase"]; exists && phase != nil && phase != "final_answer" {
		return ""
	}
	for _, k := range []string{"text", "content"} {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	return ""
}
func turnText(v any) string {
	a, _ := v.([]any)
	for i := len(a) - 1; i >= 0; i-- {
		if s := itemText(a[i]); s != "" {
			return s
		}
	}
	return ""
}
