# Rhiza v0.27.5 consumer assessment

Beesuh main `5fc58f92e059ca1ad71c2ccc334518811c35bc49` uses Rhiza v0.23.3. The [v0.27.5 release](https://github.com/mrchypark/rhiza/releases/tag/v0.27.5) resolves to `97bd7314ed8aa22581d848d46ae96a5f12743575`; the Go module checksum is `h1:4rKdB7c3yqQ82phXejg3nCX458RVRc6jtcgN3uLvGjc=`. Its module manifest is unchanged from v0.27.4 and requires Go 1.27.4 and LatticeDB Go v0.13.0. The changes from v0.27.4 concern release and Rust SDK packaging, not Beesuh's Go SDK usage.

Beesuh opens one embedded loopback voter with no object-store configuration. CI seeds run, history and session data with v0.23.3, kills the seed process and reopens its retained directory with v0.27.5; it checks result replay without a duplicate model call and continuation with recovered history. See the PR checks for actual results. This does not qualify other cross-version records, downgrade, archive recovery or managed HA.
