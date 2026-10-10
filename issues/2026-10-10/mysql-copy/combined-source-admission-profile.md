# SQL helper conflicts with existing exact filesystem source fence

Status: external owner integration/real Kubernetes acceptance pending.

The existing filesystem fence denies all other RunnerSA source Pod creates. A
second SQL allow policy cannot override it. Worker refuses absent/drifted approved
policy/binding receipt and requires positive/negative server dry-run enforcement
before source helper creation. Source helper name is deterministic; exact exec/
delete scopes must match it. No app Pod exec, arbitrary Secret/PVC projection,
live RBAC grants or policy installation are performed by this package.

Codex prompt: profile/fence owners must jointly implement a sealed reviewed
filesystem-or-SQL no-PVC helper source policy and exact verifier protocol, full
credential/CA refs and helper security shape. Persist policy/binding UID and whole
spec SHA256 evidence, fixed digest-bound denial message, and prove actual Runner
positive helper/negative Secret/PVC dry-runs in a new isolated fixture. Keep mixed
namespace capability false until that acceptance succeeds.
