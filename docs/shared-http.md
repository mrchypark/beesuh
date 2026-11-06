# Shared-workspace HTTP service

This profile connects an OIDC-verified human request, server-owned
organization/workspace registry, Grant authorization and an independently
approved Goauthy `credential_delivery` connection. The intended model is
`minimax-m3.1-flash` through the native `openai-compatible` HTTP provider at
`https://opencode.ai/inference/go/openai/v1`. It does not
start an OpenCode server. This document defines the shared-profile consumer
contract. Check the selected release's source revision and image digest before
deployment; this contract alone does not establish live integration readiness.

## Supplied owner packet (not live-verified)

The current owner packet supplies these concrete values. They do not yet form a
complete shared configuration; do not replace unresolved fields with guesses.

| Field | Supplied value / status |
| --- | --- |
| Logical organization / workspace | `operable` / `cypark-default` |
| Human subject | `operable-cypark`; verified from the token, not a config identity override |
| Issuer | `https://operable-auth.dev.conalog.com` |
| Introspection | `https://operable-auth.dev.conalog.com/oidc/introspect` |
| UserInfo | `https://operable-auth.dev.conalog.com/oidc/userinfo` |
| Consumer client ID | `operable` |
| Bee API audience supplied for v4 | `https://operable.dev.conalog.com/grant` |
| Required scopes supplied for v4 | `openid`, `goauthy.read` |
| OAuth secret reference | namespace `operable`, Secret `goauthy-operable`, key `bootstrap-client`; mount `/run/secrets/bootstrap-client` |
| Goauthy resource | `https://operable-auth.dev.conalog.com/connections` |
| Grant provider / PEP | `beesuh` / `beesuh-exposure-pep`; metadata only |
| Grant organization / learner role | NOT READY |
| Catalog/version/digest pins, learner URL, Grant hint audience | NOT SUPPLIED |
| Existing confidential client | `operable` is reusable; no new internal client |
| Additional connections audience/scopes actually issued and delivery consent | NOT VERIFIED |

The supplied `goauthy.read` is not a substitute for the code's connections
read/use scopes. Actual issuance for both audiences and connection consent still
needs confirmation. The Secret reference discloses no value and does not create
or verify an OAuth registration.

The proposed action mapping is `run_execute`, `connection_select`,
`connection_revoke` → `beesuh.run.create`; read/history/providers/options/list →
`beesuh.run.read`; `run_cancel` → `beesuh.run.cancel`. These action IDs are
registered metadata, not capability IDs or catalog/contract pins. Use the fresh
owner packet for actual capability IDs, versions and digests.

## Forwarded-header profiles

Each operation pin may declare `forward_headers`: a sorted, unique list of
nonempty header names containing only lowercase ASCII `a-z`, digits `0-9` and
hyphens. Sensitive, hop-by-hop and transport-controlled names are forbidden:
`authorization`, `proxy-authorization`, `proxy-authenticate`, `cookie`,
`set-cookie`, `connection`, `keep-alive`, `transfer-encoding`, `te`, `trailer`,
`upgrade`, `host`, `content-length`. Hash only declared headers
actually present on the request, as `name:raw-value` followed by LF, in list
order. Do not synthesize missing headers. The original Bearer is bound separately
by its credential digest; method/path, canonical query and raw body hash/length
remain bound independently.

Each present declared header must have exactly one valid UTF-8 raw value.
The generic digest loop rejects ASCII controls except HTAB, and rejects DEL;
it permits empty values, spaces and commas, and imposes no 256-byte limit.
Values from differently cased occurrences are combined before the single-value
check. Accepted raw values are hashed unchanged, without trimming or normalization.
Organization/workspace selectors and chat/selection Content-Type separately
retain their stricter `singleSharedHeader` validation: one nonempty value of at
most 256 bytes, valid UTF-8, no CR/LF/NUL or surrounding whitespace, and no commas
except in Content-Type. Content-Type must also parse as `application/json`.

