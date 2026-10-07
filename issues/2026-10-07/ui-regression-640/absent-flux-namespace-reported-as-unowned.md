# Already absent Flux namespace reported as unowned during cleanup

Status: fixed locally; deployed retest pending.

## Live evidence

After Delete of fresh failed e2e-ui-routes-627-1006627 on 0.4.640, UI reports
Delete Failed / preview namespace is not owned by the environment. Read-only
checks show namespace, Flux Kustomization and both current-run PVs NotFound.

## Cause / Codex implementation prompt

IsNamespaceManaged returns false for both NotFound and foreign ownership.
cleanup_flux_namespace treats both as an ownership violation, although Flux
may already have removed the namespace. Distinguish verified absence from an
existing foreign namespace. Existing foreign namespaces must remain blocked;
RBAC, transport and parse failures must not imply absence. Preserve exact
namespace and Runner-scope guards. Test absent, owned, foreign and failed lookup.

## Fix

On a negative ownership result, perform an exact namespace existence check with
kubectl --ignore-not-found. Confirmed empty successful result means cleanup
already complete; no deletion is attempted. All lookup errors remain failures.

Validation: full Runner Go suite, targeted Runner/Orchestrator tests passed;
golangci-lint reported 0 issues. Existing foreign-namespace rejection is retained.
