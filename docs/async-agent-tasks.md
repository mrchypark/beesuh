# Asynchronous agent tasks

Beesuh can accept a request durably without keeping the HTTP connection open.
The existing Runtime.Run path still performs every model and tool call; the
task worker is a queue around that path.

## HTTP contract

POST /v1/chat and /v1/compos support the bare RFC 7240 preference
respond-async. The preference is case-insensitive and may appear in a
comma-separated Prefer list. Unknown preferences, parameters such as
respond-async; wait=1, and values such as respond-async=false are ignored.

~~~http
Prefer: respond-async
~~~

An accepted request returns:

~~~http
HTTP/1.1 202 Accepted
Preference-Applied: respond-async
Vary: Prefer
Location: /v1/runs/REQUEST_ID?workspace_id=PUBLIC_WORKSPACE_ID
Content-Type: application/json

{"request_id":"REQUEST_ID","state":"queued"}
~~~

Poll the relative Location with the same authenticated workspace. Before the
run row exists, GET /v1/runs/... reads the pending task and reports queued,
running, or blocked. Once a run row exists, the run is authoritative. A queued
task can be cancelled through the existing POST /v1/runs/{request_id}/cancel
endpoint; cancellation records a tombstone and prevents dispatch.

The same identity-scoped workspace and request ID are the deduplication key for
synchronous and asynchronous submissions. Identical input returns the existing
task or run. A new or still queued/running duplicate returns 202; a duplicate
whose run is terminal returns 200 with the stored terminal result. Reusing the
key with different normalized input returns 409. Existing task and run lookup
happens before PrepareAsync preflight, so a restart-safe credential exchange is
not required merely to replay an accepted request.

## Durable task records

Rhiza migration 15 adds agent_tasks. A row contains the identity-scoped
workspace, request ID, stored Principal, explicitly selected request fields,
the normalized input hash, state, failure category and enqueue time. States are
queued, running, and blocked.

The request payload is serialized from an explicit field list and limited to
64 KiB together with the principal. Bearer tokens, API keys, CredentialRef
values, resolved credential objects and authentication contexts are not stored.
The stored prompt, session and connection selection are operational data; apply
the same access and retention policy as run results and journals.

## Worker and recovery

StartAgentWorker starts one FIFO worker. It claims at most one task, invokes
PrepareAsync again, and calls the existing Runtime.Run. Queue capacity uses the
configured run limits and counts retained runs, run reservations and agent
tasks. Run keeps its current membership, connection ownership and current
Goauthy credential checks.

The worker deletes the task after any run row becomes authoritative. On startup
and before each claim it deletes tasks with a run row and returns a running task
without a run row to queued. A malformed or tampered payload is marked blocked
without a model call. A cancellation that races a claim leaves the tombstone
visible, so Run exits before dispatch.

When the caller context cancels, the worker clears its registration as it
exits. StartAgentWorker can then be called again with a fresh context to resume
durable queued work. Runtime.Close marks the worker as closing, so that restart
cannot race database shutdown.

Runtime.Close stops and waits for the worker before closing Rhiza and files.
Start and Close share the worker lifecycle lock, preventing a worker from
starting after shutdown and using a closed database.

## Host preparation

An identity-mode host owns restart-safe execution authority. The durable
`agent_tasks` row stores the verified `Principal` and the selected provider and
Conductor connection IDs. It does not store the enqueue request's bearer or a
resolved credential. At execution time the worker follows this path:

1. The worker calls `Options.PrepareAsync` with the stored Principal and obtains
   fresh host authority, including the current human authorization required by
   the host or Goauthy delivery implementation.
2. `Runtime.Run` reconstructs the request and rechecks the Principal, workspace,
   and selected connection ID, kind, target, ownership, generation, and current
   authorization before execution.
3. `completeModel` calls `Options.ResolveCredential` immediately before a model
   request. A Conductor tool calls it again for its own connection immediately
   before the gateway request. Existing Goauthy delivery APIs are invoked only
   from those execution-time resolutions.
4. The model request receives only the selected provider credential. The
   Conductor gateway request receives only its Conductor gateway credential plus
   the Principal-derived user, workspace, and connection scope headers. The two
   credential clients remain distinct; the fixed model-provider client is never
   reused for the gateway.

Raw bearer tokens, API keys, credential clients, and resolver contexts remain
in memory for the call and are never persisted in `agent_tasks`, runs, or the
journal. `PrepareAsync` has this contract:

~~~go
func prepare(ctx context.Context, p beesuh.Principal) (context.Context, error) {
    // Rebuild host-private authority from p and external secret storage.
    return beesuh.WithPrincipal(ctx, p), nil
}
~~~

The callback must reconstruct a fresh execution context from the stored
principal without relying on the original HTTP request. It is called for
enqueue preflight after deduplication and again by the worker. The callback
context is derived from the worker or preflight input, carries the configured
execution timeout, and is canceled when that call finishes. The callback must
preserve input cancellation and must bound any work that cannot be interrupted
directly. Returned context values stay in memory for that call and are never
persisted. The callback and `Authorize` must enforce the host's current access
rules.

Operator mode has no principal and needs no PrepareAsync; the worker uses a
principalless context. If identity mode has no hook, or preflight fails,
Enqueue returns auth.ErrUnavailable before storing a task. HTTP maps this to
503 service_unavailable. A restart that cannot reconstruct the host session
uses the existing recovery behavior: an unfinished task returns to queued and
then becomes blocked when execution-time preparation fails.

## Current limitations

The CLI service configuration has no restart-safe server-side session for
current human authorization. It deliberately leaves PrepareAsync unset, so
asynchronous POSTs fail closed with 503 even when a Conductor is configured;
synchronous requests continue to work. Shared service mode also requires the
original request snapshot for owner authorization and remains unsupported for
asynchronous execution.

This work adds durable task execution, not event subscriptions or a cron
scheduler. There is no SSE event stream or approval inbox. A blocked task is
retained for inspection and can be cancelled to release its queue capacity.
Background execution survives process restart only when the host can recreate
authority through PrepareAsync.