Omitting `forward_headers` retains v0.5 legacy behavior: a single matching
`X-Workspace-ID` is required, GET/bodyless operations bind both selectors, and
chat/selection bind Content-Type plus both selectors. Explicit `[]` is valid
and hashes the empty preimage; explicit `null` is invalid. Empty metadata does
not waive identity, organization, Content-Type or body/query validation.

### New single-constant-workspace profile

For the first scope, configure exactly one context for `cypark-default` and
explicit operation metadata excluding `x-workspace-id`:

- GET: `"forward_headers": ["x-organization-id"]`
- Chat/selection POST: `"forward_headers": ["content-type", "x-organization-id"]`

Exposure's corresponding `request.forwardHeaders` lists must contain these same
names, without the unsupported `x-workspace-id`. No new nginx hop is needed.
These are consumer configuration requirements, not upstream or live approval.

With `X-Organization-ID: operable`, the exact GET preimage is:

```text
x-organization-id:operable
```

Its digest is `sha256:d4acfbfe68c7544aacf2625a708860c5029ff58dc85705403afd60add17ace8f`.
The POST preimage for exactly `Content-Type: application/json` is:

```text
content-type:application/json
x-organization-id:operable
```

Its digest is `sha256:b19b9c03c91f1cbacfbf48e3e3e4479dd9e4a89a0ab99659ff44b8351675bd59`.
Explicit `[]` hashes to
`sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`.
All nonempty preimages above end in LF.

### Legacy v0.5 preimages (field omitted)

GET/bodyless:

```text
x-organization-id:operable
x-workspace-id:cypark-default
```

SHA-256 for that exact preimage is
`sha256:818f1c996cee0ceea1860229107f8d7ccc367853d0e5c6d822962790a9705363`.
Chat and connection-selection POSTs bind three lines instead:

```text
content-type:application/json
x-organization-id:operable
x-workspace-id:cypark-default
```

For exactly `application/json`, that POST digest is
`sha256:084be18acf9b8af74f91e5eaff09e93a9eebccde8f4f94393234cef2da7cc985`.
If Content-Type has parameters, the complete actual value is hashed unchanged,
including its case, spacing and parameters; it is not MIME-reserialized. Bee
requires one safe Content-Type header whose parsed media type is
`application/json`. Missing, duplicate, malformed or non-JSON Content-Type
denies before Grant, key delivery or model dispatch. Authorization is excluded
from both header preimages and bound separately by the credential digest.

Owner source `20683606` requires `content-type` in POST `request.forwardHeaders`
when `request.contentTypes` declares `application/json`; it is not automatically
forwarded. Legacy POST capabilities bind all three headers, while legacy
GET/bodyless capabilities bind two. New explicit lists above bind two and one,
respectively. No HTTP transport
workaround or automatic Content-Type insertion is added.

The owner-confirmed POST catalog `maxBytes` is `262144` (256 KiB), matching
Bee's existing `sharedBodyLimit`. `65536` was only an example, not an official
ceiling. Exposure's global `MAX_REQUEST_BYTES` must be at least `262144`; its
supplied default is 4 MiB. No smaller Bee limit or additional validation is
introduced. Forward Content-Type's raw value, including parameters, and bind it
only when declared. POST Content-Type validation remains mandatory even when it
is excluded from the explicit digest list.

Exposure owner source `20683606` confirms only `pathSegment`, `queryScalar`,
`constant` and `jsonPointer` resource extractors; there is no header extractor.
The current one-workspace baseline therefore uses type `workspace`, resource ID
`cypark-default`, and existing constant evidence:

```json
{"kind":"constant","name":"","canonicalValue":"cypark-default"}
```

Use that resource/evidence for each enabled capability in the owner catalog.
The Exposure resource selector is `{"kind":"constant","value":"cypark-default"}`;
do not add `constant.format` or a `format` field to this selector.
Forwarded selector hashing and resource extraction are separate contracts.
Grant must still authorize the actual organization member/learner role against
that workspace, and Bee independently checks headers and body/query workspace
against its server binding. This remains shared authorization, with no fallback
to a personal workspace. Exposure forwards the original human Bearer and mints
no new assertion. The existing constant fixture matches this public schema;
there is no unknown-enum or new-header-feature prerequisite.

