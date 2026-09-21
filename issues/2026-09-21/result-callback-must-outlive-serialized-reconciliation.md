# Recovery: Runner command-result delivery must outlive serialized reconciliation

## Observed

Two same-project release-plan results are now serialized correctly, but the
second Runner callback waits for the first executor Helm reconciliation to
finish. The generic Runner HTTP timeout is ten seconds, so the second callback
is cancelled before the control plane can queue `create`. Its render command is
already persisted as successful and the environment remains `creating`.

## Required implementation

Use a dedicated, bounded command-result delivery timeout that is long enough
for same-cluster reconciliation and fresh Runner heartbeat confirmation. Keep
polling and heartbeat request timeouts short; they must not inherit the longer
result-delivery timeout.

## Acceptance criteria

- Two concurrent release-plan results for one project can be serialized without
  client-side cancellation.
- The second result queues `create` after the first reconciliation completes.
- Polling and heartbeat timeout defaults remain unchanged.
- The result timeout is configurable and validated.
