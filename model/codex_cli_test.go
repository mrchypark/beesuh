package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeCodexCLI() {
	args := os.Args[2:]
	var output string
	configs := map[string]bool{}
	for i := range args {
		if args[i] == "--output-last-message" && i+1 < len(args) {
			output = args[i+1]
		}
		if args[i] == "-c" && i+1 < len(args) {
			configs[args[i+1]] = true
		}
	}
	if output == "" || !filepath.IsAbs(output) || !strings.Contains(strings.Join(args, " "), "--strict-config") || !strings.Contains(strings.Join(args, " "), "--ignore-user-config") || !strings.Contains(strings.Join(args, " "), "--ephemeral") || !strings.Contains(strings.Join(args, " "), "--sandbox read-only") || os.Getenv("BEESUH_MODEL_API_KEY") != "" || os.Getenv("CONDUCTOR_TOKEN") != "" {
		os.Exit(9)
	}
	for _, key := range []string{"features.plugins=false", "features.hooks=false", "features.browser_use=false", "features.computer_use=false", "features.image_generation=false", "features.browser_use_external=false", "features.in_app_browser=false", "features.in_app_chat=false", "features.in_app_local_automation=false", "features.remote_plugin=false", "features.skill_search=false", "features.skill_mcp_dependency_install=false", "features.code_mode_host=false", "features.view_image=false", "features.goals=false"} {
		if !configs[key] {
			os.Exit(9)
		}
	}
	for _, key := range []string{"plugins=false", "hooks=false", "browser_use=false", "computer_use=false", "image_generation=false"} {
		if configs[key] {
			os.Exit(9)
		}
	}
	wd, _ := os.Getwd()
	if !filepath.IsAbs(os.Getenv("CODEX_HOME")) || !strings.Contains(wd, "beesuh-codex-") {
		os.Exit(9)
	}
	for i := range args {
		if args[i] == "--output-schema" && i+1 < len(args) {
			schema, _ := os.ReadFile(args[i+1])
			if !strings.Contains(string(schema), `"tool_calls"`) {
				os.Exit(9)
			}
		}
	}
	input, _ := os.ReadFile("/dev/stdin")
	if strings.Contains(string(input), "secret-state") {
		os.Exit(9)
	}
	if strings.Contains(string(input), `"content":"sleep"`) {
		time.Sleep(time.Second)
	}
	if strings.Contains(string(input), `"content":"fail"`) {
		os.Exit(7)
	}
	if strings.Contains(string(input), `"content":"helper:`) {
		start := strings.Index(string(input), `helper:`) + len("helper:")
		end := strings.Index(string(input)[start:], `"`)
		if end > 0 {
			marker := string(input)[start : start+end]
			child := exec.Command(os.Args[0], "cli-helper", marker)
			if err := child.Start(); err == nil {
				// Record the helper PID synchronously so the test observes the
				// helper process directly instead of inferring it from a sleep.
				_ = os.WriteFile(marker+".pid", []byte(strconv.Itoa(child.Process.Pid)), 0o600)
			}
		}
	}
	response := `{"content":"ok","tool_calls":[]}`
	if strings.Contains(string(input), `"content":"tool"`) {
		response = `{"content":"","tool_calls":[{"id":"call_1","name":"conductor_emit","arguments":"{}"}]}`
	}
	if strings.Contains(string(input), `"content":"invalid"`) {
		response = `{"content":"ok","tool_calls":[{"id":"call_1","name":"nope","arguments":"{}"}]}`
	}
	if err := os.WriteFile(output, []byte(response), 0o600); err != nil {
		os.Exit(8)
	}
	os.Exit(0)
}