## Trusted configuration

Start with the existing `-service-config` and `-providers` flags. No new CLI
flag, approval API, OAuth client-registration endpoint or Exposure-minted token
is introduced. All values below are templates; obtain actual registrations and
audiences from their owners. Digest placeholders deliberately fail validation.

`service.json`:

```json
{
  "issuer": "https://accounts.example",
  "introspection_url": "https://accounts.example/oidc/introspect",
  "userinfo_url": "https://accounts.example/oidc/userinfo",
  "client_id": "OWNER_CONFIRMED_CONSUMER",
  "client_secret_file": "/run/beesuh-client-secret",
  "audience": "OWNER_CONFIRMED_BEE_API_AUDIENCE",
  "required_scopes": ["OWNER_CONFIRMED_BEE_API_SCOPE"],
  "goauthy": {
    "resource": "OWNER_CONFIRMED_CONNECTIONS_RESOURCE_AUDIENCE",
    "providers": {"OWNER_CONFIRMED_GOAUTHY_PROVIDER_ID": "minimax"}
  },
  "shared": {
    "grant_url": "https://grant.example",
    "app_key_file": "/run/beesuh-grant-app-key",
    "grant_credential_hint_audience": "OWNER_CONFIRMED_GRANT_HINT_AUDIENCE",
    "contexts": [{
      "organization_id": "operable",
      "workspace_id": "cypark-default",
      "provider_id": "registered-bee-provider",
      "pep_client_id": "registered-pep",
      "catalog_generation": 1,
      "catalog_digest": "sha256:OWNER_SUPPLIED_64_LOWERCASE_HEX",
      "resource_type": "workspace",
      "resource_id": "cypark-default",
      "resource_evidence": {
        "canonicalValue": "cypark-default",
        "kind": "constant",
        "name": ""
      },
      "operations": {
        "connection_options": {
          "capability_id": "OWNER_CONNECTION_OPTIONS_CAPABILITY",
          "capability_version": 1,
          "contract_digest": "sha256:OWNER_SUPPLIED_64_LOWERCASE_HEX",
          "action_id": "OWNER_CONNECTION_OPTIONS_ACTION",
          "forward_headers": ["x-organization-id"]
        },
        "connection_select": {
          "capability_id": "OWNER_CONNECTION_SELECT_CAPABILITY",
          "capability_version": 1,
          "contract_digest": "sha256:OWNER_SUPPLIED_64_LOWERCASE_HEX",
          "action_id": "OWNER_CONNECTION_SELECT_ACTION",
          "forward_headers": ["content-type", "x-organization-id"]
        },
        "run_execute": {
          "capability_id": "OWNER_RUN_EXECUTE_CAPABILITY",
          "capability_version": 1,
          "contract_digest": "sha256:OWNER_SUPPLIED_64_LOWERCASE_HEX",
          "action_id": "OWNER_RUN_EXECUTE_ACTION",
          "forward_headers": ["content-type", "x-organization-id"]
        }
      }
    }]
  }
}
```

The example enables discovery, selection and chat only. A context requires a
nonempty subset of the known operation keys. Omitted operations return 403 at
authorization, before Grant policy evaluation, Goauthy metadata/key delivery or
model dispatch. Unknown operation keys and malformed pins refuse startup. Adding
UI history, run lookup, provider listing or cancellation requires their actual
registered pins, not fabricated aliases of `run_execute`.

`goauthy.resource` remains one connections-resource audience, not a per-operation
resource map. Model keys remain memory-only and must not enter environment
variables, Kubernetes Secrets, logs or persisted execution/history records.

There may be 1–128 contexts for one organization, with unique
`(organization_id, workspace_id)` pairs. Every configured operation must have a
positive capability version and a valid contract digest. Catalog generation is
positive; digests use `sha256:` followed by 64 lowercase hex characters. The
resource evidence canonical value must equal the configured resource ID; its
kind/name and workspace resource must match the owner catalog. A present but
incomplete `shared`, including explicit null or an empty object, refuses startup.
Omitting the `shared` field entirely retains the personal-workspace profile.

