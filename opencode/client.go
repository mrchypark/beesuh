// Package opencode provides the small OpenCode HTTP API surface used by Beesuh.
// It applies a 60-second deadline when callers do not provide one.
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
	"time"
)

const (
	maxResponseBytes = 8 << 20
	// defaultTimeout bounds requests made with a deadline-less context. It is
	// deliberately long enough for a healthy OpenCode request to start.
	defaultTimeout = 60 * time.Second
)

type Config struct {
	BaseURL   string
	Directory string
	Username  string
	Password  string
	// Timeout bounds requests made with a context that has no deadline. A
	// zero value uses the default 60-second timeout. A caller-provided
	// deadline always takes precedence.
	Timeout    time.Duration
	HTTPClient *http.Client
}

type Client struct {
	baseURL   *url.URL
	directory string
	username  string
	password  string
	timeout   time.Duration
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
	if cfg.Timeout < 0 {
		return nil, errors.New("opencode: Timeout must not be negative")
	}
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
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	return &Client{baseURL: u, directory: cfg.Directory, username: cfg.Username, password: cfg.Password, timeout: timeout, http: &copy}, nil
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

// DeleteSession releases a remote session that Beesuh created but could not
// track. Without it a failed local mapping insert leaks an untracked session
// that no prompt, abort, or retention policy can ever reach. The endpoint
// replies with a boolean that reports whether the session was removed, so a
// false result is a failed reclaim rather than a silent success.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	if !validSessionID(id) {
		return errors.New("opencode: invalid session ID")
	}
	var ok bool
	if err := c.call(ctx, http.MethodDelete, "/session/"+id, nil, true, &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("opencode: delete was not accepted")
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

// ListSessionsByTitle returns the IDs of the sessions whose title matches
// exactly, newest first. A session Beesuh is about to create is titled with an
// unguessable ownership token, so an exact match is proof the session is that
// runtime's own orphan whose create response was lost to a cancelled or expired
// request. Without this the session would exist on the server with no local
// record and could never be reached by ID again.
func (c *Client) ListSessionsByTitle(ctx context.Context, title string) ([]string, error) {
	if strings.TrimSpace(title) == "" {
		return nil, errors.New("opencode: session title is required")
	}
	var out []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := c.call(ctx, http.MethodGet, "/session", nil, true, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out))
	for _, s := range out {
		if s.Title == title && validSessionID(s.ID) {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
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
	if ctx != nil {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.timeout)
			defer cancel()
		}
	}
	u := *c.baseURL
	u.Path = path.Join("/", strings.TrimSuffix(c.baseURL.Path, "/"), endpoint)
	if directory {
		q := u.Query()
		q.Set("directory", c.directory)
		u.RawQuery = q.Encode()
	}
	var r io.ReadCloser
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opencode: encode request: %w", err)
		}
		r = io.NopCloser(bytes.NewReader(b))
	}
	req, err := newRequest(ctx, method, u.String(), r)
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
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("opencode: invalid JSON response: %w", err)
	}
	return nil
}

func newRequest(ctx context.Context, method, url string, body io.ReadCloser) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil && body != nil {
		_ = body.Close()
	}
	return req, err
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
