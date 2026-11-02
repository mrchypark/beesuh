# Shared-workspace HTTP service

This source profile connects an OIDC-verified human request, server-owned
organization/workspace registry, Grant authorization and an independently
approved Goauthy `credential_delivery` connection. The intended model is
`minimax-m3.1-flash` through the native `openai-compatible` HTTP provider at
`https://opencode.ai/inference/go/openai/v1`. It does not
start an OpenCode server. This document describes source behavior, not a verified
live deployment or a published image containing the shared profile.

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

The candidate Exposure/catalog forwarded-header contract contains exactly
`x-organization-id` and `x-workspace-id`. Bee hashes these UTF-8 bytes, using the
validated server binding, lowercase sorted names, no spaces, and a trailing LF:

```text
x-organization-id:operable
x-workspace-id:cypark-default
```

SHA-256 for that exact preimage is
`sha256:818f1c996cee0ceea1860229107f8d7ccc367853d0e5c6d822962790a9705363`.
Authorization is excluded from this header digest and remains bound separately
by the original credential digest. Catalog forwarded headers must include both
selectors. Content-Type forwarding remains an owner contract question: the
owner's example includes `content-type` as a third forwarded header, while the
two-selector candidate above awaits confirmation that POST Content-Type is
preserved. Do not change the digest or claim compatibility until that is settled.

Exposure owner source `20683606` confirms only `pathSegment`, `queryScalar`,
`constant` and `jsonPointer` resource extractors; there is no header extractor.
The current one-workspace baseline therefore uses type `workspace`, resource ID
`cypark-default`, and existing constant evidence:

```json
{"kind":"constant","name":"","canonicalValue":"cypark-default"}
```

Use that resource/evidence for all nine capabilities in the owner catalog.
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
          "action_id": "OWNER_CONNECTION_OPTIONS_ACTION"
        },
        "connection_select": {
          "capability_id": "OWNER_CONNECTION_SELECT_CAPABILITY",
          "capability_version": 1,
          "contract_digest": "sha256:OWNER_SUPPLIED_64_LOWERCASE_HEX",
          "action_id": "OWNER_CONNECTION_SELECT_ACTION"
        },
        "run_execute": {
          "capability_id": "OWNER_RUN_EXECUTE_CAPABILITY",
          "capability_version": 1,
          "contract_digest": "sha256:OWNER_SUPPLIED_64_LOWERCASE_HEX",
          "action_id": "OWNER_RUN_EXECUTE_ACTION"
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
plus single `X-Organization-ID` and `X-Workspace-ID` selectors. Use the canonical
Bearer spelling shown here. The token is introspected against the pinned
authority, requiring active status, subject, API audience, future expiry, valid
not-before and scopes. `openid` and matching UserInfo subject prove the human
access-token path. If introspection supplies `iss`, it must match; absent `iss`
uses the pinned introspection authority. Shared authorization also requires
`client_id` equal to the configured consumer. No unsigned header assertion,
editable organization claim or caller-supplied principal establishes identity.

The server maps selectors only into its configured registry, hashes
`(configured issuer, verified sub)` into the user ID, and asks Grant about that
human's registered workspace operation. Unknown, missing or ambiguous selectors
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

All routes require the headers above and their enabled operation pin. Query/body
workspace must match the selected server workspace. Status and Compos routes
are not shared operations. Unknown query keys, duplicate selectors/queries,
encoded path variants and unsupported operations fail closed.

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

## Current functional evidence and blockers

The source includes local protocol fixtures for repeated authorization, workspace
isolation, policy denial, missing-operation denial and independent consent
revocation. They are not actual Grant/learner/Exposure registration evidence;
the golden wire artifact is not an owner acceptance result. No host Go command,
test, build, image publication or live model call was run for this transplant.

To exercise the real flow, owners still need to supply the actual catalog and
capability pins, learner/resource registration, app-key provisioning and
credential-hint audience; confirm actual token issuance for the supplied
issuer/client/API/resource audiences and required scopes; settle Content-Type
forwarding against the two-selector candidate; and supply the approved Goauthy
provider/connector/delivery grant for the configured model endpoint. The owner
has confirmed original-human-Bearer forwarding as the source contract; its live
deployment has not been verified here.
No new freshness API, external approval API or client-registration feature is
required by this implementation.
