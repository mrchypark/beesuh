# Uncertainty, admission, and retention policy

This records the decisions taken by ChatGPT Pro review of the whole project at
`105d47f8a069f9060fa6aa3450e3f0164bde08f1`, plus the deployment obligations and
the highest-risk assumption that must be verified before shipping. It exists so
the reasoning behind the runtime contracts is discoverable rather than
re-derived.

## 1. Uncertainty scope is the workspace

An unresolved external effect blocks new external effects **in the affected
workspace**, backed by a durable record of the unresolved operation.

The pending row is committed **before** the external POST leaves the process
(`recordUncertainIntent`) and deleted only once the dispatch is acknowledged
(`clearUncertainIntent`). This closes the crash window that the earlier
post-dispatch-only design left open. Committing intent before dispatch means a
crash can conservatively fence an operation that never actually reached
Conductor, which is the safe direction. A failed acknowledgement delete leaves
the row pending rather than silently clearing evidence.

An effect key that already exists refuses the dispatch. A reused key must not be
able to slip past the fence just because its previous row reached a terminal
disposition.

On startup the runtime reconciles pre-upgrade evidence: any `tool_intent`
journal row for a `conductor_emit` with no matching `tool_result` is treated as
unresolved and fenced (`reconcileLegacyIntents`). Creating an empty uncertainty
table must not silently reopen a workspace that already lost an acknowledgement
on an older version.

Inspection, reconciliation, and revocation stay available. Neither a restart,
nor a replacement session or connection, removes the block. Recovery must work
independently of the stuck run: a run holds `life.RLock()` for its whole
execution, so `Close` cannot interrupt it.

Runtime-wide blocking would give one tenant a denial-of-service primitive.
Session- or connection-level blocking lets another route bypass the unresolved
workspace state. Workspace ownership does not prove independence of downstream
effects, so this scope is correct only while workspaces do not require
coordinated recovery over shared mutable downstream resources.

The accepted cost is that independent work in that workspace may pause.

Run recovery has a separate in-memory fence. `Recover` holds the exclusive
lifecycle lock and reads both active session requests and running run records
before rebuilding that fence. This releases a stale fence when `finish`
committed but its acknowledgement was lost. Each completed workspace is checked
against both sources again before its fence is cleared. A running record without
a recoverable session stays fenced and makes recovery return `ErrUncertain`.
Query failures never count as proof that work has finished. None of these steps
resolves a pending external-effect record.

## 2. Revocation is two-phase: local denial first, external confirmation later

Under the connection lock, record revocation intent durably, advance the
connection generation, and deny further use. Release the lock **before** the
external call. A failure or restart leaves the connection denied and revocation
pending; the same intent is retried. The state distinguishes *local use
disabled* from *external revocation confirmed*.

Intent, generation increment, and the local `revoked` flip commit as one
statement batch. A mutex alone provides no crash atomicity; Rhiza applies a
multi-statement batch inside a single transaction.

After that authorized transition commits, confirmation checks the exact stored
connection identity and revocation generation without reauthorizing the user.
Disabling the user must not prevent completion of the admitted cleanup. New
public requests still require current authorization. The host revoker must be
idempotent for the supplied connection generation and must not redirect its
credential reference to a newer credential while an old callback can run.

Trusted provisioning with `State: "revoked"` declares an already-terminal
connection. Registration stores its confirmed revocation row atomically, as the
legacy backfill does, so repeated revocation does not invoke the host again.

The host callback runs under the earlier of the caller's deadline and
`Options.RevocationTimeout` (30s default). Go cannot forcibly terminate
arbitrary callback code, so the callback must be cancellation-aware. A timeout
keeps the row `pending`. Concurrent revocations of one connection are
serialized so callbacks cannot overlap.

**No startup reconciliation.** A restart does *not* complete a pending remote
revocation; it is retried only on a later `SetConnectionState(..., "revoked")`
request. Until that succeeds the connection stays locally unusable, which is
the safe direction.

Connection waiters must not consume global execution slots, and queued work
revalidates the connection generation at the protected transition into dispatch.

Two-phase revocation and asynchronous completion are compatible: the first
establishes safety, the second determines scheduling. Effects already dispatched
may still complete.

## 3. Token-budget and usage honesty

Token limits are reported as `enforced`, `requested`, or `unavailable`,
separately from whether usage is available. The Codex CLI and app-server
adapters treat a requested maximum as a hint, not an enforced ceiling, and do
not return real usage; that is reported as `unknown`, not as zero.

