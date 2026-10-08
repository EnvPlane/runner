# Verify namespace absence after asynchronous Flux cleanup

Priority: P2
Status: fixed locally; not pushed or deployed.

The 0.4.677 lifecycle test showed that DeleteNamespace uses --wait=false while the Runner immediately reports verified/terminated. The API then dispatched redundant Secret cleanup, which failed during namespace destruction.

Implementation: after the owned namespace delete request, query NamespaceExists. Report terminating/unverified while it exists; certify termination only on successful absence. Preserve lookup failures as failures. Coordinate with the control-plane fix that skips Secret cleanup for dedicated Flux namespace deletion, retaining Secret cleanup for release-only deletion.

Tests cover pending finalization, immediate and pre-existing absence, forbidden reads and foreign ownership. Full internal/runner and internal/orchestrator suites passed. Publish both components together and repeat the UI lifecycle test. No push or deployment was performed.
