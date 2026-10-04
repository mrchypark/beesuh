# Local integration boundaries — 2026-09-07

Beesuh remains an independent Go runtime. Direct `/v1/chat` and optional
`/v1/compos` input share the runtime and user-owned connection lookup. Compos is
not required for direct chat. Conductor is an optional tool adapter. Tests use
local HTTP fixtures and disposable Rhiza directories; no sibling application
packages, databases, or deployment are imported. The module pins Rhiza v0.18.0. Optional untracked `go.work` files can select
the user-requested local Rhiza checkout during development.

## Identity and external credentials

`httpapi.NewAuthenticated` accepts a host authenticator; `WithPrincipal` alone
is not authentication. The opt-in [IdP pilot](goauthy-local-pilot.md) verifies a
standard bearer token by RFC 7662 introspection and OIDC UserInfo. Its fixed
membership list is a test fixture, not automatic workspace enrollment. No
production browser login/callback/session server is added by this pilot.

The target integration uses GoAuthy's **optional credential-profile extension**
for external authentication lifecycle and authorized use. This is distinct from
its standard OAuth2/OIDC endpoints. Profiles are generic OAuth2/API-key profiles,
not separate AI/SaaS credential types. Beesuh and Conductor are consumers with
explicit consent; credentials are not implicitly shared between them.

Beesuh's existing `ResolveCredential` callback consumes an authenticated HTTP
client and stable upstream account ID. It is the adapter boundary for the future
published extension API. The adapter must bind verified user, consumer, profile,
logical authorization generation, and permitted provider destination/purpose.
No extension endpoint or unrestricted token-export/proxy protocol is invented
here. Local credential adapters remain available for independent tests; no
existing credentials are moved. The existing managed Codex CLI path uses an explicit isolated `CodexHome`.
The experimental `codex-app-server` consumer also accepts a profile-bound external
ChatGPT token source; see [Codex profiles](codex-profiles.md). This does not add
account connection management to Beesuh or treat an ordinary HTTP API key as a CLI session.

`Connection.Generation` currently changes on authentication-state changes and
revocation, not routine refresh. A request-level logical generation pin and
external-store rebind/CAS contract are not yet implemented. Account identity is
already part of opaque model-session binding. These existing protections must
not be described as complete generation-pinned workflow execution.

## Conductor execution adapter

Every allowed event has an operator-configured handler/version/operation binding.
The runtime creates workspace and execution-request linkage; reserved model data
is rejected before recording an effect intent. Acceptance requires HTTP 202 and
one matching acknowledged execution ID. The response ID is preserved; status
lookup uses `/v1/executions/{id}`. The old `conductor_get_run` tool name and Go
`GetRun` alias refer to this execution, not a workflow run.

The actual Conductor server currently has a service-token gate, not Beesuh user
ACLs. `RequireScope` is only a declaration of a separately implemented scope
checking gateway; setting it cannot enable those checks on Conductor. Direct
local integration is a non-SaaS service fixture. User delegation, profile use,
and side-effect rollback are not established by that fixture.

The legacy OpenCode `conductor_emit` entrypoint rejects every call before
dispatch, including stale tool definitions. There is no environment override.
Use the native tool boundary for emission. The legacy backend can still read
execution status with `conductor_get_run`.

To test an explicitly started isolated worker:

```sh
BEESUH_E2E_CONDUCTOR=1 \
BEESUH_E2E_CONDUCTOR_URL=http://127.0.0.1:YOUR_FIXTURE_PORT \
BEESUH_E2E_CONDUCTOR_TOKEN_FILE=/private/fixture/service-token \
go test . -run '^TestConductorLiveE2E$' -count=1 -v
```

The token file must be private 0600, regular, and not a symlink. The test sends
only `payload.echo@1` in workspace `beesuh-pilot`, checks the native model/tool
boundary, preserves the returned execution ID, replays the same effect and
native request, and waits for the echo result. The Conductor owner separately
verifies its callback sink; Beesuh does not claim callback verification itself.

## Compos adapter gaps

The current `/v1/compos` consumes one agent outbox data object, not the outer
Conductor events envelope, and returns a synchronous Beesuh Result. It accepts
instructions, object input/output schemas, and execution linkage. It deliberately
rejects workflow route/step metadata without a route output contract and retry
policies above one attempt. Actor/subject payloads never create a Principal.

A complete workflow integration still needs a Compos-owned dispatch route,
verified invocation authority and recipient ACL, explicit profile/generation
snapshot, route result schema, terminal callback projection/deduplication, and
cancel/retry/resume agreement. Current request replay and explicit Go `Recover`
are not that protocol. HTTP context cancellation is best effort; an uncertain
external effect blocks automatic replay and does not imply rollback.

## Verified local evidence

On 2026-09-07, `TestConductorLiveE2E` passed (test 0.53s, package 1.629s)
against the Conductor owner's isolated loopback process/worker with a private
service-token file. Beesuh: `main@8606a43a68c3289e33e8207132bd4b7aaa21c962`
plus uncommitted implementation; Conductor owner reported `main@909b200` plus
local changes. Environment: Go 1.27.0, darwin/arm64, disposable local Rhiza.
The execution reached `succeeded` with the expected echo output; HTTP effect
replay and Beesuh request replay preserved one execution identity.

The Conductor owner separately reported one matching terminal callback at its
fixture sink with `attempt_count=1`, `provider_key=payload.echo`, and matching
workspace/result. The owner subsequently verified canonical callback replay returned HTTP 204:
two deliveries produced one unique record in its fixture receiver, then stopped
the owned Conductor and sink processes. This is not evidence of a real Compos
callback consumer.
No real SaaS or user-delegated execution was performed.

The final Go candidate passed `BEESUH_TEST_OPENCODE=1 go test -race ./...`,
`go vet ./...`, and CLI build. The OpenCode integration uses its isolated mock
provider. Fake-IdP checks remain independent local tests. A subsequent actual standard
GoAuthy identity pilot passed positive/repeated identity, workspace403, and
wrong-audience/wrong-scope/revoked401 cases; EXPIRED was skipped. See the
[dated IdP pilot evidence](goauthy-local-pilot.md). This does not establish
external provider/Codex credential-profile integration. Opt-in live tests
explicitly skip when fixtures are absent.

The API-key delivery consumer now has synthetic TLS issuer/model coverage through
the real Runtime/Rhiza: two profiles owned by one user can run concurrently;
each model call fetches a fresh credential, a rejected grant stops model dispatch,
and the other profile remains usable. `go test -race ./...`, `go vet ./...`, and
CLI build passed on 2026-09-07. The subsequent actual HTTPS GoAuthy producer
bridge passed before and after issuer restart: each profile initially dispatched
its exact API key twice to the certificate-verified local TLS model. After work
grant revocation, the producer checked credential POST 404 and zero additional
work model calls; personal succeeded twice more (personal 4, work 2).
`/tmp/goauthy-beesuh-delivery-live-20260907.log` contains both profile/revocation
PASS sequences and the post-restart PASS. Exact-key and counter checks are
producer evidence; child output and secrets were not logged. See the
[delivery fixture contract and evidence](goauthy-api-key-delivery.md).
This uses a fixture Principal/Authorize callback, not the public Beesuh login
path. OAuth/Codex token delivery and external SaaS are excluded; no production
keys, publishing, or deployment were involved.