func TestCompleteCLIProtocol(t *testing.T) {
	t.Setenv("BEESUH_MODEL_API_KEY", "secret")
	t.Setenv("CONDUCTOR_TOKEN", "secret")
	c := &Client{command: os.Args[0], model: "test", codexHome: t.TempDir()}
	request := Request{Messages: []Message{{Role: "user", Content: "tool", ProviderState: json.RawMessage(`"secret-state"`)}}, Tools: []Tool{{Type: "function", Function: Function{Name: "conductor_emit", Parameters: json.RawMessage(`{"type":"object"}`)}}}}
	out, err := c.completeCLI(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if out.FinishReason != "tool_calls" || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "conductor_emit" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if out.Usage.Available() || out.Usage.PromptTokens != -1 || out.Usage.CompletionTokens != -1 || out.Usage.TotalTokens != -1 {
		t.Fatalf("usage must be unknown: %+v", out.Usage)
	}
	final, err := c.completeCLI(context.Background(), Request{Messages: []Message{{Role: "user", Content: "final"}}})
	if err != nil || final.FinishReason != "stop" || final.Message.Content != "ok" {
		t.Fatalf("unexpected final response: %+v, %v", final, err)
	}
}

func TestCompleteCLIRequiresAdvisoryBudgetOptIn(t *testing.T) {
	c := &Client{kind: "codex-cli", command: os.Args[0], model: "test", codexHome: t.TempDir()}
	in := Request{Messages: []Message{{Role: "user", Content: "final"}}, MaxTokens: 12}
	if _, err := c.Complete(context.Background(), in); !errors.Is(err, ErrTokenLimitNotEnforced) {
		t.Fatalf("got %v", err)
	}
	out, err := c.Complete(AllowAdvisoryTokenBudget(context.Background()), in)
	if err != nil || out.Message.Content != "ok" || out.Usage.Available() {
		t.Fatalf("response: %+v, %v", out, err)
	}
}

func TestCompleteCLIRejectsUnknownToolAndCancels(t *testing.T) {
	c := &Client{command: os.Args[0], model: "test", codexHome: t.TempDir()}
	tools := []Tool{{Type: "function", Function: Function{Name: "conductor_emit", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	if _, err := c.completeCLI(context.Background(), Request{Messages: []Message{{Role: "user", Content: "invalid"}}, Tools: tools}); err == nil {
		t.Fatal("expected unknown tool error")
	}
	if _, err := c.completeCLI(context.Background(), Request{Messages: []Message{{Role: "user", Content: "fail"}}}); err == nil {
		t.Fatal("expected nonzero CLI error")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := c.completeCLI(ctx, Request{Messages: []Message{{Role: "user", Content: "sleep"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestCompleteCLICleansOwnedProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "helper-ran")
	c := &Client{command: os.Args[0], model: "test", codexHome: t.TempDir()}
	if _, err := c.completeCLI(context.Background(), Request{Messages: []Message{{Role: "user", Content: "helper:" + marker}}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(marker + ".pid")
	if err != nil {
		t.Fatalf("helper pid not recorded: %v", err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil || pid <= 1 {
		t.Fatalf("unexpected helper pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned helper %d survived", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("owned helper survived: %v", err)
	}
}

func TestCLIConfigOutputAndDefaultTimeout(t *testing.T) {
	for _, cfg := range []Config{
		{Kind: "codex-cli", Model: "m", BaseURL: "https://example.com"},
		{Kind: "codex-cli", Model: "m", API: "responses"},
		{Kind: "codex-cli", Model: "m", APIKey: "secret"},
		{Kind: "codex-cli", Model: "m", APIKeyEnv: "CODEX_API_KEY"},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("expected incompatible config error: %+v", cfg)
		}
	}
	if _, err := decodeCodexOutput([]byte(`{"content":null,"tool_calls":[]}`), nil); err == nil {
		t.Fatal("expected null content error")
	}
	if _, err := decodeCodexOutput([]byte(`{"content":"","tool_calls":null}`), nil); err == nil {
		t.Fatal("expected null tools error")
	}
	if _, err := decodeCodexOutput([]byte(`{"content":"","tool_calls":[]}`), nil); err == nil {
		t.Fatal("expected empty final error")
	}
	ctx, cancel := cliContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < defaultTimeout-time.Second {
		t.Fatal("expected CLI default timeout")
	}
	t.Setenv("CODEX_HOME", "/operator/codex")
	if !containsEnv(codexEnv(""), "CODEX_HOME=/operator/codex") || containsEnv(codexEnv("/selected/codex"), "CODEX_HOME=/operator/codex") || !containsEnv(codexEnv("/selected/codex"), "CODEX_HOME=/selected/codex") {
		t.Fatal("unexpected Codex home isolation")
	}
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
