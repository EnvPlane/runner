# Native MySQL helper cleanup exhausts its budget after receipt commit

Status: implementation and local regressions verified; fresh Kubernetes replay required.

Observed: the isolated live fixture committed a verified shutdown receipt, but
returned failure because sequential helper deletion/wait consumed the cleanup
deadline. The receipt alone is not overall success. Teardown also hid the primary
failure behind a generic redacted error.

Fix: request all in-memory-owned Pod deletions with exact UID preconditions and
unchanged 30-second grace before concurrent disappearance polling. Polls recheck
pinned namespace and Pod UIDs. Cleanup gets an independent bounded 45-second
context; failures still clear the returned receipt and refuse success. Export
only fixed stage/code diagnostics; do not serialize underlying error text.

Codex implementation prompt: preserve helper manifests, ownership and full grace;
test two helpers with a concurrent-wait barrier, namespace/Pod replacement,
cancelled parent context, cleanup failure after receipt, primary transfer failure
plus cleanup failure, and secret/SQL-body redaction. Run unit/race/vet/lint, then
replay the fresh isolated live harness. Never mutate an existing application or
grant permissions, and do not mark full publication acceptance from a receipt.

Local evidence (Go 1.26.9, private workspace): full runner race suite and vet
passed; golangci-lint reported 0 issues. Opt-in real disposable MySQL engine
fixture passed: 170 restored snapshot rows, 11661 dump bytes, separate source
writer advanced to 283 rows. This engine test is not a Kubernetes cleanup proof.
