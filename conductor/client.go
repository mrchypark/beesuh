// Package conductor is the narrow client for the Conductor execution API.
package conductor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/mrchypark/beesuh/auth"
)

const (
	maxBodyBytes   = 16 << 10
	requestTimeout = 10 * time.Second
)

// MaxIDBytes is the maximum size of an execution ID accepted by Emit and GetExecution.
const MaxIDBytes = 256

// Config configures a Conductor API client.
type Config struct {
	// RequireScope is an explicit contract with a gateway that authenticates this
	// client and enforces user/workspace/connection scope for events AND execution reads.
	RequireScope  bool
	BaseURL       string
	Token         string
	AllowedEvents []string
	// Bindings maps approved tool event names to registered execution handlers.
	Bindings   map[string]Binding
	HTTPClient *http.Client
}

// Binding is operator configuration, never a model-supplied route.
type Binding struct {
	HandlerKey     string `json:"handler_key"`
	HandlerVersion string `json:"handler_version"`
	OperationKey   string `json:"operation_key"`
}

// Client queues approved handler executions and reads their status.
type Client struct {
	requireScope bool
	baseURL      *url.URL
	token        string
	allowed      map[string]Binding
	http         *http.Client
}

// New validates Config and returns a client that never follows redirects.
func New(config Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("conductor base URL is invalid")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("conductor base URL must use http or https")
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("conductor base URL must not contain credentials, a query, or a fragment")
	}
	if strings.ContainsAny(config.Token, "\r\n\x00") {
		return nil, errors.New("conductor token is invalid")
	}

	allowed := make(map[string]Binding, len(config.AllowedEvents))
	for _, name := range config.AllowedEvents {
		if name != strings.TrimSpace(name) || validID(name, "allowed event name") != nil {
			return nil, errors.New("conductor allowed event name is invalid")
		}
		binding, ok := config.Bindings[name]
		if !ok {
			return nil, errors.New("conductor event requires an explicit handler binding")
		}
		for _, value := range []string{binding.HandlerKey, binding.HandlerVersion, binding.OperationKey} {
			if validID(value, "handler binding") != nil {
				return nil, errors.New("conductor handler binding is invalid")
			}
		}
		allowed[name] = binding
	}
	if len(allowed) == 0 {
		return nil, errors.New("conductor requires at least one allowed event")
	}

	httpClient := http.DefaultClient
	if config.HTTPClient != nil {
		httpClient = config.HTTPClient
	}
	copy := *httpClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copy.Timeout <= 0 || copy.Timeout > requestTimeout {
		copy.Timeout = requestTimeout
	}
	return &Client{baseURL: base, token: config.Token, allowed: allowed, http: &copy, requireScope: config.RequireScope}, nil
}

// Emit posts one allowlisted event. data must be a JSON object.
func (c *Client) Emit(ctx context.Context, workspace, name string, data json.RawMessage, idempotencyKey string) (json.RawMessage, error) {
	body, err := c.eventBody(ctx, workspace, name, data, idempotencyKey)
	if err != nil {
		return nil, err
	}
	output, err := c.request(ctx, http.MethodPost, "/v1/events", body)
	if err != nil {
		return nil, err
	}
	var ack struct {
		Accepted bool `json:"accepted"`
		Events   []struct {
			ExecutionID string `json:"execution_id"`
			RequestID   string `json:"execution_request_id"`
		} `json:"events"`
	}
	if json.Unmarshal(output, &ack) != nil || !ack.Accepted || len(ack.Events) != 1 || ack.Events[0].RequestID != idempotencyKey || validID(ack.Events[0].ExecutionID, "execution ID") != nil {
		return nil, errors.New("conductor execution acknowledgement is invalid")
	}
	return json.Marshal(struct {
		ExecutionID string `json:"execution_id"`
	}{ExecutionID: ack.Events[0].ExecutionID})
}

// ValidateEmit checks the complete request before the runtime records an effect intent.
func (c *Client) ValidateEmit(ctx context.Context, workspace, name string, data json.RawMessage, key string) error {
	_, err := c.eventBody(ctx, workspace, name, data, key)
	return err
}

