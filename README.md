# EnvPilot Runner

Job execution runtime for EnvPilot.

## Scope

- Runner registration and heartbeat.
- Redis-backed job queue consumer.
- Create, delete, recreate, and reconciliation job execution.
- Idempotent job handling and retry support.

## Source Origin

The runner runtime is currently embedded in `apps/api/main.go` behind the `runner` command. This repository contains that entrypoint and the supporting job packages from the original monorepo.

## Current Command

```bash
go run ./apps/api runner
```

## Recovering a stale bootstrap identity

If `/health` reports `stale_bootstrap_identity`, the project bootstrap session
or the runner credentials no longer match the persisted runner auth token. The
runner remains live but unready; it deliberately does not CrashLoop or retry an
invalid identity indefinitely.

1. In **Settings → Remote clusters**, select the affected target and use the
   audited **Rotate managed identity** or **Repair** action.
2. The Remote Cluster Reconciler replaces the target Secret and rolls the same
   release identity forward; it does not expose a command or raw credential.
3. Wait for a fresh authenticated heartbeat before retrying environment work.

The runner records only a hash of the registration token beside its persisted
auth token. A new registration token therefore supersedes the old persisted
auth token on the next rollout; do not delete the auth PVC as part of normal
recovery. Never copy the one-time command into logs, Git, or support tickets.

For remote targets the Runner is always installed by the management-cluster
reconciler from signed compatibility pins. It must use a stable target-Pod-
reachable HTTPS endpoint; Service DNS is supported only for same-cluster mode.
See the [remote-cluster guide](https://github.com/envpilot/deploy/blob/main/docs/remote-clusters.md).

## Follow-up

Extract a dedicated `cmd/envpilot-runner` binary and replace duplicated internal imports with the contracts module.
