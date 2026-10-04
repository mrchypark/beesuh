package beesuh

import (
	"context"
	"errors"
)

// recoverSessionCreates settles durable session-create intents without admitting
// new work. Its caller holds life exclusively. It never creates, prompts, or
// dispatches tools: only a uniquely titled, already-existing remote session may
// become a durable mapping.
func (r *Runtime) recoverSessionCreates(ctx context.Context) error {
	if r.native != nil {
		return nil
	}
	rows, err := r.query(ctx, `SELECT workspace,session_key,ownership_token,backend,directory,profile,phase,remote_id FROM pending_session_creates ORDER BY workspace,session_key`)
	if err != nil {
		return err
	}
	var recovered error
	for _, row := range rows {
		// Stop between durable intents when the operator's recovery deadline has
		// elapsed. The individual provider cleanup calls below deliberately get
		// one detached, bounded budget so a cancellation can settle its own
		// remote outcome; this check prevents a large intent set from starting
		// further calls after that budget is exhausted.
		if err := ctx.Err(); err != nil {
			return errors.Join(recovered, err)
		}
		req := Request{WorkspaceID: row[0].(string), SessionKey: row[1].(string)}
		if row[3] != r.opts.BackendID || row[4] != r.opts.OpenCode.Directory || !r.compatibleProfile(row[5].(string)) {
			recovered = errors.Join(recovered, ErrConflict)
			continue
		}
		switch row[6].(string) {
		case pendingPhaseCreating:
			if err := r.recoverCreatingSession(ctx, req, row[2].(string)); err != nil {
				recovered = errors.Join(recovered, err)
			}
		case pendingPhaseCleanup:
			if err := r.recoverCleanupSession(ctx, req, row[7].(string)); err != nil {
				recovered = errors.Join(recovered, err)
			}
		default:
			recovered = errors.Join(recovered, ErrUncertain)
		}
	}
	return recovered
}

func (r *Runtime) recoverCreatingSession(ctx context.Context, req Request, token string) error {
	// A mapping for this key means the intent is stale. A mapping for another
	// key that names the candidate must remain evidence: it cannot be adopted.
	mapped, exists, err := r.recoveryMapping(ctx, req)
	if err != nil {
		return ErrUncertain
	}
	if exists {
		if mapped.backend != r.opts.BackendID || mapped.directory != r.opts.OpenCode.Directory || !r.compatibleProfile(mapped.profile) {
			return ErrConflict
		}
		return r.clearPendingCreate(req)
	}
	// adoptOrReclaim uses one detached, reclaimTimeout-bounded provider call so
	// a recovery whose caller was cancelled can still settle that call's result.
	// recoverSessionCreates checks ctx before each subsequent durable intent.
	id, err := r.adoptOrReclaim(ctx, token)
	if err != nil {
		return errors.Join(ErrUncertain, err)
	}
	if id == "" {
		return ErrUncertain
	}
	_, referenced := r.sessionUntracked(ctx, req, id)
	if referenced {
		return ErrUncertain
	}
	// pending=true consumes the slot already held by this durable intent, even
	// if the configured session limit was lowered after the crash.
	if err := r.insertSession(ctx, req, id, true); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	return nil
}

type recoverySessionMapping struct {
	backend, directory, profile string
}

// recoveryMapping reads only the mapping for this intent key. Its compatibility
// is checked before clearing a stale creating intent: an incompatible mapping
// must remain paired with the durable evidence for operator reconciliation.
func (r *Runtime) recoveryMapping(ctx context.Context, req Request) (recoverySessionMapping, bool, error) {
	rows, err := r.query(ctx, `SELECT backend,directory,profile FROM sessions WHERE workspace=? AND session_key=?`, req.WorkspaceID, req.SessionKey)
	if err != nil {
		return recoverySessionMapping{}, false, err
	}
	if len(rows) == 0 {
		return recoverySessionMapping{}, false, nil
	}
	if len(rows) != 1 {
		return recoverySessionMapping{}, false, ErrUncertain
	}
	return recoverySessionMapping{backend: rows[0][0].(string), directory: rows[0][1].(string), profile: rows[0][2].(string)}, true, nil
}

func (r *Runtime) recoverCleanupSession(ctx context.Context, req Request, remoteID string) error {
	if remoteID == "" {
		return ErrUncertain
	}
	// Never retry a delete when a durable mapping references its target. An
	// unreadable mapping query is also treated as referenced by sessionUntracked.
	_, referenced := r.sessionUntracked(ctx, req, remoteID)
	if referenced {
		return ErrUncertain
	}
	_, err := r.reclaimPendingSession(ctx, req, remoteID)
	return err
}
