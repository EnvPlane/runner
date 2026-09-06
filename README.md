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

The control-plane exclusively owns PostgreSQL schema migrations. Runner SQL
integration tests require a schema provisioned by control-plane and never apply
runner-local migrations. In the assembled workspace, verify this invariant with
`deploy/scripts/check-schema-ownership.sh --root .`. The canonical OpenAPI
document is owned by contracts.

When running SQL integration tests, set `ENVPLANE_MIGRATIONS_DIR` to the
control-plane `migrations/postgres` artifact and set
`ENVPLANE_TEST_DATABASE_SCHEMA_READY=1` after that artifact has been applied.

## Development

```bash
go run ./cmd/envplane-runner
go test ./...
go build ./...
docker build -t envplane-runner:dev .
```

The `runner` compatibility argument and `runner-connectivity-check` support
safe upgrades of existing Helm releases.

Set `ENVPLANE_HELM_ALLOWED_CHART_HOSTS` to a comma-separated allowlist of
registry hosts used by direct `oci://` or `http(s)://` chart references.
Repository aliases are resolved through the runner's local Helm repository
configuration; direct references to hosts not in this allowlist are rejected
before Helm is invoked, including metadata and cluster-internal addresses.

The typed sandbox validator is currently not connected to the runner command
path. It must not be treated as an active runtime security control until a
sandbox execution gateway invokes `sandbox.ValidateLaunch` and adds integration
coverage for that path.

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
