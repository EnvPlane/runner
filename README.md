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

1. In Bootstrap, select an audited recovery reason and use **Rotate bootstrap
   credentials**.
2. Apply the newly generated one-time bootstrap Secret command.
3. Run the newly generated `helm upgrade --install` command (or restart the
   existing Deployment after applying equivalent Helm values).

The runner records only a hash of the registration token beside its persisted
auth token. A new registration token therefore supersedes the old persisted
auth token on the next rollout; do not delete the auth PVC as part of normal
recovery. Never copy the one-time command into logs, Git, or support tickets.

## Follow-up

Extract a dedicated `cmd/envpilot-runner` binary and replace duplicated internal imports with the contracts module.
