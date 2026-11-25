# Rhiza v0.27.4 consumer assessment

Beesuh main `5fc58f92e059ca1ad71c2ccc334518811c35bc49` uses Rhiza v0.23.3. The [v0.27.4 release](https://github.com/mrchypark/rhiza/releases/tag/v0.27.4) resolves to `94e9ef3f29ddf52b3d543133bae3e3d8ba57558c`; the Go module checksum is `h1:Ahu7+37UsIPRWIIb8kqkPnLeM42SS/1SG7C3ewFuJHw=`. It requires Go 1.27.4 and LatticeDB Go v0.13.0.

Beesuh opens one embedded loopback voter with no object-store configuration. Rhiza's new optional HTTP service client is not used. CI seeds run, history and session data with v0.23.3, kills the seed process and reopens its retained directory with v0.27.4; it checks result replay without a duplicate model call and continuation with recovered history. See the PR checks for actual results.

This fixture does not establish recovery after loss of the local volume, no-PVC object-store recovery, managed HA, Kubernetes/CSI behavior, downgrade, or recovery of every application record type. Beesuh does not expose object-store configuration; using the SDK's object-store features separately would not establish Beesuh disaster recovery.
