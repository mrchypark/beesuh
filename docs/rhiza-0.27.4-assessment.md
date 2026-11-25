# Rhiza v0.27.4 consumer assessment

Beesuh pins [Rhiza v0.27.4](https://github.com/mrchypark/rhiza/releases/tag/v0.27.4), commit `94e9ef3f29ddf52b3d543133bae3e3d8ba57558c`. Its [module manifest](https://proxy.golang.org/github.com/mrchypark/rhiza/@v/v0.27.4.mod) requires Go 1.27.4. The Docker build and CI use that version. The dependency update also advances LatticeDB Go and OpenTelemetry dependencies; see `go.mod` and `go.sum` for the resolved versions.

The CI recovery fixture builds a seed binary with Rhiza v0.23.3, writes run, journal and session data to a disposable directory, kills the seed process, and reopens the directory with v0.27.4. It checks replay without another model dispatch and continuation with recovered history. Consult the candidate's CI results for the outcome.

This fixture does not qualify cross-version connection, cancellation, async-task, or recovery-fence records, downgrade, remote archive or empty-directory recovery, or managed HA. Preserve the old data and binary and validate a disposable copy before upgrading. See the [previous assessment](rhiza-0.23.3-assessment.md) for historical results.
