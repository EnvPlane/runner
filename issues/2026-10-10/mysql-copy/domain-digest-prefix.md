# Worker metadata digest validation rejected shared contract format

Status: resolved locally; plan fixture uses canonical shared sha256 prefix.

The shared domain/Secret-gate digests are sha256:<64 hex>, while worker local
plan/receipt digests are raw 64 hex. Initial length-only validation rejected every
real domain plan. Plan now validates canonical prefixed domain/gate digests and
keeps the internal receipt/volume-claim digest separate.

Codex prompt: include adapter-shaped digest fixtures in worker changes and avoid
stripping lifecycle/template/Secret-gate authority to make plans pass validation.
