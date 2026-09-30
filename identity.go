package beesuh

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/mrchypark/beesuh/auth"
	"github.com/mrchypark/beesuh/model"
	"strings"

	"github.com/mrchypark/rhiza"
)

var (
	ErrUnauthenticated       = errors.New("beesuh: authenticated service identity required")
	ErrForbidden             = errors.New("beesuh: access denied")
	ErrConnectionUnavailable = errors.New("beesuh: connection requires authentication or was revoked")
)

// Principal is established by the hosting service, never by request JSON or
// unverified identity headers. It deliberately contains no browser session/token.
type Principal struct {
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
}

type principalKey struct{}

// WithPrincipal is a trusted service integration API, not an authenticator.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
func (r *Runtime) RequiresIdentity() bool { return r.opts.RequireIdentity }

func (r *Runtime) requirePrincipal(ctx context.Context) (Principal, error) {
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	p, ok := PrincipalFromContext(ctx)
	if !ok || validIdentity(p.UserID, p.WorkspaceID) != nil || strings.ContainsAny(p.UserID+p.WorkspaceID, "\r\n") {
		return Principal{}, ErrUnauthenticated
	}
	if !r.opts.RequireIdentity || r.opts.Authorize == nil {
		return Principal{}, ErrForbidden
	}
	// The service checks current user activity and workspace membership on every
	// invocation, including queued and background work, without requiring a cookie.
	if err := r.opts.Authorize(ctx, p); err != nil {
		return Principal{}, ErrForbidden
	}
	return p, nil
}
func (r *Runtime) authorizeContext(ctx context.Context) error {
	if !r.opts.RequireIdentity {
		return ctx.Err()
	}
	_, err := r.requirePrincipal(ctx)
	return err
}

func (r *Runtime) scopedWorkspace(ctx context.Context, workspace string) (string, error) {
	if !r.opts.RequireIdentity {
		return workspace, nil
	}
	p, err := r.requirePrincipal(ctx)
	if err != nil {
		return "", err
	}
	if workspace != p.WorkspaceID {
		return "", ErrForbidden
	}
	// All existing session, request, memory and journal keys use this namespace.
	// JSON tuple encoding avoids delimiter collisions and hides raw identity in IDs.
	return "user_" + digest([]string{p.UserID, p.WorkspaceID}), nil
}

func (r *Runtime) bindRequest(ctx context.Context, req *Request) error {
	if !r.opts.RequireIdentity {
		if req.ConnectionID != "" || req.ConductorConnectionID != "" {
			return ErrForbidden
		}
		return nil
	}
	workspace, err := r.scopedWorkspace(ctx, req.WorkspaceID)
	if err != nil {
		return err
	}
	if req.ConnectionID == "" {
		return ErrConnectionUnavailable
	}
	c, err := r.lookupConnection(ctx, req.ConnectionID)
	if err != nil {
		return err
	}
	if c.Kind != "provider" || (req.Provider != "" && req.Provider != c.TargetID) {
		return ErrForbidden
	}
	req.Provider = c.TargetID
	if req.ConductorConnectionID != "" {
		c, err = r.lookupConnection(ctx, req.ConductorConnectionID)
		if err != nil {
			return err
		}
		if c.Kind != "conductor" {
			return ErrForbidden
		}
	}
	req.WorkspaceID = workspace
	return nil
}

var identityMigration = rhiza.Migration{Version: 4, Name: "identity-mode", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE runtime_identity (id INTEGER PRIMARY KEY CHECK(id=1), mode TEXT NOT NULL)`},
}}

// Prevent a later operator-mode open from bypassing isolation on a secured DB.
// Existing operator data must be explicitly migrated, never silently reassigned.
func (r *Runtime) pinIdentityMode(ctx context.Context) error {
	mode := "operator"
	if r.opts.RequireIdentity {
		mode = "service"
	}
	rows, err := r.query(ctx, `SELECT mode FROM runtime_identity WHERE id=1`)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		if rows[0][0] != mode {
			return errors.New("beesuh: data directory identity mode mismatch")
		}
		return nil
	}
	if r.opts.RequireIdentity {
		rows, err = r.query(ctx, `SELECT session_id FROM sessions LIMIT 1`)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			return errors.New("beesuh: existing operator sessions require explicit ownership migration")
		}
	}
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO runtime_identity(id,mode) VALUES(1,?)`, Args: []any{mode}}})
}

