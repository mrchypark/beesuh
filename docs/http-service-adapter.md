# OAuth2/OIDC HTTP service

The standalone/Docker server uses existing OAuth2 endpoints. It does **not**
require an external Beesuh-specific `/authenticate`, `/authorize`, or
`/credential` API. The runtime callbacks and engine remain unchanged. The server
adds its own discovery/selection routes; no Goauthy endpoint is added.
Custom authority configuration is rejected.

## Credential delivery and injected HTTP clients

The common pattern is: verify the original human request and approved Goauthy
connection, deliver the external credential into request-local memory, then
inject a credential-bound HTTP client into the existing model adapter.
`cmd/beesuh/discovery.go` returns `beesuh.Credential.HTTPClient`; the model uses
`model.Config.HTTPClient`. The `github.com/mrchypark/beesuh/goauthy` package is
Beesuh's local delivery/transport implementation, not an official Goauthy SDK.
No AMP package or authenticated-client interface is imported or required for
LLM calls. An AMP-style authenticated interface is only a comparable design
pattern, not an implemented dependency or replacement API.

This preserves the v0.6.0 public flow: `GET /v1/connection-options`,
`POST /v1/connections`, then `POST /v1/chat`, using the same incoming human
Bearer profile and existing scopes. Delivery remains the bodyless
`POST /auth/v1/connection-grants/{grant_id}/credential` to the pinned issuer.
The injected provider client permits only POST to the exact configured HTTPS
destination: the provider base URL plus `/chat/completions` or `/responses`.
It is not a general-purpose SaaS client: alternate scheme/host/path, userinfo,
query and fragment are rejected. For the OpenCode configuration, the destination
is `https://opencode.ai/inference/go/openai/v1/chat/completions` with the
server-configured model `minimax-m3.1-flash`, not a caller-selected URL.

API-key execution binds the delivered version and one in-memory snapshot to
model state. The current candidate revalidates authority through the request's
credential source on every dispatch, before checking snapshot expiry. Revoked or
unavailable authority fails closed. Dispatch retains the pinned snapshot: it
never delivers the key again, substitutes a rotated key or automatically
refreshes credentials. Published v0.6.0 lacked this per-dispatch authority guard;
the candidate implements the fix, but its CI acceptance is still pending.
Delivery does not store keys or human tokens in environment variables,
Kubernetes Secrets, logs or execution history. There is no automatic refresh,
new `allow_refresh` feature, connections-write scope expansion or new handoff
endpoint. Login, browser-bound handoff and reauthorization remain host/BFF
responsibilities using Goauthy's existing APIs.

External SaaS tools have a separate target path: Conductor HTTP calls through
Exposure and Grant under the original human's authority. In identity-mode Go
embedding, a separate Conductor connection goes through `ResolveCredential`;
the runtime clears `Config.Token` and injects that connection's
`Credential.HTTPClient`. It does not reuse the model provider credential.
The host can attach the request-bound human Bearer to this client; Beesuh does
not automatically extract and forward the ingress Bearer for Conductor.

The existing Conductor client sends `POST /v1/events` and
`GET /v1/executions/{id}`, adding `X-Beesuh-User-ID`, `X-Beesuh-Workspace-ID` and
`X-Beesuh-Connection-ID`. These context headers are not authentication proof;
the gateway must enforce the actual identity and authority. Compatibility of
these calls with the new Conductor/Exposure/Grant route is unverified and remains
an integration blocker, not evidence of end-to-end SaaS readiness.

The embedding hook is available, but the published standalone HTTP server
rejects Conductor configuration and `-conductor-tools`. See
[shared authorization](shared-http.md) for the existing Grant/Exposure boundary;
this client alignment adds no API, permission or authority bypass.

## Goauthy discovery and approved connection selection

Use `goauthy` instead of a per-user `connections` manifest. Only global provider
routing remains in configuration:

```json
{
  "issuer": "https://accounts.example",
  "introspection_url": "https://accounts.example/oidc/introspect",
  "userinfo_url": "https://accounts.example/oidc/userinfo",
  "client_id": "beesuh",
  "client_secret_file": "/run/beesuh-client-secret",
  "audience": "https://beesuh.example",
  "required_scopes": ["beesuh.execute"],
  "goauthy": {
    "resource": "https://accounts.example/connections",
    "providers": {"opencode-provider-id-in-goauthy": "personal"}
  }
}
```

`personal` is a provider ID in the existing providers file, with its fixed model
HTTPS URL/model/protocol and no API key. `goauthy.providers` maps Goauthy registered
provider IDs to those operator-owned destinations. Users cannot supply endpoints,
binding digests, account IDs, header formats, key versions, or model credentials.
Static `connections` configuration is rejected.

This mode gives every verified `(issuer, sub)` its own personal workspace, using
the same `oidc_<sha256>` identifier as the user. Selection returns that workspace
ID; use it in subsequent chat/history calls. This is deliberately private owner
isolation, not shared-workspace membership. No OIDC workspace claim or editable
connection metadata grants workspace access. Shared-workspace directory policy
is outside this standalone profile.

