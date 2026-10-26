# OAuth2/OIDC HTTP service

The standalone/Docker server uses existing OAuth2 endpoints. It does **not**
require an external Beesuh-specific `/authenticate`, `/authorize`, or
`/credential` API. The runtime callbacks and engine remain unchanged. The server
adds its own discovery/selection routes; no Goauthy endpoint is added.
The previous custom authority configuration is rejected, rather than silently
retained as a fallback.

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
Discovery and static `connections` configuration cannot be combined.

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
   Beesuh confidential consumer's `client_id`. See the audience/client setup below.
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

For the legacy static OAuth configuration below, workspace membership is a **trusted local ACL**, not a standard OIDC workspace
claim. Each configured subject maps to one workspace (and may have multiple
model connections). Ambiguous mappings fail startup. Adding/removing membership
requires editing the config and restarting Beesuh. Introspection checks current
token activity at execution; it does not discover changes in an external group
membership directory. The persisted identity is a hash of the exact `(issuer,
sub)` tuple, so identical subjects at different issuers cannot share history or
connections. Connection IDs must be unique; existing ownership is immutable.

The request token remains only in the authenticated request context. Every runtime
authorization, queued execution, and model dispatch revalidates it. There is no
user-indexed token cache, refresh-token store or persisted login session. Work
without that authenticated context, including after restart, fails closed and
requires a fresh request/token. In-flight calls already accepted by a provider
cannot be recalled by revocation.

## Model access: two explicit OAuth choices

A configured model endpoint must actually accept OAuth Bearer access tokens
issued for its protected resource. Neither OIDC nor token exchange makes a
third-party model provider accept a token it does not trust.

* Default (`token_exchange:false`): relay the request token only after fresh
  introspection verifies the model audience, configured model scopes, issuer,
  expiry and the original subject. Thus this token must be authorized for both
  Beesuh and that model resource. No token endpoint call is made.
* Optional (`token_exchange:true`): immediately before each model request, POST
  the standard RFC 8693 grant to the configured existing token endpoint using
  the incoming access token as `subject_token`, both token types set to the
  access-token URN, and the connection's fixed `scope` and either `resource`
  (when configured) or `audience`. The expected audience is still checked on the
  result in either case. The issuer must explicitly support/permit this grant for this
  confidential client and target. Require a Bearer access-token response with
  positive `expires_in` if present, then independently introspect the returned token to
  verify the same issuer and subject, model audience, scopes and expiry. No
  exchange occurs during startup, registration, listing, history, or denial.

Both modes pin credentials to the exact configured provider POST endpoint
(`/chat/completions` or `/responses`). Credentials cannot select a destination;
redirects are disabled and tokens never enter URLs, config, environment, Secret
objects, history or logs. Provider and issuer implementations remain trusted to
protect their own bodies/logs. The credential-bearing request is not returned
through `Response.Request`.

## Legacy static Goauthy API-key delivery

**API-key vault delivery is not an OAuth2/OIDC standard.** The standalone server
also supports the existing `goauthy.NewAPIKeyClient` adapter through the optional
`goauthy_api_key` connection setting. It calls Goauthy's **existing**
`POST /auth/v1/connection-grants/{grant_id}/credential`; no new external API,
login flow, token header or approval endpoint is introduced. Standard OIDC still
authenticates the incoming request. This mode is mutually exclusive with the
connection's OAuth `audience`, `scopes`, `resource` and `token_exchange` settings.

Immediately before delivery, Beesuh revalidates the original request token for
the Beesuh audience, required execution scopes and matching UserInfo subject.
It independently introspects that same token for `goauthy_api_key.resource`,
`goauthy.connections.use`, the configured subject, and `client_id` equal to the
top-level confidential `client_id`. The token stays in request memory. No token
exchange, persisted human token file or Bee-only token forwarding is used.
Goauthy additionally checks that this is a current human access token from the
consented consumer and grant owner; machine, exchanged, actor and DPoP tokens
are not suitable for this delivery protocol.

For an existing Goauthy managed confidential client, a feasible deployment is:

1. Let `B` be the Beesuh audience and `D` the existing
   `GOAUTHY_CONNECTIONS_RESOURCE`. Configure the existing managed client with
   `audience:[B,D]` and `default_aud:[D]`, allow `openid`, your Beesuh execution
   scope and `goauthy.connections.use`, and enable authorization code. Merely
   allowlisting both audiences does not add both to the issued token.
2. The host's existing authorization-code + S256 PKCE flow requests
   `resource=B` and those scopes. Redeem the code using that same confidential
   client and its verifier, without adding a second `resource` at redemption.
   Goauthy combines the explicitly granted resource with the managed client's
   default audience. Verify introspection reports both B and D and this client ID.
   This is a Goauthy deployment profile, not portable OAuth behavior.
3. Use an existing approved `credential_delivery` grant whose owner is the OIDC
   subject and whose consumer is that exact client. Pin the reviewed grant
   metadata below; do not obtain trusted bindings from the browser or the first
   delivery response. The host sends the resulting human access token in the
   ordinary Beesuh `Authorization` header.

