package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mrchypark/rhiza"
)

func TestOrganizationNamespacesPreservePersonalData(t *testing.T) {
	r := &Runtime{opts: Options{RequireIdentity: true, Authorize: func(context.Context, Principal) error { return nil }}}
	seen := map[string]bool{}
	for _, p := range []Principal{
		{UserID: "user", WorkspaceID: "workspace"},
		{OrganizationID: "org-a", UserID: "user", WorkspaceID: "workspace"},
		{OrganizationID: "org-b", UserID: "user", WorkspaceID: "workspace"},
		{OrganizationID: "org-a", UserID: "other", WorkspaceID: "workspace"},
	} {
		scope, err := r.scopedWorkspace(WithPrincipal(context.Background(), p), p.WorkspaceID)
		if err != nil || seen[scope] {
			t.Fatalf("scope collision or error for %+v: %q %v", p, scope, err)
		}
		seen[scope] = true
		if p.OrganizationID == "" && scope != "user_"+digest([]string{p.UserID, p.WorkspaceID}) {
			t.Fatal("personal namespace changed")
		}
		if got := r.scopedWorkspaceForConnection(Connection{OrganizationID: p.OrganizationID, UserID: p.UserID, WorkspaceID: p.WorkspaceID}); got != scope {
			t.Fatalf("connection scope %q != %q", got, scope)
		}
	}
}

func TestOrganizationIdentityValidation(t *testing.T) {
	calls := 0
	r := &Runtime{opts: Options{RequireIdentity: true, Authorize: func(context.Context, Principal) error { calls++; return nil }}}
	for _, org := range []string{" ", "org\x00", "org\r", "org\n", strings.Repeat("a", 257), "\xff"} {
		ctx := WithPrincipal(context.Background(), Principal{OrganizationID: org, UserID: "u", WorkspaceID: "w"})
		if _, err := r.requirePrincipal(ctx); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("organization %q accepted: %v", org, err)
		}
		if err := validateConnection(Connection{OrganizationID: org, ID: "c", UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}); err == nil {
			t.Fatalf("connection organization %q accepted", org)
		}
	}
	if calls != 0 {
		t.Fatal("invalid identity reached authorizer")
	}
	for _, org := range []string{"", "조직", strings.Repeat("a", 256)} {
		if _, err := r.requirePrincipal(WithPrincipal(context.Background(), Principal{OrganizationID: org, UserID: "u", WorkspaceID: "w"})); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(Principal{UserID: "u", WorkspaceID: "w"})
	if err != nil || strings.Contains(string(b), "organization_id") {
		t.Fatalf("personal JSON changed: %s %v", b, err)
	}
}

