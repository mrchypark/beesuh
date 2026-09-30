package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrchypark/beesuh/model"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestNativeMemoryIsolationSkillsAndForget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []model.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		last := in.Messages[len(in.Messages)-1]
		if last.Role == "tool" {
			completion(w, last.Content, "stop")
			return
		}
		name, args := "memory_recall", `{"query":""}`
		switch last.Content {
		case "remember":
			name, args = "memory_save", `{"name":"drink","content":"tea"}`
		case "forget":
			name, args = "memory_forget", `{"name":"drink"}`
		case "skill":
			name, args = "load_skill", `{"name":"workflow"}`
		}
		completion(w, "", "tool_calls", model.ToolCall{ID: fmt.Sprint(calls.Add(1)), Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}})
	}))
	defer server.Close()
	skills := t.TempDir()
	if err := os.WriteFile(filepath.Join(skills, "workflow.md"), []byte("Check workflow status before reporting completion."), 0600); err != nil {
		t.Fatal(err)
	}
	opts := nativeOptions(t.TempDir(), t.TempDir(), server.URL, nil)
	opts.Native.Memory = true
	opts.Native.SkillsDir = skills
	r := nativeOpen(t, opts)
	run := func(session, id, text string) Result {
		t.Helper()
		out, err := r.Run(context.Background(), Request{WorkspaceID: "w", SessionKey: session, RequestID: id, Prompt: Prompt{Text: text}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run("s", "1", "remember")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Native sessions must not be bound to the incidental CLI working directory.
	opts.OpenCode.Directory = t.TempDir()
	r = nativeOpen(t, opts)
	defer r.Close()
	if out := run("s", "2", "lookup"); !strings.Contains(string(out.Output), "tea") {
		t.Fatalf("lost memory: %s", out.Output)
	}
	if out := run("other", "3", "lookup"); strings.Contains(string(out.Output), "tea") {
		t.Fatal("cross-session memory leak")
	}
	if out := run("s", "4", "skill"); !strings.Contains(string(out.Output), "Check workflow") {
		t.Fatal("skill not loaded")
	}
	run("s", "5", "forget")
	if out := run("s", "6", "lookup"); strings.Contains(string(out.Output), "tea") {
		t.Fatal("active memory not forgotten")
	}
	rows, err := r.query(context.Background(), `SELECT content FROM memories`)
	if err != nil || len(rows) != 0 {
		t.Fatalf("memories=%v err=%v", rows, err)
	}
}

func TestNativeEncodedLimitsDoNotPoisonRuntime(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		completion(w, strings.Repeat("\x01", 5000), "stop")
	}))
	defer server.Close()
	r := nativeOpen(t, nativeOptions(t.TempDir(), t.TempDir(), server.URL, nil))
	defer r.Close()
	req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "oversize-input", Prompt: Prompt{Text: strings.Repeat("\x01", 5000)}}
	if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("input err=%v", err)
	}
	if calls.Load() != 0 || r.blocked.Load() {
		t.Fatal("invalid input performed work or blocked runtime")
	}
	req.RequestID, req.Prompt.Text = "oversize-output", "hello"
	out, err := r.Run(context.Background(), req)
	if err == nil || out.State != "failed" || r.blocked.Load() {
		t.Fatalf("out=%+v err=%v blocked=%v", out, err, r.blocked.Load())
	}
	// Even a confirmed tool result with many escaped bytes is a bounded omission,
	// not an uncertain side effect that blocks unrelated sessions.
	r.native.skills["large"] = strings.Repeat("\x01", 6000)
	r.native.tools = nil
	r.native.schemas = map[string]*jsonschema.Schema{}
	if err := r.native.configureTools(); err != nil {
		t.Fatal(err)
	}
	result, err := r.nativeTool(context.Background(), req, model.ToolCall{ID: "large", Type: "function", Function: model.FunctionCall{Name: "load_skill", Arguments: `{"name":"large"}`}})
	if err != nil || !strings.Contains(string(result), "output_omitted") {
		t.Fatalf("large tool result=%s err=%v", result, err)
	}
}

func TestNativeContextKeepsWholeTurnsAndRejectsSymlinks(t *testing.T) {
	n := nativeEngine{opts: NativeOptions{MaxContextBytes: 4096}}
	history := [][]model.Message{{{Role: "user", Content: strings.Repeat("old", 700)}, {Role: "assistant", Content: "old answer"}}}
	current := []model.Message{{Role: "user", Content: strings.Repeat("new", 700)}}
	messages, err := n.context("system", history, current, nil)
	if err != nil || len(messages) != 2 || messages[1].Content != current[0].Content {
		t.Fatalf("context=%v err=%v", messages, err)
	}
	current[0].Content = strings.Repeat("x", 4096)
	if _, err := n.context("system", nil, current, nil); err == nil {
		t.Fatal("active turn exceeded budget")
	}
	skills := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(skills, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := newNative(NativeOptions{Provider: model.Config{BaseURL: "http://127.0.0.1:1/v1", Model: "test"}, SkillsDir: skills}); err == nil {
		t.Fatal("symlink skill accepted")
	}
}

func TestRequestIdentitySerializesAcrossSessions(t *testing.T) {
	f := newFakeOpenCode(t)
	gate := make(chan struct{})
	f.promptGate = gate
	r := openTestRuntime(t, f, t.TempDir(), 2)
	defer r.Close()
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := r.Run(context.Background(), testRequest("one", "duplicate", "hello")); first <- err }()
	receive(t, f.prompted, "first model call")
	go func() { _, err := r.Run(context.Background(), testRequest("two", "duplicate", "hello")); second <- err }()
	eventually(t, "workspace request identity lock", func() bool { return lockRefs(r, "request\x00workspace\x00duplicate") == 2 })
	close(gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ErrConflict) {
		t.Fatalf("second=%v", err)
	}
	if r.blocked.Load() {
		t.Fatal("duplicate request poisoned runtime")
	}
	_, prompts, _ := f.counts()
	if prompts != 1 {
		t.Fatalf("prompts=%d", prompts)
	}
}
