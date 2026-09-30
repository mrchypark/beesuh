package beesuh

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/model"
)

// This pilot targets an explicitly started loopback, non-SaaS service fixture.
// It neither starts Conductor nor claims user delegation or callback verification.
func TestConductorLiveE2E(t *testing.T) {
	if os.Getenv("BEESUH_E2E_CONDUCTOR") != "1" {
		t.Skip("opt-in live Conductor fixture; NOT VERIFIED")
	}
	base := os.Getenv("BEESUH_E2E_CONDUCTOR_URL")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		t.Fatal("pilot requires an explicit loopback HTTP fixture")
	}
	cfg := conductor.Config{BaseURL: base, AllowedEvents: []string{"echo"}, Bindings: map[string]conductor.Binding{"echo": {HandlerKey: "payload.echo", HandlerVersion: "1", OperationKey: "payload.echo"}}}
	// The token authenticates this fixture service, never a delegated user.
	if path := os.Getenv("BEESUH_E2E_CONDUCTOR_TOKEN_FILE"); path != "" {
		cfg.Token = conductorPilotToken(t, path)
	}
	client, err := conductor.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var modelCalls atomic.Int32
	ids := make(chan string, 1)
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if modelCalls.Add(1) == 1 {
			completion(w, "", "tool_calls", model.ToolCall{ID: "echo-call", Type: "function", Function: model.FunctionCall{Name: "conductor_emit", Arguments: `{"name":"echo","data":{"message":"beesuh-pilot"}}`}})
			return
		}
		var in struct {
			Messages []model.Message `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("invalid model fixture request")
			w.WriteHeader(400)
			return
		}
		for _, m := range in.Messages {
			if m.Role == "tool" && m.ToolCallID == "echo-call" {
				var ack struct {
					Events []struct {
						ID string `json:"execution_id"`
					} `json:"events"`
				}
				if json.Unmarshal([]byte(m.Content), &ack) != nil || len(ack.Events) != 1 {
					t.Error("missing execution acknowledgement")
					w.WriteHeader(400)
					return
				}
				ids <- ack.Events[0].ID
			}
		}
		completion(w, "accepted", "stop")
	}))
	defer modelServer.Close()
	runtime := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), modelServer.URL, &cfg))
	defer runtime.Close()
	req := Request{WorkspaceID: "beesuh-pilot", SessionKey: "pilot", RequestID: "pilot-" + rand.Text(), Prompt: Prompt{Text: "Run the local echo fixture"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := runtime.Run(ctx, req)
	if err != nil || out.State != "succeeded" {
		t.Fatalf("native pilot failed: state=%s err=%v", out.State, err)
	}
	var id string
	select {
	case id = <-ids:
	case <-ctx.Done():
		t.Fatal("execution ID was not preserved")
	}
	key := "beesuh_" + digest([]string{req.WorkspaceID, req.SessionKey, req.RequestID, "echo-call"})
	replay, err := client.Emit(ctx, req.WorkspaceID, "echo", []byte(`{"message":"beesuh-pilot"}`), key)
	var ack struct {
		Events []struct {
			ID string `json:"execution_id"`
		} `json:"events"`
	}
	if err != nil || json.Unmarshal(replay, &ack) != nil || len(ack.Events) != 1 || ack.Events[0].ID != id {
		t.Fatal("same effect did not resolve to same execution")
	}
	if _, err := runtime.Run(ctx, req); err != nil || modelCalls.Load() != 2 {
		t.Fatal("native replay repeated model dispatch")
	}
	for {
		raw, err := client.GetExecution(ctx, id)
		if err != nil {
			t.Fatal("execution lookup failed")
		}
		var execution struct {
			ID      string          `json:"id"`
			Request string          `json:"execution_request_id"`
			Status  string          `json:"status"`
			Output  json.RawMessage `json:"output"`
		}
		if json.Unmarshal(raw, &execution) != nil || execution.ID != id || execution.Request != key {
			t.Fatal("execution linkage changed")
		}
		switch execution.Status {
		case "completed", "succeeded":
			if !strings.Contains(string(execution.Output), "beesuh-pilot") {
				t.Fatal("echo output missing")
			}
			t.Logf("Conductor service fixture: execution_id=%s execution_request_id=%s status=%s; native+HTTP replay preserved identity; callback NOT VERIFIED here", id, key, execution.Status)
			return
		case "failed", "cancelled", "canceled":
			t.Fatalf("fixture terminal status=%s", execution.Status)
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker did not reach terminal status")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func conductorPilotToken(t *testing.T, path string) string {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		t.Fatal("invalid service token fixture file")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("service token fixture unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !os.SameFile(before, info) {
		t.Fatal("service token fixture changed")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(after, info) {
		t.Fatal("service token fixture changed")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	token := strings.TrimSpace(string(raw))
	if err != nil || len(raw) > 64<<10 || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		t.Fatal("invalid service token fixture")
	}
	return token
}
