package beesuh

import (
	"context"

	"github.com/mrchypark/rhiza"
)

// retentionMigration adds the access path for direct request lookup. Pending
// uncertain effects are deliberately excluded from any expiry operation.
var retentionMigration = rhiza.Migration{Version: 7, Name: "retention-inspection", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE INDEX uncertain_effects_request ON uncertain_effects(workspace,request_id,effect_key)`},
}}

// runsHistoryIndexMigration backs nativeHistory, which filters runs by
// workspace, session and terminal state. The runs primary key is only
// (workspace,request_id), so without this index that lookup scanned every
// retained run: the benchmark in docs/history-scaling-benchmark.md measured
// 0.5ms at 1k unrelated runs growing to 24ms at 100k.
var runsHistoryIndexMigration = rhiza.Migration{Version: 9, Name: "runs-history-index", Statements: []rhiza.SQLStatement{
	{SQL: `CREATE INDEX runs_workspace_session_state ON runs(workspace,session_key,state)`},
}}

// UncertainEffect is the durable, non-secret record used for reconciliation.
type UncertainEffect struct {
	EffectKey   string `json:"effect_key"`
	SessionKey  string `json:"session_key"`
	RequestID   string `json:"request_id"`
	Event       string `json:"event"`
	Disposition string `json:"disposition"`
	RecordedAt  int64  `json:"recorded_at"`
}

// UncertainEffectPage is a keyset-paginated unresolved-effect listing.
type UncertainEffectPage struct {
	Effects    []UncertainEffect `json:"effects"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// UncertainEffect returns one effect by its durable effect key. GetRun remains
// the direct lookup for a Beesuh run/request ID.
func (r *Runtime) UncertainEffect(ctx context.Context, workspace, effectKey string) (UncertainEffect, error) {
	if err := rejectResolverReentry(ctx); err != nil {
		return UncertainEffect{}, err
	}
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return UncertainEffect{}, ErrClosed
	}
	if err := validIdentity(workspace, effectKey); err != nil {
		return UncertainEffect{}, err
	}
	workspace, err := r.scopedWorkspace(ctx, workspace)
	if err != nil {
		return UncertainEffect{}, err
	}
	effects, err := r.queryUncertainEffects(ctx, workspace, `AND effect_key=?`, effectKey)
	if err != nil {
		return UncertainEffect{}, err
	}
	if len(effects) == 0 {
		return UncertainEffect{}, ErrNotFound
	}
	return effects[0], nil
}

// UncertainEffectsForRequest returns all effects for a Beesuh request ID.
func (r *Runtime) UncertainEffectsForRequest(ctx context.Context, workspace, requestID string) ([]UncertainEffect, error) {
	if err := rejectResolverReentry(ctx); err != nil {
		return nil, err
	}
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	if err := validIdentity(workspace, requestID); err != nil {
		return nil, err
	}
	workspace, err := r.scopedWorkspace(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return r.queryUncertainEffects(ctx, workspace, `AND request_id=? ORDER BY effect_key`, requestID)
}

// UnresolvedEffects lists pending effects independently from session history.
// The cursor is the last returned effect key. Limit is capped at 200.
func (r *Runtime) UnresolvedEffects(ctx context.Context, workspace, cursor string, limit int) (UncertainEffectPage, error) {
	if err := rejectResolverReentry(ctx); err != nil {
		return UncertainEffectPage{}, err
	}
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return UncertainEffectPage{}, ErrClosed
	}
	if err := validIdentity(workspace); err != nil || (cursor != "" && validIdentity(cursor) != nil) || limit < 0 || limit > 200 {
		return UncertainEffectPage{}, ErrInvalidInput
	}
	workspace, err := r.scopedWorkspace(ctx, workspace)
	if err != nil {
		return UncertainEffectPage{}, err
	}
	if limit == 0 {
		limit = 50
	}
	effects, err := r.queryUncertainEffects(ctx, workspace, `AND disposition=? AND effect_key>? ORDER BY effect_key LIMIT ?`, dispositionPending, cursor, limit+1)
	if err != nil {
		return UncertainEffectPage{}, err
	}
	page := UncertainEffectPage{Effects: effects}
	if len(page.Effects) > limit {
		page.NextCursor = page.Effects[limit-1].EffectKey
		page.Effects = page.Effects[:limit]
	}
	return page, nil
}

// ResolveUncertainEffect records an operator-authoritative disposition for one
// unresolved effect and lifts its durable-effect fence. It is deliberately separate
// from Recover: the operator must first inspect the external effect and decide
// whether it completed (resolved) or will never complete (abandoned).
//
// Nothing else calls the underlying resolver, so a fence cannot be lifted by a
// model-driven tool call.
func (r *Runtime) ResolveUncertainEffect(ctx context.Context, workspace, effectKey, disposition string) error {
	if err := rejectResolverReentry(ctx); err != nil {
		return err
	}
	r.life.RLock()
	defer r.life.RUnlock()
	if r.closed {
		return ErrClosed
	}
	if err := validIdentity(workspace, effectKey); err != nil {
		return err
	}
	scoped, err := r.scopedWorkspace(ctx, workspace)
	if err != nil {
		return err
	}
	if err := r.resolveUncertainEffect(ctx, scoped, effectKey, disposition); err != nil {
		return err
	}
	// The effect fence is the durable row itself, so recording the disposition is
	// what lifts it. A separate run/recovery fence for this workspace stays
	// closed until Recover discharges it.
	return nil
}

// queryUncertainEffects is the sole dependency on the uncertainty table.
func (r *Runtime) queryUncertainEffects(ctx context.Context, workspace, suffix string, args ...any) ([]UncertainEffect, error) {
	rows, err := r.query(ctx, `SELECT effect_key,session_key,request_id,event,disposition,recorded_at FROM uncertain_effects WHERE workspace=? `+suffix, append([]any{workspace}, args...)...)
	if err != nil {
		return nil, err
	}
	effects := make([]UncertainEffect, 0, len(rows))
	for _, row := range rows {
		recordedAt, ok := row[5].(int64)
		if !ok {
			return nil, ErrUncertain
		}
		effects = append(effects, UncertainEffect{EffectKey: row[0].(string), SessionKey: row[1].(string), RequestID: row[2].(string), Event: row[3].(string), Disposition: row[4].(string), RecordedAt: recordedAt})
	}
	return effects, nil
}