That generic Bee multi-context support is unchanged. The current Exposure
candidate targets only the single constant workspace `cypark-default`.
Multi-workspace Exposure header extraction is unsupported and is not a
prerequisite for this workspace. Additional contexts need owner catalogs/resource
extractors that actually support their routes; no live routing is claimed here.

Grant uses HTTPS with ordinary certificate verification. The shared JSON does
not expose the package's test/private-HTTP opt-in. If the owner requires an
internal HTTP URL, report that exact requirement before changing transport
policy; do not silently relax TLS or substitute a guessed endpoint. App-key
rotation requires recreating the configured client (restart the service).

### Grant wire v2 migration and proxy allowlist

The v0.6.0 compatibility target uses Grant wire v2. Beesuh v0.5.1's older wire
contract is incompatible with Grant v0.9.0; do not treat an unchanged URL or
valid app key as proof of compatibility. This section records the completed
owner-supplied contract, based on Grant source
`f0ab0e7684f279dc84f82f22c7537d3679780eb9` and subsequent owner clarifications;
contract completeness does not establish release or live acceptance.

| Migration item | v0.5.1 → v0.6.0 |
| --- | --- |
| Grant wire | Older contract → integer wire version 2; required for Grant v0.9.0 |
| Private proxy | Retain decide POST; allow the exact provider catalog GET below |
| Service settings | Keep existing authority, app-key file and static catalog/capability pins; fetch revision online |
| User/model credentials | Keep original human Bearer, Goauthy consent and memory-only key delivery; no new client, permission or credential |
| Acceptance | Verify the selected release's CI and exact image digest; no cancellation/streaming claim for the first integration |

Allow these exact method/path pairs through the existing Grant proxy:

| Method | Path | Authentication |
| --- | --- | --- |
| GET | `/api/v2/http-capabilities/catalog/beesuh` | Only normal `X-Grant-App-Auth-Key: <raw-app-key>`; no human Bearer or organization header |
| POST | `/api/v2/http-capabilities/decide` | Existing app-key and original human Bearer contract |

The GET path's final segment is the configured `provider_id`; `beesuh` is the
consumer example, not a new endpoint or registration. Preserve the raw app-key
header rather than adding a Bearer prefix. The catalog envelope is configured
for the provider/PEP, not scoped to the user's organization. Do not add a user
token to catalog configuration or persist it for these checks.
The private proxy needs only this added GET and the existing decide POST, not a
wildcard API allowlist. Decide requires exactly one each of `Authorization`,
`X-Organization-ID` and `X-Grant-App-Auth-Key`; reject duplicate security headers.

Every authorization check must fetch the active catalog, require
`authorizationMode: "user"` and a positive `pepPolicyRevision`, then match the
configured provider, PEP, generation, catalog digest and capability pins exactly.
Only the fetched revision enters the subsequent wire-v2 decide request. Existing
static pins and service settings remain; no revision setting, extra API, OAuth
client or permission is introduced. No cache, retry or fallback may turn a
failed check into an allow. A policy change between GET and decide can produce
HTTP 403 for a stale revision; reject that request.

The catalog top-level fields are `authorizationMode` (string),
`pepPolicyRevision` (positive int64), `schemaVersion` (uint64, exactly 1), `providerId`,
`pepClientId`, `catalogDigest`, `issuedAt`, `expiresAt` (strings), `generation`
(uint64), and `capabilities` (array); `responseResources` is an optional array.
Catalog timestamps are RFC3339Nano with a 30-second TTL. Exposure checks
`expiresAt > now` without a separate issued-at skew check; do not infer a skew
tolerance from that behavior.
Catalog and contract digests follow their distinct owner-defined canonical
preimages; neither SHA-256 of the whole HTTP envelope nor one generic JCS
transformation is a substitute. Production matches the configured exact catalog
pin and canonically recomputes the selected capability's `actualContractDigest`
against its reviewed contract pin; comparing advertised pins alone is
insufficient. This selected-capability validation does not add a whole-catalog
compiler or raw-envelope-hash fallback. Existing full-Bearer credential,
body, query and forwarded-header digest rules remain unchanged. This selected
subset's upstream acceptance is separate from specification completeness.

