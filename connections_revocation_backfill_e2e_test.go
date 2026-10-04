package beesuh

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mrchypark/rhiza"
)

// TestRevokedConnectionFromOlderSchemaNeverCallsHostRevoker covers the upgrade
// path end to end. A connection revoked before revocation confirmations existed
// has no row in connection_revocations until the backfill migration seeds one.
// A repeated revocation must find that confirmed row and return without calling
// the host revoker, which is the only evidence that the backfill actually
// prevents the repeated-callback and ErrRevocationPending loop the migration
// exists to close.
func TestRevokedConnectionFromOlderSchemaNeverCallsHostRevoker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedLegacyRevokedConnection(t, ctx, filepath.Join(dir, "rhiza"))

	r := connectionRuntime(t, dir, func(context.Context, Principal) error { return nil })
	var calls atomic.Int32
	r.opts.RevokeCredential = func(context.Context, Connection) error {
		calls.Add(1)
		return errors.New("the host revoker must not run for an already-revoked connection")
	}

	principalCtx := principal(ctx, "u", "w")
	if err := r.SetConnectionState(principalCtx, "legacy", "revoked"); err != nil {
		t.Fatalf("repeated revocation after upgrade = %v, want nil from the backfilled confirmed row", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("host revoker calls = %d, want 0", got)
	}
	connections, err := r.Connections(principalCtx)
	if err != nil || len(connections) != 1 {
		t.Fatalf("Connections() = %#v, %v", connections, err)
	}
	if connections[0].Revocation != "confirmed" {
		t.Fatalf("revocation state = %q, want confirmed", connections[0].Revocation)
	}
	if connections[0].Generation != 7 {
		t.Fatalf("generation = %d, want 7 because repeated revocation preserves a revoked connection generation", connections[0].Generation)
	}
}

// seedLegacyRevokedConnection builds the newest schema that still predates
// connection_revocations, then stores a connection that is already revoked.
// The data dir is the one Open passes to Rhiza, so the runtime under test
// migrates it forward itself.
func seedLegacyRevokedConnection(t *testing.T, ctx context.Context, rhizaDir string) {
	t.Helper()
	db, err := rhiza.Open(ctx, rhiza.Config{NodeID: "beesuh", DataDir: rhizaDir, PeerAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	beforeRevocations := []rhiza.Migration{
		{Version: 1, Name: "sessions-and-runs", Statements: []rhiza.SQLStatement{
			{SQL: `CREATE TABLE sessions (workspace TEXT NOT NULL, session_key TEXT NOT NULL, backend TEXT NOT NULL, directory TEXT NOT NULL, profile TEXT NOT NULL, session_id TEXT NOT NULL, active_request TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,session_key))`},
			{SQL: `CREATE TABLE runs (workspace TEXT NOT NULL, request_id TEXT NOT NULL, session_key TEXT NOT NULL, input_hash TEXT NOT NULL, state TEXT NOT NULL, result TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,request_id))`},
		}},
		nativeMigration,
		connectionMigration,
		identityMigration,
		connectionCredentialMigration,
		workspaceUncertaintyMigration,
		retentionMigration,
		connectionRevocationMigration,
	}
	if err := db.Migrate(ctx, beforeRevocations); err != nil {
		t.Fatalf("migrate through the last schema without revocations: %v", err)
	}
	insert, err := db.Execute(ctx, rhiza.ExecuteRequest{RequestID: "legacy-revoked-connection-e2e", SQL: `INSERT INTO connections(id,user_id,workspace_id,kind,target_id,credential_ref,state,generation) VALUES(?,?,?,?,?,?,?,?)`, Args: []any{"legacy", "u", "w", "provider", "work", "credential", "revoked", int64(7)}})
	if err != nil || insert.Status != rhiza.MutationCommitted {
		t.Fatalf("insert legacy revoked connection = %+v, %v", insert, err)
	}
}
