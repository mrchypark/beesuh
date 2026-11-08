# Rhiza v0.23.3 consumer assessment

Baseline: Beesuh main `279f2a87219e362af6c4810e863b96224f6fe669`, pinned to Rhiza v0.23.0.

**Artifact gate: PASS.** The annotated [v0.23.3 release tag](https://github.com/mrchypark/rhiza/releases/tag/v0.23.3) resolves to `fd323dbb37ad86c614eb0983c406adbccd16acba`, matching [Go proxy metadata](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.3.info). All 4,927 files in the [module ZIP](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.3.zip) match the tagged source. Go proxy rules exclude three nested modules, none of which Beesuh uses. The [module manifest](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.23.3.mod) is byte-identical to v0.23.0 and still requires Go 1.27.3.

- Module: `h1:tAVJOAJgRksTOy+mYszUHkOUvnRcSdNc44ipsI9ZtrU=`
- go.mod: `h1:Cmof+NGVUgCu4+HTenSu2h5SKW88w5KxBV1S9eJNevM=`
- ZIP SHA256: `5545299308ac144e94fc52d33f33fbde3386254802571e96a745a252a1304d1a`
- go.mod SHA256: `8628e0fddc16a01c8b6057cd34a351d70cc3d4ba865942d61990afddd3801a60`

The independently recomputed Go checksums match [sumdb](https://sum.golang.org/lookup/github.com/mrchypark/rhiza@v0.23.3).

The [comparison with v0.23.0](https://github.com/mrchypark/rhiza/compare/v0.23.0...v0.23.3) includes validation and documentation changes and a managed Graph startup fix during first checkpoint publication. Beesuh opens a single loopback voter with `Local: false` and no `ObjStore` or managed Graph configuration. The root Go SDK and module requirements are unchanged, so Beesuh needs no runtime code changes.

Use the accompanying PR checks at the exact candidate head as the source of runtime acceptance. CI keeps v0.22.0 as the negative control for Query losing the context cause in both cancellation modes. A separate helper seeds run, history, and session data with v0.23.0. After SIGKILL, v0.23.3 reopens the same disposable DataDir to check replay without duplicate model dispatch and continuation with recovered history.

The upgrade check covers retained local data in that fixture. It does not qualify cross-version connection, cancellation, or recovery-fence records, downgrade, archive or empty-directory recovery, or managed HA. See the [v0.23.0 assessment](rhiza-0.23.0-assessment.md) for the historical v0.22.0 → v0.23.0 results.
