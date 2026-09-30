package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const codexWaitDelay = 2 * time.Second

func newCLI(cfg Config) (*Client, error) {
	if cfg.BaseURL != "" || cfg.API != "" || cfg.APIKey != "" || cfg.APIKeyEnv != "" {
		return nil, errors.New("model: codex-cli does not accept base_url, api, api_key, or api_key_env")
	}
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		command = "codex"
	}
	if invalidText(command) || invalidText(cfg.Model) || strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("model: Codex command and Model must not contain newlines; Model is required")
	}
	if cfg.CodexHome != "" && (!filepath.IsAbs(cfg.CodexHome) || invalidText(cfg.CodexHome)) {
		return nil, errors.New("model: CodexHome must be an absolute path without newlines")
	}
	return &Client{kind: "codex-cli", command: command, codexHome: cfg.CodexHome, model: cfg.Model}, nil
}

// completeCLI uses Codex only as a model bridge.  Tool calls are returned to
// Beesuh; Codex is not allowed to execute them itself.
func (c *Client) completeCLI(ctx context.Context, in Request) (Response, error) {
	ctx, cancel := cliContext(ctx)
	defer cancel()
	work, err := os.MkdirTemp("", "beesuh-codex-")
	if err != nil {
		return Response{}, errors.New("model: create Codex work directory failed")
	}
	defer os.RemoveAll(work)
	schemaPath := filepath.Join(work, "response-schema.json")
	outputPath := filepath.Join(work, "response.json")
	if err := os.WriteFile(schemaPath, codexOutputSchema, 0o600); err != nil {
		return Response{}, errors.New("model: write Codex response schema failed")
	}

	prompt, err := codexPrompt(in)
	if err != nil {
		return Response{}, errors.New("model: encode Codex request failed")
	}
	args := []string{"exec", "--strict-config", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--output-schema", schemaPath, "--output-last-message", outputPath, "--json", "-m", c.model,
		"-c", "features.shell_tool=false", "-c", "features.unified_exec=false", "-c", "features.multi_agent=false", "-c", "features.apps=false", "-c", "features.plugins=false", "-c", "features.hooks=false", "-c", "features.browser_use=false", "-c", "features.computer_use=false", "-c", "features.image_generation=false", "-c", "features.browser_use_external=false", "-c", "features.in_app_browser=false", "-c", "features.in_app_chat=false", "-c", "features.in_app_local_automation=false", "-c", "features.remote_plugin=false", "-c", "features.skill_search=false", "-c", "features.skill_mcp_dependency_install=false", "-c", "features.code_mode_host=false", "-c", "features.view_image=false", "-c", "features.goals=false", "-c", "web_search='disabled'", "-c", "mcp_servers={}", "-c", "project_doc_max_bytes=0", "-"}
	cmd := cliCommand(ctx, c.command, args, c.codexHome)
	cmd.Dir = work
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr cappedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, errors.New("model: Codex CLI failed")
	}
	if stdout.exceeded || stderr.exceeded {
		return Response{}, errors.New("model: Codex CLI output exceeds 1 MiB")
	}
	b, err := readSmallFile(outputPath)
	if err != nil {
		return Response{}, err
	}
	return decodeCodexOutput(b, in.Tools)
}

func cliCommand(ctx context.Context, command string, args []string, codexHome string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = codexEnv(codexHome)
	cmd.WaitDelay = codexWaitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}

func cliContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

func codexPrompt(in Request) (string, error) {
	messages := append([]Message(nil), in.Messages...)
	for i := range messages {
		messages[i].ProviderState = nil
		messages[i].ProviderID = ""
	}
	request := struct {
		Instruction string          `json:"instruction"`
		Messages    []Message       `json:"messages"`
		Tools       []Tool          `json:"tools"`
		Schema      json.RawMessage `json:"final_content_schema,omitempty"`
		MaxTokens   int             `json:"max_tokens_hint,omitempty"`
	}{
		Instruction: "You are a model bridge. Do not run shell commands, browse, call MCP, use apps, or make any external side effect. Read the conversation and tools below. Return only the required JSON object. To request a tool, put it in tool_calls; Beesuh will execute it. content must be a string and, when final_content_schema is present, must be JSON text that satisfies it.",
		Messages:    messages, Tools: in.Tools, Schema: in.Schema, MaxTokens: in.MaxTokens,
	}
	b, err := json.Marshal(request)
	return string(b), err
}

func decodeCodexOutput(b []byte, tools []Tool) (Response, error) {
	var out struct {
		Content   *string `json:"content"`
		ToolCalls *[]struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"tool_calls"`
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return Response{}, errors.New("model: invalid Codex CLI response")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Response{}, errors.New("model: invalid Codex CLI response")
	}
	if out.Content == nil || out.ToolCalls == nil || (len(*out.ToolCalls) == 0 && strings.TrimSpace(*out.Content) == "") {
		return Response{}, errors.New("model: invalid Codex CLI response")
	}
	allowed := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		allowed[tool.Function.Name] = struct{}{}
	}
	calls := make([]ToolCall, len(*out.ToolCalls))
	for i, call := range *out.ToolCalls {
		if _, ok := allowed[call.Name]; !ok {
			return Response{}, errors.New("model: Codex CLI requested an unknown tool")
		}
		calls[i] = ToolCall{ID: call.ID, Type: "function", Function: FunctionCall{Name: call.Name, Arguments: call.Arguments}}
	}
	if err := validateToolCalls(calls); err != nil {
		return Response{}, err
	}
	finish := "stop"
	if len(calls) != 0 {
		finish = "tool_calls"
	}
	return Response{Message: Message{Role: "assistant", Content: *out.Content, ToolCalls: calls}, FinishReason: finish}, nil
}

func readSmallFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, errors.New("model: Codex CLI produced no response")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResponseBytes+1))
	if err != nil || len(b) > maxResponseBytes {
		return nil, errors.New("model: Codex CLI response exceeds 1 MiB")
	}
	return b, nil
}

func codexEnv(codexHome string) []string {
	keys := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "CODEX_HOME": codexHome == ""}
	env := make([]string, 0, len(keys)+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if keys[key] {
			env = append(env, entry)
		}
	}
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	return env
}

type cappedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxResponseBytes {
		b.exceeded = true
		return 0, fmt.Errorf("output limit")
	}
	return b.Buffer.Write(p)
}

var codexOutputSchema = []byte(`{"type":"object","additionalProperties":false,"required":["content","tool_calls"],"properties":{"content":{"type":"string"},"tool_calls":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","name","arguments"],"properties":{"id":{"type":"string","minLength":1},"name":{"type":"string","minLength":1},"arguments":{"type":"string"}}}}}}`)
