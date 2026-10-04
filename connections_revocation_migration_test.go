package beesuh

import (
	"context"
	"testing"

	"github.com/mrchypark/rhiza"
)

func TestConnectionRevocationBackfillMigration(t *testing.T) {
	ctx := context.Background()
	db, err := rhiza.Open(ctx, rhiza.Config{NodeID: "connection-revocation-backfill", DataDir: t.TempDir()})
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
	}
	if err := db.Migrate(ctx, beforeRevocations); err != nil {
		t.Fatalf("migrate through version 7: %v", err)
	}
	insert, err := db.Execute(ctx, rhiza.ExecuteRequest{RequestID: "legacy-revoked-connection", SQL: `INSERT INTO connections(id,user_id,workspace_id,kind,target_id,credential_ref,state,generation) VALUES(?,?,?,?,?,?,?,?)`, Args: []any{"legacy", "user", "workspace", "provider", "work", "credential", "revoked", int64(7)}})
	if err != nil || insert.Status != rhiza.MutationCommitted {
		t.Fatalf("insert legacy revoked connection = %+v, %v", insert, err)
	}

	migrations := append(beforeRevocations, connectionRevocationMigration, runsHistoryIndexMigration, pendingCreateMigration, connectionRevocationBackfillMigration)
	if err := db.Migrate(ctx, migrations); err != nil {
		t.Fatalf("migrate through backfill: %v", err)
	}
	assertRevocationBackfill(t, ctx, db)

	// The migration ledger makes a repeated upgrade a no-op. Execute the data
	// statement again as well, so its conflict guard is independently covered.
	if err := db.Migrate(ctx, migrations); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	retry, err := db.Execute(ctx, rhiza.ExecuteRequest{RequestID: "legacy-revoked-connection-backfill-retry", SQL: connectionRevocationBackfillMigration.Statements[0].SQL})
	if err != nil || retry.Status != rhiza.MutationCommitted {
		t.Fatalf("repeat backfill = %+v, %v", retry, err)
	}
	assertRevocationBackfill(t, ctx, db)
}

func assertRevocationBackfill(t *testing.T, ctx context.Context, db *rhiza.DB) {
	t.Helper()
	result, err := db.Query(ctx, rhiza.QueryRequest{SQL: `SELECT generation,state FROM connection_revocations WHERE connection_id=?`, Args: []any{"legacy"}, Consistency: rhiza.ConsistencyLinearizable})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 2 || result.Rows[0][0] != int64(7) || result.Rows[0][1] != "confirmed" {
		t.Fatalf("backfilled revocation rows = %#v, want generation 7 and confirmed", result.Rows)
	}
}