Recomputation applies only to the selected typed-serialized capability from the
normal ActiveCatalog GET, not arbitrary staging JSON. Parse with duplicate-key
rejection and exact integers, remove only its top-level `contractDigest`, and
hash compact JSON with recursively sorted object keys, original array order and
preserved null/empty-array distinctions. Do not fill defaults or normalize fields
before hashing. Use Go string escaping with actual U+2028/U+2029 UTF-8, no HTML
escaping and no trailing LF. The result must match both the advertised digest
and the configured reviewed pin; independent vector tests are required.

In this catalog response, required `request.forwardHeaders` and
`response.forwardHeaders` may be null: treat null as an empty runtime allowlist
but preserve raw null in the canonical hash (`null` differs from `[]`). Missing
keys, null elements and empty header names remain invalid. This exception does
not extend to `pathParameters` arrays or Beesuh's configured `forward_headers`,
where explicit null remains invalid. HTTP staging validation is a separate
contract and does not override the normal ActiveCatalog response behavior.

The supplier provided one nonsecret canonical vector for `inventory/vector.read`:
contract digest `sha256:dff00647c46093e40e72139bb379208b85bc7572d1371188d1a34b2d69aff1df`
and catalog digest `sha256:c37b5c8351df45b710b9bf152e36a50884278de82f49c5035ec3105df813b890`.
These are not current Beesuh pins or a complete wire-v2 golden bundle. Consumer
fixtures must retain their owner-specification provenance and must not relabel
this single supplier vector as full-protocol acceptance evidence.

Each capability always contains `capabilityId`, `version` (uint64), `enabled`,
`accessMode`, `method`, `pathTemplate`, `actionId`, `resource`, `query`, `request`,
`response` and `contractDigest`. `actionId` is a string for authorized access
and nullable for `public_authn`. Optional fields are `pathParameters`,
`recordScope`, `batch`, `responseAuthorization` and `organizationContext`.
`resource` contains `type` (string) and `id` (extractor object). `request` contains
`body` (string), `maxBytes` (uint64), `forwardHeaders` (string array), with optional
`authorization` (string) and `contentTypes` (string array). `response` contains
`maxBytes` (uint64), `forwardHeaders` (string array), `redirects` (string), and
optional `errorStatuses` (integer array).

The decide input includes `wireVersion` (JSON integer 2) and the fetched
`pepPolicyRevision` (JSON int64, 1–9223372036854775807); preserve integer precision
without float conversion.
`actionId` is a decision response field, not an input. Neither
`authorizationMode` nor an override is a decide input. Validate the USER decision
against its request binding and configured authority, including revision, action,
resource, principal and expiry. The response does not echo raw `canonicalQuery`,
`providerCredential` or `resourceEvidence`.
Validate the full USER response schema, wire version, revision, request echoes
and timestamps before interpreting `allowed`. An `allowed: false` response must
still satisfy those checks; it is not a shorter response format.

Decision timestamps allow zero clock skew: `issuedAt <= now`, `expiresAt > now`,
and `0 < expiresAt - issuedAt <= 5 seconds`. Recheck `expiresAt > now` immediately
before dispatch. `principalDigest` is an opaque 64-character lowercase hex value
without a `sha256:` prefix; do not recompute it from the Goauthy subject. A
supported follow-up check must retain the first allow's principal digest.
`providerCredentialDigest` keeps the existing domain-separated preimage with a
U64BE length prefix over the complete `Bearer <token>` value, including the
scheme and space; do not change the existing domain or encoding.

