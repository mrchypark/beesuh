package conductor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmitUsesConductorEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/events" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var got struct {
			Events []struct {
				Name           string                     `json:"name"`
				Data           map[string]json.RawMessage `json:"data"`
				IdempotencyKey string                     `json:"idempotency_key"`
			} `json:"events"`
		}
		if err := json.Unmarshal(body, &got); err != nil || len(got.Events) != 1 || got.Events[0].Name != "order.created" || string(got.Events[0].Data["id"]) != `"42"` || got.Events[0].IdempotencyKey != "workspace:session:request:tool" {
			t.Fatalf("unexpected envelope: %s (%v)", body, err)
		}
		var link struct {
			Workspace string `json:"workspace_id"`
			Request   string `json:"execution_request_id"`
			Binding
			Actor struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"actor"`
		}
		if err := json.Unmarshal(got.Events[0].Data["_compos"], &link); err != nil || link.Workspace != "workspace" || link.Request != got.Events[0].IdempotencyKey || link.HandlerKey != "payload.echo" || link.HandlerVersion != "1" || link.OperationKey != "payload.echo" || link.Actor.Type != "service" || link.Actor.ID != "beesuh" {
			t.Fatalf("invalid trusted linkage: %+v", link)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true,"events":[{"execution_id":"server-assigned-id","execution_request_id":"workspace:session:request:tool","status":"queued"}]}`))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", AllowedEvents: []string{"order.created"}, Bindings: map[string]Binding{"order.created": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Emit(context.Background(), "workspace", "order.created", json.RawMessage(`{"id":"42"}`), "workspace:session:request:tool")
	if err != nil || string(got) != `{"execution_id":"server-assigned-id"}` {
		t.Fatalf("Emit() = %s, %v", got, err)
	}
}

func TestEmitReturnsExecutionIDWhenAcknowledgementEncodingIsLarge(t *testing.T) {
	const executionID = "server-assigned-id"
	acknowledgement := fmt.Sprintf(`{"accepted":true,"events":[{"execution_id":%q,"execution_request_id":"key","detail":"%s"}]}`,
		executionID, strings.Repeat(`\\`, 7000))
	if len(acknowledgement) >= maxBodyBytes || len(fmt.Sprintf("%q", acknowledgement)) <= 22<<10 {
		t.Fatal("invalid oversized-encoding fixture")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(acknowledgement))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, AllowedEvents: []string{"echo"}, Bindings: map[string]Binding{"echo": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Emit(context.Background(), "workspace", "echo", []byte(`{}`), "key")
	if err != nil || string(got) != `{"execution_id":"server-assigned-id"}` {
		t.Fatalf("Emit() = %s, %v", got, err)
	}
}

func TestRejectsUntrustedInputBeforeRequest(t *testing.T) {
	client, err := New(Config{BaseURL: "http://example.test", AllowedEvents: []string{"ok"}, Bindings: map[string]Binding{"ok": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Emit(context.Background(), "workspace", "no", json.RawMessage(`{}`), "key"); err == nil {
		t.Fatal("Emit accepted an unallowlisted event")
	}
	if _, err := client.Emit(context.Background(), "workspace", "ok", json.RawMessage(`[]`), "key"); err == nil {
		t.Fatal("Emit accepted an array")
	}
	if _, err := client.GetRun(context.Background(), "../secret"); err == nil {
		t.Fatal("GetRun accepted a path")
	}
}

func TestGetRunEscapesID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/executions/a%20b%3Fc" {
			t.Fatalf("escaped path = %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"state":"done"}`))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, AllowedEvents: []string{"ok"}, Bindings: map[string]Binding{"ok": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.GetRun(context.Background(), "a b?c")
	if err != nil || string(got) != `{"state":"done"}` {
		t.Fatalf("GetRun() = %s, %v", got, err)
	}
}

func TestGetRunDoesNotFollowRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			t.Fatal("client followed redirect")
		}
		w.Header().Set("Location", "/redirected")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, AllowedEvents: []string{"ok"}, Bindings: map[string]Binding{"ok": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetRun(context.Background(), "run"); err == nil {
		t.Fatal("GetRun accepted a redirect")
	}
}

func TestRejectsInvalidSuccessfulResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			_, _ = w.Write([]byte(`not JSON`))
			return
		}
		// An empty body is not a JSON value.
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, AllowedEvents: []string{"ok"}, Bindings: map[string]Binding{"ok": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Emit(context.Background(), "workspace", "ok", json.RawMessage(`{}`), "key"); err == nil {
		t.Fatal("Emit accepted invalid JSON")
	}
	if _, err := client.GetRun(context.Background(), "run"); err == nil {
		t.Fatal("GetRun accepted an empty response")
	}
}

func TestRejectsUnsafeToken(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://example.test", Token: "ok\r\nother", AllowedEvents: []string{"ok"}, Bindings: map[string]Binding{"ok": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}); err == nil {
		t.Fatal("New accepted a token with a newline")
	}
}

func TestExecutionBoundaryRejectsBeforeNetwork(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://example.test", AllowedEvents: []string{"workflow"}}); err == nil {
		t.Fatal("accepted missing handler binding")
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{}`)) }))
	defer server.Close()
	cfg := Config{BaseURL: server.URL, AllowedEvents: []string{"echo"}, Bindings: map[string]Binding{"echo": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{"_compos":{}}`, `{"subject_ref":"forged"}`} {
		if _, err := client.Emit(context.Background(), "workspace", "echo", []byte(data), "effect"); err == nil {
			t.Fatal("accepted reserved metadata")
		}
	}
	ctx := WithExecutionScope(context.Background(), ExecutionScope{UserID: "user", WorkspaceID: "other", ConnectionID: "connection"})
	if _, err := client.Emit(ctx, "workspace", "echo", []byte(`{}`), "effect"); err == nil {
		t.Fatal("accepted mismatched workspace")
	}
	cfg.RequireScope = true
	client, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Emit(context.Background(), "workspace", "echo", []byte(`{}`), "effect"); err == nil {
		t.Fatal("accepted missing scope")
	}
	if _, err := client.GetExecution(context.Background(), "execution"); err == nil {
		t.Fatal("read without required scope")
	}
	if calls != 0 {
		t.Fatal("invalid input reached network")
	}
}

