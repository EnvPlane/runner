# Render release plans without target release-storage RBAC

## Evidence

A terminated preview namespace deletes its namespace-scoped Runner Role and
RoleBinding. Control-plane Recreate correctly queued `render_release_plan` to
restore that capability, but the Runner rejected the render before rendering
because it applied the Helm release-Secret RBAC guard to a non-Helm operation.

## Required implementation

Keep target namespace authorization for Helm create, recreate, delete, and
status operations. Do not require it for `render_release_plan`: rendering does
not access Kubernetes release storage and its result triggers exact RBAC
provisioning before Helm apply.

## Acceptance criteria

- A render command succeeds when its target namespace is not currently in the
  Runner Helm namespace allowlist.
- Helm apply/delete/status remain rejected without target namespace access.
- A terminated Recreate can render, restore RBAC, then apply successfully.
