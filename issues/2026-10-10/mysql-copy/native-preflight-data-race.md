# Concurrent dump/restore metadata preflights raced

Status: resolved locally; race regression covers native driver execution.

Concurrent pipeline ends updated driver plan/namespace/image proof state during
Validate. Metadata preflights are now serialized without holding the mutex over
the actual SQL stream. One driver instance is one immutable plan execution.

Codex prompt: retain the concurrent real-data dump/restore race test when changing
driver state. Do not serialize whole streams or hold locks across io.Pipe transfer.
