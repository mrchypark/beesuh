package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/rhiza"
	"github.com/ncruces/go-sqlite3"
)

// Exercise the public Query boundary and HTTP error mapper, not a chat/model call.
func TestRhizaQueryCancellationKeepsHTTPCategory(t *testing.T) {
	db, err := rhiza.Open(t.Context(), rhiza.Config{NodeID: "beesuh", DataDir: filepath.Join(t.TempDir(), "rhiza"), PeerAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	query := func(ctx context.Context, sql string) error {
		_, err := db.Query(ctx, rhiza.QueryRequest{SQL: sql, Consistency: rhiza.ConsistencyLinearizable})
		return err
	}
	healthy := func(t *testing.T) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		rows, err := db.Query(ctx, rhiza.QueryRequest{SQL: "SELECT 1", Consistency: rhiza.ConsistencyLinearizable})
		if err != nil || len(rows.Rows) != 1 {
			t.Fatalf("healthy Query rows=%v error=%v", rows.Rows, err)
		}
	}
	healthy(t)
	invalid := query(t.Context(), "SELECT FROM")
	if invalid == nil || errors.Is(invalid, context.DeadlineExceeded) || errors.Is(invalid, context.Canceled) || errors.Is(invalid, sqlite3.INTERRUPT) {
		t.Fatalf("invalid SQL misclassified: %v", invalid)
	}
	healthy(t)
	const interruptAfter = 250 * time.Millisecond
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.DeadlineExceeded
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), interruptAfter)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				stop := time.AfterFunc(interruptAfter, cancel)
				defer stop.Stop()
				want = context.Canceled
			}
			defer cancel()
			err := query(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000000) SELECT sum(x) FROM n`)
			if !errors.Is(ctx.Err(), want) {
				t.Fatalf("Query did not reach requested cancellation: ctx=%v error=%v want=%v", ctx.Err(), err, want)
			}
			if !errors.Is(err, want) {
				if !errors.Is(err, rhiza.ErrInvalidRequest) {
					t.Fatalf("Query returned unexpected SQL boundary error: ctx=%v error=%v want=%v", ctx.Err(), err, want)
				}
				t.Fatalf("Query lost context cause: ctx=%v error=%v want=%v", ctx.Err(), err, want)
			}
			if !errors.Is(err, sqlite3.INTERRUPT) {
				t.Fatalf("Query did not preserve a driver INTERRUPT: %v", err)
			}
			out := httptest.NewRecorder()
			handler{}.fail(out, err, beesuh.Result{State: "failed"})
			var body struct {
				Code string `json:"error_code"`
			}
			if json.Unmarshal(out.Body.Bytes(), &body) != nil || out.Code != http.StatusGatewayTimeout || body.Code != "deadline_exceeded" {
				t.Fatalf("Query HTTP classification: %d %s", out.Code, out.Body.String())
			}
			healthy(t)
		})
	}
}
