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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	ErrCapacity     = errors.New("beesuh: retained capacity reached")
	// ErrResolverReentry rejects a credential resolver that calls back into the
	// runtime. Run holds the lifecycle read lock for the whole execution, so a
	// nested read lock behind a pending Close would deadlock both operations and
	// context expiry cannot interrupt it.
	ErrResolverReentry = errors.New("beesuh: credential resolver must not call back into the runtime")
)

// reclaimTimeout bounds the best-effort delete of an untracked OpenCode
// session, and the rediscovery that proves ownership of a session whose create
// response was lost. Both run on a context detached from the failed request, so
// they need their own limit to keep a slow or unresponsive server from blocking
// the caller.
const reclaimTimeout = 30 * time.Second

// pendingCreateMigration records an attempt to create an OpenCode session
// before the request is sent. Without a durable intent there is no proof that a
// session found on the server belongs to this runtime, so recovery could only
// guess and might prompt an unrelated conversation. The row is written first
// and cleared once the session mapping is durable.
var pendingCreateMigration = rhiza.Migration{Version: 10, Name: "pending-session-create", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE pending_session_creates (workspace TEXT NOT NULL, session_key TEXT NOT NULL, ownership_token TEXT NOT NULL, backend TEXT NOT NULL, directory TEXT NOT NULL, profile TEXT NOT NULL, phase TEXT NOT NULL DEFAULT 'creating', remote_id TEXT NOT NULL DEFAULT '', recorded_at INTEGER NOT NULL, PRIMARY KEY(workspace,session_key))`},
}}

// Prompt and Model preserve the original OpenCode adapter API. Native mode uses the
// configured provider model and does not accept per-request model overrides.
type Prompt = opencode.Prompt
type Model = opencode.Model

type Options struct {
	ResolveCredential func(context.Context, Connection) (Credential, error)
	// RevokeCredential must revoke only the supplied connection generation and
	// be idempotent for that identity. The host must not retarget its credential
	// reference to a newer credential while this callback can still run.
	RevokeCredential  func(context.Context, Connection) error
	RevocationTimeout time.Duration
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
	Capacity                  CapacityLimits
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
	// FailureCategory is a bounded public classification of a terminal failure.
	// It lets a replay return the same API category as the original execution
	// without parsing diagnostic text or serializing an error implementation.
	FailureCategory string `json:"failure_category,omitempty"`
}

type sessionLock struct {
	token chan struct{}
	refs  int
}

// Runtime owns its Rhiza directory exclusively. All clients of its OpenCode
// sessions must go through this instance for the concurrency limits to hold.
type Runtime struct {
	db      runtimeDB
	client  *opencode.Client
	native  *nativeEngine
	opts    Options
	profile string
	file    *os.File
	life    sync.RWMutex
	closed  bool
	// runFenced holds workspaces with runs that require Recover. It is the only
	// volatile part of the fence: the durable effect half lives in
	// uncertain_effects, so a pending row written by another runtime is never
	// missed. Status takes r.mu across both halves of its observation, so a fence
	// install cannot slip between them.
	runFenced        map[string]struct{}
	slots, admission chan struct{}
	// fenceMu keeps an operator reconciliation from resolving an effect while
	// its POST is still in flight. Dispatch holds the read lock, resolution
	// holds the write lock, so a fence row can only change disposition while no
	// effect for it is being sent. Multiple dispatches stay concurrent.
	fenceMu  sync.RWMutex
	mu       sync.Mutex
	locks    map[string]*sessionLock
	capacity CapacityLimits
}

// runtimeDB keeps production on Rhiza while allowing deterministic storage
// acknowledgement-loss tests around the narrow persistence boundary.
type runtimeDB interface {
	Close() error
	Execute(context.Context, rhiza.ExecuteRequest) (rhiza.ExecuteResponse, error)
	Query(context.Context, rhiza.QueryRequest) (rhiza.QueryResponse, error)
}

func Open(ctx context.Context, opts Options) (*Runtime, error) {
	if !opts.Capacity.valid() {
		return nil, errors.New("beesuh: capacity limits cannot be negative")
	}
	opts.Capacity = opts.Capacity.withDefaults()
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
	} else if opts.Native != nil && len(opts.Native.Conductors) != 0 {
		// Operator mode has no connection IDs and no principal scoping, so a named
		// target can never be resolved. Advertising those tools would consume model
		// turns before reporting a misconfiguration.
		return nil, errors.New("beesuh: named Conductor targets require service mode")
	}

	if opts.DataDir == "" || strings.TrimSpace(opts.BackendID) == "" || len(opts.BackendID) > 256 || len(opts.OpenCode.Directory) > 4096 || opts.MaxConcurrent < 1 || opts.MaxWaiting < 0 || opts.MaxConcurrent > 1024 || opts.MaxWaiting > 10000 {
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
		if opts.RequireIdentity {
			native, err = newServiceNative(*opts.Native)
		} else {
			native, err = newNative(*opts.Native)
		}
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
	r := &Runtime{db: db, client: client, native: native, opts: opts, file: f, slots: make(chan struct{}, opts.MaxConcurrent), admission: make(chan struct{}, opts.MaxConcurrent+opts.MaxWaiting), locks: make(map[string]*sessionLock), capacity: opts.Capacity}
	r.profile = digest(opts.Permissions)
	if native != nil {
		r.profile = native.profile
	}
	err = db.Migrate(ctx, []rhiza.Migration{{Version: 1, Name: "sessions-and-runs", Statements: []rhiza.SQLStatement{
		{SQL: `CREATE TABLE sessions (workspace TEXT NOT NULL, session_key TEXT NOT NULL, backend TEXT NOT NULL, directory TEXT NOT NULL, profile TEXT NOT NULL, session_id TEXT NOT NULL, active_request TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,session_key))`},
		{SQL: `CREATE TABLE runs (workspace TEXT NOT NULL, request_id TEXT NOT NULL, session_key TEXT NOT NULL, input_hash TEXT NOT NULL, state TEXT NOT NULL, result TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,request_id))`},
	}}, nativeMigration, connectionMigration, identityMigration, connectionCredentialMigration, workspaceUncertaintyMigration, retentionMigration, connectionRevocationMigration, runsHistoryIndexMigration, pendingCreateMigration, connectionRevocationBackfillMigration, capacityMigration})
	if err == nil {
		err = r.pinIdentityMode(ctx)
	}
	if err == nil {
		// This process exclusively owns the directory. startRun atomically
		// creates a running row and removes its reservation before dispatch.
		// Surviving reservations therefore represent pre-dispatch work only.
		// Remote session creation has its own durable intent, which stays intact.
		err = r.mutate(ctx, []rhiza.SQLStatement{{SQL: `DELETE FROM capacity_run_reservations`}})
	}
	if err == nil && native != nil {
		// Reconcile effects that predate the uncertainty table before any
		// dispatch is admitted. An empty table must not silently reopen a
		// workspace that already lost a Conductor acknowledgement.
		err = r.reconcileLegacyIntents(ctx)
	}
	if err == nil {
		// A run left mid-flight by a crash is unresolved evidence, so its workspace
		// fence starts closed for it and recovery stays explicit. The evidence is
		// recorded per workspace rather than in one runtime-wide flag: a crash in
		// workspace A says nothing about workspace B.
		var rows [][]any
		rows, err = r.query(ctx, `SELECT DISTINCT workspace FROM runs WHERE state='running'`)
		if err == nil {
			r.runFenced = make(map[string]struct{}, len(rows))
			for _, row := range rows {
				r.runFenced[row[0].(string)] = struct{}{}
			}
		}
	}
	if err == nil {
		// A durable pending effect outlives the process that recorded it, so warn
		// at startup rather than letting an operator learn about it from a
		// refused dispatch. The check also fails startup if the durable state
		// cannot be read, instead of serving a runtime whose own fence view is
		// unreliable.
		var pending bool
		pending, err = r.hasPendingEffect(ctx)
		if err == nil {
			if pending {
				slog.Warn("beesuh: opening with unresolved external effects; Conductor dispatch is refused until an operator resolves them", "backend", opts.BackendID)
			}
		}
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
	if err := rejectResolverReentry(ctx); err != nil {
		return Result{}, err
	}
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
	// A workspace with a recovery fence admits no new run: an earlier run in it
	// was left unresolved and only Recover can settle that. The durable
	// uncertain-effect record is checked per workspace rather than through a
	// runtime-wide flag, so an unresolved effect in workspace A never denies
	// service to workspace B.
	if r.runFenceHolds(req.WorkspaceID) {
		return Result{}, ErrUncertain
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	// Re-check after waiting for a slot. The reservation below is the final
	// admission gate, so a recovery fence observed late must still deny service.
	if r.runFenceHolds(req.WorkspaceID) {
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
	if err := r.reserveRun(ctx, req); err != nil {
		// A storage failure can be reported after this pre-dispatch reservation
		// committed. This request lock excludes a live sibling with these keys.
		if cleanupErr := r.releaseRunReservation(req); cleanupErr != nil {
			return Result{}, errors.Join(err, cleanupErr)
		}
		return Result{}, err
	}
	reserved := true
	defer func() {
		if reserved {
			if err := r.releaseRunReservation(req); err != nil {
				slog.Error("beesuh: releasing failed run admission reservation", "workspace", req.WorkspaceID, "request", req.RequestID, "error", err)
			}
		}
	}()
	sessionID, err := r.session(ctx, req)
	if err != nil {
		return Result{}, err
	}
	// Persist the uncertain interval before sending anything to the agent.
	// The statements form one atomic batch. A failure therefore leaves either
	// the running row plus active_request committed, which Recover
	// discovers) or neither, with no external dispatch to reconcile. Installing a
	// run fence here would strand the workspace with no evidence to lift it.
	err = r.startRun(ctx, req, hash)
	if err != nil {
		return Result{}, err
	}
	reserved = false
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
			r.fenceWorkspace(req.WorkspaceID)
			return Result{SessionID: sessionID, State: "uncertain", Error: ErrUncertain.Error()}, errors.Join(runErr, ErrUncertain, err)
		}
		if r.legacyMayHaveEffects() {
			r.fenceWorkspace(req.WorkspaceID)
			return Result{SessionID: sessionID, State: "uncertain", Error: ErrUncertain.Error()}, errors.Join(runErr, ErrUncertain)
		}
		out.State, out.Output = "failed", nil
		setFailure(&out, runErr)
	}
	if err := r.finish(cleanup, req.WorkspaceID, req.SessionKey, req.RequestID, out); err != nil {
		r.fenceWorkspace(req.WorkspaceID)
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
		return out, true, replayFailure(out.FailureCategory, out.Error)
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
		// The mapping is the authoritative ownership record, so a create intent
		// that survived a mapping commit is stale: it no longer describes an
		// unresolved create. It must not survive, because the next request would
		// otherwise take the adoption path, look up a session by its title, and
		// possibly adopt the very session this mapping already owns.
		if err := r.clearPendingCreate(req); err != nil {
			return "", errors.Join(err, errors.New("beesuh: clearing a stale session-create intent failed"))
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
		// Record the attempt before sending anything, so a create whose response
		// is lost can later be resolved from durable evidence instead of a
		// guess. The token is unguessable, which is what makes title matching a
		// proof of ownership: a session a user created by hand can never carry it.
		ownershipToken, phase, remoteID, resumed, err := r.pendingCreateToken(ctx, req)
		if err != nil {
			return "", err
		}
		adopted := ""
		switch phase {
		case pendingPhaseCleanup:
			// A delete was issued for this token's session and its outcome is
			// unknown, so the server may still be destroying it. That session
			// cannot go back into the adoption pool: adopting it would let a
			// late delete destroy a mapped, prompted conversation. Retry the
			// delete for the known ID instead, and only then settle the intent.
			reclaimed, reclaimErr := r.reclaimPendingSession(ctx, req, remoteID)
			if reclaimErr != nil {
				return "", reclaimErr
			}
			if reclaimed {
				return "", ErrUncertain
			}
			ownershipToken, phase, remoteID, resumed, err = r.pendingCreateToken(ctx, req)
			if err != nil {
				return "", err
			}
			if resumed || phase != pendingPhaseCreating {
				return "", ErrUncertain
			}
		case pendingPhaseCreating:
			// A crash between a successful create and the local mapping insert
			// strands the remote session under an intent that is still durable.
			// The intent names that session with an unguessable token, so a title
			// match proves this runtime created it under an exclusively managed
			// namespace, and the intent is cleared before any prompt is ever sent
			// through a durable mapping. Adopting it keeps the session reachable
			// instead of creating a second one and orphaning the first forever.
			// Adoption requires a title match that nothing else can produce, so the
			// deployment must dedicate the provider's session namespace to this
			// runtime; see docs/uncertainty-and-retention-policy.md.
			// This runs on a detached, bounded context because the request that
			// failed is usually already cancelled.
			var adoptErr error
			adopted, adoptErr = r.adoptOrReclaim(ctx, ownershipToken)
			if adoptErr != nil {
				return "", adoptErr
			}
		}
		if resumed && adopted == "" {
			// Zero matches after a create whose outcome is unknown does not prove
			// the earlier POST finished. The server may still be creating the
			// session now, so issuing another POST could duplicate the very
			// conversation this intent exists to recover. Keep the intent as the
			// only evidence and let a later request settle it.
			return "", ErrUncertain
		}
		if adopted != "" {
			id = adopted
		} else {
			id, err = r.client.CreateSession(ctx, pendingCreateTitle(ownershipToken), r.opts.Permissions)
			if err != nil {
				// The create may have reached the server and lost only its
				// response, so look once more before reporting the failure. A
				// durable intent is kept either way: it is the only evidence that
				// can still find the session a lost response may have created.
				var adoptErr error
				adopted, adoptErr = r.adoptOrReclaim(ctx, ownershipToken)
				if adoptErr != nil {
					return "", errors.Join(err, adoptErr)
				}
				if adopted == "" {
					return "", err
				}
				id = adopted
				err = nil
			}
		}
	}
	err = r.insertSession(ctx, req, id, r.native == nil)
	if err != nil && r.native == nil {
		// The remote session exists but nothing durable points at it, so no later
		// abort, recovery, or retention pass can ever reach it. Release it here.
		// A failed reclaim leaves an orphan, never an untracked prompt: no prompt
		// is sent on this path in either case.
		//
		// A mapping insert error alone does not prove the session is untracked.
		// Storage can report failure for a write that already committed, so
		// deleting on that evidence would destroy a mapped, possibly prompted
		// conversation. The authoritative mapping and the owning intent are both
		// consulted, and the session is released only when neither references it.
		// The mapping check must not inherit a cancelled request context: a
		// cancellation is itself a common cause of the mapping failure, and an
		// unreadable mapping would look like a referenced session and skip the
		// reclaim that keeps the remote side clean. Give the verification and
		// the reclaim their own bounded budget.
		verifyCtx, verifyCancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimTimeout)
		mapped, referenced := r.sessionUntracked(verifyCtx, req, id)
		verifyCancel()
		if referenced {
			// Another writer, or this runtime's own committed write, already owns
			// the session. Releasing it would delete live conversation history.
			slog.Error("beesuh: session mapping insert failed but the session is referenced; preserving it", "session", id, "mapped", mapped)
			// A committed pending mapping consumes its intent in the same mutation.
			return mapped, err
		}
		// Persist the cleanup phase before the first DELETE. A crash after the
		// remote call but before this write would otherwise let a resumed
		// creating intent adopt a session whose delete is still in flight.
		if markErr := r.markPendingCreateCleanup(ctx, req, id); markErr != nil {
			return "", errors.Join(err, ErrUncertain, markErr)
		}
		// The reclaim must not inherit a cancelled or expired request context,
		// because a cancelled request is itself a common cause of the mapping
		// failure and would abort the DELETE before it is sent. Bound it
		// separately so cleanup stays bounded but always gets its own budget.
		reclaimCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimTimeout)
		deleteErr := r.client.DeleteSession(reclaimCtx, id)
		cancel()
		if deleteErr != nil {
			slog.Error("beesuh: reclaiming an untracked opencode session failed", "session", id, "error", deleteErr)
			// The delete outcome is unknown, so the session may still be
			// destroyed later. Record that uncertainty against the intent and keep
			// it: clearing would discard the only evidence, let the next request
			// create a fresh session, and strand this one permanently.
			return "", err
		}
		// The remote session is released or gone, so the intent that could adopt
		// it must not survive. Leaving it would let a later create for the same
		// session key match a stale title and adopt an unrelated session.
		if clearErr := r.clearPendingCreate(req); clearErr != nil {
			slog.Error("beesuh: clearing a settled session-create intent failed", "session_key", req.SessionKey, "error", clearErr)
		}
	}
	// Never retry a prompt without a durable session mapping.
	return id, err
}

// Session-create intent phases. The phase records which remote operation is
// still unresolved, because the safe recovery differs per phase: an unresolved
// create must be looked up before anything else, while an unresolved delete
// must be finished and must never return its target to the adoption pool.
const (
	// pendingPhaseCreating means a create may have produced a session whose
	// local mapping was never written.
	pendingPhaseCreating = "creating"
	// pendingPhaseCleanup identifies a reserved delete for remote_id whose
	// outcome may become unknown. It is persisted before the first DELETE so a
	// restart never adopts a session whose delete can still be in flight.
	pendingPhaseCleanup = "cleanup"
)

// pendingCreateTitle is the only title a pending session carries. It embeds an
// unguessable token rather than the workspace and session key, so matching it
// identifies a session Beesuh created rather than one a user made by hand.
// Unguessability is a naming argument, not an authorization boundary: anyone
// who can observe the title can copy it onto another session. Adoption
// therefore requires the provider's session namespace to be dedicated to this
// runtime, which is a deployment obligation recorded in
// docs/uncertainty-and-retention-policy.md.
func pendingCreateTitle(ownershipToken string) string { return "beesuh:pending:" + ownershipToken }

// pendingCreateToken returns the ownership token for a session create, writing
// the durable intent first. A surviving intent from an earlier attempt keeps its
// token and reports resumed, so a session stranded by a crash between the
// create and the mapping insert stays reachable instead of being orphaned under
// a token nobody holds. Only a resumed intent triggers the recovery lookup, so
// a normal first-time create never depends on the session list endpoint.
//
// Without this row, a session found on the server after a lost response is
// unproven and must never be adopted, because prompting it would write into an
// unrelated conversation.
func (r *Runtime) pendingCreateToken(ctx context.Context, req Request) (token, phase, remoteID string, resumed bool, err error) {
	rows, err := r.query(ctx, `SELECT ownership_token,phase,remote_id,backend,directory,profile FROM pending_session_creates WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil {
		return "", "", "", false, err
	}
	if len(rows) > 0 {
		// A surviving intent describes a session created under the configuration
		// that was live when the attempt was recorded. Adoption maps that remote
		// session to the current request, so it must apply the same compatibility
		// test as an ordinary mapped session (see session()). Without this, a
		// restart under changed OpenCode permissions would prompt an orphan created
		// with the old permissions as if the new profile owned it, and a changed
		// backend or directory would adopt a session the current configuration
		// cannot reach. Refusing leaves the intent as durable evidence for an
		// operator instead of silently mapping it.
		if rows[0][3] != r.opts.BackendID || rows[0][4] != r.opts.OpenCode.Directory || !r.compatibleProfile(rows[0][5].(string)) {
			return "", "", "", false, ErrConflict
		}
		return rows[0][0].(string), rows[0][1].(string), rows[0][2].(string), true, nil
	}
	token = rand.Text()
	err = r.reservePendingCreate(ctx, req, token)
	if err != nil {
		return "", "", "", false, err
	}
	// A conflicting writer may have committed first. DO NOTHING reports success
	// for that case, so the token just generated might not be the durable one.
	// Creating under a token no later request can read would strand the session
	// with no intent to adopt or reclaim it, so re-read and use the winner's.
	rows, err = r.query(ctx, `SELECT ownership_token,phase,remote_id,backend,directory,profile FROM pending_session_creates WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil {
		return "", "", "", false, err
	}
	if len(rows) == 0 {
		// The conflicting writer already settled and cleared the intent. This
		// request owns no durable proof, so it must not create under a token
		// nothing can rediscover; let the caller retry from a clean state.
		return "", "", "", false, errors.New("beesuh: session-create intent was settled concurrently")
	}
	if durable := rows[0][0].(string); durable != token {
		return durable, rows[0][1].(string), rows[0][2].(string), true, nil
	}
	return token, rows[0][1].(string), rows[0][2].(string), false, nil
}

// clearPendingCreate drops a settled intent. It runs on a detached, bounded
// context because the common caller is a cancelled or expired request whose
// own context can no longer reach storage.
func (r *Runtime) clearPendingCreate(req Request) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `DELETE FROM pending_session_creates WHERE workspace=? AND session_key=?`, Args: []any{req.WorkspaceID, req.SessionKey}}})
}

// adoptOrReclaim resolves a create whose response was lost to a cancelled or
// expired request. Only a session carrying this runtime's ownership token is a
// candidate. That token is unguessable, so a match identifies a session this
// runtime created rather than an unrelated conversation, as long as the
// provider's session namespace is dedicated to this runtime; a title copied
// from an observable one would otherwise defeat the match.
//
// Exactly one candidate may be adopted, because adopting the wrong session
// would send a prompt into an unrelated conversation. Several candidates are
// ambiguous: no evidence identifies which one this intent owns, and a title
// match does not prove a session is unreferenced or unprompted, so recovery
// fails closed without deleting anything. Deleting an ambiguous candidate could
// destroy a mapped, live conversation, and clearing the intent would discard the
// only evidence that a create is still unresolved.
func (r *Runtime) adoptOrReclaim(ctx context.Context, ownershipToken string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimTimeout)
	defer cancel()
	ids, err := r.client.ListSessionsByTitle(ctx, pendingCreateTitle(ownershipToken))
	if err != nil {
		return "", fmt.Errorf("beesuh: rediscovering an untracked opencode session: %w", err)
	}
	switch len(ids) {
	case 0:
		// The server never created it, so there is nothing to reclaim.
		return "", nil
	case 1:
		return ids[0], nil
	default:
		return "", errors.New("beesuh: multiple untracked opencode sessions share one ownership token")
	}
}

// sessionUntracked reports whether a durable mapping references a remote
// session, along with the session id a mapping records for this request key.
//
// A mapping insert error is not proof that no mapping was written: storage can
// report failure for a write that already committed, so deleting on that
// evidence would destroy a mapped and possibly prompted conversation. A session
// is releasable only when one linearizable read proves that no mapping
// references it, either under this request key or under any other key in the
// workspace. referenced is true whenever a mapping exists or the answer cannot
// be read, and the mapped id is returned so the caller can report the canonical
// session instead of deleting it.
//
// Both conditions are answered by a single statement. Two sequential reads
// could each succeed while a concurrent writer committed a mapping between
// them, which would report an unreferenced session and delete a live one.
func (r *Runtime) sessionUntracked(ctx context.Context, req Request, id string) (mapped string, referenced bool) {
	rows, err := r.query(ctx, `SELECT session_id,session_key FROM sessions WHERE workspace=? AND (session_key=? OR session_id=?)`, req.WorkspaceID, req.SessionKey, id)
	if err != nil {
		// The mapping could not be read, so it cannot be proven absent. Keep the
		// session: releasing it on unreadable evidence would risk deleting a
		// mapped conversation.
		return "", true
	}
	if len(rows) > 0 {
		// A row for this request key names the canonical session for it. A row
		// under another key still proves the remote session is tracked, but that
		// mapping must not be reported as this request key's own session.
		for _, row := range rows {
			if row[1].(string) == req.SessionKey {
				mapped, _ = row[0].(string)
				break
			}
		}
		return mapped, true
	}
	return "", false
}

// markPendingCreateCleanup records the remote ID before a DELETE is issued.
// The intent is kept so a later request can finish a lost or failed delete;
// clearing it here would return the runtime to the original untracked orphan
// failure.
//
// The update is conditional on the phase it expects, and the affected row
// count is verified. Storage can report success for a statement that matched
// nothing, which would leave a creating-phase intent holding a session whose
// delete is unresolved: the next request would look the session up by title,
// adopt it, and prompt a conversation that is about to be destroyed. A count
// of zero therefore keeps the intent unusable for adoption and reports the
// uncertainty instead.
func (r *Runtime) markPendingCreateCleanup(ctx context.Context, req Request, remoteID string) error {
	if remoteID == "" {
		return errors.New("beesuh: refusing to record an unresolved cleanup without a session id")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimTimeout)
	defer cancel()
	expected := int64(1)
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `UPDATE pending_session_creates SET phase=?,remote_id=? WHERE workspace=? AND session_key=? AND phase=?`, Args: []any{pendingPhaseCleanup, remoteID, req.WorkspaceID, req.SessionKey, pendingPhaseCreating}, ExpectedRowsAffected: &expected}})
}

// reclaimPendingSession finishes a delete whose outcome was unknown. It
// reports whether the session was released. A delete that is still uncertain
// keeps the intent in the cleanup phase, and the session never returns to the
// adoption pool, because a late delete must not destroy a mapped conversation.
func (r *Runtime) reclaimPendingSession(ctx context.Context, req Request, remoteID string) (bool, error) {
	if remoteID == "" {
		// A cleanup phase with no known remote id cannot be retried, and clearing
		// it would return the session it failed to release to the adoption pool.
		// The next request would look that session up by title, adopt it, and
		// prompt a conversation whose delete outcome is unknown. Fail closed
		// until an operator settles the intent.
		return false, errors.Join(ErrUncertain, errors.New("beesuh: session cleanup is unresolved and its session id is unknown"))
	}
	reclaimCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimTimeout)
	deleteErr := r.client.DeleteSession(reclaimCtx, remoteID)
	cancel()
	if deleteErr != nil {
		// The cleanup intent already records this target before the DELETE.
		// Leave it intact for the next explicit recovery attempt.
		return false, errors.Join(ErrUncertain, deleteErr)
	}
	// The delete was confirmed, so the session is gone and the intent that could
	// adopt it must not survive.
	if err := r.clearPendingCreate(req); err != nil {
		return false, err
	}
	return true, nil
}

// Recover explicitly aborts unresolved runs and settles admitted session creates
// or cleanup without admitting a new run. It never retries tools or claims that
// their external effects rolled back.
func (r *Runtime) Recover(ctx context.Context) error {
	if err := rejectResolverReentry(ctx); err != nil {
		return err
	}
	r.life.Lock()
	defer r.life.Unlock()
	if r.closed {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	// Order the scan so one recovery attempt settles workspaces in a fixed
	// sequence. Each workspace is unfenced as soon as its own runs are persisted,
	// and a deterministic order keeps that sequence reproducible when a later
	// workspace fails and a retry resumes the work.
	rows, err := r.query(ctx, `SELECT workspace,session_key,session_id,active_request,backend,directory,profile FROM sessions WHERE active_request<>'' ORDER BY workspace,session_key`)
	if err != nil {
		return err
	}
	// A finish can commit while its acknowledgement is lost. Rebuild the run
	// fences from durable work, including running rows without a session mapping.
	// life is exclusively held, so no Run can install a newer obligation between
	// this linearizable read and the map update. Effect fences remain untouched.
	outstanding, err := r.query(ctx, `SELECT workspace FROM sessions WHERE active_request<>'' UNION SELECT workspace FROM runs WHERE state='running'`)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.runFenced = make(map[string]struct{}, len(outstanding))
	for _, row := range outstanding {
		r.runFenced[row[0].(string)] = struct{}{}
	}
	r.mu.Unlock()
	// pending tallies every discovered run each workspace owes before any of them
	// is settled. Counting during the scan itself would let the first row of a
	// two-run workspace drop the count to zero and reopen it while its second run
	// was still unresolved.
	pending := make(map[string]int)
	for _, row := range rows {
		pending[row[0].(string)]++
	}
	for _, row := range rows {
		if row[4] != r.opts.BackendID || row[5] != r.opts.OpenCode.Directory || !r.compatibleProfile(row[6].(string)) {
			return ErrConflict
		}
		workspace := row[0].(string)
		if err := r.recoverRun(ctx, workspace, row); err != nil {
			return err
		}
		pending[workspace]--
		if pending[workspace] == 0 {
			// Finishing the discovered sessions does not settle an orphan running
			// row. Prove both sources empty before clearing this workspace, without
			// delaying its release until later workspaces have also succeeded.
			if err := r.clearCompletedRunFence(ctx, workspace); err != nil {
				return err
			}
		}
	}
	r.mu.Lock()
	unresolved := len(r.runFenced) != 0
	r.mu.Unlock()
	if unresolved {
		return errors.Join(ErrUncertain, errors.New("beesuh: running work remains without a recoverable session"))
	}
	return r.recoverSessionCreates(ctx)
}

// clearCompletedRunFence requires the caller to hold life exclusively. A failed
// read leaves the fence intact even when the preceding finish already committed.
func (r *Runtime) clearCompletedRunFence(ctx context.Context, workspace string) error {
	remaining, err := r.query(ctx, `SELECT 1 FROM sessions WHERE workspace=? AND active_request<>'' UNION ALL SELECT 1 FROM runs WHERE workspace=? AND state='running' LIMIT 1`, workspace, workspace)
	if err != nil {
		return err
	}
	if len(remaining) == 0 {
		r.unfenceRunWorkspace(workspace)
	}
	return nil
}

// recoverRun persists the interrupted result of one discovered session. It
// returns ErrUncertain when the remote session could not be stopped, which
// leaves that run unsettled and its workspace fenced.
func (r *Runtime) recoverRun(ctx context.Context, workspace string, row []any) error {
	id := row[2].(string)
	if err := r.stopSession(ctx, id); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	out := Result{SessionID: id, State: "interrupted", Error: "execution interrupted; external tool effects may have occurred"}
	return r.finish(ctx, workspace, row[1].(string), row[3].(string), out)
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
		return storageMutationError{code: res.ErrorCode}
	}
	return nil
}

type storageMutationError struct{ code string }

func (e storageMutationError) Error() string {
	return fmt.Sprintf("beesuh: storage mutation rejected (%s)", e.code)
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
