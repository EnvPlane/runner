# CONTRACTS-120-001: Align runner with published contracts v0.1.120

Date: 2026-10-14
Status: Local verification passed; hosted CI and main merge pending

Problem: main consumes an older contracts release. Latest verified tag v0.1.120
points to 45be24acdcf33c40c427c39619ca35b47ee69fb4.

Codex implementation prompt: update every active contracts dependency in this
repository to v0.1.120. Regenerate immutable contracts-release.json evidence and
any derived OpenAPI consumers from the downloaded published artifact. Run
published-module verification with GOWORK=off, relevant local tests and exact-head
hosted CI. Fix discovered regressions with individual tickets and English commits.
Push verified changes and merge through normal main protection. Preserve unrelated
local work and historical release/evidence records. No cluster deployment.

Local validation: published snapshot guard and its regression tests, go test ./...
and go vet ./... passed with GOWORK=off and Go 1.26.9.
