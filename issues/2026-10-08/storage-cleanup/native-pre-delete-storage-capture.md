# Native Runner pre-delete storage capture hook

## Implemented locally, no live operations

The native Runner calls authenticated control-plane metadata capture before backend
removal for `delete`, `force_cleanup` and `cleanup_flux_namespace`. Helm paths call
after existing signed release-plan, sensitive-config boundary, cleanup inventory
and exact resolved namespace access checks. Flux calls only after its dedicated
namespace/access/managed-ownership checks, before DeleteNamespace. An already-absent
Flux namespace has no new deleting operation and does not attempt capture. Backend
ownership/removal behavior remains unchanged; no extra PV/node inventory is read by
Runner. Workspace trace found only `runner/internal/runner/runtime.go`, no duplicate
native runtime package requiring mirrored changes.

POST `/api/v1/runner/storage-cleanup/capture` uses the existing configured control
plane base origin and endpoint-mode/TLS configuration, with runtime Bearer header,
scoped tenant header and command API version. Body has exactly projectId, clusterId,
runnerId, commandId, environmentId: no body token, tenant override, Secret/PVC/PV
data, node paths or infrastructure endpoints. Config identities are authoritative;
conflicting command identities skip capture with a sanitized diagnostic.

The child context and HTTP client are capped at five seconds, response consumption
is capped at 4096 bytes, redirects are never followed and endpoint userinfo/query/
fragment are refused. Remote mode retains HTTPS/non-host-local policy. Bounded 2xx,
including the current 200 snapshot and 204 acknowledgement, acknowledge capture
only. No response data is logged or forwarded into Runner result fields.

Failure/404/unsupported API, auth rejection, canceled context, missing identities
or oversized response produce at most one constant-reason warning per attempted
cleanup, never raw URL/identity/token/response/body/error details. Capture has no
retry loop, never aborts backend invocation and never cancels its parent context.
Empty API/auth config skips immediately. CleanupVerified continues to mean the
original backend's cleanup result; **it is not storage proof**, even after capture
success. No shared contracts/result fields were changed to invent proof/coverage.

## Verification and remaining integration boundary

Local tests verify capture-before-remove for all three operations and 200/202/204,
403/404/503; guard-before-capture for invalid signed plan, invalid inventory, denied
namespace and unmanaged Flux namespace; default-result equality under capture
failure; one sanitized warning; cancellation, no credential redirects, response
bound and request deadline. Mocked backends/loopback fixtures perform no Helm,
Kubernetes, node, grant or data operation. They are not live cleanup proof.

## Codex implementation prompt / parent responsibilities

Parent control plane owns the normal capture API. It must verify runtime bearer,
trusted tenant, current claimed cleanup command/lease and matching environment,
project, cluster and Runner, and persist snapshots before removal without treating
an HTTP acknowledgement or CleanupVerified as physical reclamation evidence.
Operator proof and retention remain separate. Do not add body-token auth, broad PV
reads, arbitrary paths, redirects, an auth bypass or a cleanup-blocking retry loop
to make missing capture appear verified. Repeat controlled native integration and
verify durable snapshot creation separately before claiming live coverage.

No push, publish, cluster/node/Secret access or live grants were performed.