An explicitly shared logical audience B=D can also be deployed when intentional;
both checks still run. If the issuer cannot issue a token meeting both resource
checks, this request-bound adapter cannot execute: use an embedding host with an
owner-bound token source. Beesuh never skips its audience check or exchanges the
token to work around this boundary.

Example connection (the top-level service and provider configuration remain as
below; the provider may use `kind:"opencode-go"`):

```json
{
  "id":"alice-personal", "subject":"alice-subject",
  "workspace_id":"team", "provider":"personal",
  "goauthy_api_key":{
    "resource":"https://accounts.example/connections",
    "binding":{
      "grant_id":"approved-grant", "provider_id":"opencode",
      "connection_generation":"reviewed-generation",
      "connector_digest":"reviewed-digest", "credential_version":1,
      "header":"Authorization", "prefix":"Bearer ",
      "endpoint":"https://opencode.ai/zen/go/v1/chat/completions"
    }
  }
}
```

The binding endpoint must exactly equal the model operation derived from the
provider configuration and must use HTTPS. This mode supports Authorization
Bearer API keys. Startup/registration only construct the lazy client: they do
not fetch credentials. Every model request fetches again and validates all
binding fields and consent expiry. The account identifier is a stable hash of
issuer, subject and reviewed binding metadata. After key rotation, update the
pinned version and use a new connection ID if re-consent changes account identity.
Keys stay in the outbound request memory and never enter environment variables,
Secret objects, provider files, logs or history. A failure never uses a shared key.
Goauthy tokens go only to Goauthy, and the delivered key goes only to the fixed
model endpoint. Redirects/cookies are disabled. Actual Goauthy deployment still
requires verification against the issuer's configured client and grant.

## Configuration

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

`service.json` uses the issuer's existing endpoint URLs:

```json
{
  "issuer":"https://accounts.example",
  "introspection_url":"https://accounts.example/oauth/introspect",
  "userinfo_url":"https://accounts.example/oauth/userinfo",
  "client_id":"beesuh",
  "client_secret_file":"/run/oauth-client-secret",
  "audience":"https://beesuh.example",
  "required_scopes":["openid","agent.execute"],
  "connections":[{
    "id":"alice-personal", "subject":"alice-subject",
    "workspace_id":"team", "provider":"personal",
    "audience":"https://model.example",
    "scopes":["model.invoke"]
  }]
}
```

Use real endpoint URLs and scopes supported by your deployment; the names above
are examples, not new Beesuh endpoints or mandatory OAuth scope names. To enable
exchange, add top-level `token_url` with the issuer's token endpoint and set
`token_exchange:true` on the connection. Set `resource` only if required by the
issuer's resource policy. The introspection client must be authorized to inspect
both input and exchanged model tokens. Cross-issuer exchange is not supported.
Client authentication currently supports `client_secret_basic` only. Its secret
file is a separate OAuth client credential, not a model key, and is read on each
form request. It must not be supplied through environment variables or config.

Remote URLs require HTTPS; literal loopback HTTP is permitted for local tests or
a sidecar (DNS `localhost` is not allowed). Endpoint trust is operator-owned:
HTTPS is not an SSRF or network-egress policy. OAuth requests have a ten-second
timeout; redirects and cookie jars are disabled. Errors expose fixed categories
only; remote descriptions, bodies and transport diagnostics are not propagated.
Allowlisted OAuth errors distinguish inactive user grants from client outages;
`invalid_client`, unsupported grants, malformed replies and server/network errors
are unavailable, not evidence of invalid provider credentials. Failed validation
never falls back to a shared key.

Run a newly built image with a fresh service data volume; files must be readable
by UID 10001:

```sh
docker build -t beesuh:local .
docker run --rm -p 127.0.0.1:8080:8080 \
  -v beesuh-service-data:/data \
  -v "$PWD/config:/config:ro" \
  -v "$PWD/oauth-client-secret:/run/oauth-client-secret:ro" \
  beesuh:local \
  -listen 0.0.0.0:8080 -data-dir /data \
  -providers /config/providers.json -service-config /config/service.json
```

Send the user token to `/v1/chat` with `connection_id:"alice-personal"` and
`workspace_id:"team"`. The runtime checks connection ownership as well as the
workspace. Configured connections are registered at startup; this does not
reactivate revoked or needs-login connections. Use a new connection ID after
renewed consent when appropriate. Operator data cannot be silently reassigned
into service mode. Shared-key HTTP remains an explicit `-operator-http` mode.

Supported model protocols are Bearer-compatible chat-completions and responses,
with native `openai-compatible`, `opencode-go` or `opencode-zen` adapters. The
latter two protocol adapters do not imply their hosted services trust your issuer.
Anthropic `x-api-key`, CLI and external OpenCode server credential paths are not
supported by these standalone modes. Existing Go embedding callbacks are unchanged.

Standards: [RFC 7662](https://www.rfc-editor.org/rfc/rfc7662),
[OIDC UserInfo](https://openid.net/specs/openid-connect-core-1_0.html#UserInfo),
[RFC 8693](https://www.rfc-editor.org/rfc/rfc8693), and
[RFC 6749 client authentication](https://www.rfc-editor.org/rfc/rfc6749#section-2.3.1).
