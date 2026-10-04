package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mrchypark/rhiza"
)

// workspaceUncertaintyMigration records external effects whose outcome is not
// known. The row is the authority: a restart, a new session, or a different
// connection cannot make an unresolved effect safe again.
var workspaceUncertaintyMigration = rhiza.Migration{Version: 6, Name: "workspace-uncertainty", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE TABLE uncertain_effects (workspace TEXT NOT NULL, effect_key TEXT NOT NULL, session_key TEXT NOT NULL, request_id TEXT NOT NULL, event TEXT NOT NULL, disposition TEXT NOT NULL, recorded_at INTEGER NOT NULL, PRIMARY KEY(workspace,effect_key))`},
	{SQL: `CREATE INDEX uncertain_effects_pending ON uncertain_effects(workspace,disposition)`},
}}

// Uncertain-effect dispositions. A pending row has no retention horizon until
// an authoritative resolution is recorded.
const (
	dispositionPending   = "pending"
	dispositionResolved  = "resolved"
	dispositionAbandoned = "abandoned"
)

// recordUncertainEffect durably notes an external effect whose acknowledgement
// was lost. Returning the storage error keeps ErrUncertain in the join, so a
// failed write can never be mistaken for a resolved effect.
func (r *Runtime) recordUncertainEffect(ctx context.Context, req Request, effectKey, event string, cause error) error {
	// The record is written after the dispatch attempt on a background context:
	// the caller may already have cancelled, and losing this row would silently
	// re-admit a duplicate effect.
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := r.mutate(write, []rhiza.SQLStatement{{SQL: `INSERT INTO uncertain_effects(workspace,effect_key,session_key,request_id,event,disposition,recorded_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(workspace,effect_key) DO NOTHING`, Args: []any{req.WorkspaceID, effectKey, req.SessionKey, req.RequestID, event, dispositionPending, time.Now().Unix()}}})
	if err != nil {
		return errors.Join(errors.New("beesuh: failed to record unresolved effect"), err)
	}
	return cause
}

// recordUncertainIntent writes the pending row BEFORE the external dispatch.
// Writing it only after a failed dispatch leaves a crash window: a process
// that dies between the POST and the INSERT would leave no durable evidence
// that the effect may have been accepted. The row is removed again once the
// dispatch is acknowledged, so an acknowledged effect does not fence the
// workspace.
func (r *Runtime) recordUncertainIntent(ctx context.Context, req Request, effectKey, event string) error {
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// Rhiza serializes this write. The pending-row predicate and insert must be
	// one statement: a separate SELECT can admit two callers before either one
	// records its intent. ExpectedRowsAffected makes both an existing effect key
	// and a pending effect elsewhere in this workspace a failed reservation.
	err := r.mutate(write, []rhiza.SQLStatement{{SQL: `INSERT INTO uncertain_effects(workspace,effect_key,session_key,request_id,event,disposition,recorded_at)
SELECT ?,?,?,?,?,?,?
WHERE NOT EXISTS (
	SELECT 1 FROM uncertain_effects WHERE workspace=? AND disposition=?
)
ON CONFLICT(workspace,effect_key) DO NOTHING`, Args: []any{req.WorkspaceID, effectKey, req.SessionKey, req.RequestID, event, dispositionPending, time.Now().Unix(), req.WorkspaceID, dispositionPending}, ExpectedRowsAffected: one()}})
	if err != nil {
		// Runtime.mutate currently returns a plain error for Rhiza's
		// precondition_failed result. Classify that local, expected reservation
		// refusal as a conflict without treating other storage failures as safe.
		if errors.Is(err, ErrConflict) || strings.Contains(err.Error(), "storage mutation rejected (precondition_failed)") {
			return errors.Join(ErrUncertain, ErrConflict, errors.New("beesuh: external-effect reservation is already recorded or workspace is fenced"))
		}
		return errors.Join(errors.New("beesuh: failed to record dispatch intent"), err)
	}
	return nil
}

// clearUncertainIntent removes a pending intent after the dispatch was
// acknowledged. A failure here keeps the row pending and the workspace fenced,
// because the runtime cannot prove the acknowledgement was recorded.
func (r *Runtime) clearUncertainIntent(ctx context.Context, workspace, effectKey string) error {
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return r.mutate(write, []rhiza.SQLStatement{{SQL: `DELETE FROM uncertain_effects WHERE workspace=? AND effect_key=? AND disposition=?`, Args: []any{workspace, effectKey, dispositionPending}}})
}

// assertWorkspaceRecoverable refuses new external effects in a workspace that
// still holds an unresolved effect. Inspection, reconciliation and revocation
// stay available, and recovery does not depend on the stuck run itself.
//
// The test is workspace-scoped, matching the documented policy: an unresolved
// effect is evidence about the workspace that issued it, not about every
// workspace the process serves, so a fenced workspace B must stay usable while
// workspace A awaits reconciliation. It gates effects only. New runs in a
// fenced workspace are refused separately, by the in-process fence, because a
// run that is merely admitted may still be in flight and is not durable
// evidence of anything.
func (r *Runtime) assertWorkspaceRecoverable(ctx context.Context, workspace string) error {
	rows, err := r.query(ctx, `SELECT effect_key FROM uncertain_effects WHERE workspace=? AND disposition=? LIMIT 1`, workspace, dispositionPending)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return errors.Join(ErrUncertain, errors.New("beesuh: workspace has an unresolved external effect"))
	}
	return nil
}

// runFenceHolds reports whether a workspace carries a recovery obligation. Only
// a map can answer this cheaply: a running row is also written for a healthy
// in-flight run, so durable state alone cannot separate the two.
func (r *Runtime) runFenceHolds(workspace string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, fenced := r.runFenced[workspace]
	return fenced
}

// fenceWorkspace records a run/recovery fence. Only Recover may lift it.
func (r *Runtime) fenceWorkspace(workspace string) {
	r.mu.Lock()
	if r.runFenced == nil {
		r.runFenced = make(map[string]struct{})
	}
	r.runFenced[workspace] = struct{}{}
	r.mu.Unlock()
}

// unfenceRunWorkspace lifts one run/recovery fence after Recover persists every
// interrupted run for that workspace.
func (r *Runtime) unfenceRunWorkspace(workspace string) {
	r.mu.Lock()
	delete(r.runFenced, workspace)
	r.mu.Unlock()
}

// hasPendingEffect reports whether any durable unresolved effect exists across
// every workspace. A pending row is the authority for the effect half of the
// fence: it survives this process, is visible to whichever runtime reads the
// shared database, and cannot be lost by a crash. An in-process mirror would
// instead let a pending row written by another runtime go unnoticed. Existence
// is the only question Status asks, so the query stops at the first hit instead
// of scanning every pending row.
func (r *Runtime) hasPendingEffect(ctx context.Context) (bool, error) {
	rows, err := r.query(ctx, `SELECT 1 FROM uncertain_effects WHERE disposition=? LIMIT 1`, dispositionPending)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// resolveUncertainEffect records an authoritative disposition so the workspace
// fence lifts. Only an operator-driven reconciliation may call it.
func (r *Runtime) resolveUncertainEffect(ctx context.Context, workspace, effectKey, disposition string) error {
	if disposition != dispositionResolved && disposition != dispositionAbandoned {
		return errors.Join(ErrInvalidInput, errors.New("beesuh: disposition must be resolved or abandoned"))
	}
	// Wait for any in-flight dispatch to finish before changing a disposition.
	// The write lock excludes every conductor_emit dispatch, so a fence row can
	// never leave pending while its POST is still on the wire. Without this a
	// second request could pass the reservation and apply a duplicate effect
	// after the operator had already declared this one resolved or abandoned.
	r.fenceMu.Lock()
	defer r.fenceMu.Unlock()
	return r.mutate(ctx, []rhiza.SQLStatement{{SQL: `UPDATE uncertain_effects SET disposition=? WHERE workspace=? AND effect_key=? AND disposition=?`, Args: []any{disposition, workspace, effectKey, dispositionPending}, ExpectedRowsAffected: one()}})
}

// reconcileLegacyIntents upgrades pre-uncertainty rows. Before this table
// existed, an unacknowledged Conductor effect was recorded only as a
// tool_intent journal row. Creating an empty uncertainty table would therefore
// reopen a workspace that had already lost an acknowledgement, so every
// tool_intent without a matching tool_result is treated as unresolved and
// fenced until an operator inspects it.
//
// This is deliberately conservative: it can fence an operation that never
// reached Conductor, which is safe, and never clears one that might have.
func (r *Runtime) reconcileLegacyIntents(ctx context.Context) error {
	rows, err := r.query(ctx, `SELECT seq,workspace,session_key,request_id,payload FROM journal WHERE kind='tool_intent'`)
	if err != nil {
		return err
	}
	type intent struct {
		workspace string
		session   string
		requestID string
		effectKey string
		event     string
		callID    string
	}
	pending := make([]intent, 0, len(rows))
	for _, row := range rows {
		var payload struct {
			EffectKey string `json:"effect_key"`
			Call      struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"call"`
		}
		if jsonErr := json.Unmarshal([]byte(row[4].(string)), &payload); jsonErr != nil {
			// An unreadable intent is unresolved evidence; fence the workspace
			// rather than assuming the effect completed.
			pending = append(pending, intent{workspace: row[1].(string), session: row[2].(string), requestID: row[3].(string), effectKey: "legacy_journal_" + fmt.Sprint(row[0]), event: "unknown", callID: ""})
			continue
		}
		// Only Conductor emits are external effects. Local tools cannot lose an
		// acknowledgement to a third party.
		if payload.Call.Function.Name != "conductor_emit" || payload.EffectKey == "" {
			continue
		}
		pending = append(pending, intent{workspace: row[1].(string), session: row[2].(string), requestID: row[3].(string), effectKey: payload.EffectKey, event: "legacy", callID: payload.Call.ID})
	}
	if len(pending) == 0 {
		return nil
	}
	statements := make([]rhiza.SQLStatement, 0, len(pending))
	for _, item := range pending {
		// Match a tool_result recorded for the same call, not merely for the
		// same request. A different tool completing in that request says nothing
		// about whether this Conductor emit was acknowledged.
		qerr := error(nil)
		var results [][]any
		if item.callID != "" {
			results, qerr = r.query(ctx, `SELECT 1 FROM journal WHERE workspace=? AND session_key=? AND request_id=? AND kind='tool_result' AND json_extract(payload,'$.call_id')=? LIMIT 1`, item.workspace, item.session, item.requestID, item.callID)
		} else {
			// The intent could not be parsed, so the call identity is unknown.
			// Correlating by request alone would risk clearing it on an
			// unrelated result, so only an exact request match is accepted.
			results, qerr = r.query(ctx, `SELECT 1 FROM journal WHERE workspace=? AND session_key=? AND request_id=? AND kind='tool_result' LIMIT 1`, item.workspace, item.session, item.requestID)
		}
		if qerr != nil {
			return qerr
		}
		if len(results) > 0 {
			continue
		}
		statements = append(statements, rhiza.SQLStatement{SQL: `INSERT INTO uncertain_effects(workspace,effect_key,session_key,request_id,event,disposition,recorded_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(workspace,effect_key) DO NOTHING`, Args: []any{item.workspace, item.effectKey, item.session, item.requestID, item.event, dispositionPending, time.Now().Unix()}})
	}
	if len(statements) == 0 {
		return nil
	}
	if err := r.mutate(ctx, statements); err != nil {
		return errors.Join(errors.New("beesuh: failed to reconcile pre-upgrade effects"), err)
	}
	return nil
}
