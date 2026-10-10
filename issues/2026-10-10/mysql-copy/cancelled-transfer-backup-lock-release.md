# Cancelled transfer kills the backup-lock session before verified release

Status: scoped fix locally verified; fresh Kubernetes cancellation replay pending.

Evidence: isolated Kubernetes fixture 4160 passed positive copy and completed
retry. Cancellation returned transfer/cancelled plus release_ddl/operation_failed;
helper cleanup succeeded. The lock session inherited the operation context, so
transfer cancellation terminated it before independent bounded Release ran.

Fix: detach only the lock session from operation cancellation, keeping its
deadline bounded by the operation deadline plus five seconds. Release sends
UNLOCK INSTANCE, verifies a fixed acknowledgment with pinned connection ID and
server UUID, then explicitly quits and requires clean process exit. Lost proof,
deadline or nonzero exit remains an error, never a waiver.

Codex implementation prompt: test cancelled-operation session survival with a
bounded deadline, same-session unlock acknowledgment, wrong ID/server, missing
acknowledgment and failed exit. Verify actual MySQL DDL works after cancellation
and release, run race/vet/lint, commit only owned worker files, and coordinate a
fresh native Kubernetes cancellation/partial-target replay. Do not change helper
manifests, source data, runtime dispatch, policy/RBAC, or mask release failures.

Local verification: full runner race suite and vet passed with Go 1.26.9/private
workspace; golangci-lint reported 0 issues. Real disposable MySQL engine test
passed with 187 snapshot rows/12800 bytes while source writes advanced to 307
rows. After cancelling the operation context, the fixed control protocol
acknowledged UNLOCK on its pinned session, exited cleanly, and actual ALTER TABLE
succeeded. Restore remains binary-mode=1/local-infile=0; only fixed backup-control
commands allow quit. This is not yet a Kubernetes cancellation acceptance claim.
