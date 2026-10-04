package beesuh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
)

var errPostCommitAcknowledgementLoss = errors.New("injected post-commit acknowledgement loss")

func TestUnreadableMappingEvidenceKeepsCreateIntentForReopen(t *testing.T) {
	f := newFakeOpenCode(t)
	gate := make(chan struct{})
	f.createGate = gate
	data := t.TempDir()
	opts := Options{DataDir: data, BackendID: "test", MaxConcurrent: 1, Timeout: 10 * time.Second, OpenCode: opencode.Config{BaseURL: f.server.URL, Directory: "/workspace"}}
	r, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_unreadable_mapping BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'mapping fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	r.db = &faultingRuntimeDB{DB: r.db, queryFault: func(req rhiza.QueryRequest) bool {
		return req.SQL == `SELECT session_id,session_key FROM sessions WHERE workspace=? AND (session_key=? OR session_id=?)`
	}}
	req := testRequest("s", "request", "answer")
	done := make(chan error, 1)
	go func() { _, err := r.Run(t.Context(), req); done <- err }()
	select {
	case <-f.created:
	case <-time.After(10 * time.Second):
		t.Fatal("remote create did not start")
	}
	// The rejected mapping has an unreadable authoritative verification. The
	// error must retain the creating intent and must not reclaim the session.
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("run succeeded after storage became unreadable")
	}
	creates, prompts, _ := f.counts()
	deletes := f.deletesSnapshot()
	if creates != 1 || prompts != 0 || deletes != 0 {
		t.Fatalf("first attempt creates=%d prompts=%d deletes=%d", creates, prompts, deletes)
	}
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `DROP TRIGGER reject_unreadable_mapping`}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Run(t.Context(), req); err != nil {
		t.Fatalf("retry did not adopt retained intent: %v", err)
	}
	creates, prompts, _ = f.counts()
	deletes = f.deletesSnapshot()
	if creates != 1 || prompts != 1 || deletes != 0 {
		t.Fatalf("retry creates=%d prompts=%d deletes=%d; want adoption without replacement create", creates, prompts, deletes)
	}
}

type faultingRuntimeDB struct {
	DB           runtimeDB
	queryFault   func(rhiza.QueryRequest) bool
	executeFault func(rhiza.ExecuteRequest) bool
	faulted      bool
}

func (db *faultingRuntimeDB) Close() error { return db.DB.Close() }

func (db *faultingRuntimeDB) Execute(ctx context.Context, req rhiza.ExecuteRequest) (rhiza.ExecuteResponse, error) {
	response, err := db.DB.Execute(ctx, req)
	if err == nil && response.Status == rhiza.MutationCommitted && !db.faulted && db.executeFault != nil && db.executeFault(req) {
		db.faulted = true
		return response, errPostCommitAcknowledgementLoss
	}
	return response, err
}

func (db *faultingRuntimeDB) Query(ctx context.Context, req rhiza.QueryRequest) (rhiza.QueryResponse, error) {
	if db.queryFault != nil && db.queryFault(req) {
		return rhiza.QueryResponse{}, errors.New("injected unreadable authoritative mapping")
	}
	return db.DB.Query(ctx, req)
}

func TestCleanupPhaseIsDurableBeforeFirstDelete(t *testing.T) {
	var runtimeRef *Runtime
	var creates, prompts atomic.Int32
	phaseSeen := make(chan error, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/session":
			_, _ = fmt.Fprint(w, `[]`)
		case req.Method == http.MethodPost && req.URL.Path == "/session":
			creates.Add(1)
			_, _ = fmt.Fprint(w, `{"id":"ses_cleanup"}`)
		case req.Method == http.MethodDelete && req.URL.Path == "/session/ses_cleanup":
			rows, err := runtimeRef.query(context.Background(), `SELECT phase,remote_id FROM pending_session_creates WHERE workspace='w' AND session_key='s'`)
			if err != nil || len(rows) != 1 || rows[0][0] != pendingPhaseCleanup || rows[0][1] != "ses_cleanup" {
				phaseSeen <- fmt.Errorf("phase rows=%v err=%v", rows, err)
			} else {
				phaseSeen <- nil
			}
			http.Error(w, "lost delete acknowledgement", http.StatusInternalServerError)
		case req.Method == http.MethodPost && req.URL.Path == "/session/ses_cleanup/message":
			prompts.Add(1)
			http.Error(w, "unexpected prompt", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	data := t.TempDir()
	opts := Options{DataDir: data, BackendID: "test", MaxConcurrent: 1, Timeout: 10 * time.Second, OpenCode: opencode.Config{BaseURL: server.URL, Directory: "/workspace"}}
	r, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRef = r
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_mapping BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'mapping fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	req := Request{WorkspaceID: "w", SessionKey: "s", RequestID: "r", Prompt: Prompt{Text: "answer"}}
	if _, err := r.Run(t.Context(), req); err == nil {
		t.Fatal("run succeeded after mapping failure")
	}
	select {
	case err := <-phaseSeen:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("failed mapping never reached the cleanup DELETE")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRef = r
	defer r.Close()
	if _, err := r.Run(t.Context(), Request{WorkspaceID: "w", SessionKey: "s", RequestID: "retry", Prompt: Prompt{Text: "answer"}}); err == nil {
		t.Fatal("retry succeeded while delete acknowledgement remains lost")
	}
	select {
	case err := <-phaseSeen:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("reopened cleanup never retried the DELETE")
	}
	if creates.Load() != 1 || prompts.Load() != 0 {
		t.Fatalf("creates=%d prompts=%d; cleanup retry must not adopt or prompt", creates.Load(), prompts.Load())
	}
}
