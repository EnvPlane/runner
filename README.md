# EnvPlane Runner

Target-cluster execution runtime for [EnvPlane](https://envplane.dev). The
runner performs explicitly authorized Helm lifecycle operations and reports
bounded results to the control plane.

## Responsibilities

- Register with the control plane and emit authenticated heartbeats.
- Poll the versioned runner command API.
- Execute authorized Helm operations in approved namespaces.
- Report execution results over HTTPS.
- Run connectivity preflight checks during installation.

Redis remains an internal control-plane queue and is never exposed directly to
the runner.

## Development

```bash
go run ./cmd/envpilot-runner
go test ./...
go build ./...
docker build -t envplane-runner:dev .
```

The `runner` compatibility argument and `runner-connectivity-check` support
safe upgrades of existing Helm releases.

## Related components

- [Control Plane](https://github.com/EnvPlane/control-plane)
- [Agent](https://github.com/EnvPlane/agent)
- [Contracts](https://github.com/EnvPlane/contracts)
- [Deploy](https://github.com/EnvPlane/deploy)

## Security

Inject short-lived credentials through managed Kubernetes Secrets. Never commit
tokens, kubeconfigs, cloud credentials, or unrestricted namespace permissions.

## Status

Private EnvPlane platform component under active development.
