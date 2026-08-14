# Runner configuration migration (EP-BRAND-004)

Runner accepts canonical `ENVPLANE_*` configuration names with legacy
`ENVPILOT_*` fallback. If both names are set, the canonical value wins. Legacy
use produces a warning containing variable names only; registration tokens,
runtime auth tokens and project-config tokens are never logged.

The alias layer covers control-plane access, project/cluster/runner identity,
bootstrap and runtime token settings, Helm Direct execution settings, feature
namespace controls, heartbeat/preflight retry settings and API configuration.

Registration, command polling, runtime-auth token handling, persisted auth-file
format, command API headers and Helm execution semantics are unchanged. Existing
Kubernetes labels, annotations, namespace names and release identifiers remain
stable so an upgrade cannot orphan live workloads. Metrics and API paths remain
compatible during the migration window.

New chart values should emit `ENVPLANE_*`; removing those values safely falls
back to the existing `ENVPILOT_*` configuration for rollback.