func (c *Client) eventBody(ctx context.Context, workspace, name string, data json.RawMessage, idempotencyKey string) ([]byte, error) {
	if _, scoped := ctx.Value(executionScopeKey{}).(ExecutionScope); c.requireScope && !scoped {
		return nil, errors.New("conductor execution scope is required")
	}
	binding, ok := c.allowed[name]
	if !ok {
		return nil, errors.New("conductor event is not allowed")
	}
	if err := validID(idempotencyKey, "idempotency key"); err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxBodyBytes {
		return nil, errors.New("conductor event data is invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("conductor event data must be a JSON object")
	}
	if err := validID(workspace, "workspace ID"); err != nil {
		return nil, err
	}
	if scope, ok := ctx.Value(executionScopeKey{}).(ExecutionScope); ok && scope.WorkspaceID != workspace {
		return nil, errors.New("conductor workspace does not match execution scope")
	}
	for _, key := range []string{"_compos", "subject_ref"} {
		if _, exists := object[key]; exists {
			return nil, errors.New("conductor event data contains reserved metadata")
		}
	}
	link, err := json.Marshal(map[string]any{
		"workspace_id": workspace, "execution_request_id": idempotencyKey,
		"handler_key": binding.HandlerKey, "handler_version": binding.HandlerVersion,
		"operation_key": binding.OperationKey, "actor": map[string]string{"type": "service", "id": "beesuh"},
	})
	if err != nil {
		return nil, err
	}
	object["_compos"] = link
	data, err = json.Marshal(object)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Events []struct {
			Name           string          `json:"name"`
			Data           json.RawMessage `json:"data"`
			IdempotencyKey string          `json:"idempotency_key"`
		} `json:"events"`
	}{Events: []struct {
		Name           string          `json:"name"`
		Data           json.RawMessage `json:"data"`
		IdempotencyKey string          `json:"idempotency_key"`
	}{{Name: name, Data: data, IdempotencyKey: idempotencyKey}}})
	if err != nil || len(body) > maxBodyBytes {
		return nil, errors.New("conductor event request is too large")
	}
	return body, nil
}

// GetExecution retrieves an execution_id returned by Emit.
func (c *Client) GetExecution(ctx context.Context, id string) (json.RawMessage, error) {
	if err := validID(id, "execution ID"); err != nil {
		return nil, err
	}
	return c.request(ctx, http.MethodGet, "/v1/executions/"+id, nil)
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("conductor request is invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	scope, scoped := ctx.Value(executionScopeKey{}).(ExecutionScope)
	if c.requireScope && !scoped {
		return nil, errors.New("conductor execution scope is required")
	}
	if scoped {
		for _, value := range []string{scope.UserID, scope.WorkspaceID, scope.ConnectionID} {
			if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
				return nil, errors.New("conductor execution scope is invalid")
			}
		}
		request.Header.Set("X-Beesuh-User-ID", scope.UserID)
		request.Header.Set("X-Beesuh-Workspace-ID", scope.WorkspaceID)
		request.Header.Set("X-Beesuh-Connection-ID", scope.ConnectionID)
	}
	if c.token != "" && request.Header.Get("Authorization") == "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, auth.Sanitize(err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &auth.HTTPError{StatusCode: response.StatusCode}
	}
	if method == http.MethodPost && response.StatusCode != http.StatusAccepted {
		return nil, errors.New("conductor execution was not acknowledged with 202")
	}
	output, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return nil, errors.New("conductor response could not be read")
	}
	if len(output) > maxBodyBytes {
		return nil, errors.New("conductor response exceeds 16 KiB")
	}
	if !json.Valid(output) {
		return nil, errors.New("conductor response is not valid JSON")
	}
	return json.RawMessage(output), nil
}

func validID(value, label string) error {
	if strings.TrimSpace(value) == "" || len(value) > MaxIDBytes || value == "." || value == ".." || strings.ContainsAny(value, "/\\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("conductor %s is invalid", label)
	}
	return nil
}

// ExecutionScope comes from the authenticated service; never from tool arguments.
// A scope-aware gateway must authorize this scope using the service credential.
// These headers do not confer authority on a server that does not enforce it.
type ExecutionScope struct{ UserID, WorkspaceID, ConnectionID string }
type executionScopeKey struct{}

func WithExecutionScope(ctx context.Context, scope ExecutionScope) context.Context {
	return context.WithValue(ctx, executionScopeKey{}, scope)
}

// GetRun is retained for existing callers; id must be a Conductor execution ID.
func (c *Client) GetRun(ctx context.Context, id string) (json.RawMessage, error) {
	return c.GetExecution(ctx, id)
}