HTTP adapters also report unknown usage when required counters are missing,
null, or negative. Explicit zero counters remain valid measured usage.
Anthropic cache counters are optional and are included when present; totals
that overflow the local integer range are reported as unknown.

A request that requires an enforced limit is rejected before execution. An
advisory budget is allowed only when the caller explicitly permits it. If the
existing `max_tokens` contract promises a ceiling, that promise can only be kept
by rejecting until the caller opts into advisory behavior.

Inclusive token totals are not monetary cost totals. Cached input counts toward
prompt tokens, so cached workloads report larger numbers than before.

## 4. Retention: preserve unresolved evidence (pruning NOT implemented)

ChatGPT Pro reviewed this section and rejected the earlier claim: *"a
declaration implements no retention policy." The previous draft asserted a
30-day terminal TTL and idle-session expiry. **Neither exists in code.** They
are recorded here as intended future work, not as shipped behaviour. The
`DefaultTerminalRetention` constant was removed for exactly this reason.

Implemented today:

- Uncertain effects, and the evidence needed to reconcile them, have **no TTL
  until authoritative resolution**; their payload retention period starts at
  resolution.
- Operators get direct lookup by run, request, or effect ID, and a paginated
  unresolved-effects listing, independent of the latest-200 history view.
- `ResolveUncertainEffect` is the only path that lifts a fence, and it is not
  reachable from any model-driven tool call.
- Reconciliation cannot run while a dispatch is on the wire. `conductor_emit`
  holds a shared fence lock from before the POST until the intent row is
  cleared or recorded, and resolution takes that lock exclusively. Without it
  an operator could mark an effect abandoned while its POST was still in
  flight; a second request would then pass the reservation and the first POST
  would still reach Conductor, applying the operation twice. Displacement
  between different effect keys stays concurrent; only the same fence row is
  serialized. This lock is in-process, so it carries the same one-Runtime-per-
  data-directory requirement as the session lifecycle lock.

Not implemented, and therefore **not** claimed:

- Terminal run and history payload pruning.
- Idle-session expiry.
- Replay tombstone compaction. Note Pro's warning: `abandoned` does **not**
  establish nonexecution, so it must retain its unknown outcome and suppress
  replay rather than being treated as a clean success.

### 4.1 Orphaned OpenCode sessions: intents, adoption, and cleanup

`opencode.Client.DeleteSession` calls the provider's `DELETE /session/{id}` and
treats a `false` body as a failed reclaim rather than a silent success. When a
remote session is created but its local mapping insert fails, `Runtime` reclaims
that session and never sends a prompt.

Reclaiming on a failed insert alone is unsafe. Storage can report failure for a
write that already committed, so a delete issued on that evidence can destroy a
mapped, prompted conversation. The reclaim therefore runs only after one
linearizable read proves that no mapping references the session, under this
session key **or** any other key in the workspace. An unreadable read is not
proof of absence, so it keeps the session and logs the condition.

A create whose response was lost leaves a durable intent
(`pending_session_creates`, migration v10) whose `phase` records which remote
operation is still unresolved:

- `creating` — a create may have produced a session that was never mapped. The
  intent carries an unguessable ownership token; the next request looks the
  session up by title and **adopts** it rather than creating a duplicate.
- `cleanup` — a delete was issued for `remote_id` and its outcome is unknown.
  The next request retries the delete by that known id. A session in this phase
  is never returned to the adoption pool, because a late delete must not
  destroy a mapped conversation.

The recovery lookup runs on a detached, bounded context, because the request
that failed is usually already cancelled. An empty lookup after a resumed
`creating` intent returns `ErrUncertain` and issues **no** replacement POST: an
empty list does not prove the earlier POST finished, so a second create could
duplicate the very conversation the intent exists to recover. Several sessions
sharing one token are ambiguous; recovery fails closed and deletes nothing,
because a title match does not identify which candidate the intent owns.

An intent that survives a committed mapping is stale and is cleared on the next
request, so recovery can never adopt the session that mapping already owns.

**Concurrency boundary.** `Runtime.Run` holds the session lifecycle lock for
the whole of create, adoption, mapping, and cleanup, so those phases are
mutually exclusive for one workspace and session key within a runtime. The lock
is in-process: a deployment must run **one** `Runtime` per data directory. A
second process against the same directory could delete a session another is
prompting. `TestSessionLifecycleLockSerialisesConcurrentRequestsForOneKey`
pins this behaviour.

### 4.2 Fail-closed states operators must expect

These states are **not** auto-resolved. Each blocks the affected session key and
returns `ErrUncertain`; there is no automatic reaper and no model-reachable
resolution path.