There is no official public SDK or complete supplier wire-v2 golden bundle.
Consumer fixtures derived from the owner specification must identify that
provenance and must not be described as supplier goldens. This contract alone
does not establish implementation or live acceptance. The catalog GET uses the
same normal app key, with no new credentials; issuer and original human Bearer
handling are unchanged. Goauthy consent,
bodyless credential delivery, user isolation and memory-only model keys remain
unchanged; Grant authorization never substitutes for them.

`providers.json` uses existing credential-free service provider fields:

```json
{
  "default": "minimax",
  "providers": [{
    "id": "minimax",
    "kind": "openai-compatible",
    "api": "chat-completions",
    "base_url": "https://opencode.ai/inference/go/openai/v1",
    "model": "minimax-m3.1-flash"
  }]
}
```

The endpoint/model are the authoritative consumer configuration supplied through
Operable. Goauthy does not validate the model endpoint or model ID: Bee owns this
fixed HTTPS routing policy. The actual external Goauthy provider ID, approved
consent, model availability and account entitlement remain unverified. OpenCode
is an `api_key` provider/connector here, not OAuth2. Do not set `api_key`,
`api_key_env`, a local login store, or a cloud/model secret in either JSON file.

## Request identity and UI routes

Every request carries the exact original `Authorization: Bearer <human-token>`
plus a single `X-Organization-ID`. Legacy operations also require a single
`X-Workspace-ID`. In the new profile, an absent workspace header selects a
context only when exactly one context exists, its resource type is `workspace`,
its resource ID equals its workspace ID, its evidence is `constant` with that
same canonical value and an empty name, and the requested operation has explicit
`forward_headers` excluding `x-workspace-id`. A present workspace header must
still be single and match the configured context. Multiple contexts never use
an implicit fallback, even if only one seems eligible. Use the canonical
Bearer spelling shown here. The token is introspected against the pinned
authority, requiring active status, subject, API audience, future expiry, valid
not-before and scopes. `openid` and matching UserInfo subject prove the human
access-token path. If introspection supplies `iss`, it must match; absent `iss`
uses the pinned introspection authority. Shared authorization also requires
`client_id` equal to the configured consumer. No unsigned header assertion,
editable organization claim or caller-supplied principal establishes identity.

The server maps selectors only into its configured registry, hashes
`(configured issuer, verified sub)` into the user ID, and asks Grant about that
human's registered workspace operation. Unknown or ambiguous selectors, missing
organization, and missing workspace outside the explicit single-context rule
fail closed. Registry presence is not learner permission: the owner must provide
the actual Grant learner/resource relationship and Exposure routing/catalog
registration. Bee forwards the original human Bearer to Grant; this profile
does not assume Exposure mints a replacement identity token.

The token must carry both the Bee API audience and Goauthy connections-resource
audience, plus `openid`, the configured API scopes and
`goauthy.connections.read`; selection/execution also need
`goauthy.connections.use`. The separate Grant credential-hint audience comes
from its registered provider contract. Do not infer it from `aud`, `client_id`,
the provider URL or the model name. Repeated checks bind the same request's
method/path, canonical query, body hash/length and original credential digest.
No bearer or allow-decision cache survives the request.

For discovery, selection and execution, obtain a human token with `aud` containing
both `OWNER_CONFIRMED_BEE_API_AUDIENCE` and
`OWNER_CONFIRMED_CONNECTIONS_RESOURCE_AUDIENCE`, and `scope` containing `openid`,
every configured `required_scopes` entry, `goauthy.connections.read` and
`goauthy.connections.use`. Its `client_id` must equal
`OWNER_CONFIRMED_CONSUMER`. The same registered confidential client may be reused;
Bee provides no token-exchange endpoint. The Grant hint audience is a separate
configuration binding, not an instruction to invent another access-token audience.

| Operation key | UI request | Query/body |
| --- | --- | --- |
| `connection_options` | `GET /v1/connection-options` | No query/body |
| `connection_select` | `POST /v1/connections` | JSON `collection_id`, `connection_id`, `grant_id` |
| `run_execute` | `POST /v1/chat` | JSON shown below |
| `providers_list` | `GET /v1/providers` | No query/body |
| `connections_list` | `GET /v1/connections` | No query/body |
| `connection_revoke` | `DELETE /v1/connections/{connection_id}` | No query/body; local connection revocation |
| `history_read` | `GET /v1/history` | Exactly `workspace_id`, `session_key` |
| `run_read` | `GET /v1/runs/{request_id}` | Exactly `workspace_id` |
| `run_cancel` | `POST /v1/runs/{request_id}/cancel` | Exactly `workspace_id`; no body |

