# Single-node Kubernetes deployment

This profile runs one Beesuh process with embedded Rhiza v0.21.1, pinned to
[commit 66e2416b52e3d64aa86134b7b0c8b70c5ace88be](https://github.com/mrchypark/rhiza/tree/66e2416b52e3d64aa86134b7b0c8b70c5ace88be).
Both containers specify the release image `ghcr.io/mrchypark/beesuh:v0.4.0`
with `imagePullPolicy: IfNotPresent`. Verify that this version's CI and image
publication have completed successfully before deployment. The published
`ghcr.io/mrchypark/beesuh:v0.3.0` lacks both the new
`-storage-init` flag and Rhiza v0.21.1 and cannot run this profile's init command.
There is no separate Rhiza server, Operator, or RBAC resource. Keep `replicas: 1`;
this is not a Rhiza cluster or an HA deployment. The profile targets Linux/amd64.

## Prerequisites

- A cluster supporting StatefulSet PVC retention and `ReadWriteOncePod`, with a
  default StorageClass backed by a compatible CSI driver. No StorageClass is
  selected explicitly. The dedicated 10Gi PVC must support UID/GID 10001 and
  `fsGroup: 10001`; `OnRootMismatch` governs kubelet ownership handling where
  applicable (CSI drivers may handle mount groups themselves).
- An externally supplied ConfigMap named `beesuh-config` in the target namespace,
  with keys `service.json` and `providers.json`. Follow the
  [HTTP authentication configuration](../../docs/http-service-adapter.md) using
  real issuer endpoints and credential-free provider routing. Set the service
  field `client_secret_file` to `/run/beesuh-client-secret`.
- An existing Secret named `beesuh-oauth-client` with key `client-secret`, holding
  the OAuth confidential client's secret, readable by UID/GID 10001. No human
  tokens or model credentials belong in these resources. The file uses a read-only
  `subPath` mount: restart the pod after rotating this Secret. Restart after
  changing configuration as well; Beesuh loads configuration at startup.

## Local initialization and reopen validation

The init container executes the application-owned command directly, as UID 10001:

```sh
/usr/local/bin/beesuh -storage-init -data-dir /data/beesuh
```

It acquires the same `runtime.lock` as the main runtime, calls SDK `rhiza.Open`
with the existing application configuration, then closes the DB and releases the
lock. Open or Close errors fail init and prevent the main container from starting.
It needs no provider, model, or authentication configuration and starts no HTTP
listener. Directory creation is handled by the storage open path; no shell or
separate mkdir container is needed. Existing incompatible volume permissions
require operator correction.

Init and main use the same image, UID/GID, mounted PVC, application data root
`/data/beesuh`, and Rhiza DataDir `/data/beesuh/rhiza`. Both use NodeID `beesuh`,
PeerAddr `127.0.0.1:0`, and the same default embedded single-voter `Local: false`
configuration without `LocalArchive` or an object store. The main process opens
the same storage again before serving requests. This is local initialization and
reopen validation on the retained PVC and may recover its durable local WAL;
it does not recover a lost volume or complete uncertain application
work. Use a fresh dedicated PVC initially and retain that PVC on restarts; do not
reuse another database's directory.

This sequence follows the [embedded lifecycle contract](https://github.com/mrchypark/rhiza/blob/66e2416b52e3d64aa86134b7b0c8b70c5ace88be/docs/embedded-lifecycle.md#L9-L31).
The separate [official Local archive init guide](https://github.com/mrchypark/rhiza/blob/66e2416b52e3d64aa86134b7b0c8b70c5ace88be/docs/local-init-container-recovery.md)
documents a real app-owned SDK Open/Close example specifically for `Local: true`
with an object store and `BeforeAck`; allowing a PVC does not make it a universal
no-archive recovery contract. That remote archive example applies only
if Beesuh later configures that feature consistently in both modes, including
storage identity and credentials. That example uses `Local: true` and
`BeforeAck`; its archive durability and `Async` loss-window conditions do not
apply to this profile, which has no archive. No object storage or no-PVC mode is
introduced here.

The guide does not qualify Kubernetes deployment, fencing, or production cloud
providers. The [v0.21.1 release disclosure](https://github.com/mrchypark/rhiza/releases/tag/v0.21.1)
also reports an unresolved S3 `TestEmbeddedObjectStorageCheckpointAndTailTotalLoss`
failure, `Local maintenance control changed during read`
([upstream job](https://github.com/mrchypark/rhiza/actions/runs/40697711614/job/122848820051)).
That archive-path failure is outside Beesuh's current no-archive configuration;
it establishes neither a Beesuh reproduction nor remote recovery guarantees.

PVCs are retained on scale-down and StatefulSet deletion. Retention is not a
backup; actual storage preservation also depends on the PV reclaim policy.
Manual PVC deletion remains destructive. Do not force-delete a pod or start a
second writer while the original process may still be using its data. One replica
and RWOP are not fencing: establish that the previous writer has stopped.

## Render and deploy

The release workflow validates the version tag and tests its resolved commit
before publishing the Linux/amd64 image. It publishes only the requested version
tag, with no `latest` or commit-SHA image tag. Wait for the v0.4.0 workflow,
including image publication, to succeed before using this profile. Any Kustomize
image override must cover both init and main and use an explicit version or digest.

Validate offline without contacting a cluster:

```sh
kubectl kustomize deploy/kubernetes
```

After separately supplying the ConfigMap and Secret, an authorized operator can
apply to an explicitly selected context and namespace:

```sh
kubectl --context "$KUBE_CONTEXT" --namespace "$KUBE_NAMESPACE" apply -k deploy/kubernetes
```

The ClusterIP Service exposes port 8080 only within the cluster. HTTP requests
require authentication; no anonymous success endpoint is added. TCP startup and
readiness probes establish only that the listener accepts connections, not that
authentication, providers, storage recovery, or end-to-end execution works.
Init success alone does not establish readiness.
Startup has a ten-minute failure window; exceeding it can restart the container.
There is no liveness probe to trigger restarts during a stalled operation.

The pod allows 120 seconds for termination. HTTP shutdown has a 15-second timeout;
the subsequent database close is unbounded, so the pod grace period cannot
guarantee close completion before Kubernetes sends SIGKILL. The current source
propagates Close errors. Review shutdown
and recovery behavior with the actual storage driver before operational use.

Offline rendering does not verify admission, CSI permissions and single-pod
attachment, image execution with a read-only root filesystem and writable `/tmp`,
OAuth/provider connectivity, database startup/recovery, or graceful termination.
With the Rhiza module verified as v0.21.1, full-package (`./...`) normal tests,
race tests, and `go vet` passed with exit code 0. Tests cover fresh and repeated
init, absence of application migrations during init, writer-lock conflicts,
invalid CLI arguments, and lock release after failed initialization. A retained
local-directory test also passed: runtime writes a result, closes, init opens and
closes the same storage, and runtime reopens and returns the stored result without
another model request. This is not a Kubernetes PVC recovery test.
Offline `kubectl kustomize deploy/kubernetes` rendering passed.

Docker CI checks init and main execution with a shared volume and TLS
authentication. Consult the version's release CI and release notes for actual
results. Actual Kubernetes deployment, PVC/CSI attachment and
permissions, Pod termination, and retained-PVC reopening remain unverified.
No real external OAuth/provider connectivity or volume-loss recovery result is
claimed. No archive feature tests are needed for this profile because it does
not configure an archive.