func TestOrganizationConnectionsAndStoredReadsAreIsolated(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	ctx := context.Background()
	var resolved, revoked []string
	r.opts.ResolveCredential = func(_ context.Context, c Connection) (Credential, error) {
		resolved = append(resolved, c.OrganizationID)
		return Credential{APIKey: "test", AccountID: "account"}, nil
	}
	r.opts.RevokeCredential = func(_ context.Context, c Connection) error { revoked = append(revoked, c.OrganizationID); return nil }
	for _, org := range []string{"", "org-a", "org-b"} {
		c := Connection{OrganizationID: org, ID: "conn-" + org, UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "ready"}
		if err := r.RegisterConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
		pctx := WithPrincipal(ctx, Principal{OrganizationID: org, UserID: "u", WorkspaceID: "w"})
		scope, err := r.scopedWorkspace(pctx, "w")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.mutate(ctx, []rhiza.SQLStatement{
			{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state,result) VALUES(?,?,?,?,?,?)`, Args: []any{scope, "same", "same", "hash", "completed", `{"state":"completed","output":{"org":"` + org + `"}}`}},
			{SQL: `INSERT INTO journal(workspace,session_key,request_id,kind,payload) VALUES(?,?,?,?,?)`, Args: []any{scope, "same", "same", "test", `{"org":"` + org + `"}`}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(resolved, ",") != ",org-a,org-b" {
		t.Fatalf("resolver lost org: %v", resolved)
	}
	for _, org := range []string{"", "org-a", "org-b"} {
		pctx := WithPrincipal(ctx, Principal{OrganizationID: org, UserID: "u", WorkspaceID: "w"})
		list, err := r.Connections(pctx)
		if err != nil || len(list) != 1 || list[0].OrganizationID != org {
			t.Fatalf("list %q: %+v %v", org, list, err)
		}
		providers, err := r.Providers(pctx)
		if err != nil || len(providers) != 1 || providers[0].ConnectionID != "conn-"+org {
			t.Fatalf("providers leaked organization %q: %+v %v", org, providers, err)
		}
		if _, err := r.Connection(pctx, "conn-"+org); err != nil {
			t.Fatal(err)
		}
		for _, other := range []string{"", "org-a", "org-b"} {
			if other == org {
				continue
			}
			if _, err := r.Connection(pctx, "conn-"+other); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign read: %v", err)
			}
			if err := r.SetConnectionState(pctx, "conn-"+other, "revoked"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign revoke: %v", err)
			}
			before := len(resolved)
			if _, err := r.Run(pctx, Request{WorkspaceID: "w", SessionKey: "forged", RequestID: "forged", ConnectionID: "conn-" + other, Prompt: Prompt{Text: "hello"}}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign execution: %v", err)
			}
			if len(resolved) != before {
				t.Fatal("foreign organization reached credential resolver")
			}
		}
		history, err := r.History(pctx, "w", "same")
		if err != nil || len(history) != 1 || string(history[0].Payload) != `{"org":"`+org+`"}` {
			t.Fatalf("history %q: %+v %v", org, history, err)
		}
		if out, err := r.GetRun(pctx, "w", "same"); err != nil || string(out.Output) != `{"org":"`+org+`"}` {
			t.Fatalf("run %q: %+v %v", org, out, err)
		}
		otherUser := WithPrincipal(ctx, Principal{OrganizationID: org, UserID: "other", WorkspaceID: "w"})
		if list, err := r.Connections(otherUser); err != nil || len(list) != 0 {
			t.Fatalf("cross-user connections: %+v %v", list, err)
		}
		if history, err := r.History(otherUser, "w", "same"); err != nil || len(history) != 0 {
			t.Fatalf("cross-user history: %+v %v", history, err)
		}
		if _, err := r.GetRun(otherUser, "w", "same"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-user run: %v", err)
		}
		changed := list[0]
		changed.OrganizationID = "other-org"
		if err := r.RegisterConnection(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("org retarget: %v", err)
		}
		if err := r.SetConnectionState(pctx, "conn-"+org, "revoked"); err != nil {
			t.Fatal(err)
		}
		committed, err := r.ownedConnection(pctx, "conn-"+org)
		if err != nil {
			t.Fatal(err)
		}
		committed.OrganizationID = "other-org"
		if _, err := r.revocationSnapshot(ctx, committed); !errors.Is(err, ErrRevocationPending) {
			t.Fatalf("foreign revocation confirmation: %v", err)
		}
	}
	if strings.Join(revoked, ",") != ",org-a,org-b" {
		t.Fatalf("revoker lost org: %v", revoked)
	}
}

func TestOrganizationConnectionCapacityIsIndependent(t *testing.T) {
	r := connectionRuntime(t, t.TempDir(), func(context.Context, Principal) error { return nil })
	r.capacity.ConnectionsPerWorkspace = 1
	for _, org := range []string{"", "a", "b"} {
		c := Connection{OrganizationID: org, ID: "c-" + org, UserID: "u", WorkspaceID: "w", Kind: "provider", TargetID: "work", CredentialRef: "ref", State: "needs_login"}
		if err := r.RegisterConnection(context.Background(), c); err != nil {
			t.Fatalf("organization %q inherited another quota: %v", org, err)
		}
		c.ID += "-extra"
		if err := r.RegisterConnection(context.Background(), c); !errors.Is(err, ErrCapacity) {
			t.Fatalf("organization %q bypassed quota: %v", org, err)
		}
	}
}

// Build the actual pre-organization schema, persist personal records, then
// reopen through Open so migration 14 must preserve their original ownership.
func TestOrganizationMigrationPreservesPriorPersonalDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, lock, err := openStorage(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(); _ = lock.Close() })
	legacy := []rhiza.Migration{{Version: 1, Name: "sessions-and-runs", Statements: []rhiza.SQLStatement{
		{SQL: `CREATE TABLE sessions (workspace TEXT NOT NULL, session_key TEXT NOT NULL, backend TEXT NOT NULL, directory TEXT NOT NULL, profile TEXT NOT NULL, session_id TEXT NOT NULL, active_request TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,session_key))`},
		{SQL: `CREATE TABLE runs (workspace TEXT NOT NULL, request_id TEXT NOT NULL, session_key TEXT NOT NULL, input_hash TEXT NOT NULL, state TEXT NOT NULL, result TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,request_id))`},
	}}, nativeMigration, connectionMigration, identityMigration, connectionCredentialMigration, workspaceUncertaintyMigration, retentionMigration, connectionRevocationMigration, runsHistoryIndexMigration, pendingCreateMigration, connectionRevocationBackfillMigration, capacityMigration, cancellationMigration}
	if err := db.Migrate(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	old := &Runtime{db: db}
	columns, err := old.query(ctx, `PRAGMA table_info(connections)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range columns {
		if column[1] == "organization_id" {
			t.Fatal("fixture already has organization column")
		}
	}
	scope := "user_" + digest([]string{"u", "w"})
	if err := old.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `INSERT INTO runtime_identity(id,mode) VALUES(1,'service')`},
		{SQL: `INSERT INTO connections(id,user_id,workspace_id,kind,target_id,state,credential_ref,generation) VALUES('legacy','u','w','provider','work','ready','ref',1)`},
		{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state,result) VALUES(?,'old','old','hash','completed','{"state":"completed","output":{"legacy":true}}')`, Args: []any{scope}},
		{SQL: `INSERT INTO journal(workspace,session_key,request_id,kind,payload) VALUES(?,'old','old','test','{"legacy":true}')`, Args: []any{scope}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(db.Close(), lock.Close()); err != nil {
		t.Fatal(err)
	}
	r := connectionRuntime(t, dir, func(context.Context, Principal) error { return nil })
	rows, err := r.query(ctx, `SELECT organization_id FROM connections WHERE id='legacy'`)
	if err != nil || len(rows) != 1 || rows[0][0] != "" {
		t.Fatalf("migrated organization default: %v %v", rows, err)
	}
	pctx := principal(ctx, "u", "w")
	c, err := r.Connection(pctx, "legacy")
	if err != nil || c.OrganizationID != "" {
		t.Fatalf("legacy connection: %+v %v", c, err)
	}
	if out, err := r.GetRun(pctx, "w", "old"); err != nil || string(out.Output) != `{"legacy":true}` {
		t.Fatalf("legacy run unreachable: %+v %v", out, err)
	}
	if history, err := r.History(pctx, "w", "old"); err != nil || len(history) != 1 || string(history[0].Payload) != `{"legacy":true}` {
		t.Fatalf("legacy history unreachable: %+v %v", history, err)
	}
	orgctx := WithPrincipal(ctx, Principal{OrganizationID: "org", UserID: "u", WorkspaceID: "w"})
	if _, err := r.Connection(orgctx, "legacy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("organization inherited legacy connection: %v", err)
	}
	if _, err := r.GetRun(orgctx, "w", "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("organization inherited legacy run: %v", err)
	}
}
