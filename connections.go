package beesuh

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mrchypark/rhiza"
)

// Connection is a service-owned reference to a configured provider or
// Conductor connector. It deliberately contains no credential material.
type Connection struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	WorkspaceID   string `json:"workspace_id"`
	Kind          string `json:"kind"`
	TargetID      string `json:"-"`
	CredentialRef string `json:"-"`
	State         string `json:"state"`
	Generation    int64  `json:"generation"`
}

var connectionMigration = rhiza.Migration{Version: 3, Name: "connections", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE connections (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, workspace_id TEXT NOT NULL, kind TEXT NOT NULL, target_id TEXT NOT NULL, state TEXT NOT NULL)`},
	{SQL: `CREATE INDEX connections_owner ON connections(user_id,workspace_id,id)`},
}}

var connectionCredentialMigration = rhiza.Migration{Version: 5, Name: "connection-credentials", Statements: []rhiza.SQLStatement{
	{SQL: `ALTER TABLE connections ADD COLUMN credential_ref TEXT NOT NULL DEFAULT ''`},
	{SQL: `ALTER TABLE connections ADD COLUMN generation INTEGER NOT NULL DEFAULT 1`},
}}

func validConnectionText(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n")
}

func validateConnection(c Connection) error {
	if !validConnectionText(c.ID) || !validConnectionText(c.UserID) || !validConnectionText(c.WorkspaceID) || !validConnectionText(c.TargetID) || !validConnectionText(c.CredentialRef) {
		return errors.New("beesuh: connection identities must be nonempty, at most 256 bytes, without NUL")
	}
	if c.Kind != "provider" && c.Kind != "conductor" {
		return errors.New("beesuh: connection kind must be provider or conductor")
	}
	if c.State != "ready" && c.State != "needs_login" && c.State != "revoked" {
		return errors.New("beesuh: invalid connection state")
	}
	if c.Generation < 0 {
		return errors.New("beesuh: invalid connection generation")
	}
	return nil
}

// RegisterConnection provisions an immutable service-owned connection. It is
// intentionally not principal-scoped: only the service's trusted setup path
// should call it. Credentials remain with the corresponding adapter.
func (r *Runtime) RegisterConnection(ctx context.Context, c Connection) error {
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return ErrClosed
	}
	if !r.opts.RequireIdentity || r.native == nil {
		return errors.Join(ErrInvalidInput, errors.New("beesuh: connections require identity-enabled native runtime"))
	}
	if err := validateConnection(c); err != nil {
		return errors.Join(ErrInvalidInput, err)
	}
	c.Generation = 1
	if c.Kind == "provider" {
		if _, ok := r.native.clients[c.TargetID]; !ok {
			return errors.Join(ErrInvalidInput, errors.New("beesuh: connection provider is not configured"))
		}
	} else if _, ok := r.native.opts.Conductors[c.TargetID]; !ok {
		return errors.Join(ErrInvalidInput, errors.New("beesuh: connection conductor is not configured"))
	}
	unlock, err := r.lockSession(ctx, "connection\x00"+c.ID)
	if err != nil {
		return err
	}
	defer unlock()

	rows, err := r.query(ctx, `SELECT user_id,workspace_id,kind,target_id,credential_ref,state,generation FROM connections WHERE id=?`, c.ID)
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		existing := connectionFromRow(c.ID, rows[0])
		if existing.UserID == c.UserID && existing.WorkspaceID == c.WorkspaceID && existing.Kind == c.Kind && existing.TargetID == c.TargetID && existing.CredentialRef == c.CredentialRef {
			return nil
		}
		return errors.Join(ErrConflict, errors.New("beesuh: connection ID already belongs to another target"))
	}
	if c.State == "ready" {
		// Validate before insertion; a new connection has no persisted state to
		// invalidate if the service cannot resolve its account.
		pending := c
		pending.State = "needs_login"
		if _, err := r.resolveCredential(ctx, pending); err != nil {
			return err
		}
	}
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `INSERT INTO connections(id,user_id,workspace_id,kind,target_id,credential_ref,state,generation) VALUES(?,?,?,?,?,?,?,?)`, Args: []any{c.ID, c.UserID, c.WorkspaceID, c.Kind, c.TargetID, c.CredentialRef, c.State, c.Generation}}})
}

// Connections returns only connections owned by the authenticated principal.
// TargetID is intentionally omitted from JSON so callers cannot treat this as
// a credential or routing configuration endpoint.
func (r *Runtime) Connections(ctx context.Context) ([]Connection, error) {
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	p, err := r.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.query(ctx, `SELECT id,user_id,workspace_id,kind,target_id,credential_ref,state,generation FROM connections WHERE user_id=? AND workspace_id=? ORDER BY id`, p.UserID, p.WorkspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]Connection, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectionFromFullRow(row))
	}
	return out, nil
}

// Connection returns a ready connection owned by the authenticated principal.
func (r *Runtime) Connection(ctx context.Context, id string) (Connection, error) {
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return Connection{}, ErrClosed
	}
	return r.lookupConnection(ctx, id)
}

// SetConnectionState changes an owned connection's state. A revoked
// connection is terminal and must be provisioned under a fresh ID.
func (r *Runtime) SetConnectionState(ctx context.Context, id, state string) error {
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return ErrClosed
	}
	if state != "ready" && state != "needs_login" && state != "revoked" {
		return errors.Join(ErrInvalidInput, errors.New("beesuh: invalid connection state"))
	}
	unlock, err := r.lockSession(ctx, "connection\x00"+id)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := r.ownedConnection(ctx, id)
	if err != nil {
		return err
	}
	if c.State == "revoked" {
		if state == "revoked" {
			return nil
		}
		return errors.Join(ErrConnectionUnavailable, errors.New("beesuh: revoked connection cannot be reactivated"))
	}
	if c.State == state {
		return nil
	}
	// Readiness requires the service adapter to confirm an authenticated account;
	// ownership alone cannot reactivate an unresolved credential reference.
	if state == "ready" {
		if _, err := r.resolveCredential(ctx, c); err != nil {
			return err
		}
	}
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `UPDATE connections SET state=?,generation=generation+1 WHERE id=? AND user_id=? AND workspace_id=?`, Args: []any{state, c.ID, c.UserID, c.WorkspaceID}, ExpectedRowsAffected: one()}})
}

// checkConnection is used by execution paths while they already hold life.
func (r *Runtime) checkConnection(ctx context.Context, id, kind, target string) error {
	c, err := r.lookupConnection(ctx, id)
	if err != nil {
		return err
	}
	if c.Kind != kind || c.TargetID != target {
		return fmt.Errorf("%w: connection does not authorize this target", ErrForbidden)
	}
	return nil
}

func (r *Runtime) lookupConnection(ctx context.Context, id string) (Connection, error) {
	c, err := r.ownedConnection(ctx, id)
	if err != nil {
		return Connection{}, err
	}
	if c.CredentialRef == "" || c.State != "ready" {
		return Connection{}, errors.Join(ErrConnectionUnavailable, fmt.Errorf("beesuh: connection state is %s", c.State))
	}
	return c, nil
}

func (r *Runtime) ownedConnection(ctx context.Context, id string) (Connection, error) {
	if !validConnectionText(id) {
		return Connection{}, errors.Join(ErrInvalidInput, errors.New("beesuh: invalid connection ID"))
	}
	p, err := r.requirePrincipal(ctx)
	if err != nil {
		return Connection{}, err
	}
	rows, err := r.query(ctx, `SELECT id,user_id,workspace_id,kind,target_id,credential_ref,state,generation FROM connections WHERE id=? AND user_id=? AND workspace_id=?`, id, p.UserID, p.WorkspaceID)
	if err != nil {
		return Connection{}, err
	}
	if len(rows) == 0 {
		return Connection{}, ErrNotFound
	}
	return connectionFromFullRow(rows[0]), nil
}

func connectionFromFullRow(row []any) Connection {
	return Connection{ID: row[0].(string), UserID: row[1].(string), WorkspaceID: row[2].(string), Kind: row[3].(string), TargetID: row[4].(string), CredentialRef: row[5].(string), State: row[6].(string), Generation: row[7].(int64)}
}

func connectionFromRow(id string, row []any) Connection {
	return Connection{ID: id, UserID: row[0].(string), WorkspaceID: row[1].(string), Kind: row[2].(string), TargetID: row[3].(string), CredentialRef: row[4].(string), State: row[5].(string), Generation: row[6].(int64)}
}