func TestNewRejectsNoncanonicalEventNames(t *testing.T) {
	for _, name := range []string{" echo ", "echo/next", "echo\\next", "echo\x01next"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			_, err := New(Config{BaseURL: "http://example.test", AllowedEvents: []string{name}, Bindings: map[string]Binding{name: {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
			if err == nil {
				t.Fatal("New accepted a noncanonical event name")
			}
		})
	}
}

func TestExecutionIDLimitIsSharedByEmitAndLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"accepted":true,"events":[{"execution_id":%q,"execution_request_id":"key"}]}`, strings.Repeat("a", MaxIDBytes))))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, AllowedEvents: []string{"echo"}, Bindings: map[string]Binding{"echo": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Emit(context.Background(), "workspace", "echo", []byte(`{}`), "key"); err != nil {
		t.Fatalf("Emit rejected a %d-byte execution ID: %v", MaxIDBytes, err)
	}
	if _, err := client.GetExecution(context.Background(), strings.Repeat("a", MaxIDBytes)); err != nil {
		t.Fatalf("GetExecution rejected a %d-byte execution ID: %v", MaxIDBytes, err)
	}
	for _, size := range []int{MaxIDBytes + 1, 512} {
		if _, err := client.GetExecution(context.Background(), strings.Repeat("a", size)); err == nil {
			t.Fatalf("GetExecution accepted a %d-byte execution ID", size)
		}
	}
}

func TestEmitRejectsUnconfirmedAcknowledgement(t *testing.T) {
	for _, body := range []string{`{}`, `{"accepted":false,"events":[]}`, `{"accepted":true,"events":[{"execution_id":"id","execution_request_id":"other"}]}`, `{"accepted":true,"events":[{"execution_request_id":"key"}]}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				w.Write([]byte(body))
			}))
			defer server.Close()
			c, err := New(Config{BaseURL: server.URL, AllowedEvents: []string{"echo"}, Bindings: map[string]Binding{"echo": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Emit(context.Background(), "workspace", "echo", []byte(`{}`), "key"); err == nil {
				t.Fatal("accepted malformed acknowledgement")
			}
		})
	}
}
