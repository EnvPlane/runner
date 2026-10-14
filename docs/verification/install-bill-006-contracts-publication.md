# INSTALL-BILL-006 published contracts consumer

Date:2026-10-14. Component:runner. Local published-module acceptance PASS.

Pin v0.1.118; source a1b9c2482576c67dd8292d549f56b553933f5cb4.
Exact module/go.mod sums and OpenAPI digest are in contracts-release.json.
The immutable version was published by contracts run38569906044 after PR14
passed exact-head checks and ordinary protected merge; no existing tag moved.

Local commands (GOWORK=off, existing GOPRIVATE policy, no local replace):
- python3 -B scripts/check-contract-snapshot.py: PASS
- go test -race -count=1 -json ./...: 571 passes, 0 failures, 2 fixture skips, 7 package passes
- go build ./...: PASS
- go vet ./...: PASS

These default tests retain existing fixture-dependent skips; this is dependency
compatibility evidence, not installed DB/provider/integration acceptance.
Exact raw local logs: /private/tmp/install-bill-publication-evidence/runner-*.log.
Actual AWS/provider/secret/installed database actions: NONE. No deployment or
customer enrollment enabled. Hosted exact-head PR checks and merge remain separate
review/release gates until their current run is read back.
