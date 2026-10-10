# Offline filesystem PVC copy

This package copies real filesystem bytes through `kubectl exec`; it does not
implement logical MySQL backup, database-aware recovery, online copy, CSI clone,
or an environment-ready signal. No live cluster was used for package tests.

## Integration API

`DomainExecutor.ExecuteDomain(ctx, domain.PVCCopyPlan, trustedNamespaces []string,
trustedImage string) ([]Marker, error)` is the multi-item provision-and-copy
adapter. Configure `Kubectl`, `Permission domain.PVCCopyPermission`, `Sources`
from authenticated compiled scan data, and a mandatory `Authorize` callback.
The callback must enforce namespace tenancy/ownership, Flux-only eligibility,
quota, source admission fence, and target prepublication/serialized-command
gates; it cannot trust the job body. Source fence remains held through return.
An allowed namespace configuration can be a superset of the selected plan.

The adapter validates all fresh source UIDs/specs/classifications before writes,
requires `envplane.io/pvc-copy-offline=true`, and probes exact helper manifests
with server-side dry-run before provisioning. Unknown live environment classes
are classified as production, requiring exact production-source approval and a
matching trusted scan identity. It creates only native filesystem PVCs without
`dataSource`/`dataSourceRef`. Foreign target objects fail; no adoption or deletion.
Copy ownership uses dedicated `envplane.io/pvc-copy-*` labels and exact plan/item
annotations. Ordinary lifecycle labels remain plain, separate values.

`Executor{Transport, Policy, Permission}.Execute(ctx, Plan) (Marker, error)` is
the single-item entrypoint for an already-created target with actual UID. Every
call requires trusted out-of-band image, source namespaces and target binding,
plus a mandatory fresh-permission callback. `Pending` target claims are accepted
only before helpers, allowing WFFC; both PVCs must be `Bound` before streaming.

`CopyLocal(ctx, sourceDir, targetDir, Plan)` runs the same native archive and
durable-target protocol on local directories. `Export`, `Relay`, `Receive`,
`Inspect`, `Complete` and `HelperCLI` are separately testable entrypoints.

`cmd/envplane-runner` dispatches `pvc-export`, `pvc-import`, `pvc-verify`, and
`pvc-complete` without initializing runtime. Helpers run the exact compatible
runner image configured by `Kubectl.RunnerImage` (sha256 digest required).
The Pod command explicitly overrides ENTRYPOINT with bounded `sleep`.

## Safety protocol

1. Re-read exact PVC UID, mode, binding and source OFFLINE declaration. Refuse
   any nonterminal Pod using either PVC, including read-only, Pending, Unknown,
   and terminating consumers. Ignore only the exact newly created helper UID.
2. List source namespace Deployment/StatefulSet/ReplicaSet/DaemonSet/Job/CronJob.
   Consumers require zero desired/observed replicas or suspended inactive jobs.
   Source-consuming DaemonSets are unsupported. Missing list permission fails.
3. Create read-only source and writable target helper Pods with the existing
   configured SA, no token mount, no fsGroup, no ownership change on source,
   read-only root filesystem, seccomp, resource limits and deadline. Require
   recursive read-only source mount support; unsupported runtimes fail.
4. Claim a completely empty target with an exclusive, fsynced state directory.
   Even `lost+found` is nonempty. Stream via two `io.Pipe`s, not `kubectl cp` or
   shell tar. Validate and re-encode bounded USTAR archives at runner and target.
   Refuse traversal, links (including source hardlinks), devices, sparse/PAX/GNU
   metadata, duplicates, missing parents, special permissions, truncation,
   trailing data, byte/entry bounds. Include root directory metadata.
5. Preserve regular permission bits and numeric UID/GID; hash these alongside
   entry type, path, size and every file byte. Restore files exclusively, then
   directories bottom-up. Reread the target and compare its durable receipt to
   the runner receipt. Recheck consumers/controllers and stream/hash the source
   again before exclusively writing and fsyncing the completion marker.
6. Marker binds immutable domain plan digest (when provided), exact source UID,
   actual target UID, local plan digest, byte/entry count and tree SHA256. Retry
   succeeds only after rehashing a complete matching target. Claimed, partial,
   malformed, tampered or mismatched targets are never overwritten or resumed.
7. Cleanup deletes helper Pods only, verifies exact ownership, and sends an API
   UID precondition through raw DELETE. No PVC, workload, source file or RBAC
   cleanup occurs. Failed cleanup/unknown creates require operator attention.

## Explicit deployment limitations

No observation is an atomic scheduling fence. The annotation is an owner
maintenance declaration, not a lock we install. The owner/Authorize integration
must hold the source admission fence against new consumers through return,
including rescaling, admission-injected mounts, out-of-cluster writers and
storage aliases. Target isolation relies on exclusively owned namespace,
server prepublication gate and serialized runner-leased command, plus fresh
consumer checks at each stage; this package claims no target-wide admission
fence (which would block later workload publication). We
recheck before/after transfer and source checksum; these checks detect drift but
cannot replace that operational contract or prove database consistency.

UID 10001 helpers are the default and can copy only when filesystem permissions
and ownership permit exact restoration. Root-owned PVC roots or different
ownership normally need trusted `Kubectl.AllowRootHelpers=true`. That opt-in
uses root with all capabilities dropped; source adds only `DAC_OVERRIDE`, target
adds `DAC_OVERRIDE`, `CHOWN`, `FOWNER`. Source remains recursively read-only and
never executes chown. PodSecurity/admission denial is surfaced, never bypassed.
Restricted namespaces may therefore not support copies of root-owned volumes.

This is a Unix native filesystem subset: no ACLs, xattrs, SELinux labels,
timestamps, filesystem flags, file capabilities, hardlinks or sparse layout
preservation. Workloads relying on such metadata must not be advertised as
usable copies without additional verification/support. USTAR path constraints
apply. Numeric IDs above signed 32-bit are rejected. Storage quantity parsing
accepts ordinary decimal/binary byte units; other forms fail closed.

Local hard bounds: 1 TiB, 100000 entries including root, 6 hours. Domain adapter
also bounds total operation time and item count (1000). Kubernetes JSON is
bounded; large/inaccessible/incomplete inventories fail. No automatic retries,
policy changes, root permission grants, RBAC grants or pushes are performed.

## Validation

Tests use real temp directories and fake kubectl API/argv with real native CLI
execution. They cover bytes/metadata, WFFC, receipts/markers, idempotency,
authorization, live-spec/class drift, active consumers/controllers, PodSecurity
refusal, partial state, target replacement, source mutation, timeout and unsafe
archives. Race tests do not prove CSI/mount durability or live cluster admission.
While the domain contract is unpublished, tests require a local Go workspace
using the current contracts tree; pinned published contracts must be aligned by
the integrator before standalone release builds.
