# Image publisher missing exact-SHA Go CI prerequisite

Status: fixed locally; hosted enforcement awaits publication of these commits.

Defect: the component publisher could push an image after its own basic test step
without waiting for required Go CI, including lint/race/coverage/contracts gates.

Fix: a separate read-only required-go-ci job uses trusted main gate code, checks
main ancestry and exact ci.yaml workflow identity (Runner CI), then requires the
latest push-to-main run and current successful test-job attempt for github.sha.
Missing/pending evidence gets at most 20 minutes; API errors, failure, skipped CI,
foreign/PR/self evidence and incomplete inventories fail closed. Publishing needs
this job. No workflow_run trigger, artifact consumption or write token in gate.
Existing image/version/dispatch steps stay unchanged.

Regression: node --test .github/scripts/required-go-ci.test.cjs (44 tests), appended
to ci.yaml without changing contracts snapshot steps. Workflow syntax validated
with actionlint v1.7.12; GitHub Script v8 pinned to
ed597411d8f924073f98dfc5c65a23a2325f34cd.

Codex prompt: preserve exact trusted main SHA/workflow/run-attempt checks whenever
changing this publisher; do not accept green check names, PR workflow_run payloads,
older success masking newer CI, or missing/API-denied evidence as release authority.
