package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// completeAnthropic implements Messages. Anthropic has no strict JSON-schema
// output mode, so the schema is an instruction and the runtime validates text.
func (c *Client) completeAnthropic(ctx context.Context, in Request) (Response, error) {
	system, messages, err := anthropicMessages(in.Messages)
	if err != nil {
		return Response{}, err
	}
	if len(in.Schema) != 0 {
		system = append(system, "Return only a JSON object that validates against this JSON Schema:\n"+string(in.Schema))
	}
	body := map[string]any{"model": c.model, "messages": messages, "max_tokens": in.MaxTokens}
	if in.MaxTokens == 0 {
		body["max_tokens"] = 4096
	}
	if len(system) != 0 {
		body["system"] = strings.Join(system, "\n")
	}
	if len(in.Tools) != 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, tool := range in.Tools {
			tools = append(tools, map[string]any{"name": tool.Function.Name, "description": tool.Function.Description, "input_schema": json.RawMessage(tool.Function.Parameters)})
		}
		body["tools"] = tools
	}
	var wire anthropicWire
	if err := c.postJSON(ctx, "messages", in.SessionID, body, &wire, func(req *http.Request) {
		if c.apiKey != "" {
			req.Header.Set("x-api-key", c.apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	}); err != nil {
		return Response{}, err
	}
	if wire.Role != "assistant" || (wire.StopReason != "end_turn" && wire.StopReason != "tool_use") {
		return Response{}, errors.New("model: unsupported anthropic completion")
	}
	out := Response{Message: Message{Role: "assistant"}, Usage: Usage{PromptTokens: wire.Usage.InputTokens, CompletionTokens: wire.Usage.OutputTokens, TotalTokens: wire.Usage.InputTokens + wire.Usage.OutputTokens}}
	for _, block := range wire.Content {
		switch block.Type {
		case "text":
			out.Message.Content += block.Text
		case "tool_use":
			args, err := json.Marshal(block.Input)
			if err != nil {
				return Response{}, errors.New("model: invalid anthropic tool input")
			}
			out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{ID: block.ID, Type: "function", Function: FunctionCall{Name: block.Name, Arguments: string(args)}})
		default:
			return Response{}, errors.New("model: unsupported anthropic content")
		}
	}
	if wire.StopReason == "tool_use" {
		if len(out.Message.ToolCalls) == 0 {
			return Response{}, errors.New("model: anthropic tool completion has no tool calls")
		}
		out.FinishReason = "tool_calls"
	} else if len(out.Message.ToolCalls) != 0 {
		return Response{}, errors.New("model: anthropic stopped completion contains tool calls")
	} else {
		out.FinishReason = "stop"
	}
	if err := validateToolCalls(out.Message.ToolCalls); err != nil {
		return Response{}, err
	}
	return out, nil
}

type anthropicWire struct {
	Role       string `json:"role"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Input any    `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func anthropicMessages(input []Message) ([]string, []any, error) {
	var system []string
	messages := make([]any, 0, len(input))
	var toolResults []map[string]string
	flushToolResults := func() {
		if len(toolResults) != 0 {
			messages = append(messages, map[string]any{"role": "user", "content": toolResults})
			toolResults = nil
		}
	}
	for _, message := range input {
		switch message.Role {
		case "system":
			system = append(system, message.Content)
		case "user":
			flushToolResults()
			messages = append(messages, map[string]any{"role": "user", "content": message.Content})
		case "assistant":
			flushToolResults()
			content := make([]any, 0, 1+len(message.ToolCalls))
			if message.Content != "" {
				content = append(content, map[string]string{"type": "text", "text": message.Content})
			}
			for _, call := range message.ToolCalls {
				var args json.RawMessage
				if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
					return nil, nil, errors.New("model: invalid anthropic tool call")
				}
				content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": args})
			}
			if len(content) == 0 {
				return nil, nil, errors.New("model: empty anthropic assistant message")
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": content})
		case "tool":
			toolResults = append(toolResults, map[string]string{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.Content})
		default:
			return nil, nil, errors.New("model: invalid anthropic message role")
		}
	}
	flushToolResults()
	return system, messages, nil
}