1. Obtain a human access token using the existing code + PKCE login. It needs the
   Beesuh audience/API scope and `openid`. Discovery additionally requires
   `goauthy.connections.read` and the exact configured connections resource audience.
   Selection/execution also require `goauthy.connections.use` and the configured
   Beesuh confidential consumer's `client_id`. Configure the issuer to issue
   the human token for both audiences; allowlisting resources alone is insufficient.
2. Call `GET /v1/connection-options` with that Bearer token. Beesuh calls existing
   `GET /auth/v1/connection-collections` and
   `GET /auth/v1/connections/{collection_id}`. It returns only public
   `id`, `collection_id`, `owner_subject`, and `state` fields for the authenticated
   owner, from enabled API-key/OAuth2 collections. Arbitrary `metadata` is omitted.
   These are candidates, not proof of consent or execution readiness.
3. The host application uses Goauthy's **existing** connection-handoff flow to let
   the user approve `credential_delivery` for Beesuh. Its BFF holds expiring,
   one-use state bound to the authenticated browser session and expected selection;
   it verifies the returned state and re-fetches grant status against those stored
   expectations. No approval is automatic. There is no Bearer grant-list API and
   Beesuh does not invent one, implement a new login flow, or forward Goauthy cookies.
   See [Goauthy's existing handoff contract](https://github.com/mrchypark/goauthy/blob/main/docs/connection-use-handoffs.md).
4. Select the already approved connection through Beesuh:

   ```http
   POST /v1/connections
   Authorization: Bearer <human-access-token>
   Content-Type: application/json

   {"collection_id":"models","connection_id":"my-connection","grant_id":"approved-grant"}
   ```

   Beesuh independently GETs the existing connection detail and
   `/auth/v1/connections/{collection_id}/{connection_id}/grants/{grant_id}`.
   It checks owner, selection, consumer, resource, delivery mode, generation,
   provider policy, revocation and expiry, then registers an immutable local
   reference. The 201 response is the actual stored Beesuh connection, including
   its `id`, `workspace_id`, `generation` and `state`. Re-selecting a locally revoked
   connection fails and never reactivates it. Only safe references are persisted;
   the compact encoded reference must fit the runtime's 256-byte reference limit.
5. Pass the returned Beesuh `id` as `connection_id` to the existing `/v1/chat`
   request. At execution, Beesuh checks current identity/consent again. API keys
   are fetched once by the ready execution resolver from existing bodyless
   `POST /auth/v1/connection-grants/{grant_id}/credential`. The positive delivered
   version is bound into the model account identity **before** constructing model
   state. The model HTTP client uses that exact memory-only snapshot, so rotation
   cannot change the key between model-state binding and dispatch. No version is
   guessed from metadata, and there is no second delivery for that model call.
   Registration/readiness/listing fetch no key. Normal OpenCode chat and Responses
   providers are supported. OAuth2 connections use existing `credential-status`
   metadata and the existing OAuth delivery adapter with account/scope validation.

GET `/v1/connections` continues to list locally selected runtime references; it is
not an external consent list. A displayed local ready state is not a fresh grant
assertion: execution always revalidates current external authority. Deleting one
through the existing Beesuh route revokes local execution only. External consent
must be revoked in Goauthy's existing account UI/API.

New discovery/selection requests have a 20-second overall deadline, bounded JSON
responses, at most 128 collections and 1,000 total candidates. Oversized results
fail rather than silently truncate. Responses disable caching and use bounded
write deadlines. Malformed references return 400, invalid authentication 401,
denied bindings 403, and unavailable dependencies 503; remote error bodies never
reach responses/history. Model secrets and human tokens are never stored in
configuration, environment, Secret objects, logs, or execution history. After a
restart, existing selected references work only with a new authenticated request;
there is no stored human token or unattended refresh capability.

## Authentication and workspace authorization

Clients obtain an access token from their existing authorization server (for
example, authorization code + PKCE in the host application). Beesuh is a resource
server, not a new OAuth issuer or browser login application. Send the access token
in `Authorization: Bearer ...`; cookies, identity headers, ID tokens and request
JSON do not establish identity.

Beesuh calls the configured RFC 7662 introspection endpoint with a form POST and
`client_secret_basic`. This service's strict token profile requires `active:true`,
`sub`, an `aud` string or array containing the Beesuh
audience, a future numeric `exp`, and all configured scopes. Optional `nbf` must
not be in the future; optional `iss` must exactly match the pinned issuer.
An absent `iss` uses the issuer associated with the trusted introspection URL.
Optional `token_type` must be Bearer. The required subject, audience and expiry
claims are optional in RFC 7662 itself: an issuer omitting these required claims
does not satisfy this profile. Unknown protocol response fields are ignored. Responses are bounded to
64 KiB and must contain one JSON value. Unverified JWT payloads are never decoded
as identity evidence.

OIDC UserInfo is required for incoming human authentication. Introspection's
hint and Bearer usage do not prove access-token type. The access token must include
`openid`; Beesuh GETs that endpoint using the token and requires its `sub` to
match introspection. The issuer's UserInfo endpoint must validate access tokens
(Goauthy's implementation does); refresh tokens fail this access-only check.
UserInfo alone does not establish audience or token expiry.
All endpoints are explicitly operator-pinned; no URL or issuer comes from a
browser request or token. Configure URLs from your issuer's existing metadata.
This implementation does not perform automatic discovery or local JWT/JWKS
validation. OAuth-only issuers without UserInfo are unsupported by this narrower
OIDC profile.

The persisted identity is a hash of the exact `(issuer, sub)` tuple. Identical
subjects at different issuers cannot share history or connections. The personal
workspace has the same identifier; local connection metadata cannot grant access
to another workspace.

The request token remains only in the authenticated request context. Runtime
authorization and the ready execution resolver revalidate identity and current
grant approval before admitting execution and obtaining one request-local API-key
snapshot. An unavailable UserInfo or grant authority fails closed at that boundary.
Published v0.6.0's snapshot transport checked the fixed destination and snapshot
expiry without repeating identity/grant checks per dispatch, so revocation after
admission could leave that snapshot usable until expiry. The current candidate
adds request-source authority revalidation before each snapshot expiry check,
without a second delivery or automatic refresh. Candidate CI is pending.
Provider calls already in flight cannot be recalled.

There is no user-indexed token cache, refresh-token store or persisted login
session. Work without the authenticated request context, including after restart,
requires a fresh request/token.

## Configuration and startup

Every standalone `-listen` invocation requires `-service-config`, `-backend native`
(the default), and `-providers`. The service file must contain the `goauthy` block
shown above. Per-user `connections` manifests and `token_url` are rejected, even
when empty: a local ACL does not establish current external approval.
`-operator-http` has been removed. `BEESUH_API_TOKEN` cannot authenticate requests.

`providers.json` contains routing/model metadata, with no shared credentials:

```json
{
  "default":"personal",
  "providers":[{
    "id":"personal", "kind":"openai-compatible",
    "base_url":"https://model.example/v1", "model":"your-model"
  }]
}
```

Only Bearer-compatible chat-completions and responses providers are supported,
using `openai-compatible`, `opencode-go`, or `opencode-zen`. All entries must meet
this profile, including providers not mapped in `goauthy.providers`. Provider
`api_key_env`, command and login-store settings are rejected. Local Codex CLI or
app-server login, a global `BEESUH_MODEL_API_KEY`, and Conductor credentials
cannot supply a model credential. `-conductor-tools` is rejected before reading
its environment configuration. A denied or unavailable connection never falls
back to a shared key. Generic Go embedding callbacks and local CLI credential
configuration remain available outside this standalone HTTP profile.

Use real issuer endpoint URLs and scopes supported by your deployment. The
introspection client must support `client_secret_basic`; its secret file is a
separate OAuth client credential and is read on each form request. Do not put
that secret in the service JSON or environment. The issuer and model destinations
require HTTPS. Endpoint trust belongs to the operator; HTTPS alone does not
provide an egress policy. OAuth requests have a ten-second timeout; redirects and
cookie jars are disabled. Errors expose fixed categories, and remote descriptions,
bodies and transport diagnostics do not reach responses or history.

Run a newly built image with a fresh service data volume; configuration and secret
files must be readable by UID 10001. Existing operator-mode data is rejected by
the runtime's identity-mode pin and cannot be reassigned to authenticated users.

```sh
docker build -t beesuh:local .
docker run --rm -p 127.0.0.1:8080:8080 \
  -v beesuh-service-data:/data \
  -v "$PWD/config:/config:ro" \
  -v "$PWD/oauth-client-secret:/run/beesuh-client-secret:ro" \
  beesuh:local \
  -listen 0.0.0.0:8080 -data-dir /data \
  -providers /config/providers.json -service-config /config/service.json
```

The service JSON at the beginning of this document uses
`/run/beesuh-client-secret`. After selecting an approved connection, use the
returned `id` and `workspace_id` in chat requests:

```sh
curl http://127.0.0.1:8080/v1/chat \
  -H "Authorization: Bearer $HUMAN_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"workspace_id":"<returned-workspace-id>","session_key":"chat-1","request_id":"http-1","connection_id":"<returned-id>","message":"Hello"}'
```

Standards: [RFC 7662](https://www.rfc-editor.org/rfc/rfc7662),
[OIDC UserInfo](https://openid.net/specs/openid-connect-core-1_0.html#UserInfo), and
[RFC 6749 client authentication](https://www.rfc-editor.org/rfc/rfc6749#section-2.3.1).
