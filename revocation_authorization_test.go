package beesuh

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/mrchypark/rhiza"
)

// Once an authorized caller commits local revocation, the remote revoke must
// still run if the user's membership changes before confirmation. Otherwise a
// deactivated user leaves a durable pending row that no longer has an
// authorized retry path.
func TestRevocationCompletesAfterPostCommitAuthorizationChange(t *testing.T) {
	var authorizations, callbacks atomic.Int32
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error {
		if authorizations.Add(1) == 1 {
			return nil // authorize the state transition and durable local denial
		}
		return ErrForbidden // simulate deactivation before remote confirmation
	})
	r.opts.RevokeCredential = func(_ context.Context, connection Connection) error {
		if connection.CredentialRef != "credential" || connection.Generation != 2 {
			t.Errorf("callback connection = %#v, want committed credential ref and generation 2", connection)
		}
		callbacks.Add(1)
		return nil
	}
	if err := r.RegisterConnection(context.Background(), Connection{
		ID: "connection", UserID: "user", WorkspaceID: "workspace",
		Kind: "provider", TargetID: "work", CredentialRef: "credential", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	ctx := principal(context.Background(), "user", "workspace")
	if err := r.SetConnectionState(ctx, "connection", "revoked"); err != nil {
		t.Fatalf("revoke after committed local denial = %v, want confirmation", err)
	}
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("host revoker calls = %d, want 1", got)
	}
	rows, err := r.query(context.Background(), `SELECT c.state,c.generation,v.state FROM connections c JOIN connection_revocations v ON v.connection_id=c.id WHERE c.id=?`, "connection")
	if err != nil || len(rows) != 1 {
		t.Fatalf("durable revocation = %#v, %v", rows, err)
	}
	if state, generation, revocation := rows[0][0], rows[0][1], rows[0][2]; state != "revoked" || generation != int64(2) || revocation != "confirmed" {
		t.Fatalf("durable revocation = state=%#v generation=%#v status=%#v", state, generation, revocation)
	}
	if err := r.SetConnectionState(ctx, "connection", "revoked"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("fresh denied retry = %v, want ErrForbidden", err)
	}
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("host revoker calls after denied retry = %d, want 1", got)
	}
}