The first Operable integration covers connection discovery, selection and
synchronous execution, with lookup/history enabled only when their pins are
registered. It does not advertise cancellation or streaming. `run_cancel` is an
optional server operation requiring its own registered pin and separate consumer
validation; it cannot guarantee reversal of external effects. There is no SSE or
WebSocket event endpoint.

All routes require the headers above and their enabled operation pin. Query/body
workspace must match the selected server workspace. Status and Compos routes
are not shared operations. Unknown query keys, duplicate selectors/queries,
encoded path variants and unsupported operations fail closed.

For example, run lookup is
`GET /v1/runs/request-1?workspace_id=cypark-default`, and history is
`GET /v1/history?workspace_id=cypark-default&session_key=chat-1`. Use the actual
`workspace_id` query spelling; an owner example's `workspaceId` placeholder is
not a supported Bee query key. Chat and selection POSTs additionally require
`Content-Type: application/json` (valid parameters are preserved and bound).

1. List `/v1/connection-options`; this returns safe owner connection metadata,
   not approval or model keys.
2. Use Goauthy's existing browser handoff and consent flow. The host UI/BFF
   validates its one-use session-bound state and expected selection. Grant allow
   never substitutes for Goauthy consent.
3. POST the approved reference to `/v1/connections`:

   ```json
   {"collection_id":"models","connection_id":"my-connection","grant_id":"approved-grant"}
   ```

   Store the returned Bee `id` for chat. Selection persists an immutable
   organization/user/workspace-scoped connection reference, not a usable key.
   HTTP 201 returns the connection envelope, for example:

   ```json
   {"organization_id":"operable","id":"RETURNED_BEE_CONNECTION_ID","user_id":"VERIFIED_USER_ID","workspace_id":"cypark-default","kind":"provider","state":"ready","generation":1}
   ```
4. POST `/v1/chat` with the same headers:

   ```json
   {
     "workspace_id": "cypark-default",
     "session_key": "chat-1",
     "request_id": "request-1",
     "connection_id": "RETURNED_BEE_CONNECTION_ID",
     "message": "Hello"
   }
   ```

   Chat waits for completion; it is not an asynchronous HTTP 202 submission.
   A successful HTTP 200 response has this shape (generated IDs are placeholders):

   ```json
   {"provider":"minimax","request_id":"request-1","session_id":"SESSION_ID","message_id":"MESSAGE_ID","state":"succeeded","output":{"text":"Hello"}}
   ```

Run lookup returns HTTP 200 with `{"request_id":"request-1","session_id":"","state":"running"}` while
a stored run is active, the stored result after completion, or 404 if no row
exists yet. A concurrent submission may not have created its row when polled.
History returns an array of the latest 200 journal entries in ascending sequence
order, with `sequence`, `request_id`, `kind` and `payload`; it has no cursor and
is not exclusively a chat-message array. Completed identical requests replay their
stored result; conflicting reuse of a request ID returns 409.
The native model path rejects blank text; an empty text response is not a
successful chat example.

### Denial examples

| Request condition | Consumer-visible result |
| --- | --- |
| Missing/invalid/expired human Bearer or required API audience/scope | Authentication fails; HTTP 401 |
| Missing/duplicate/unknown organization; duplicate/mismatched workspace; missing workspace outside the explicit single-context rule; omitted operation pin; Grant denies | Shared authorization fails; HTTP 403 |
| Chat body or lookup/history query workspace differs from selected workspace | HTTP 403; no access to the other workspace |
| Missing/malformed/non-JSON chat or selection Content-Type | HTTP 403 before Grant evaluation or credential delivery |
| Missing connections audience/read/use scope, wrong consumer, revoked or mismatched consent | Connection discovery/selection or execution fails; no model dispatch |

