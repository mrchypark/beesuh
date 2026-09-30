package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
