# Rhiza v0.23.0 consumer assessment

Assessed 2026-11-06 against Beesuh main `99a73f3681bae3364521cd4345cfcc464695858c` (previous pin v0.22.0). **Artifact gate: PASS.** Exact candidate build/runtime acceptance is recorded in the accompanying PR checks/results. This document records provenance and scope rather than deployment state. The candidate includes dependency pin/checksum updates, this assessment, regression tests, and CI changes.

## Required artifacts and provenance

- Module: `github.com/mrchypark/rhiza@v0.23.0`, Go `1.27.3`.
- Annotated tag `6f774d90ee535a6f47ac5c0cfe5086aa5467fb69` resolves to commit `f050d6e30f57718736c8c4fdb5edef777183bc05`; proxy origin matches. Release published `2026-11-05T22:04:27Z`.
- Module checksum: `h1:RFLhLytoi2Q84MD4WkA9Z8T7wvUp7LPj4nxIMFBSaNw=`.
- go.mod checksum: `h1:Cmof+NGVUgCu4+HTenSu2h5SKW88w5KxBV1S9eJNevM=`.
- Proxy ZIP SHA256: `5d8fff87174fe688b3db207c659583a301a062c623c4d3c46a186dfe9f61e19f` (archive hash, distinct from Go's module checksum).

The independently completed gate verified module checksums against sumdb. Retained local evidence is `/tmp/beesuh-rhiza023-gate.iyiFQJet`: release/tag/proxy metadata, source ZIP, sumdb record, complete 4,908-member `zip-batch-preimage`, and `mod-dirhash-preimage`. The complete ZIP preimage and mod preimage reproduce the two checksums above; shorter exploratory preimages are not acceptance evidence. No required artifact is missing or mismatched; no redelivery is needed.

Beesuh consumes Go source and its transitive modules for a `CGO_ENABLED=0` linux/amd64 Docker build and darwin/arm64 cross-build. Rhiza's go.mod is byte-identical to v0.22.0, so no additional module requirements are introduced. Rust crates, native FFI archives, and operator images are not required by this consumer path.

Primary sources: [release](https://github.com/mrchypark/rhiza/releases/tag/v0.23.0), [annotated tag](https://api.github.com/repos/mrchypark/rhiza/git/tags/6f774d90ee535a6f47ac5c0cfe5086aa5467fb69), [proxy info](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.0.info), [go.mod](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.0.mod), [source ZIP](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.0.zip), [sumdb](https://sum.golang.org/lookup/github.com/mrchypark/rhiza@v0.23.0).

## Consumed contract and prior hold

No source-level changes are required for Beesuh's `Open`/`Config`, `Query` with linearizable consistency, transactional `Execute`, `SQLStatement.ExpectedRowsAffected`, `MutationCommitted`, contiguous migrations 1–14, or `Close`. Public root SDK changes from v0.22.0 are reconfiguration documentation; internal execution and recovery changes still require application regression checks. See the [version comparison](https://github.com/mrchypark/rhiza/compare/v0.22.0...v0.23.0) and [tagged SDK](https://github.com/mrchypark/rhiza/blob/v0.23.0/rhiza.go).

The prior uncommitted `docs/rhiza-0.22.1-assessment.md` in the original Beesuh checkout recorded loss of Query context identity in both v0.22.0 and v0.22.1. [PR #270](https://github.com/mrchypark/rhiza/pull/270), merged as `647dfc2466f359a31c64ec661e234fc2314f1189`, is an [ancestor of v0.23.0](https://github.com/mrchypark/rhiza/compare/647dfc2466f359a31c64ec661e234fc2314f1189...v0.23.0). The tagged [materializer](https://github.com/mrchypark/rhiza/blob/v0.23.0/pkg/materializer/materializer.go#L2532) joins actual SQLite interruption with the completed call context, and the [Query boundary](https://github.com/mrchypark/rhiza/blob/v0.23.0/pkg/network/server.go#L1681) preserves the driver cause with `%w`. Published [regression tests](https://github.com/mrchypark/rhiza/blob/v0.23.0/pkg/network/query_cancellation_cause_test.go) cover deadline/cancel identity and ordinary SQL errors. Fix inclusion is confirmed; the earlier Beesuh full-suite failure is not retroactively cleared.

Beesuh opens a loopback single-node peer (`Local` remains false), not managed HA. Managed recovery profiles 7/8 are rejected in v0.23.0 in favor of 9/10; Beesuh does not consume those profiles. The release's Local-mode and managed-HA qualifications do not establish Beesuh's runtime acceptance.

## Candidate verification and limits

Remote candidate CI must establish:

- Real interrupted Query errors preserve context and driver identity, reach Beesuh's HTTP 504 mapping, and leave subsequent queries healthy; invalid SQL remains an ordinary error.
- Transaction rollback and expected-row rejection remain atomic; ambiguous mutation outcomes retain the existing uncertainty handling without automatic resubmission.
- The cross-version helper seeds run/history/session data under v0.22.0, then checks replay without duplicate model dispatch and conversation continuation after SIGKILL and v0.23.0 reopen. It does not seed or verify pre-existing connection, cancellation, or recovery-fence records across versions.
- The full normal/race suite covers connection isolation, cancellation, and recovery-fence regressions within the candidate version separately. The linux/amd64 Docker smoke and darwin/arm64 cross-build must also pass.

These checks cover retained local data, not remote-archive recovery or empty-directory disaster recovery. No archive, performance non-regression, production-cloud parity, or downgrade compatibility claim is made. Test old data through disposable copies; reverse-version reopen remains unqualified. No local builds, tests, or shared-cache writes were performed; a read-only `go version` check is outside those restrictions.
