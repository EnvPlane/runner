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

## Follow-up

Extract a dedicated `cmd/envpilot-runner` binary and replace duplicated internal imports with the contracts module.
