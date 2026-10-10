# Docs-only contracts publication invalidates otherwise reproducible consumer CI

Status: immediate mitigation in progress (align all consumers to v0.1.111); release coordination follow-up OPEN.

## Reproduction and evidence

Contracts v0.1.110 points to f015966. A later docs-only main push e69da40 published v0.1.111, with no difference in domain/sdk/openapi/go.mod/go.sum. Runner CI 38079091752, Agent CI 38079091342 and control-plane Go CI 38079091843 failed before their tests at check-contract-version.sh: `contracts pin v0.1.110 is not the latest release v0.1.111`. Local released-module builds/race suites and strict native Kubernetes acceptance had passed.

The guard consults mutable remote latest at job execution, rather than the consumer's reviewed release snapshot. Image publisher workflows completed independently despite the consumer CI failures; an image publication is therefore not proof that required checks passed.

## Current safe mitigation

Resolve the exact immutable v0.1.111 tag, update all direct consumer pins/checksums and frontend OpenAPI ref, and rerun the real network-resolved guard without overriding latest. Do not waive failures, force tags, rewrite published modules or mark pending hosted checks green.

## Codex implementation prompt for follow-up

Design coordinated immutable contracts release snapshots for consumer verification, exclude genuinely docs-only changes from automatic module publication where appropriate, and gate production image/umbrella selection on the exact candidate SHA's required successful checks. Preserve schema/version compatibility checks and fail closed on unavailable GitHub status data. Test delayed publisher/CI ordering, docs-only releases, real incompatible contracts changes, concurrent candidate updates, fork/PR event trust and exact-SHA checkout. Do not solve the race by simply disabling pin verification or accepting arbitrary old modules.