Authentication failures on runtime routes use HTTP 401/403 with
`{"session_id":"","state":"failed","error":"authorization required"}`;
this envelope has no `error_code`. Discovery/selection failures instead use
`{"error":"connection unavailable"}` (401/403 for authentication/authorization,
400 for invalid input, or 503 for other failures). Runtime errors after
authentication use the result envelope and an `error_code`, for example HTTP 403
with `{"session_id":"","state":"failed","error":"authorization required","error_code":"forbidden"}`.
Do not infer rollback from a failed or `uncertain` result.

Execution rechecks identity/Grant and the current Goauthy grant's owner, consumer,
resource, `credential_delivery` mode, generations, revisions, provider, expiry
and revocation. API-key delivery uses the existing bodyless
`POST /auth/v1/connection-grants/{grant_id}/credential`; the delivered key stays
in the model HTTP client's memory. Human/app credentials do not become model
credentials. An allow from either Grant or Goauthy cannot bypass the other.

## Goauthy API-key setup and delivery contract

The owner-supplied public contract is pinned at commit
`908cc698b1f238fb12ce83a47e2656d8333e2523`:
[connection handoffs](https://github.com/mrchypark/goauthy/blob/908cc698b1f238fb12ce83a47e2656d8333e2523/docs/connection-use-handoffs.md)
and [connection grants](https://github.com/mrchypark/goauthy/blob/908cc698b1f238fb12ce83a47e2656d8333e2523/docs/connection-use-grants.md#L174-L212).
No upstream source change or new internal OAuth client is required.

- Account key setup uses session authentication and CSRF protection with
  `PUT /auth/v1/account/connections/{collection}/{connection}/api-key`. The
  payload contains `api_key`, initial `version: 0` and the actual
  `connector_digest`. The key is submitted to Goauthy, never to Bee config or
  persisted by Bee. Do not fabricate a digest or repeat version zero as a
  rotation strategy.
- Explicit runtime `credential_delivery` handoff consent is mandatory. Creating
  the handoff needs a human Bearer with `goauthy.connections.write` and the exact
  connections-resource audience. An uploaded key alone grants no consumer access.
- Runtime credential delivery is an empty-body POST using the existing
  confidential consumer's user Bearer, requiring `goauthy.connections.use` and
  the exact `GOAUTHY_CONNECTIONS_RESOURCE` audience. It is not a client-credentials
  token or Basic-auth key-delivery request. Bee also retains its existing
  connections-read, identity, client, audience and consent checks.
- `goauthy.read` alone is insufficient. A GET-only connector proxy does not
  prohibit the raw-delivery consumer from making the model POST. Bee's fixed
  route and memory-only delivered credential govern that call.

The discovered grant's nonempty `connector_digest` already selects Bee's API-key
delivery branch; no OAuth2 adaptation is needed for this OpenCode connection.

Users may share an authorized workspace context while connection ownership and
runtime data remain user-scoped. This does not grant access to another user's
model connection or promise collaborative cross-user history.

## Verification boundary and deployment prerequisites

The source includes local protocol fixtures for repeated authorization, workspace
isolation, policy denial, missing-operation denial and independent consent
revocation. They are not actual Grant/learner/Exposure registration evidence;
the golden wire artifact is not an owner acceptance result. Release CI and
published-image smoke results must be checked at the selected revision; protocol
fixtures do not establish live Grant, Goauthy or model-account availability.

To exercise the real flow, owners still need to supply the actual catalog and
capability pins, learner/resource registration, app-key provisioning and
credential-hint audience; confirm actual token issuance for the supplied
issuer/client/API/resource audiences and required scopes; supply catalog pins
matching the selected explicit-header or legacy contract; and supply the approved Goauthy
provider/connector/delivery grant for the configured model endpoint. The owner
has confirmed original-human-Bearer forwarding as the source contract; its live
deployment has not been verified here.
No new freshness API, external approval API or client-registration feature is
required by this implementation.
