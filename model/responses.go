package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/mrchypark/beesuh/auth"
)

// completeResponses implements the public OpenAI Responses API. ProviderState
// is the prior response.output array; preserving it keeps encrypted reasoning
// items usable on the next turn without exposing them to the runtime.
func (c *Client) completeResponses(ctx context.Context, in Request) (Response, error) {
	input, err := c.responsesInput(in.Messages)
	if err != nil {
		return Response{}, err
	}
	body := map[string]any{
		"model":   c.model,
		"input":   input,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}
	if len(in.Tools) != 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, tool := range in.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": tool.Function.Name, "description": tool.Function.Description, "parameters": json.RawMessage(tool.Function.Parameters)})
		}
		body["tools"] = tools
	}
	if len(in.Schema) != 0 {
		body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "response", "schema": json.RawMessage(in.Schema)}}
	}
	if in.MaxTokens != 0 {
		body["max_output_tokens"] = in.MaxTokens
	}
	var wire responsesWire
	if err := c.postJSON(ctx, "responses", in.SessionID, body, &wire, nil); err != nil {
		return Response{}, err
	}
	if wire.Status != "completed" || wire.Error != nil || wire.IncompleteDetails != nil {
		return Response{}, errors.New("model: incomplete responses completion")
	}
	state, err := json.Marshal(wire.Output)
	if err != nil || !validResponsesState(state) {
		return Response{}, errors.New("model: invalid responses output")
	}
	var out Response
	out.Message = Message{Role: "assistant", ProviderState: state}
	for _, raw := range wire.Output {
		var item responsesOutput
		if json.Unmarshal(raw, &item) != nil {
			return Response{}, errors.New("model: invalid responses output")
		}
		switch item.Type {
		case "message":
			if item.Role != "assistant" || item.Status == "incomplete" || item.Status == "failed" || item.Status == "refused" {
				return Response{}, errors.New("model: unsupported responses message")
			}
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					out.Message.Content += content.Text
				case "refusal":
					return Response{}, errors.New("model: responses refusal")
				default:
					return Response{}, errors.New("model: unsupported responses content")
				}
			}
		case "function_call":
			if item.Status != "" && item.Status != "completed" {
				return Response{}, errors.New("model: incomplete responses function call")
			}
			out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{ID: item.CallID, Type: "function", Function: FunctionCall{Name: item.Name, Arguments: item.Arguments}})
		case "reasoning":
			// Opaque state is retained above. It is intentionally not prompt text.
		default:
			return Response{}, errors.New("model: unsupported responses output item")
		}
	}
	if len(out.Message.ToolCalls) > 0 {
		out.FinishReason = "tool_calls"
	} else {
		out.FinishReason = "stop"
	}
	if err := validateToolCalls(out.Message.ToolCalls); err != nil {
		return Response{}, err
	}
	out.Usage = Usage{PromptTokens: wire.Usage.InputTokens, CompletionTokens: wire.Usage.OutputTokens, TotalTokens: wire.Usage.TotalTokens}
	return out, nil
}

type responsesWire struct {
	Status            string            `json:"status"`
	Error             any               `json:"error"`
	IncompleteDetails any               `json:"incomplete_details"`
	Output            []json.RawMessage `json:"output"`
	Usage             struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type responsesOutput struct {
	Type      string `json:"type"`
	Status    string `json:"status"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (c *Client) responsesInput(messages []Message) ([]any, error) {
	out := make([]any, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "system":
			out = append(out, map[string]any{"role": "developer", "content": []map[string]string{{"type": "input_text", "text": message.Content}}})
		case "user":
			out = append(out, map[string]any{"role": "user", "content": []map[string]string{{"type": "input_text", "text": message.Content}}})
		case "assistant":
			if len(message.ProviderState) != 0 {
				var state []json.RawMessage
				if err := json.Unmarshal(message.ProviderState, &state); err != nil || !validResponsesState(message.ProviderState) {
					return nil, errors.New("model: invalid responses provider state")
				}
				for _, item := range state {
					out = append(out, item)
				}
				continue
			}
			if message.Content != "" {
				out = append(out, map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": message.Content}}})
			}
			for _, call := range message.ToolCalls {
				out = append(out, map[string]string{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		case "tool":
			out = append(out, map[string]string{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content})
		default:
			return nil, errors.New("model: invalid responses message role")
		}
	}
	return out, nil
}

func validResponsesState(raw json.RawMessage) bool {
	var items []struct {
		Type string `json:"type"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		switch item.Type {
		case "message", "function_call", "reasoning":
		default:
			return false
		}
	}
	return true
}

func (c *Client) postJSON(ctx context.Context, endpoint, sessionID string, body any, target any, headers func(*http.Request)) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("model: encode request: %w", err)
	}
	u := *c.baseURL
	u.Path = path.Join("/", strings.TrimSuffix(c.baseURL.Path, "/"), endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("model: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "beesuh/0.1")
	if c.kind == "opencode-go" || c.kind == "opencode-zen" {
		if strings.TrimSpace(sessionID) == "" || invalidText(sessionID) {
			return errors.New("model: OpenCode provider requires a session ID")
		}
		req.Header.Set("x-opencode-session", sessionID)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if headers != nil {
		headers(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return auth.Sanitize(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &auth.HTTPError{StatusCode: resp.StatusCode}
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("model: read response failed")
	}
	if len(b) > maxResponseBytes {
		return errors.New("model: response exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if decoder.Decode(target) != nil {
		return errors.New("model: invalid JSON response")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("model: invalid JSON response")
	}
	return nil
}
