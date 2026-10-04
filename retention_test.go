package beesuh

import (
	"context"
	"testing"

	"github.com/mrchypark/rhiza"
)

func TestUncertainEffectInspection(t *testing.T) {
	f := newFakeOpenCode(t)
	r := openTestRuntime(t, f, t.TempDir(), 1)
	t.Cleanup(func() { _ = r.Close() })
	ctx := context.Background()
	if err := r.mutate(ctx, []rhiza.SQLStatement{
		{SQL: `INSERT INTO uncertain_effects(workspace,effect_key,session_key,request_id,event,disposition,recorded_at) VALUES(?,?,?,?,?,?,?)`, Args: []any{"workspace", "effect-a", "session", "request-a", "event", dispositionPending, int64(1)}},
		{SQL: `INSERT INTO uncertain_effects(workspace,effect_key,session_key,request_id,event,disposition,recorded_at) VALUES(?,?,?,?,?,?,?)`, Args: []any{"workspace", "effect-b", "session", "request-b", "event", dispositionPending, int64(2)}},
		{SQL: `INSERT INTO uncertain_effects(workspace,effect_key,session_key,request_id,event,disposition,recorded_at) VALUES(?,?,?,?,?,?,?)`, Args: []any{"workspace", "effect-c", "session", "request-a", "event", dispositionResolved, int64(3)}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		effectKey string
		requestID string
		wantErr   error
	}{
		{effectKey: "effect-a", requestID: "request-a"},
		{effectKey: "missing", wantErr: ErrNotFound},
	} {
		effect, err := r.UncertainEffect(ctx, "workspace", tc.effectKey)
		if err != tc.wantErr || (err == nil && (effect.RequestID != tc.requestID || effect.Disposition != dispositionPending)) {
			t.Fatalf("effect %q = %#v, %v", tc.effectKey, effect, err)
		}
	}
	byRequest, err := r.UncertainEffectsForRequest(ctx, "workspace", "request-a")
	if err != nil || len(byRequest) != 2 || byRequest[1].EffectKey != "effect-c" {
		t.Fatalf("request effects = %#v, %v", byRequest, err)
	}
	page, err := r.UnresolvedEffects(ctx, "workspace", "", 1)
	if err != nil || len(page.Effects) != 1 || page.Effects[0].EffectKey != "effect-a" || page.NextCursor != "effect-a" {
		t.Fatalf("first page = %#v, %v", page, err)
	}
	page, err = r.UnresolvedEffects(ctx, "workspace", page.NextCursor, 1)
	if err != nil || len(page.Effects) != 1 || page.Effects[0].EffectKey != "effect-b" || page.NextCursor != "" {
		t.Fatalf("second page = %#v, %v", page, err)
	}
}
