# Source fence permits injected volume metadata mutations before readback

Severity: critical
Status: fixed locally; no live policy update or live source-data validation performed
Scope: runner/internal/pvccopy/fence.go and fence_test.go

## Evidence and risk

The source-helper admission exception required read-only PVC and container
mounts but did not constrain the Pod security context. A mutating webhook could
inject fsGroup, fsGroupChangePolicy or SELinux settings. Kubelet/runtime volume
ownership or relabeling can occur before post-create helper inspection, making
readback too late as the primary defense against metadata changes.

Reference: https://kubernetes.io/docs/tasks/configure-pod-container/security-context/

## Local fix

Require admission-time absence of fsGroup/change policy, supplemental groups/
policy, SELinux options/change policy and sysctls. Require exact Pod RuntimeDefault
seccomp and correlated user/group/nonroot modes (0/0/false or 10001/10001/true).
Reject container runAs, SELinux, seccomp/AppArmor/procMount overrides and legacy
profile annotations. Root source helpers allow only DAC_OVERRIDE; nonroot helpers
add no capabilities. Require the bounded sleep/deadline shape and reject command,
working-directory, lifecycle/probe and profile-path side channels.

Each source requires three non-persistent server-side dry-run rejections:
a writable helper, a read-only helper with injected fsGroup, and a read-only
helper with injected SELinux options. Only the exact held policy/binding and
source-authority digest message counts as enforcement evidence. Unrelated
RBAC/PodSecurity denial or admission of either metadata probe fails closed.

Canonical-spec verification refuses older weaker policies under the same stable
source-authority names. The operator must render and update the held policy;
executors never create/update/delete admission policies or RBAC. A copy result
does not release the parent workload-readiness gate.

## Implementation/verification prompt for Codex

Keep changes scoped to the fence implementation and its tests. Verify every
source-consumer and Runner-source-namespace admission expression contains all
metadata/security guards. Reject policies with any guard removed or weakened,
missing/stale policy acceptance/type-checking status, or unrelated probe denial.
Use synthetic JSON/probe injection tests only; do not access a live cluster,
read secrets, alter source metadata, change policy/RBAC, or edit worker files.
Before any authorized deployment, have the operator update the existing
source-bound policy using FenceManifests and run both the parent positive helper
preflight and exact negative admission probes. Unsupported cluster schemas must
remain fail closed. Report local checks separately from live enforcement.