func TestRevocationDoesNotConfirmAnUnownedConnection(t *testing.T) {
	var callbacks atomic.Int32
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	r.opts.RevokeCredential = func(context.Context, Connection) error {
		callbacks.Add(1)
		return nil
	}
	if err := r.RegisterConnection(context.Background(), Connection{
		ID: "connection", UserID: "owner", WorkspaceID: "workspace",
		Kind: "provider", TargetID: "work", CredentialRef: "credential", State: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	err := r.SetConnectionState(principal(context.Background(), "other-user", "workspace"), "connection", "revoked")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign revoke = %v, want ErrNotFound", err)
	}
	if got := callbacks.Load(); got != 0 {
		t.Errorf("host revoker calls = %d, want 0", got)
	}
}

func TestRegisterRevokedConnectionConfirmsLocalTerminalState(t *testing.T) {
	var callbacks atomic.Int32
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	r.opts.RevokeCredential = func(context.Context, Connection) error {
		callbacks.Add(1)
		return nil
	}
	connection := Connection{
		ID: "revoked", UserID: "user", WorkspaceID: "workspace",
		Kind: "provider", TargetID: "work", CredentialRef: "credential", State: "revoked",
	}
	if err := r.RegisterConnection(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	// Registration is idempotent and must not recreate or downgrade its terminal
	// revocation confirmation.
	if err := r.RegisterConnection(context.Background(), connection); err != nil {
		t.Fatalf("duplicate revoked registration = %v", err)
	}
	if err := r.SetConnectionState(principal(context.Background(), "user", "workspace"), "revoked", "revoked"); err != nil {
		t.Errorf("repeated revoked state = %v, want confirmed terminal state", err)
	}
	if got := callbacks.Load(); got != 0 {
		t.Errorf("host revoker calls = %d, want 0", got)
	}
	rows, err := r.query(context.Background(), `SELECT c.generation,v.generation,v.state FROM connections c JOIN connection_revocations v ON v.connection_id=c.id WHERE c.id=?`, "revoked")
	if err != nil || len(rows) != 1 {
		t.Errorf("registered terminal revocation = %#v, %v", rows, err)
		return
	}
	if connectionGeneration, revocationGeneration, revocation := rows[0][0], rows[0][1], rows[0][2]; connectionGeneration != int64(1) || revocationGeneration != int64(1) || revocation != "confirmed" {
		t.Errorf("registered terminal revocation = connection generation=%#v revocation generation=%#v state=%#v", connectionGeneration, revocationGeneration, revocation)
	}
}

func TestRegisterNonRevokedConnectionHasNoRevocationRow(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	for _, state := range []string{"ready", "needs_login"} {
		connection := Connection{
			ID: state, UserID: "user", WorkspaceID: "workspace",
			Kind: "provider", TargetID: "work", CredentialRef: "credential-" + state, State: state,
		}
		if err := r.RegisterConnection(context.Background(), connection); err != nil {
			t.Fatalf("register %s = %v", state, err)
		}
	}
	rows, err := r.query(context.Background(), `SELECT connection_id FROM connection_revocations ORDER BY connection_id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("non-revoked registrations created revocation rows: %#v", rows)
	}
}

func TestRevocationSnapshotRejectsMismatchedCommittedIntent(t *testing.T) {
	base := Connection{
		ID: "connection", UserID: "user", WorkspaceID: "workspace", Kind: "provider",
		TargetID: "work", CredentialRef: "credential", State: "revoked", Generation: 7,
	}
	for _, test := range []struct {
		name          string
		want          Connection
		rowState      string
		withRow       bool
		wantErr       error
		wantCalls     int32
		storedState   string
		rowGeneration int64
	}{
		{name: "wrong user", want: func() Connection { c := base; c.UserID = "other"; return c }(), rowState: "pending", withRow: true, wantErr: ErrRevocationPending},
		{name: "wrong workspace", want: func() Connection { c := base; c.WorkspaceID = "other"; return c }(), rowState: "pending", withRow: true, wantErr: ErrRevocationPending},
		{name: "wrong kind", want: func() Connection { c := base; c.Kind = "conductor"; return c }(), rowState: "pending", withRow: true, wantErr: ErrRevocationPending},
		{name: "wrong target", want: func() Connection { c := base; c.TargetID = "other"; return c }(), rowState: "pending", withRow: true, wantErr: ErrRevocationPending},
		{name: "wrong credential ref", want: func() Connection { c := base; c.CredentialRef = "other"; return c }(), rowState: "pending", withRow: true, wantErr: ErrRevocationPending},
		{name: "wrong generation", want: func() Connection { c := base; c.Generation--; return c }(), rowState: "pending", withRow: true, wantErr: ErrRevocationPending},
		{name: "stored connection not revoked", want: base, rowState: "pending", withRow: true, storedState: "needs_login", wantErr: ErrRevocationPending},
		{name: "wrong durable row generation", want: base, rowState: "pending", withRow: true, rowGeneration: 6, wantErr: ErrRevocationPending},
		{name: "missing revocation row", want: base, withRow: false, wantErr: ErrRevocationPending},
		{name: "confirmed", want: base, rowState: "confirmed", withRow: true, wantCalls: 0},
		{name: "unavailable", want: base, rowState: "unavailable", withRow: true, wantCalls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var callbacks atomic.Int32
			r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
			r.opts.RevokeCredential = func(context.Context, Connection) error {
				callbacks.Add(1)
				return nil
			}
			storedState := test.storedState
			if storedState == "" {
				storedState = base.State
			}
			rowGeneration := test.rowGeneration
			if rowGeneration == 0 {
				rowGeneration = base.Generation
			}
			statements := []rhiza.SQLStatement{{SQL: `INSERT INTO connections(id,user_id,workspace_id,kind,target_id,credential_ref,state,generation) VALUES(?,?,?,?,?,?,?,?)`, Args: []any{base.ID, base.UserID, base.WorkspaceID, base.Kind, base.TargetID, base.CredentialRef, storedState, base.Generation}}}
			if test.withRow {
				statements = append(statements, rhiza.SQLStatement{SQL: `INSERT INTO connection_revocations(connection_id,generation,state) VALUES(?,?,?)`, Args: []any{base.ID, rowGeneration, test.rowState}})
			}
			if err := r.mutate(t.Context(), statements); err != nil {
				t.Fatal(err)
			}
			err := r.revokeCredential(t.Context(), test.want)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("revokeCredential() = %v, want %v", err, test.wantErr)
			}
			if got := callbacks.Load(); got != test.wantCalls {
				t.Fatalf("host revoker calls = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

func TestRegisterRevokedConnectionIsAtomicWithConfirmation(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	if err := r.mutate(t.Context(), []rhiza.SQLStatement{{SQL: `CREATE TRIGGER reject_revocation_confirmation BEFORE INSERT ON connection_revocations BEGIN SELECT RAISE(ABORT, 'confirmation fault'); END`}}); err != nil {
		t.Fatal(err)
	}
	err := r.RegisterConnection(context.Background(), Connection{
		ID: "revoked", UserID: "user", WorkspaceID: "workspace",
		Kind: "provider", TargetID: "work", CredentialRef: "credential", State: "revoked",
	})
	if err == nil {
		t.Fatal("revoked registration succeeded despite confirmation failure")
	}
	rows, queryErr := r.query(t.Context(), `SELECT id FROM connections WHERE id=?`, "revoked")
	if queryErr != nil || len(rows) != 0 {
		t.Fatalf("failed atomic registration left connection rows=%#v err=%v", rows, queryErr)
	}
}
