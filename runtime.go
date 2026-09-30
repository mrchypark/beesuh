// Package beesuh runs durable assistant conversations with bounded concurrency.
package beesuh

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mrchypark/beesuh/opencode"
	"github.com/mrchypark/rhiza"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	ErrOverloaded   = errors.New("beesuh: waiting capacity exhausted")
	ErrConflict     = errors.New("beesuh: identity already used with different content or backend")
	ErrUncertain    = errors.New("beesuh: unresolved execution; inspect effects, then explicitly recover")
	ErrClosed       = errors.New("beesuh: runtime closed")
	ErrInvalidInput = errors.New("beesuh: invalid input")
	ErrNotFound     = errors.New("beesuh: run not found")
)

// Prompt and Model preserve the original OpenCode adapter API. Native mode uses the
// configured provider model and does not accept per-request model overrides.
type Prompt = opencode.Prompt
type Model = opencode.Model

type Options struct {
	ResolveCredential func(context.Context, Connection) (Credential, error)
	// RequireIdentity enables service-owned user isolation; Authorize checks current membership/activity.
	RequireIdentity           bool
	Authorize                 func(context.Context, Principal) error
	Native                    *NativeOptions
	DataDir, BackendID        string
	OpenCode                  opencode.Config
	Model                     *opencode.Model
	Permissions               []opencode.PermissionRule
	MaxConcurrent, MaxWaiting int
	Timeout                   time.Duration
}

type Request struct {
	ConnectionID                       string `json:",omitempty"`
	ConductorConnectionID              string `json:",omitempty"`
	Provider                           string `json:",omitempty"`
	WorkspaceID, SessionKey, RequestID string
	Prompt                             opencode.Prompt
	Timeout                            time.Duration
}