- A `creating` intent whose lookup stays empty. The earlier create may still be
  in flight, so no replacement is authorised.
- A `cleanup` intent whose session id is unknown. The delete cannot be retried,
  and clearing the intent would return that session to the adoption pool.
- An ambiguous `creating` intent (several title matches).

Operator responsibilities: inspect `pending_session_creates` together with the
provider-side session listing, confirm whether the remote session is mapped or
prompted, and settle the intent deliberately. Beesuh deliberately ships no
automatic reaper: an automatic one could delete a session another writer mapped.

### 4.3 Deployment obligation: an exclusively managed session namespace

Adoption identifies a session by an unguessable title token. That is a naming
argument, not an authorization boundary: anyone who can observe the title can
copy it onto another session. A deployment must therefore dedicate the
provider's OpenCode session namespace to Beesuh. There is no runtime switch to
turn adoption off: it is part of the `creating`-intent recovery path, and it
can only be avoided by never running the OpenCode backend against a namespace
that a direct OpenCode user shares. Where a direct OpenCode user shares the
same provider account and directory, that deployment must not use this
candidate.

A blanket TTL eventually deletes either the evidence needed for recovery or the
identity that prevents re-execution. The accepted cost is that unresolved
evidence can grow indefinitely.

## 5. `/v1/compos` is a trusted-producer contract

Permission to originate Compos executions is explicit, through a producer
credential or permission. Accepted identifiers are bound to the authenticated
producer and workspace, and authority is never derived from `subject_ref`.
Under this contract those fields are producer assertions.

If a downstream decision requires proof of an actual Conductor execution, that
proof is a signed Conductor attestation, or a lookup binding execution ID,
workspace, subject, and relevant payload. An existence-only lookup adds latency
and an availability dependency without proving the necessary bindings.

"Any authenticated caller" is not automatically a trusted producer, and
documentation cannot turn a caller assertion into verified Conductor evidence.
The accepted cost is that an authorized producer can assert false provenance
within its permitted scope.

## 6. Operator authentication stays bearer-based; cookie auth implies host CSRF

Explicit `Authorization` bearer authentication remains the default. A host that
introduces cookie authentication must enforce CSRF protection **before** calling
mutating runtime operations. That requirement is an embedding-host obligation and
is stated as such.

Rejecting requests that carry a `Cookie` header does not work: it cannot see
authentication performed upstream, cannot stop a proxy from translating cookie
auth into a bearer-authenticated request, and rejects unrelated cookies.

## 7. Measure history scaling before optimizing anything else

Run a small benchmark against the real Rhiza history query: hold the returned
latest 200 records constant while other retained workspace records grow to
1,000, 10,000, and 100,000. Measure latency, allocations, and records examined if
cheaply observable, with provider execution excluded.

If query work grows with unrelated retained data, fix the access path. If it
stays flat, defer that hypothesis.

**Measured** (see `docs/history-scaling-benchmark.md`): the public latest-200
`History` query is flat (0.31-0.94 ms, flat allocations) at 1k / 10k / 100k
unrelated records. `nativeHistory` was **not** flat: it grew from 0.47-0.76 ms
to 23.51-24.27 ms because the `runs` primary key is only
`(workspace,request_id)` while the query filters on
`workspace,session_key,state`. Migration v9 adds
`runs_workspace_session_state` to back it.

This comes first because the other counts (seven authorization calls per fresh
run, four connection reads, and per-request Codex process startup) describe
bounded per-request overhead, while history may introduce degradation that grows
throughout a deployment's lifetime.

## Deployment obligations outside this repository

- **Conductor scope enforcement.** Beesuh validates owned connections and sends
  trusted scope headers; the gateway must enforce user, workspace, and
  connection scope on both writes and reads.
- **Idempotency durability.** Whether the real Conductor consumes the idempotency
  key, and for how long, must be verified against the deployment.
- **Pinned binaries.** Deployed Codex and OpenCode binary revisions must be
  pinned; local fixtures do not prove that boundary.
- **GoAuthy token source binding.** The host must bind the token query to
  `(serviceUserID, connectionID, generation)`, not to whichever profile the UI
  currently has selected.

## Highest-risk assumption to verify before shipping

That an uncertain Conductor effect can be reconciled or safely retried without
repeating the downstream action. Force an effect to commit, lose its
acknowledgement, restart Beesuh, then attempt concurrent recovery through a fresh
session and a fresh connection. The downstream action must occur exactly once,
and an unprovable outcome must stay durably uncertain with retry blocked.

Deduplicating request acceptance alone does not establish this guarantee.
