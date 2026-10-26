# Actual Goauthy / Beesuh Docker acceptance fixture

From the Beesuh checkout, build its Dockerfile image, then run:

```sh
scripts/goauthy-live.sh -image beesuh:local -goauthy /absolute/path/to/goauthy
```

Requires Go and a working Docker engine. The Goauthy checkout is read-only. The
launcher caches its Go helper outside the repository. It builds the actual
Goauthy standalone Linux executable and records its SHA256 and checkout state;
this tests that local working tree, not an official Goauthy release image.
The Beesuh image is pinned to its inspected image ID throughout the run.

The helper creates disposable containers/network, TLS certificates, a browser
user, a confidential managed consumer with dual audiences, a provider and a
connection. Authorization uses real code + S256 PKCE, introspection, UserInfo,
and Goauthy's existing HTML handoff approval with session-bound CSRF. Only the
model response is synthetic. All model keys and issued human tokens remain in
helper/request memory; the original key is registered in Goauthy's real encrypted
vault via its existing API. The separate OAuth client secret is copied to Beesuh
with UID/GID 10001 and mode 0600. The image's USER is preserved.

A transparent TLS proxy forwards every issuer response to the actual Goauthy
binary, recording route counts only. Assertions cover no delivery before
execution, unapproved selection denial, approved execution, Beesuh restart with
selected references and a new human token, and no further delivery after consent
revocation. The model verifies that only the provider key reaches its auth header
and that neither key nor human token appears in its request body. Environment,
configuration, logs, responses and persisted Beesuh data are scanned for fixture
credentials. No paid provider, production account or live cloud service is used.

The model runs in the auth-proxy helper container under the reserved fixture-only
`/__fixture_model/` path on its existing TLS listener. A separate random control
secret protects the fixture-only control route; the host sends the model key and
token leak filters over verified TLS into helper memory. Filters are updated on
fresh token issuance. Existing Goauthy protocol routes remain transparent.
No host model listener or `host.docker.internal` mapping is required. TLS reachability
is checked by the same cross-compiled Go helper, running as UID 10001 with
`SSL_CERT_FILE`; no extra image tools or certificate verification bypass are used.
Host bind mounts are not required; fixture/config files use `docker cp`.

Each run has a twelve-minute deadline. Cleanup has independent bounded Docker
calls and verifies removal by the actual Docker create IDs, independent run-label
and exact-name-prefix inventories, ID inspection, and temporary-directory absence.
Cleanup failure makes the command fail and lists unverified owned IDs.
Sanitized failure evidence is retained outside the removed fixture directory;
the output prints its path. The helper build cache is intentionally retained.