type Result struct {
	Provider  string          `json:"provider,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	SessionID string          `json:"session_id"`
	MessageID string          `json:"message_id,omitempty"`
	State     string          `json:"state"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type sessionLock struct {
	token chan struct{}
	refs  int
}

// Runtime owns its Rhiza directory exclusively. All clients of its OpenCode
// sessions must go through this instance for the concurrency limits to hold.
type Runtime struct {
	db               *rhiza.DB
	client           *opencode.Client
	native           *nativeEngine
	opts             Options
	profile          string
	file             *os.File
	life             sync.RWMutex
	closed           bool
	blocked          atomic.Bool
	slots, admission chan struct{}
	mu               sync.Mutex
	locks            map[string]*sessionLock
}

func Open(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.RequireIdentity {
		if opts.Native == nil || opts.Authorize == nil || opts.ResolveCredential == nil {
			return nil, errors.New("beesuh: service mode requires native backend, authorization and credential resolvers")
		}
		if opts.Native.Conductor != nil {
			return nil, errors.New("beesuh: service mode requires named scoped Conductor connections")
		}
		for _, cfg := range opts.Native.Conductors {
			if !cfg.RequireScope {
				return nil, errors.New("beesuh: service Conductor requires a scope-enforcing gateway")
			}
		}
	} else if opts.Authorize != nil || opts.ResolveCredential != nil {
		return nil, errors.New("beesuh: authorization callback requires service mode")
	}

	if opts.DataDir == "" || strings.TrimSpace(opts.BackendID) == "" || opts.MaxConcurrent < 1 || opts.MaxWaiting < 0 || opts.MaxConcurrent > 1024 || opts.MaxWaiting > 10000 {
		return nil, errors.New("beesuh: data directory, backend identity and bounded positive concurrency are required")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Timeout < 0 {
		return nil, errors.New("beesuh: timeout must be positive")
	}
	if len(opts.Permissions) == 0 {
		opts.Permissions = []opencode.PermissionRule{{Permission: "*", Pattern: "*", Action: "deny"}}
	}
	opts.Permissions = append([]opencode.PermissionRule(nil), opts.Permissions...)
	if opts.Model != nil {
		if opts.Model.ProviderID == "" || opts.Model.ModelID == "" {
			return nil, errors.New("beesuh: default model requires provider and model IDs")
		}
		model := *opts.Model
		opts.Model = &model
	}
	for _, p := range opts.Permissions {
		if p.Permission == "" || p.Pattern == "" || (p.Action != "allow" && p.Action != "deny") {
			return nil, errors.New("beesuh: permissions require patterns and allow/deny; interactive ask is unsupported")
		}
	}
	var client *opencode.Client
	var native *nativeEngine
	var err error
	if opts.Native != nil {
		if opts.Model != nil {
			return nil, errors.New("beesuh: native model is configured through Native.Provider")
		}
		native, err = newNative(*opts.Native)
		// Native conversations have no OpenCode working-directory dependency.
		opts.OpenCode.Directory = ""
	} else {
		client, err = opencode.New(opts.OpenCode)
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.DataDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(opts.DataDir, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("beesuh: data directory is in use: %w", err)
	}
	db, err := rhiza.Open(ctx, rhiza.Config{NodeID: "beesuh", DataDir: filepath.Join(opts.DataDir, "rhiza"), PeerAddr: "127.0.0.1:0"})
	if err != nil {
		f.Close()
		return nil, err
	}
	r := &Runtime{db: db, client: client, native: native, opts: opts, file: f, slots: make(chan struct{}, opts.MaxConcurrent), admission: make(chan struct{}, opts.MaxConcurrent+opts.MaxWaiting), locks: make(map[string]*sessionLock)}
	r.profile = digest(opts.Permissions)
	if native != nil {
		r.profile = native.profile
	}
	err = db.Migrate(ctx, []rhiza.Migration{{Version: 1, Name: "sessions-and-runs", Statements: []rhiza.SQLStatement{
		{SQL: `CREATE TABLE sessions (workspace TEXT NOT NULL, session_key TEXT NOT NULL, backend TEXT NOT NULL, directory TEXT NOT NULL, profile TEXT NOT NULL, session_id TEXT NOT NULL, active_request TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,session_key))`},
		{SQL: `CREATE TABLE runs (workspace TEXT NOT NULL, request_id TEXT NOT NULL, session_key TEXT NOT NULL, input_hash TEXT NOT NULL, state TEXT NOT NULL, result TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,request_id))`},
	}}, nativeMigration, connectionMigration, identityMigration, connectionCredentialMigration})
	if err == nil {
		err = r.pinIdentityMode(ctx)
	}
	if err == nil {
		var rows [][]any
		rows, err = r.query(ctx, `SELECT request_id FROM runs WHERE state='running' LIMIT 1`)
		r.blocked.Store(len(rows) > 0)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *Runtime) Close() error {
	r.life.Lock()
	defer r.life.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return errors.Join(r.db.Close(), r.file.Close())
}

// Run stores the result before returning it. Repeating an identical request
// returns that stored result; it never sends a second prompt.
func (r *Runtime) Run(ctx context.Context, req Request) (Result, error) {
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return Result{}, ErrClosed
	}
	if err := r.bindRequest(ctx, &req); err != nil {
		return Result{}, err
	}
	if req.Prompt.Model == nil {
		req.Prompt.Model = r.opts.Model
	}
	if r.native != nil {
		if r.native.named && req.Provider == "" {
			req.Provider = r.native.opts.DefaultProvider
		}
		if err := r.native.validate(req); err != nil {
			return Result{}, errors.Join(ErrInvalidInput, err)
		}
	}
	if r.native == nil && req.Provider != "" {
		return Result{}, errors.Join(ErrInvalidInput, errors.New("beesuh: named providers require native backend"))
	}
	if err := validateRequest(req); err != nil {
		return Result{}, errors.Join(ErrInvalidInput, err)
	}
	schema, err := compileSchema(req.Prompt.Schema)
	if err != nil {
		return Result{}, errors.Join(ErrInvalidInput, err)
	}
	timeout := r.opts.Timeout
	if req.Timeout > 0 && req.Timeout < timeout {
		timeout = req.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case r.admission <- struct{}{}:
		defer func() { <-r.admission }()
	default:
		return Result{}, ErrOverloaded
	}
	// Request IDs are workspace-wide, including across different sessions.
	// Claim request identity before the session lock to avoid concurrent INSERT
	// collisions being misclassified as uncertain storage outcomes.
	unlockRequest, err := r.lockSession(ctx, "request\x00"+req.WorkspaceID+"\x00"+req.RequestID)
	if err != nil {
		return Result{}, err
	}
	defer unlockRequest()
	unlock, err := r.lockSession(ctx, req.WorkspaceID+"\x00"+req.SessionKey)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	hash := digest(req)
	if out, found, err := r.previous(ctx, req, hash); found || err != nil {
		return out, err
	}
	if r.blocked.Load() {
		return Result{}, ErrUncertain
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	if r.blocked.Load() {
		return Result{}, ErrUncertain
	}
	if err := r.authorizeContext(ctx); err != nil {
		return Result{}, err
	}
	if r.opts.RequireIdentity {
		if err := r.checkConnection(ctx, req.ConnectionID, "provider", req.Provider); err != nil {
			return Result{}, err
		}
	}
	sessionID, err := r.session(ctx, req)
	if err != nil {
		return Result{}, err
	}
	// Persist the uncertain interval before sending anything to the agent.
	err = r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state) VALUES(?,?,?,?,'running')`, Args: []any{req.WorkspaceID, req.RequestID, req.SessionKey, hash}},
		{SQL: `UPDATE sessions SET active_request=? WHERE workspace=? AND session_key=? AND active_request=''`, Args: []any{req.RequestID, req.WorkspaceID, req.SessionKey}, ExpectedRowsAffected: one()},
	})
	if err != nil {
		r.blocked.Store(true)
		return Result{}, err
	}
	if r.native != nil {
		return r.runNative(ctx, req, sessionID, schema)
	}
	prompt := req.Prompt
	prompt.MessageID = "msg_" + digest([]string{req.WorkspaceID, req.RequestID})[:32]
	msg, runErr := r.client.Prompt(ctx, sessionID, prompt)
	out := Result{RequestID: req.RequestID, SessionID: sessionID, MessageID: msg.Info.ID, State: "succeeded"}
	if runErr == nil {
		if msg.Info.Role != "assistant" || msg.Info.SessionID != sessionID || msg.Info.ID == "" || msg.Info.Time.Completed == 0 || msg.Info.Finish == "length" || msg.Info.Finish == "tool-calls" {
			runErr = errors.New("beesuh: incomplete or mismatched assistant response")
		} else if schema != nil {
			out.Output, runErr = msg.StructuredOutput()
			if runErr == nil {
				var value any
				value, runErr = jsonschema.UnmarshalJSON(bytes.NewReader(out.Output))
				if runErr == nil && schema.Validate(value) != nil {
					runErr = errors.New("beesuh: output does not match schema")
				}
			}
		} else {
			var text strings.Builder
			for _, part := range msg.Parts {
				if part.Type == "text" {
					text.WriteString(part.Text)
				}
			}
			out.Output, _ = json.Marshal(map[string]string{"text": text.String()})
		}
	}
	// Rhiza's replicated mutation limit is 128 KiB; reserve room for escaping.
	if runErr == nil && len(out.Output) > 16<<10 {
		runErr = errors.New("beesuh: output exceeds 16 KiB durable result limit")
	}
	cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if runErr != nil {
		// A legacy OpenCode session can execute configured tools before its prompt
		// response fails validation. Abort only stops future work; it cannot prove
		// that an already-issued external effect did not happen. Keep the durable
		// run active for explicit recovery instead of recording a replayable failure.
		if err := r.stopSession(cleanup, sessionID); err != nil {
			r.blocked.Store(true)
			return Result{SessionID: sessionID, State: "uncertain", Error: ErrUncertain.Error()}, errors.Join(runErr, ErrUncertain, err)
		}
		if r.legacyMayHaveEffects() {
			r.blocked.Store(true)
			return Result{SessionID: sessionID, State: "uncertain", Error: ErrUncertain.Error()}, errors.Join(runErr, ErrUncertain)
		}
		out.State, out.Error, out.Output = "failed", runErr.Error(), nil
	}
	if err := r.finish(cleanup, req.WorkspaceID, req.SessionKey, req.RequestID, out); err != nil {
		r.blocked.Store(true)
		out.State, out.Error = "uncertain", ErrUncertain.Error()
		return out, errors.Join(ErrUncertain, err)
	}
	return out, runErr
}

// legacyMayHaveEffects is deliberately conservative: any permitted OpenCode
// capability might run an external tool before a failed prompt response.
func (r *Runtime) legacyMayHaveEffects() bool {
	for _, permission := range r.opts.Permissions {
		if permission.Action == "allow" {
			return true
		}
	}
	return false
}

func (r *Runtime) previous(ctx context.Context, req Request, hash string) (Result, bool, error) {
	rows, err := r.query(ctx, `SELECT input_hash,state,result FROM runs WHERE workspace=? AND request_id=?`, req.WorkspaceID, req.RequestID)
	if err != nil || len(rows) == 0 {
		return Result{}, false, err
	}
	if rows[0][0] != hash {
		return Result{}, true, ErrConflict
	}
	if rows[0][1] == "running" {
		return Result{}, true, ErrUncertain
	}
	var out Result
	if err := json.Unmarshal([]byte(rows[0][2].(string)), &out); err != nil {
		return Result{}, true, err
	}
	if out.State != "succeeded" {
		return out, true, errors.New(out.Error)
	}
	return out, true, nil
}

func (r *Runtime) session(ctx context.Context, req Request) (string, error) {
	rows, err := r.query(ctx, `SELECT backend,directory,profile,session_id,active_request FROM sessions WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil {
		return "", err
	}
	if len(rows) > 0 {
		row := rows[0]
		if row[0] != r.opts.BackendID || row[1] != r.opts.OpenCode.Directory || !r.compatibleProfile(row[2].(string)) {
			return "", ErrConflict
		}
		if row[4] != "" {
			return "", ErrUncertain
		}
		id := row[3].(string)
		if r.native != nil {
			return id, nil
		}
		if err := r.client.GetSession(ctx, id); err != nil {
			return "", err
		}
		idle, err := r.client.Idle(ctx, id)
		if err != nil {
			return "", err
		}
		if !idle {
			return "", ErrUncertain
		}
		return id, nil
	}
	id := "ses_" + rand.Text()
	if r.native == nil {
		id, err = r.client.CreateSession(ctx, "beesuh:"+req.WorkspaceID+":"+req.SessionKey, r.opts.Permissions)
		if err != nil {
			return "", err
		}
	}
	err = r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO sessions(workspace,session_key,backend,directory,profile,session_id) VALUES(?,?,?,?,?,?)`, Args: []any{req.WorkspaceID, req.SessionKey, r.opts.BackendID, r.opts.OpenCode.Directory, r.profile, id}}})
	// If saving fails, this unused upstream session can be orphaned, but no prompt
	// is sent. Never retry a prompt without a durable session mapping.
	return id, err
}

// Recover explicitly aborts unresolved sessions and records interrupted results.
// It never retries their tools or claims that their external effects rolled back.
func (r *Runtime) Recover(ctx context.Context) error {
	r.life.Lock()
	defer r.life.Unlock()
	if r.closed {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	rows, err := r.query(ctx, `SELECT workspace,session_key,session_id,active_request,backend,directory,profile FROM sessions WHERE active_request<>''`)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row[4] != r.opts.BackendID || row[5] != r.opts.OpenCode.Directory || !r.compatibleProfile(row[6].(string)) {
			return ErrConflict
		}
		id := row[2].(string)
		if err := r.stopSession(ctx, id); err != nil {
			return errors.Join(ErrUncertain, err)
		}
		out := Result{SessionID: id, State: "interrupted", Error: "execution interrupted; external tool effects may have occurred"}
		if err := r.finish(ctx, row[0].(string), row[1].(string), row[3].(string), out); err != nil {
			return err
		}
	}
	r.blocked.Store(false)
	return nil
}

func (r *Runtime) stopSession(ctx context.Context, id string) error {
	if r.native != nil {
		return nil
	}
	if err := r.client.Abort(ctx, id); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		idle, err := r.client.Idle(ctx, id)
		if err != nil {
			return err
		}
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Runtime) finish(ctx context.Context, workspace, key, request string, out Result) error {
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `UPDATE runs SET state=?,result=? WHERE workspace=? AND request_id=? AND state='running'`, Args: []any{out.State, string(b), workspace, request}, ExpectedRowsAffected: one()},
		{SQL: `UPDATE sessions SET active_request='' WHERE workspace=? AND session_key=? AND active_request=?`, Args: []any{workspace, key, request}, ExpectedRowsAffected: one()},
	})
}

func (r *Runtime) query(ctx context.Context, sql string, args ...any) ([][]any, error) {
	res, err := r.db.Query(ctx, rhiza.QueryRequest{SQL: sql, Args: args, Consistency: rhiza.ConsistencyLinearizable})
	return res.Rows, err
}

func (r *Runtime) mutate(ctx context.Context, statements []rhiza.SQLStatement) error {
	res, err := r.db.Execute(ctx, rhiza.ExecuteRequest{RequestID: rand.Text(), Statements: statements})
	if err != nil {
		return err
	}
	if res.Status != rhiza.MutationCommitted {
		return fmt.Errorf("beesuh: storage mutation rejected (%s)", res.ErrorCode)
	}
	return nil
}

func (r *Runtime) lockSession(ctx context.Context, key string) (func(), error) {
	r.mu.Lock()
	l := r.locks[key]
	if l == nil {
		l = &sessionLock{token: make(chan struct{}, 1)}
		r.locks[key] = l
	}
	l.refs++
	r.mu.Unlock()
	release := func() {
		r.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(r.locks, key)
		}
		r.mu.Unlock()
	}
	select {
	case l.token <- struct{}{}:
		return func() { <-l.token; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func validateRequest(req Request) error {
	for _, id := range []string{req.WorkspaceID, req.SessionKey, req.RequestID} {
		if strings.TrimSpace(id) == "" || len(id) > 256 || strings.ContainsRune(id, 0) {
			return errors.New("beesuh: identities must be nonempty, at most 256 bytes, without NUL")
		}
	}
	if req.Timeout < 0 || req.Prompt.NoReply || req.Prompt.MessageID != "" || strings.TrimSpace(req.Prompt.Text) == "" {
		return errors.New("beesuh: prompt text required; noReply, external message IDs and negative timeout unsupported")
	}
	if len(req.Prompt.Text)+len(req.Prompt.System)+len(req.Prompt.Schema) > 256<<10 {
		return errors.New("beesuh: input exceeds 256 KiB")
	}
	if m := req.Prompt.Model; m != nil && (m.ModelID == "" || m.ProviderID == "") {
		return errors.New("beesuh: model requires provider and model IDs")
	}
	return nil
}

type noExternalSchema struct{}

func (noExternalSchema) Load(string) (any, error) {
	return nil, errors.New("beesuh: external schema references unsupported")
}
func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noExternalSchema{})
	if err := c.AddResource("https://beesuh.invalid/output.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("https://beesuh.invalid/output.json")
}

func digest(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func one() *int64         { v := int64(1); return &v }

func (r *Runtime) compatibleProfile(profile string) bool {
	if r.native != nil {
		return r.native.compatibleProfile(profile)
	}
	return profile == r.profile
}
