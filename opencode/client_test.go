package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestClientSessionAndPrompt(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("directory") != "/work" {
			t.Errorf("directory = %q", r.URL.Query().Get("directory"))
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != "user" || p != "pass" {
			t.Error("missing basic auth")
		}
		switch r.URL.Path {
		case "/session":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["title"] != "title" {
				t.Errorf("title = %#v", body["title"])
			}
			if _, ok := body["permission"]; !ok {
				t.Error("permission missing")
			}
			_, _ = w.Write([]byte(`{"id":"ses_1"}`))
		case "/session/ses_1/message":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["format"]; !ok {
				t.Error("format missing")
			}
			if _, ok := body["outputFormat"]; ok {
				t.Error("stale outputFormat sent")
			}
			_, _ = w.Write([]byte(`{"info":{"id":"msg_1","sessionID":"ses_1","role":"assistant","time":{"completed":2},"structured":{"answer":1}},"parts":[{"type":"text","text":"ok"}]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Directory: "/work", Username: "user", Password: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateSession(context.Background(), "title", []PermissionRule{{Permission: "read", Pattern: "*", Action: "allow"}})
	if err != nil || id != "ses_1" {
		t.Fatalf("CreateSession = %q, %v", id, err)
	}
	m, err := c.Prompt(context.Background(), id, Prompt{Text: "hi", Schema: json.RawMessage(`{"type":"object"}`)})
	if err != nil || m.Parts[0].Text != "ok" {
		t.Fatalf("Prompt = %#v, %v", m, err)
	}
	if _, err := m.StructuredOutput(); err != nil {
		t.Fatal(err)
	}
}

func TestAbortFalseAndHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses_1/abort" {
			_, _ = w.Write([]byte(`false`))
			return
		}
		http.Error(w, "private prompt", http.StatusBadRequest)
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Directory: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Abort(context.Background(), "ses_1"); err == nil {
		t.Fatal("Abort accepted false")
	}
	if err := c.GetSession(context.Background(), "ses_1"); err == nil {
		t.Fatal("GetSession accepted error")
	}
}

func TestIdleAbsentFallsBackToSessionAndNilPermissionIsOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["permission"]; ok {
				t.Error("nil permission must be omitted")
			}
			_, _ = w.Write([]byte(`{"id":"ses_1"}`))
		case "/session/status":
			_, _ = w.Write([]byte(`{}`))
		case "/session/ses_1":
			_, _ = w.Write([]byte(`{"id":"ses_1"}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Directory: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSession(context.Background(), "title", nil); err != nil {
		t.Fatal(err)
	}
	idle, err := c.Idle(context.Background(), "ses_1")
	if err != nil || !idle {
		t.Fatalf("Idle = %v, %v", idle, err)
	}
}

func TestGetSessionRejectsMismatchedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ses_other"}`))
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Directory: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GetSession(context.Background(), "ses_1"); err == nil {
		t.Fatal("GetSession accepted mismatched response")
	}
}

func TestNewAddsDeadlineForZeroTimeoutBaseClient(t *testing.T) {
	var deadlines []time.Time
	c, err := New(Config{
		BaseURL:   "https://example.test",
		Directory: "/work",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			deadline, ok := request.Context().Deadline()
			if !ok {
				t.Fatal("request has no deadline")
			}
			deadlines = append(deadlines, deadline)
			return nil, context.DeadlineExceeded
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Health error = %v", err)
	}
	if remaining := time.Until(deadlines[0]); remaining <= 0 || remaining > defaultTimeout {
		t.Fatalf("deadline remaining = %v", remaining)
	}
	callerDeadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()
	if _, err := c.Health(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Health with caller deadline error = %v", err)
	}
	if !deadlines[1].Equal(callerDeadline) {
		t.Fatalf("caller deadline = %v, got %v", callerDeadline, deadlines[1])
	}
}

func TestNewRequestClosesBodyOnPreDispatchFailure(t *testing.T) {
	body := &trackingReadCloser{Reader: strings.NewReader("body")}
	if _, err := newRequest(nil, http.MethodPost, "https://example.test", body); err == nil {
		t.Fatal("newRequest accepted a nil context")
	}
	if !body.closed {
		t.Fatal("newRequest did not close the body")
	}
}

func TestDeleteSessionSendsDirectoryAndRequiresAccepted(t *testing.T) {
	type seen struct {
		method string
		path   string
		query  string
	}
	var got seen
	c, err := New(Config{
		BaseURL:   "http://opencode.test",
		Directory: "/work tree",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = seen{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`true`)),
				Request:    r,
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSession(context.Background(), "ses_1"); err != nil {
		t.Fatalf("DeleteSession = %v", err)
	}
	if got.method != http.MethodDelete || got.path != "/session/ses_1" {
		t.Fatalf("request = %s %s", got.method, got.path)
	}
	if got.query != "directory=%2Fwork+tree" {
		t.Fatalf("query = %q", got.query)
	}
}

func TestDeleteSessionRejectsNotAcceptedAndInvalidID(t *testing.T) {
	reply := false
	c, err := New(Config{
		BaseURL:   "http://opencode.test",
		Directory: "/work",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(strconv.FormatBool(reply))),
				Request:    r,
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSession(context.Background(), "ses_1"); err == nil {
		t.Fatal("DeleteSession accepted a false result")
	}
	reply = true
	if err := c.DeleteSession(context.Background(), "../escape"); err == nil {
		t.Fatal("DeleteSession accepted an invalid ID")
	}
	if err := c.DeleteSession(context.Background(), ""); err == nil {
		t.Fatal("DeleteSession accepted an empty ID")
	}
}
