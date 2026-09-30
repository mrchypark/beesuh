// Package compos converts the existing Compos agent outbox data into Beesuh requests.
package compos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/opencode"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const maxInputBytes = 256 << 10
const maxDurationSeconds = int64((1<<63 - 1) / int64(time.Second))

type deniedLoader struct{}

func (deniedLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference is not allowed: %s", url)
}

// Decode reads exactly one Compos agent outbox data object. It does not accept
// the outer Conductor events envelope.
func Decode(r io.Reader, sessionKey string) (beesuh.Request, error) {
	if strings.TrimSpace(sessionKey) == "" {
		return beesuh.Request{}, errors.New("compos: session key is required")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxInputBytes+1))
	if err != nil {
		return beesuh.Request{}, fmt.Errorf("compos: read input: %w", err)
	}
	if len(raw) > maxInputBytes {
		return beesuh.Request{}, errors.New("compos: input exceeds 256 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var data map[string]any
	if err := decoder.Decode(&data); err != nil {
		return beesuh.Request{}, fmt.Errorf("compos: invalid input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return beesuh.Request{}, errors.New("compos: input must contain exactly one JSON value")
		}
		return beesuh.Request{}, fmt.Errorf("compos: invalid trailing input: %w", err)
	}
	agent, err := object(data, "agent_definition")
	if err != nil {
		return beesuh.Request{}, err
	}
	instructions, ok := agent["instructions"].(string)
	if !ok || strings.TrimSpace(instructions) == "" {
		return beesuh.Request{}, errors.New("compos: agent_definition.instructions is required")
	}
	for _, field := range []string{"model_policy", "context_policy", "permissions", "memory_policy"} {
		if value, exists := agent[field]; exists {
			policy, ok := value.(map[string]any)
			if !ok {
				return beesuh.Request{}, fmt.Errorf("compos: agent_definition.%s must be an object", field)
			}
			if len(policy) != 0 {
				return beesuh.Request{}, fmt.Errorf("compos: agent_definition.%s is unsupported", field)
			}
		}
	}
	if value, exists := agent["tools"]; exists {
		tools, ok := value.([]any)
		if !ok {
			return beesuh.Request{}, errors.New("compos: agent_definition.tools must be a list")
		}
		if len(tools) != 0 {
			return beesuh.Request{}, errors.New("compos: agent_definition.tools is unsupported")
		}
	}

	task, err := object(data, "task_definition")
	if err != nil {
		return beesuh.Request{}, err
	}
	inputSchema, err := schema(task, "input_schema")
	if err != nil {
		return beesuh.Request{}, err
	}
	outputSchema, err := schema(task, "output_schema")
	if err != nil {
		return beesuh.Request{}, err
	}
	input, err := object(data, "input")
	if err != nil {
		return beesuh.Request{}, err
	}
	if err := validate(inputSchema, input); err != nil {
		return beesuh.Request{}, fmt.Errorf("compos: input does not match task_definition.input_schema: %w", err)
	}
	subject, err := object(data, "subject_ref")
	if err != nil {
		return beesuh.Request{}, err
	}
	meta, err := object(data, "_compos")
	if err != nil {
		return beesuh.Request{}, err
	}
	workspaceID, err := requiredString(meta, "workspace_id")
	if err != nil {
		return beesuh.Request{}, err
	}
	requestID, err := requiredString(meta, "execution_request_id")
	if err != nil {
		return beesuh.Request{}, err
	}
	if hasValue(meta, "workflow_step_id") || hasValue(meta, "workflow_route_id") {
		return beesuh.Request{}, errors.New("compos: workflow routes are unsupported without a route output schema")
	}
	if err := validateRetry(meta["retry_policy"]); err != nil {
		return beesuh.Request{}, err
	}
	timeout, err := timeoutValue(meta["timeout_seconds"])
	if err != nil {
		return beesuh.Request{}, err
	}

	text, err := json.Marshal(map[string]any{
		"task_definition": task,
		"input":           input,
		"subject_ref":     subject,
	})
	if err != nil {
		return beesuh.Request{}, fmt.Errorf("compos: encode prompt: %w", err)
	}
	return beesuh.Request{
		WorkspaceID: workspaceID,
		SessionKey:  sessionKey,
		RequestID:   requestID,
		Timeout:     timeout,
		Prompt: opencode.Prompt{
			System: instructions,
			Text:   string(text),
			Schema: outputSchema,
		},
	}, nil
}

func object(parent map[string]any, key string) (map[string]any, error) {
	value, ok := parent[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("compos: %s must be a JSON object", key)
	}
	return value, nil
}

func schema(task map[string]any, key string) (json.RawMessage, error) {
	value, ok := task[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("compos: task_definition.%s must be a JSON object", key)
	}
	if kind, _ := value["type"].(string); kind != "object" {
		return nil, fmt.Errorf("compos: task_definition.%s must have type object", key)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("compos: encode %s: %w", key, err)
	}
	if err := validateSchema(raw); err != nil {
		return nil, fmt.Errorf("compos: invalid task_definition.%s: %w", key, err)
	}
	return raw, nil
}

func validateSchema(raw json.RawMessage) error {
	document, err := schemaDocument(raw)
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(deniedLoader{})
	if err := compiler.AddResource("mem://compos/schema.json", document); err != nil {
		return err
	}
	_, err = compiler.Compile("mem://compos/schema.json")
	return err
}

func validate(raw json.RawMessage, value any) error {
	document, err := schemaDocument(raw)
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(deniedLoader{})
	if err := compiler.AddResource("mem://compos/schema.json", document); err != nil {
		return err
	}
	schema, err := compiler.Compile("mem://compos/schema.json")
	if err != nil {
		return err
	}
	return schema.Validate(value)
}

func schemaDocument(raw json.RawMessage) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

func requiredString(value map[string]any, key string) (string, error) {
	text, ok := value[key].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("compos: _compos.%s is required", key)
	}
	return text, nil
}

func hasValue(value map[string]any, key string) bool {
	item, exists := value[key]
	if !exists || item == nil {
		return false
	}
	text, ok := item.(string)
	return !ok || strings.TrimSpace(text) != ""
}

func validateRetry(value any) error {
	if value == nil {
		return nil
	}
	policy, ok := value.(map[string]any)
	if !ok {
		return errors.New("compos: _compos.retry_policy must be an object")
	}
	attempts, ok := policy["max_attempts"].(json.Number)
	if !ok {
		return errors.New("compos: _compos.retry_policy.max_attempts is required")
	}
	count, err := attempts.Int64()
	if err != nil || count != 1 {
		return errors.New("compos: retry_policy.max_attempts greater than 1 is unsupported")
	}
	return nil
}

func timeoutValue(value any) (time.Duration, error) {
	if value == nil {
		return 0, nil
	}
	seconds, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("compos: _compos.timeout_seconds must be an integer")
	}
	count, err := seconds.Int64()
	if err != nil || count < 1 || count > maxDurationSeconds {
		return 0, errors.New("compos: _compos.timeout_seconds must be a positive integer")
	}
	return time.Duration(count) * time.Second, nil
}
