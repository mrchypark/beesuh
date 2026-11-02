package grantpolicy

// Integration contract:
//
// The server constructs ActualSnapshot from the exact outgoing business request,
// selects immutable reviewed capability metadata using its fixed operation
// resolver, and calls Check immediately
// before sending that same request. This package never sends business requests.
// BodyDigest is Digest(body bytes), BodyLength is their byte count. The caller
// supplies the actual canonical path/query and the digest of the exact forwarded
// header preimage. Only empty query/header canonicalization has a delivered
// golden vector; nonempty contracts require independent owner-reviewed vectors.
// Query spaces use %20, never +. Actual snapshots vary per request; Grant's PDP
// enforces the registered capability method/path contract. A client is reusable.
// CredentialDigest binds the exact original Authorization value, including the
// Bearer prefix. Subject must be verified for that credential. Audience must be
// an explicit registered and verified provider hint, never a guessed aud/client_id.
// OrganizationID is server context, never a forwarded browser assertion.
//
// A nil Check result is the sole allow signal. ErrDenied covers malformed,
// unknown, denied, unavailable, stale, mismatched and transport-failure cases.
// Callers must not retry through an operator bypass or treat this as Goauthy
// consent. Grant validates registered capability metadata; this is not a Bee
// administrator permission grant and introduces no approval API.
//
// Static reviewed binding mode deliberately does not fetch or compile catalogs.
// CatalogDigest and ContractDigest are configured pins, not hashes recomputed
// from arbitrary DTO serialization. The included fixture proves exact golden
// preimage hashes and wire checks only, not real authentication, live Grant
// authorization, or the existence of an owner-side freshness proof index.
//
// HTTPS uses normal certificate validation. AllowPrivateHTTP explicitly opts in
// to loopback/private IP literals or localhost for existing local deployments.
// Redirects, environment proxies and secret-bearing error details are disabled.
// AppKeyFile is read once at construction; recreate Client to rotate the key.
// No bearer token or decision is cached and the package performs no logging.
