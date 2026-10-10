# MySQL dump command inherited unsupported connection option

Status: resolved locally; production acceptance pending main harness.

The disposable MySQL8 fixture showed mysqldump rejects mysql's
`--connect-timeout=10`. Dump command now filters mysql-only options, retains
context cancellation, and has a regression test. Actual MySQL session wrappers
also contain padded whitespace; the strict fixed SET allowlist handles it.

Codex prompt: preserve deterministic bounded dump options and add real pinned
client acceptance for any future option changes. Never expose passwords/rows
while diagnosing CLI failures, and never substitute a raw DB datadir archive.
