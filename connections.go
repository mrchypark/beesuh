package beesuh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mrchypark/rhiza"
)

// Connection is a service-owned reference to a configured provider or
// Conductor connector. It deliberately contains no credential material.
type Connection struct {
	OrganizationID string `json:"organization_id,omitempty"`
	ID             string `json:"id"`
	UserID         string `json:"user_id"`
	WorkspaceID    string `json:"workspace_id"`
	Kind           string `json:"kind"`
	TargetID       string `json:"-"`
	CredentialRef  string `json:"-"`
	State          string `json:"state"`
	Generation     int64  `json:"generation"`
	Revocation     string `json:"revocation,omitempty"`
}

var connectionMigration = rhiza.Migration{Version: 3, Name: "connections", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE connections (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, workspace_id TEXT NOT NULL, kind TEXT NOT NULL, target_id TEXT NOT NULL, state TEXT NOT NULL)`},
	{SQL: `CREATE INDEX connections_owner ON connections(user_id,workspace_id,id)`},
}}

// Empty organization preserves ownership of existing personal connections.
var organizationMigration = rhiza.Migration{Version: 14, Name: "connection-organizations", Statements: []rhiza.SQLStatement{
	{SQL: `ALTER TABLE connections ADD COLUMN organization_id TEXT NOT NULL DEFAULT ''`},
	{SQL: `CREATE INDEX connections_organization_owner ON connections(organization_id,user_id,workspace_id,id)`},
}}

var connectionCredentialMigration = rhiza.Migration{Version: 5, Name: "connection-credentials", Statements: []rhiza.SQLStatement{
	{SQL: `ALTER TABLE connections ADD COLUMN credential_ref TEXT NOT NULL DEFAULT ''`},
	{SQL: `ALTER TABLE connections ADD COLUMN generation INTEGER NOT NULL DEFAULT 1`},
}}

var connectionRevocationMigration = rhiza.Migration{Version: 8, Name: "connection-revocations", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE connection_revocations (connection_id TEXT PRIMARY KEY, generation INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','confirmed','unavailable')))`},
}}

// connectionRevocationBackfillMigration closes the upgrade gap from versions
// that persisted revoked connections before revocation confirmations existed.
// Those connections are already locally terminal, so retries must not call a
// host revoker again.
var connectionRevocationBackfillMigration = rhiza.Migration{Version: 11, Name: "connection-revocation-backfill", Statements: []rhiza.SQLStatement{
	{SQL: `INSERT INTO connection_revocations(connection_id,generation,state)
SELECT id,generation,'confirmed' FROM connections WHERE state='revoked'
ON CONFLICT(connection_id) DO NOTHING`},
}}

var ErrRevocationPending = errors.New("beesuh: external connection revocation is pending")

const defaultRevocationTimeout = 30 * time.Second

func validConnectionText(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n")
}

func validateConnection(c Connection) error {
	if !validOrganizationID(c.OrganizationID) {
		return errors.New("beesuh: organization ID must be valid UTF-8, nonblank, at most 256 bytes, without NUL, CR or LF")
	}
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
	if err := rejectResolverReentry(ctx); err != nil {
		return err
	}
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

	rows, err := r.query(ctx, `SELECT user_id,workspace_id,kind,target_id,credential_ref,state,generation,organization_id FROM connections WHERE id=?`, c.ID)
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		existing := connectionFromRow(c.ID, rows[0])
		if existing.OrganizationID == c.OrganizationID && existing.UserID == c.UserID && existing.WorkspaceID == c.WorkspaceID && existing.Kind == c.Kind && existing.TargetID == c.TargetID && existing.CredentialRef == c.CredentialRef {
			return nil
		}
		return errors.Join(ErrConflict, errors.New("beesuh: connection ID already belongs to another target"))
	}
	if err := r.connectionCapacityAvailable(ctx, c.OrganizationID, c.WorkspaceID); err != nil {
		return err
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
	statements := []rhiza.SQLStatement{{SQL: `INSERT INTO connections(id,user_id,workspace_id,kind,target_id,credential_ref,state,generation,organization_id)
SELECT ?,?,?,?,?,?,?,?,?
WHERE (SELECT COUNT(*) FROM connections WHERE organization_id=? AND workspace_id=?) < ?
  AND (SELECT COUNT(*) FROM connections) < ?`, Args: []any{c.ID, c.UserID, c.WorkspaceID, c.Kind, c.TargetID, c.CredentialRef, c.State, c.Generation, c.OrganizationID, c.OrganizationID, c.WorkspaceID, r.capacity.ConnectionsPerWorkspace, r.capacity.ConnectionsGlobal}, ExpectedRowsAffected: one()}}
	if c.State == "revoked" {
		// Trusted provisioning declares an already-terminal connection, just as
		// the legacy backfill does. Persist confirmation in the same transaction.
		statements = append(statements, rhiza.SQLStatement{SQL: `INSERT INTO connection_revocations(connection_id,generation,state) VALUES(?,?,'confirmed')`, Args: []any{c.ID, c.Generation}})
	}
	err = r.mutate(ctx, statements)
	if capacityRejected(err) {
		if capacityErr := r.connectionCapacityAvailable(ctx, c.OrganizationID, c.WorkspaceID); capacityErr != nil {
			return capacityErr
		}
	}
	return err
}