// Credential is resolved by the service for an owned Connection. HTTPClient is
// already authenticated; its transport owns OAuth refresh and persistence.
// APIKey remains a compatibility input, normalized to a client before execution.
// Managed CLI profiles supply CodexHome; external app-server profiles supply
// a profile-bound CodexAuth source. No credential field is serialized.
type Credential struct {
	HTTPClient *http.Client          `json:"-"`
	APIKey     string                `json:"-"`
	CodexHome  string                `json:"-"`
	AccountID  string                `json:"-"`
	CodexAuth  model.CodexAuthSource `json:"-"`
}

// Called while the caller holds the connection lifecycle lock. Resolvers must
// not call back into this connection's state mutation methods.
func (r *Runtime) resolveCredential(ctx context.Context, c Connection) (credential Credential, err error) {
	defer func() {
		if err != nil {
			err = r.connectionAuthError(ctx, c, err)
		}
	}()
	if r.opts.ResolveCredential == nil || c.CredentialRef == "" {
		return Credential{}, ErrConnectionUnavailable
	}
	credential, err = r.opts.ResolveCredential(ctx, c)
	if err != nil {
		return Credential{}, err
	}
	if validIdentity(credential.AccountID) != nil || strings.ContainsAny(credential.AccountID+credential.APIKey, "\r\n\x00") {
		return Credential{}, ErrConnectionUnavailable
	}
	var base *http.Client
	header := "Authorization"
	value := "Bearer " + credential.APIKey
	if c.Kind == "provider" {
		cfg, ok := r.native.opts.Providers[c.TargetID]
		if !ok {
			return Credential{}, ErrForbidden
		}
		if cfg.Kind == "codex-app-server" {
			if credential.CodexAuth == nil || credential.HTTPClient != nil || credential.APIKey != "" || credential.CodexHome != "" {
				return Credential{}, ErrConnectionUnavailable
			}
			return credential, nil
		}
		if credential.CodexAuth != nil {
			return Credential{}, ErrConnectionUnavailable
		}
		if cfg.Kind == "codex-cli" {
			if credential.HTTPClient != nil || credential.APIKey != "" || !filepath.IsAbs(credential.CodexHome) || strings.ContainsAny(credential.CodexHome, "\r\n\x00") {
				return Credential{}, ErrConnectionUnavailable
			}
			return credential, nil
		}
		base = cfg.HTTPClient
		if cfg.API == "anthropic" {
			header = "x-api-key"
			value = credential.APIKey
		}
	} else if c.Kind == "conductor" {
		cfg, ok := r.native.opts.Conductors[c.TargetID]
		if !ok {
			return Credential{}, ErrForbidden
		}
		base = cfg.HTTPClient
	} else {
		return Credential{}, ErrForbidden
	}
	if credential.CodexAuth != nil || credential.CodexHome != "" || (credential.HTTPClient != nil && credential.APIKey != "") {
		return Credential{}, ErrConnectionUnavailable
	}
	if credential.HTTPClient == nil {
		if strings.TrimSpace(credential.APIKey) == "" {
			return Credential{}, ErrConnectionUnavailable
		}
		credential.HTTPClient, err = auth.HeaderClient(base, http.Header{header: []string{value}})
		if err != nil {
			return Credential{}, ErrConnectionUnavailable
		}
	}
	credential.APIKey = ""
	return credential, nil
}

// A confirmed authentication rejection invalidates only its connection. Other
// failures keep login state intact. Never persist or expose upstream diagnostics.
func (r *Runtime) connectionAuthError(ctx context.Context, c Connection, err error) error {
	if err == nil {
		return nil
	}
	invalid := errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, ErrConnectionUnavailable)
	if invalid {
		if c.State == "ready" {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if e := r.mutate(cleanup, []rhiza.SQLStatement{{SQL: `UPDATE connections SET state='needs_login',generation=generation+1 WHERE id=? AND generation=? AND state='ready'`, Args: []any{c.ID, c.Generation}, ExpectedRowsAffected: one()}}); e != nil {
				r.blocked.Store(true)
				return ErrUncertain
			}
		}
		return ErrConnectionUnavailable
	}
	if errors.Is(err, ErrForbidden) || errors.Is(err, auth.ErrForbidden) {
		return ErrForbidden
	}
	if errors.Is(err, ErrUnauthenticated) {
		return ErrUnauthenticated
	}
	return auth.Sanitize(err)
}
