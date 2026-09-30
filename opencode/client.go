// Package opencode provides the small OpenCode HTTP API surface used by Beesuh.
package opencode

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
)

const maxResponseBytes = 8 << 20

type Config struct {
	BaseURL    string
	Directory  string
	Username   string
	Password   string
	HTTPClient *http.Client
}

type Client struct {
	baseURL   *url.URL
	directory string
	username  string
	password  string
	http      *http.Client
}

type PermissionRule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

type Model struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

type Prompt struct {
	MessageID string
	System    string
	Text      string
	Model     *Model
	Schema    json.RawMessage
	NoReply   bool
}

type Message struct {
	Info  MessageInfo `json:"info"`
	Parts []Part      `json:"parts"`
}

type MessageInfo struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Role       string          `json:"role"`
	Error      json.RawMessage `json:"error"`
	Structured json.RawMessage `json:"structured"`
	Finish     string          `json:"finish"`
	Time       struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type Part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (m Message) StructuredOutput() (json.RawMessage, error) {
	if len(m.Info.Structured) == 0 || bytes.Equal(m.Info.Structured, []byte("null")) {
		return nil, errors.New("opencode: structured output is absent")
	}
	return m.Info.Structured, nil
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("opencode: BaseURL must be an absolute HTTP URL without credentials, query, or fragment")
	}
	if !strings.HasPrefix(cfg.Directory, "/") {
		return nil, errors.New("opencode: Directory must be absolute")
	}
	h := cfg.HTTPClient
	if h == nil {
		h = &http.Client{}
	}
	copy := *h
	copy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("opencode: redirects are disabled")
	}
	return &Client{baseURL: u, directory: cfg.Directory, username: cfg.Username, password: cfg.Password, http: &copy}, nil
}

func (c *Client) CreateSession(ctx context.Context, title string, permission []PermissionRule) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	body := map[string]any{"title": title}
	if permission != nil {
		body["permission"] = permission
	}
	err := c.call(ctx, http.MethodPost, "/session", body, true, &out)
	if err != nil {
		return "", err
	}
	if !validSessionID(out.ID) {
		return "", errors.New("opencode: invalid session ID in response")
	}
	return out.ID, nil
}

func (c *Client) GetSession(ctx context.Context, id string) error {
	if !validSessionID(id) {
		return errors.New("opencode: invalid session ID")
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodGet, "/session/"+id, nil, true, &out); err != nil {
		return err
	}
	if out.ID != id {
		return errors.New("opencode: returned session ID does not match request")
	}
	return nil
}

func (c *Client) Prompt(ctx context.Context, id string, p Prompt) (Message, error) {
	if !validSessionID(id) {
		return Message{}, errors.New("opencode: invalid session ID")
	}
	if len(p.Schema) != 0 && !json.Valid(p.Schema) {
		return Message{}, errors.New("opencode: Schema must be valid JSON")
	}
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": p.Text}}}
	if p.MessageID != "" {
		body["messageID"] = p.MessageID
	}
	if p.System != "" {
		body["system"] = p.System
	}
	if p.Model != nil {
		body["model"] = p.Model
	}
	if p.NoReply {
		body["noReply"] = true
	}
	if len(p.Schema) != 0 {
		body["format"] = map[string]any{"type": "json_schema", "schema": json.RawMessage(p.Schema)}
	}
	var out Message
	if err := c.call(ctx, http.MethodPost, "/session/"+id+"/message", body, true, &out); err != nil {
		return Message{}, err
	}
	if len(out.Info.Error) != 0 && !bytes.Equal(out.Info.Error, []byte("null")) {
		return Message{}, errors.New("opencode: message completed with an error")
	}
	return out, nil
}

func (c *Client) Abort(ctx context.Context, id string) error {
	if !validSessionID(id) {
		return errors.New("opencode: invalid session ID")
	}
	var ok bool
	if err := c.call(ctx, http.MethodPost, "/session/"+id+"/abort", nil, true, &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("opencode: abort was not accepted")
	}
	return nil
}

func (c *Client) Idle(ctx context.Context, id string) (bool, error) {
	if !validSessionID(id) {
		return false, errors.New("opencode: invalid session ID")
	}
	var status map[string]struct {
		Type string `json:"type"`
	}
	if err := c.call(ctx, http.MethodGet, "/session/status", nil, true, &status); err != nil {
		return false, err
	}
	s, ok := status[id]
	if ok {
		return s.Type == "idle", nil
	}
	if err := c.GetSession(ctx, id); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) Health(ctx context.Context) (string, error) {
	var out struct {
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}
	if err := c.call(ctx, http.MethodGet, "/global/health", nil, false, &out); err != nil {
		return "", err
	}
	if !out.Healthy {
		return "", errors.New("opencode: server is unhealthy")
	}
	return out.Version, nil
}

func (c *Client) call(ctx context.Context, method, endpoint string, body any, directory bool, out any) error {
	u := *c.baseURL
	u.Path = path.Join("/", strings.TrimSuffix(c.baseURL.Path, "/"), endpoint)
	if directory {
		q := u.Query()
		q.Set("directory", c.directory)
		u.RawQuery = q.Encode()
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opencode: encode request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return fmt.Errorf("opencode: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("opencode: unexpected HTTP status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("opencode: read response: %w", err)
	}
	if len(b) > maxResponseBytes {
		return errors.New("opencode: response exceeds 8 MiB")
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("opencode: invalid JSON response: %w", err)
	}
	return nil
}

func validSessionID(id string) bool {
	if !strings.HasPrefix(id, "ses") || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
