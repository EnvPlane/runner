# Reviewed contracts release snapshot

Status: accepted, 2026-10-10.

## Context

A docs-only contracts publication advanced the latest tag while consumer CI was
running. Identical reviewed source bytes were rejected solely because the remote
latest changed. Independent image publication also did not establish Go CI success.

## Decision

Each direct Go consumer checks in `contracts-release.json` alongside its module
pin. CI verifies the exact immutable tag commit, Go module and go.mod checksums,
local go.sum entries, and canonical OpenAPI SHA-256. Local contracts replacements,
missing evidence and altered bytes fail closed. Verification never selects latest
and does not accept environment-variable overrides.

Dependency updates regenerate the snapshot in the update pull request; version,
snapshot and generated consumers must be reviewed together. The contracts publisher
avoids releases for documentation-only changes. Image and umbrella publication
must independently require successful CI for the exact candidate commit.

## Alternatives and consequences

Checking remote latest is fresh but non-reproducible. Removing the guard entirely
would accept unreviewed dependencies. A reviewed snapshot retains integrity and
reproducibility without conflating freshness with correctness. It deliberately
does not promise that a reviewed release is the newest: dependency update automation
and security review remain responsible for freshness. Remote evidence outages block
publication; no offline success is synthesized. Snapshot duplication across separate
repositories is intentional and protected by identical regression tests.

## Verification and rollback

Run `python3 -B scripts/test-contract-snapshot.py` and
`./scripts/check-contract-version.sh` with published modules (`GOWORK=off`).
Rollback the consumer dependency and its snapshot together to a reviewed release.
Do not rewrite published tags or weaken verification to bypass an unavailable gate.
