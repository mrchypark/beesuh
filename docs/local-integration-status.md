# Standalone runtime and optional authentication adapters

Beesuh is an independent Go runtime. Native CLI and `/v1/chat` call the configured
model provider directly, preserving conversations and runs in Rhiza. Optional
memory and static skills run locally. Tests use
local HTTP fixtures and disposable Rhiza directories; no sibling application
packages, databases, or deployment are imported. The module pins Rhiza v0.18.0. Optional untracked `go.work` files can select
the user-requested local Rhiza checkout during development.

Compos and Conductor services are unavailable in the current environment. Retained
adapter packages, flags, endpoints, and `examples/compos-input.json` are compatibility
code and regression fixtures, not standalone setup requirements. The TypeScript
examples under `examples/opencode/` are also retained for regression coverage;
they are not installation guidance for an available external service.

## Identity and external credentials

`httpapi.NewAuthenticated` accepts a host authenticator; `WithPrincipal` alone
is not authentication. The opt-in [IdP pilot](goauthy-local-pilot.md) verifies a
standard bearer token by RFC 7662 introspection and OIDC UserInfo. Its fixed
membership list is a test fixture, not automatic workspace enrollment. No
production browser login/callback/session server is added by this pilot.

The target integration uses GoAuthy's **optional credential-profile extension**
for external authentication lifecycle and authorized use. This is distinct from
its standard OAuth2/OIDC endpoints. Profiles are generic OAuth2/API-key profiles,
not separate AI/SaaS credential types. Beesuh consumes credentials with explicit
consent; profiles do not implicitly authorize their use.

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

## Historical local test evidence

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
