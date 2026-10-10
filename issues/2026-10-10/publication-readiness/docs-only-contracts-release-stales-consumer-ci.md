# Docs-only contracts publication invalidates otherwise reproducible consumer CI

Status: v0.1.111 mitigation completed; coordinated release fix implemented locally, publication verification pending.

## Reproduction and evidence

Contracts v0.1.110 points to f015966. A later docs-only main push e69da40 published v0.1.111, with no difference in domain/sdk/openapi/go.mod/go.sum. Runner CI 38079091752, Agent CI 38079091342 and control-plane Go CI 38079091843 failed before their tests at check-contract-version.sh: `contracts pin v0.1.110 is not the latest release v0.1.111`. Local released-module builds/race suites and strict native Kubernetes acceptance had passed.

The guard consults mutable remote latest at job execution, rather than the consumer's reviewed release snapshot. Image publisher workflows completed independently despite the consumer CI failures; an image publication is therefore not proof that required checks passed.

## Current safe mitigation

Resolve the exact immutable v0.1.111 tag, update all direct consumer pins/checksums and frontend OpenAPI ref, and rerun the real network-resolved guard without overriding latest. Do not waive failures, force tags, rewrite published modules or mark pending hosted checks green.

## Codex implementation prompt for follow-up

Design coordinated immutable contracts release snapshots for consumer verification, exclude genuinely docs-only changes from automatic module publication where appropriate, and gate production image/umbrella selection on the exact candidate SHA's required successful checks. Preserve schema/version compatibility checks and fail closed on unavailable GitHub status data. Test delayed publisher/CI ordering, docs-only releases, real incompatible contracts changes, concurrent candidate updates, fork/PR event trust and exact-SHA checkout. Do not solve the race by simply disabling pin verification or accepting arbitrary old modules.

## Local implementation and evidence

All seven product Go consumers and both deploy live-driver modules now record the
reviewed v0.1.111 commit, module checksums and OpenAPI digest. The real network-backed
verifier passed for all nine. Nine regression tests passed in each product consumer;
moving latest and environment overrides cannot alter the accepted snapshot. Product
CI invokes the verifier. See `docs/contracts-release-snapshot.md` for the decision.
Publisher, dependency-update bot and exact-candidate CI gates were verified together:
32 publisher tests, 13 updater tests, 176 component publisher gate tests and 17
umbrella candidate gate tests passed. All seven product repositories passed isolated
Go builds and complete unit suites. Workflow actionlint passed. Hosted checks for
the new commits must still run after push;
local passing tests are not a claim of completed hosted publication or deployment.
