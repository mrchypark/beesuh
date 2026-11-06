# Rhiza v0.23.1 consumer assessment

Assessed 2026-11-06 against Beesuh main `64db9ef2ee9b3ec4f558611fdcdc5e9525e46bda` (previous pin v0.23.0). **Artifact gate: PASS.** Candidate CI is pending at preparation; exact acceptance belongs to the accompanying PR checks/results, not this document's deployment state.

## Artifact provenance

- Module: `github.com/mrchypark/rhiza@v0.23.1`, Go `1.27.3`.
- Annotated tag `13e542ea073c856e3de6774eb46e0f0143090ae3` resolves to source `2262531f266f2638eb21baea6f65de45465ffe90`.
- Published `2026-11-06T07:33:04Z` (16:33:04 KST).
- Module checksum: `h1:2RUTV9nQUPwUVaDp3t2o7JEX1A49VITK3m0k1gbLznk=`.
- go.mod checksum: `h1:Cmof+NGVUgCu4+HTenSu2h5SKW88w5KxBV1S9eJNevM=` (unchanged).
- Proxy ZIP SHA256: `7e90bebd5c8887b5b7a3f68d9302f60d15842c57cd6ebc34d17713b2299be340` (distinct from Go's module checksum).

Required public Go source/module artifacts are available and match the independent gate: proxy info/mod/ZIP and sumdb agree, including independent module/go.mod hashing and the ZIP SHA256 above. No missing artifact, mismatch, or redelivery request remains. Beesuh consumes Go source/transitive modules for CGo-free linux/amd64 Docker and darwin/arm64 cross-builds. Rust crates, native FFI archives, and operator images are not required.

Primary sources: [release](https://github.com/mrchypark/rhiza/releases/tag/v0.23.1), [tag](https://api.github.com/repos/mrchypark/rhiza/git/tags/13e542ea073c856e3de6774eb46e0f0143090ae3), [proxy info](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.1.info), [go.mod](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.1.mod), [ZIP](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.1.zip), [sumdb](https://sum.golang.org/lookup/github.com/mrchypark/rhiza@v0.23.1).

## Compatibility and candidate checks

The [published comparison](https://github.com/mrchypark/rhiza/compare/v0.23.0...v0.23.1) changes benchmarks, tests, validation/docs, and Rust versions; production Go engine APIs and go.mod/go.sum are unchanged. This is a validation-reliability patch, not an engine performance improvement. Beesuh needs no production call-site changes for Open/Config, Query, Execute, expected-row checks, migrations 1–14, or Close. The PR270 Query-cause fix remains in the unchanged engine; [v0.23.0 evidence](rhiza-0.23.0-assessment.md) remains historical, not candidate acceptance.

Remote CI is pending for normal/race regressions, vet, Query context and SQLite interruption identity with HTTP controls, transaction behavior, linux/amd64 Docker smoke, and CGo-free darwin/arm64 build. The v0.22.0 Query negative oracle remains separate from retained-data compatibility. The cross-version helper seeds run/history/session data with v0.23.0 and checks retained-data SIGKILL/reopen, replay without duplicate model dispatch, and conversation continuation under v0.23.1. This is not cross-version validation of connection/cancellation/fence records; those features have separate same-version regressions.

Beesuh retains its loopback single-node peer with Local:false and no object archive. No NoPVC/object DR, operator, managed profile, performance non-regression, live deployment/cloud, or downgrade qualification is added. No local Go commands, builds, tests, module downloads, shared-cache writes, or Docker operations were performed for this integration.