func (r *Runtime) connectionCapacityAvailable(ctx context.Context, organization, workspace string) error {
	rows, err := r.query(ctx, `SELECT COUNT(*) FROM connections WHERE organization_id=? AND workspace_id=?`, organization, workspace)
	if err != nil {
		return err
	}
	if len(rows) == 1 && rows[0][0].(int64) >= int64(r.capacity.ConnectionsPerWorkspace) {
		return &CapacityError{Resource: "connections per workspace", Limit: r.capacity.ConnectionsPerWorkspace}
	}
	rows, err = r.query(ctx, `SELECT COUNT(*) FROM connections`)
	if err != nil {
		return err
	}
	if len(rows) == 1 && rows[0][0].(int64) >= int64(r.capacity.ConnectionsGlobal) {
		return &CapacityError{Resource: "connections global", Limit: r.capacity.ConnectionsGlobal}
	}
	return nil
}

// Connections returns only connections owned by the authenticated principal.
// TargetID is intentionally omitted from JSON so callers cannot treat this as
// a credential or routing configuration endpoint.
func (r *Runtime) Connections(ctx context.Context) ([]Connection, error) {
	if err := rejectResolverReentry(ctx); err != nil {
		return nil, err
	}
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	p, err := r.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.query(ctx, `SELECT c.id,c.user_id,c.workspace_id,c.kind,c.target_id,c.credential_ref,c.state,c.generation,COALESCE(v.state,''),c.organization_id FROM connections c LEFT JOIN connection_revocations v ON v.connection_id=c.id WHERE c.organization_id=? AND c.user_id=? AND c.workspace_id=? ORDER BY c.id`, p.OrganizationID, p.UserID, p.WorkspaceID)
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
	if err := rejectResolverReentry(ctx); err != nil {
		return Connection{}, err
	}
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
	if err := rejectResolverReentry(ctx); err != nil {
		return err
	}
	r.life.RLock()
	if r.closed {
		r.life.RUnlock()
		return ErrClosed
	}
	if state != "ready" && state != "needs_login" && state != "revoked" {
		r.life.RUnlock()
		return errors.Join(ErrInvalidInput, errors.New("beesuh: invalid connection state"))
	}
	unlock, err := r.lockSession(ctx, "connection\x00"+id)
	if err != nil {
		r.life.RUnlock()
		return err
	}
	c, err := r.ownedConnection(ctx, id)
	if err != nil {
		unlock()
		r.life.RUnlock()
		return err
	}
	if c.State == "revoked" {
		unlock()
		r.life.RUnlock()
		if state == "revoked" {
			return r.revokeCredential(ctx, c)
		}
		return errors.Join(ErrConnectionUnavailable, errors.New("beesuh: revoked connection cannot be reactivated"))
	}
	if c.State == state {
		unlock()
		r.life.RUnlock()
		return nil
	}
	// Readiness requires the service adapter to confirm an authenticated account;
	// ownership alone cannot reactivate an unresolved credential reference.
	if state == "ready" {
		if _, err := r.resolveCredential(ctx, c); err != nil {
			unlock()
			r.life.RUnlock()
			return err
		}
	}
	if state != "revoked" {
		err = r.mutate(ctx, []rhiza.SQLStatement{{SQL: `UPDATE connections SET state=?,generation=generation+1 WHERE id=? AND organization_id=? AND user_id=? AND workspace_id=?`, Args: []any{state, c.ID, c.OrganizationID, c.UserID, c.WorkspaceID}, ExpectedRowsAffected: one()}})
		unlock()
		r.life.RUnlock()
		return err
	}
	err = r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `INSERT INTO connection_revocations(connection_id,generation,state) VALUES(?,?,'pending')`, Args: []any{c.ID, c.Generation + 1}},
		{SQL: `UPDATE connections SET state='revoked',generation=generation+1 WHERE id=? AND organization_id=? AND user_id=? AND workspace_id=?`, Args: []any{c.ID, c.OrganizationID, c.UserID, c.WorkspaceID}, ExpectedRowsAffected: one()},
	})
	unlock()
	r.life.RUnlock()
	if err != nil {
		return err
	}
	c.State, c.Generation, c.Revocation = "revoked", c.Generation+1, "pending"
	return r.revokeCredential(ctx, c)
}

