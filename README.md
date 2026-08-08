# EnvPilot Runner

Standalone target-cluster execution runtime for EnvPilot.

## Run

```bash
go run ./cmd/envpilot-runner
```

The process registers with control-plane, emits authenticated heartbeats,
polls the versioned Runner command API, executes Helm lifecycle operations in
explicitly authorized namespaces, and reports each result over HTTP. Redis is
an internal control-plane queue and is intentionally not exposed to Runner.

The legacy `apps/api runner` entrypoint has been removed. The binary accepts a
temporary no-op `runner` argument for safe upgrades of older Helm releases and
supports `runner-connectivity-check` for the chart preflight init container.

Runtime identity recovery and remote endpoint requirements are documented in
the canonical deploy repository's remote-cluster guide.
