# GoAuthy local pilot

This is a test-only compatibility harness. It starts a disposable Beesuh HTTP
handler, native local model fixture, and Rhiza directory. It does not register a
production GoAuthy client, implement browser login, change another service, or
send credentials to a provider.

The example loopback address below is not an always-running service.
GoAuthy must register an issuing/introspection client before the live test can
run. The required resource is `https://beesuh.local.test`; required access-token
scope is `openid`. Beesuh introspects every request, requires active token,
exact resource audience, `openid`, valid `exp`/`nbf`, and matching UserInfo
subject. Its user ID is a stable hash of `beesuh:oidc-user:v1`, issuer, and
subject; a caller-supplied identity is not used as the principal ID.

The service supplies fixed membership fixtures—there is no auto-enrollment:

```json
[{"issuer":"http://localhost:17889","subject":"goauthy-subject","workspace_id":"workspace-a"}]
```

All `*_FILE` values must be 0600, regular, non-symlink files, each under 64 KiB.
The membership JSON must be compacted onto one line (a final newline is allowed).
Do not place credentials in command arguments, logs, or this document.

```sh
BEESUH_E2E_GOAUTHY=1 \
BEESUH_E2E_GOAUTHY_ISSUER=http://localhost:17889 \
BEESUH_E2E_GOAUTHY_RESOURCE=https://beesuh.local.test \
BEESUH_E2E_GOAUTHY_CLIENT_ID=beesuh-local-e2e \
BEESUH_E2E_GOAUTHY_CLIENT_SECRET_FILE=/private/pilot/client-secret \
BEESUH_E2E_GOAUTHY_TOKEN_FILE=/private/pilot/access-token \
BEESUH_E2E_GOAUTHY_MEMBERSHIPS_FILE=/private/pilot/memberships.json \
go test ./httpapi -run '^TestGoAuthyLiveE2E$' -count=1 -v
```

Optional `BEESUH_E2E_GOAUTHY_{WRONG_AUD,WRONG_SCOPE,EXPIRED,REVOKED}_TOKEN_FILE`
fixtures must each represent the named condition and differ from the positive
token. Omitted negative cases are reported as `SKIP / NOT VERIFIED`. The live
test checks positive direct chat, stable repeat identity, workspace refusal, and
each supplied negative token. A fake-issuer test verifies the harness itself;
it is not live GoAuthy evidence.

## Actual standard IdP pilot — 2026-09-07

The GoAuthy task reported its disposable runner exited 0. The local log
`/tmp/goauthy-beesuh-identity-live-20260907.log` confirms
`TestBeesuhConsumerFixture` PASS (1.29s) and `TestGoAuthyLiveE2E` PASS
(0.67s, Beesuh package 1.447s). It used the current uncommitted Beesuh source.
The issuer owner provisioned a dedicated RP and separate introspection caller;
credentials and compact 0600 membership fixture were injected through FILE paths.

Verified by the live harness: positive chat, stable repeated identity, two model
fixture calls, non-member workspace 403, and WRONG_AUD / WRONG_SCOPE / REVOKED
rejection with 401. EXPIRED alone was explicitly SKIP / NOT VERIFIED. The issuer
owner reported cleanup of the disposable issuer, DB, and credential files; the
example issuer address is not an always-running service.

This verifies standard identity consumption only. It does not verify a real
external provider account, Codex login/refresh, or GoAuthy's credential-profile
extension and authorized token delivery.

### Final common fixture candidate

After confirming Beesuh's required `openid` scope,
the issuer owner reran the final fixture candidate. The log
`/tmp/goauthy-beesuh-identity-live-final-20260907.log` confirms fixture PASS
(1.25s) and Beesuh consumer PASS (0.71s, package 1.307s); EXPIRED remained SKIP.
The owner reported runner exit 0 and cleanup of all temporary credentials and
issuer resources. Reproduction is documented in GoAuthy's
`docs/consumer-integrations.md`. The verified identity-only scope and external
credential-profile limitations above are unchanged.
