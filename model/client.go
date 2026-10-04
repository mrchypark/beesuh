// Package model provides a small OpenAI-compatible Chat Completions client.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/mrchypark/beesuh/auth"
)

const (
	maxResponseBytes = 1 << 20
	defaultTimeout   = 60 * time.Second
)

type Config struct {
	Kind           string          `json:"kind,omitempty"`
	API            string          `json:"api,omitempty"`
	BaseURL        string          `json:"base_url,omitempty"`
	Model          string          `json:"model"`
	APIKeyEnv      string          `json:"api_key_env,omitempty"`
	Command        string          `json:"command,omitempty"`
	CodexHome      string          `json:"codex_home,omitempty"`
	APIKey         string          `json:"-"`
	HTTPClient     *http.Client    `json:"-"`
	CodexAuth      CodexAuthSource `json:"-"`
	CodexAccountID string          `json:"-"`
}

type Client struct {
	kind, api, command, codexHome string
	codexAuth                     CodexAuthSource
	codexAccountID                string
	baseURL                       *url.URL
	apiKey                        string
	model                         string
	http                          *http.Client
}

type Request struct {
	SessionID string
	Messages  []Message
	Tools     []Tool
	Schema    json.RawMessage
	MaxTokens int
}

type Message struct {
	ProviderState json.RawMessage `json:"provider_state,omitempty"`
	ProviderID    string          `json:"provider_id,omitempty"`
	Role          string          `json:"role"`
	Content       string          `json:"content"`
	ToolCalls     []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Response struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Usage        Usage   `json:"usage"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func New(cfg Config) (*Client, error) {
	if cfg.Kind == "" {
		cfg.Kind = "openai-compatible"
	}
	if cfg.Kind == "codex-app-server" {
		return newAppServer(cfg)
	}
	if cfg.CodexAuth != nil || cfg.CodexAccountID != "" {
		return nil, errors.New("model: external ChatGPT auth requires codex-app-server")
	}
	if cfg.Kind == "codex-cli" {
		return newCLI(cfg)
	}
	switch cfg.Kind {
	case "opencode-go":
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://opencode.ai/zen/go/v1"
		}
	case "opencode-zen":
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://opencode.ai/zen/v1"
		}
	case "codex-api":
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.openai.com/v1"
		}
		if cfg.API != "" && cfg.API != "responses" {
			return nil, errors.New("model: codex-api requires Responses API")
		}
		cfg.API = "responses"
	case "openai-compatible":
	default:
		return nil, errors.New("model: unsupported provider kind")
	}
	if cfg.API == "" {
		cfg.API = "chat-completions"
	}
	if cfg.API != "chat-completions" && cfg.API != "responses" && cfg.API != "anthropic" {
		return nil, errors.New("model: unsupported API protocol")
	}
	if cfg.Command != "" || cfg.CodexHome != "" {
		return nil, errors.New("model: command and codex_home require codex-cli")
	}

	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("model: BaseURL must be an absolute HTTP URL without credentials, query, or fragment")
	}
	if invalidText(cfg.APIKey) || invalidText(cfg.Model) || strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("model: APIKey and Model must not contain newlines; Model is required")
	}
	h := cfg.HTTPClient
	if h == nil {
		h = &http.Client{Timeout: defaultTimeout}
	}
	copy := *h
	if copy.Timeout <= 0 {
		copy.Timeout = defaultTimeout
	}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("model: redirects are disabled") }
	return &Client{kind: cfg.Kind, api: cfg.API, baseURL: u, apiKey: cfg.APIKey, model: cfg.Model, http: &copy}, nil
}

func (c *Client) Complete(ctx context.Context, in Request) (Response, error) {
	if err := validateRequest(in); err != nil {
		return Response{}, err
	}
	if c.kind == "codex-app-server" {
		return c.completeAppServer(ctx, in)
	}
	if c.kind == "codex-cli" {
		return c.completeCLI(ctx, in)
	}
	if c.api == "responses" {
		return c.completeResponses(ctx, in)
	}
	if c.api == "anthropic" {
		return c.completeAnthropic(ctx, in)
	}
	messages := append([]Message(nil), in.Messages...)
	for i := range messages {
		messages[i].ProviderState = nil
		messages[i].ProviderID = ""
	}
	body := map[string]any{"model": c.model, "messages": messages}
	if len(in.Tools) != 0 {
		body["tools"] = in.Tools
	}
	if len(in.Schema) != 0 {
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "response", "schema": json.RawMessage(in.Schema)},
		}
	}
	if in.MaxTokens != 0 {
		body["max_tokens"] = in.MaxTokens
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("model: encode request: %w", err)
	}
	u := *c.baseURL
	u.Path = path.Join("/", strings.TrimSuffix(c.baseURL.Path, "/"), "chat", "completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return Response{}, fmt.Errorf("model: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "beesuh/0.1")
	if c.kind == "opencode-go" || c.kind == "opencode-zen" {
		if strings.TrimSpace(in.SessionID) == "" || invalidText(in.SessionID) {
			return Response{}, errors.New("model: OpenCode provider requires a session ID")
		}
		req.Header.Set("x-opencode-session", in.SessionID)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, auth.Sanitize(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, &auth.HTTPError{StatusCode: resp.StatusCode}
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, errors.New("model: read response failed")
	}
	if len(b) > maxResponseBytes {
		return Response{}, errors.New("model: response exceeds 1 MiB")
	}
	var wire struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return Response{}, errors.New("model: invalid JSON response")
	}
	if len(wire.Choices) != 1 {
		return Response{}, errors.New("model: response must contain exactly one choice")
	}
	out := Response{Message: wire.Choices[0].Message, FinishReason: wire.Choices[0].FinishReason, Usage: unknownUsage()}
	if wire.Usage != nil {
		out.Usage = measuredUsage(wire.Usage.PromptTokens, wire.Usage.CompletionTokens, wire.Usage.TotalTokens)
	}
	if out.Message.Role != "assistant" || (out.FinishReason != "stop" && out.FinishReason != "tool_calls") {
		return Response{}, errors.New("model: unsupported completion response")
	}
	if out.FinishReason == "tool_calls" && len(out.Message.ToolCalls) == 0 {
		return Response{}, errors.New("model: tool completion has no tool calls")
	}
	if out.FinishReason == "stop" && len(out.Message.ToolCalls) != 0 {
		return Response{}, errors.New("model: stopped completion contains tool calls")
	}
	if err := validateToolCalls(out.Message.ToolCalls); err != nil {
		return Response{}, err
	}
	return out, nil
}

func validateRequest(in Request) error {
	if len(in.Messages) == 0 || in.MaxTokens < 0 {
		return errors.New("model: messages are required and MaxTokens cannot be negative")
	}
	if len(in.Schema) != 0 && (!json.Valid(in.Schema) || !isJSONObject(in.Schema)) {
		return errors.New("model: Schema must be a JSON object")
	}
	seen := make(map[string]struct{}, len(in.Tools))
	for _, tool := range in.Tools {
		if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" || invalidText(tool.Function.Name) || !json.Valid(tool.Function.Parameters) || !isJSONObject(tool.Function.Parameters) {
			return errors.New("model: invalid function tool")
		}
		if _, ok := seen[tool.Function.Name]; ok {
			return errors.New("model: duplicate function tool")
		}
		seen[tool.Function.Name] = struct{}{}
	}
	for _, message := range in.Messages {
		switch message.Role {
		case "system", "user", "assistant", "tool":
		default:
			return errors.New("model: invalid message role")
		}
		if message.Role == "tool" && strings.TrimSpace(message.ToolCallID) == "" {
			return errors.New("model: tool message requires ToolCallID")
		}
		if err := validateToolCalls(message.ToolCalls); err != nil {
			return err
		}
	}
	return nil
}

func validateToolCalls(calls []ToolCall) error {
	if len(calls) > 16 {
		return errors.New("model: too many tool calls")
	}
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" || call.Type != "function" || strings.TrimSpace(call.Function.Name) == "" || !json.Valid([]byte(call.Function.Arguments)) || !isJSONObject(json.RawMessage(call.Function.Arguments)) {
			return errors.New("model: invalid tool call")
		}
		if _, ok := seen[call.ID]; ok {
			return errors.New("model: duplicate tool call ID")
		}
		seen[call.ID] = struct{}{}
	}
	return nil
}

func invalidText(s string) bool { return strings.ContainsAny(s, "\r\n\x00") }

func isJSONObject(raw json.RawMessage) bool {
	var value any
	return json.Unmarshal(raw, &value) == nil && func() bool { _, ok := value.(map[string]any); return ok }()
}
