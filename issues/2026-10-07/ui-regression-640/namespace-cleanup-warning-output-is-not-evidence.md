# Namespace lookup must not confuse warnings with object evidence

Status: fixed locally; deployed retest pending.

## Evidence / cause

Code audit of the absent-namespace cleanup fix found that the CLI executor uses
CombinedOutput. Successful kubectl may emit warnings on stderr even when an
ignore-not-found query returns no object; the nonempty combined bytes were
classified as present. Ownership JSON parsing can also fail on warning prefixes.

## Codex implementation prompt / fix

Separate namespace read stdout from warning stderr, keeping command failures
fail-closed. For a nonempty presence response require valid JSON with the exact
requested namespace name. Reject blank targets, malformed JSON and mismatched
identities. Do not interpret warnings or RBAC/network failures as confirmed
absence and do not weaken foreign-namespace ownership checks.

Tests cover stdout-only selection, absent/present, malformed/mismatched output,
blank target and RBAC/network failure, plus existing Runner ownership tests.

Validation: full Go suite passed; golangci-lint: 0 issues; brand check passed.
Failed lookup stderr is retained for NotFound/RBAC classification, while only
successful stdout is parsed. Additional regression covers this distinction.