// revokeCredential runs after local denial has committed and the connection
// lock has been released. Hosts must make this callback idempotent per ID and
// generation because an interrupted confirmation is retried.
func (r *Runtime) revokeCredential(ctx context.Context, c Connection) error {
	if c.Revocation == "confirmed" || c.Revocation == "unavailable" {
		return nil
	}
	// This guard serializes retries for one connection. It is deliberately
	// separate from the connection lock, which is released before the callback.
	unlock, err := r.lockSession(ctx, "connection-revocation\x00"+c.ID)
	if err != nil {
		return ErrRevocationPending
	}
	defer unlock()
	r.life.RLock()
	if r.closed {
		r.life.RUnlock()
		return ErrRevocationPending
	}
	current, err := r.revocationSnapshot(ctx, c)
	r.life.RUnlock()
	if err != nil {
		return ErrRevocationPending
	}
	if current.Revocation == "confirmed" || current.Revocation == "unavailable" {
		return nil
	}
	c = current
	if r.opts.RevokeCredential == nil {
		if err := r.setRevocationState(ctx, c, "unavailable"); err != nil {
			return ErrRevocationPending
		}
		return nil
	}
	timeout := r.opts.RevocationTimeout
	if timeout == 0 {
		timeout = defaultRevocationTimeout
	}
	if timeout < 0 {
		return ErrRevocationPending
	}
	revokeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := r.opts.RevokeCredential(revokeCtx, c); err != nil {
		return ErrRevocationPending
	}
	// A callback can ignore its context and return nil after its deadline. In
	// that case the runtime cannot prove whether the remote revocation took
	// effect before the deadline, so retain the durable pending state.
	if err := revokeCtx.Err(); err != nil {
		return ErrRevocationPending
	}
	if err := r.setRevocationState(ctx, c, "confirmed"); err != nil {
		return ErrRevocationPending
	}
	return nil
}

// revocationSnapshot verifies the exact intent already authorized and committed
// by SetConnectionState. Completing that intent must not require fresh user
// authorization: the user may have been disabled after local denial committed.
func (r *Runtime) revocationSnapshot(ctx context.Context, want Connection) (Connection, error) {
	rows, err := r.query(ctx, `SELECT c.id,c.user_id,c.workspace_id,c.kind,c.target_id,c.credential_ref,c.state,c.generation,v.state,c.organization_id
FROM connections c JOIN connection_revocations v ON v.connection_id=c.id
WHERE c.id=? AND c.organization_id=? AND c.user_id=? AND c.workspace_id=? AND c.kind=? AND c.target_id=? AND c.credential_ref=? AND c.state='revoked' AND c.generation=? AND v.generation=c.generation`,
		want.ID, want.OrganizationID, want.UserID, want.WorkspaceID, want.Kind, want.TargetID, want.CredentialRef, want.Generation)
	if err != nil {
		return Connection{}, err
	}
	if len(rows) != 1 {
		return Connection{}, ErrRevocationPending
	}
	return connectionFromFullRow(rows[0]), nil
}

func (r *Runtime) setRevocationState(ctx context.Context, c Connection, state string) error {
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return ErrClosed
	}
	unlock, err := r.lockSession(ctx, "connection\x00"+c.ID)
	if err != nil {
		return err
	}
	defer unlock()
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `UPDATE connection_revocations SET state=? WHERE connection_id=? AND generation=? AND state='pending'`, Args: []any{state, c.ID, c.Generation}, ExpectedRowsAffected: one()}})
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
	rows, err := r.query(ctx, `SELECT c.id,c.user_id,c.workspace_id,c.kind,c.target_id,c.credential_ref,c.state,c.generation,COALESCE(v.state,''),c.organization_id FROM connections c LEFT JOIN connection_revocations v ON v.connection_id=c.id WHERE c.id=? AND c.organization_id=? AND c.user_id=? AND c.workspace_id=?`, id, p.OrganizationID, p.UserID, p.WorkspaceID)
	if err != nil {
		return Connection{}, err
	}
	if len(rows) == 0 {
		return Connection{}, ErrNotFound
	}
	return connectionFromFullRow(rows[0]), nil
}

func connectionFromFullRow(row []any) Connection {
	return Connection{ID: row[0].(string), UserID: row[1].(string), WorkspaceID: row[2].(string), Kind: row[3].(string), TargetID: row[4].(string), CredentialRef: row[5].(string), State: row[6].(string), Generation: row[7].(int64), Revocation: row[8].(string), OrganizationID: row[9].(string)}
}

func connectionFromRow(id string, row []any) Connection {
	return Connection{ID: id, UserID: row[0].(string), WorkspaceID: row[1].(string), Kind: row[2].(string), TargetID: row[3].(string), CredentialRef: row[4].(string), State: row[5].(string), Generation: row[6].(int64), OrganizationID: row[7].(string)}
}
