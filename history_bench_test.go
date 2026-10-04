package beesuh

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mrchypark/rhiza"
)

const (
	historyWorkspace = "history-benchmark-workspace"
	historySession   = "history-benchmark-session"
)

// TestHistoryScalingFixture keeps the public History result at 200 entries as
// unrelated retained records grow. It verifies the fixture used by the benchmark.
func TestHistoryScalingFixture(t *testing.T) {
	for _, other := range []int{1_000, 10_000, 100_000} {
		t.Run(fmt.Sprintf("other=%d", other), func(t *testing.T) {
			r := historyBenchmarkRuntime(t)
			seedHistoryJournal(t, r, other)
			entries, err := r.History(context.Background(), historyWorkspace, historySession)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 200 {
				t.Fatalf("History returned %d entries, want 200", len(entries))
			}
		})
	}
}

func BenchmarkHistoryScaling(b *testing.B) {
	for _, other := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("HistoryLatest200/OtherWorkspaceRecords=%d", other), func(b *testing.B) {
			r := historyBenchmarkRuntime(b)
			seedHistoryJournal(b, r, other)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				entries, err := r.History(ctx, historyWorkspace, historySession)
				if err != nil {
					b.Fatal(err)
				}
				if len(entries) != 200 {
					b.Fatalf("History returned %d entries, want 200", len(entries))
				}
			}
		})

		b.Run(fmt.Sprintf("NativeRecentSuccessfulTurns/OtherWorkspaceRuns=%d", other), func(b *testing.B) {
			r := historyBenchmarkRuntime(b)
			seedNativeHistory(b, r, other)
			req := Request{WorkspaceID: historyWorkspace, SessionKey: historySession}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				history, err := r.nativeHistory(ctx, req)
				if err != nil {
					b.Fatal(err)
				}
				if len(history) != 8 {
					b.Fatalf("nativeHistory returned %d turns, want 8", len(history))
				}
			}
		})
	}
}

func historyBenchmarkRuntime(tb testing.TB) *Runtime {
	tb.Helper()
	db, err := rhiza.Open(context.Background(), rhiza.Config{
		NodeID:   "history-benchmark",
		DataDir:  tb.TempDir(),
		PeerAddr: "127.0.0.1:0",
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background(), []rhiza.Migration{{
		Version: 1,
		Name:    "sessions-and-runs",
		Statements: []rhiza.SQLStatement{
			{SQL: `CREATE TABLE sessions (workspace TEXT NOT NULL, session_key TEXT NOT NULL, backend TEXT NOT NULL, directory TEXT NOT NULL, profile TEXT NOT NULL, session_id TEXT NOT NULL, active_request TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,session_key))`},
			{SQL: `CREATE TABLE runs (workspace TEXT NOT NULL, request_id TEXT NOT NULL, session_key TEXT NOT NULL, input_hash TEXT NOT NULL, state TEXT NOT NULL, result TEXT NOT NULL DEFAULT '', PRIMARY KEY(workspace,request_id))`},
		},
	}, nativeMigration}); err != nil {
		tb.Fatal(err)
	}
	return &Runtime{db: db}
}

func seedHistoryJournal(tb testing.TB, r *Runtime, other int) {
	tb.Helper()
	const rowsPerStatement = 512
	rows := make([]string, 0, rowsPerStatement)
	appendRow := func(session, request string) {
		rows = append(rows, fmt.Sprintf("('%s','%s','%s','message','{\"role\":\"user\",\"content\":\"benchmark\"}')", historyWorkspace, session, request))
		if len(rows) == cap(rows) {
			flushHistoryJournalRows(tb, r, rows)
			rows = rows[:0]
		}
	}
	for i := 0; i < 200; i++ {
		appendRow(historySession, fmt.Sprintf("target-%06d", i))
	}
	for i := 0; i < other; i++ {
		appendRow("other-session", fmt.Sprintf("other-%06d", i))
	}
	flushHistoryJournalRows(tb, r, rows)
}

func flushHistoryJournalRows(tb testing.TB, r *Runtime, rows []string) {
	tb.Helper()
	if len(rows) == 0 {
		return
	}
	if err := r.mutate(context.Background(), []rhiza.SQLStatement{{
		SQL: `INSERT INTO journal(workspace,session_key,request_id,kind,payload) VALUES ` + strings.Join(rows, ","),
	}}); err != nil {
		tb.Fatal(err)
	}
}

func seedNativeHistory(tb testing.TB, r *Runtime, other int) {
	tb.Helper()
	statements := make([]rhiza.SQLStatement, 0, 64)
	appendRun := func(session, request string) {
		statements = append(statements, rhiza.SQLStatement{SQL: `INSERT INTO runs(workspace,request_id,session_key,input_hash,state,result) VALUES(?,?,?,?,?,?)`, Args: []any{historyWorkspace, request, session, "benchmark", "succeeded", "{}"}})
		if len(statements) == cap(statements) {
			flushHistoryStatements(tb, r, statements)
			statements = statements[:0]
		}
	}
	for i := 0; i < 8; i++ {
		request := fmt.Sprintf("target-%06d", i)
		appendRun(historySession, request)
		if err := r.mutate(context.Background(), []rhiza.SQLStatement{{SQL: `INSERT INTO journal(workspace,session_key,request_id,kind,payload) VALUES(?,?,?,?,?)`, Args: []any{historyWorkspace, historySession, request, "message", `{"role":"assistant","content":"benchmark"}`}}}); err != nil {
			tb.Fatal(err)
		}
	}
	for i := 0; i < other; i++ {
		appendRun("other-session", fmt.Sprintf("other-%06d", i))
	}
	flushHistoryStatements(tb, r, statements)
}

func flushHistoryStatements(tb testing.TB, r *Runtime, statements []rhiza.SQLStatement) {
	tb.Helper()
	if len(statements) == 0 {
		return
	}
	sql := statements[0].SQL
	valuesAt := strings.Index(sql, "VALUES")
	if valuesAt < 0 {
		tb.Fatalf("fixture statement has no VALUES clause: %q", sql)
	}
	value := strings.TrimSpace(sql[valuesAt+len("VALUES"):])
	values := make([]string, 0, len(statements))
	args := make([]any, 0, len(statements)*len(statements[0].Args))
	for _, statement := range statements {
		if statement.SQL != sql {
			tb.Fatalf("mixed fixture statements: %q and %q", sql, statement.SQL)
		}
		values = append(values, value)
		args = append(args, statement.Args...)
	}
	statement := rhiza.SQLStatement{SQL: sql[:valuesAt+len("VALUES")] + " " + strings.Join(values, ","), Args: args}
	if err := r.mutate(context.Background(), []rhiza.SQLStatement{statement}); err != nil {
		tb.Fatal(err)
	}
}
