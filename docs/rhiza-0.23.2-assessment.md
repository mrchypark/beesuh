# Rhiza v0.23.2 consumer assessment

Assessed against Beesuh v0.6.0 source `279f2a8`, whose Rhiza pin is v0.23.0.
**Artifact gate: PASS. Candidate CI: pending.** This is a v0.23.0 → v0.23.2
candidate assessment, not merge, release or live-deployment acceptance. The
separate v0.23.1 candidate in PR #10 is not part of this assessment.

## Required artifacts and provenance

- Module: `github.com/mrchypark/rhiza@v0.23.2`, Go `1.27.3`.
- Annotated tag `d71ccc204c7a310cf3668bb435ea3b192b3439c3` resolves to source
  `2dc60add1c4cf99a9ff1050bfc7ad586f29ae7e1`.
- Published `2026-11-06 20:02:16 KST` (`2026-11-06T11:02:16Z`).
- Module checksum: `h1:qTHD8YZ7jBkxxPCowTPIxGn7s+KmxpM9ZJOuqE1WI8Y=`.
- go.mod checksum: `h1:Cmof+NGVUgCu4+HTenSu2h5SKW88w5KxBV1S9eJNevM=`.
- Proxy ZIP SHA256: `51e4cbd3aecbcd6bda3d34b7306213b47f71f557885e706a862e06f906935c34`.
- go.mod SHA256: `8628e0fddc16a01c8b6057cd34a351d70cc3d4ba865942d61990afddd3801a60`.

Proxy info/mod/ZIP identify the exact source; the archived go.mod matches the
standalone module file. Independent hashing of all 4,915 module files agrees
with sumdb. The module file is unchanged from v0.23.0/v0.23.1. All required
CGo-free Go source/module artifacts are present and match; no redelivery is
needed. Beesuh does not require FFI archives or operator images.

Primary sources: [release](https://github.com/mrchypark/rhiza/releases/tag/v0.23.2),
[annotated tag](https://api.github.com/repos/mrchypark/rhiza/git/tags/d71ccc204c7a310cf3668bb435ea3b192b3439c3),
[proxy info](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.2.info),
[go.mod](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.2.mod),
[source ZIP](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.2.zip),
[sumdb](https://sum.golang.org/lookup/github.com/mrchypark/rhiza@v0.23.2).

## Compatibility and pending verification

The changes include operational documentation, regressions, Rust qualification
and a test hook disabled by default; this is not a documentation-only release.
No production Go SDK call-site adaptation is required by the reported consumed
contract. The unchanged root module requirements do not establish runtime
acceptance. Existing Grant wire-v2 authorization, Goauthy consent, user isolation
and memory-only model-key handling remain unchanged.

Pending candidate CI covers normal/race tests, vet, CGo-free darwin/arm64 build,
linux/amd64 Docker build/smoke, the v0.22.0 Query negative oracle, and retained
v0.23.0-seeded run/history/session data reopened under v0.23.2 after SIGKILL.
The latter checks result replay without duplicate model dispatch and conversation
continuation, not every pre-existing connection/cancellation/fence record.
Historical v0.23.0 qualification and any v0.23.1 results are not current candidate
acceptance.

The retained-DataDir test does not cover whole-emptyDir loss or remote object
recovery. Beesuh still has no object archive wired into its server configuration.
No No-PVC DR, performance, HA, production-cloud or downgrade claim is made.
Preserve original data and binaries; verify disposable copies before upgrades.
No local builds, tests, module downloads or cache writes were performed for this
documentation update.
