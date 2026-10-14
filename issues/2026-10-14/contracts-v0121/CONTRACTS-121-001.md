# CONTRACTS-121-001: Align runner with latest published contracts v0.1.121

Date: 2026-10-14
Status: Local verification passed; exact-head hosted CI and merge pending

During the workspace update, Dependabot PR contracts#15 merged and published
v0.1.121 at e5cdd85d21998b8b23523e85842e015f40ca8012. The previous v0.1.120 batch is
superseded for freshness. This release updates filippo.io/edwards25519 to v1.1.1.

Codex implementation prompt: update all active contracts pins, immutable release
snapshots and copied SDK provenance to v0.1.121 from the published module. Verify
exact remote tag, checksum and OpenAPI identity, run local consumer tests with
GOWORK=off and exact-head hosted CI. Keep strict issuer financial profile aligned
without weakening assertions. Commit, push and merge to main through passing CI.
Preserve unrelated local work and historical evidence; no live deployment.

Local validation: snapshot verifier regression suite, exact published release
verification, go test ./... and go vet ./... passed with GOWORK=off, Go 1.26.9.
