# Worker verification — 2026-10-10

Stable production entrypoints: `DomainExecutor.ExecuteDomain(ctx, Plan)`,
`NewProofDriver(DriverConfig)`, `KubeTransport`, `HelperCLI`,
`SourceClientHelperName(Ref)`, `SourceClientHelperSpec(Plan, serviceAccount)`.
Constructor/config and helper manifest protocol are unchanged by this iteration.

All commands below were executed against the current shared runner tree with
`GOWORK=/private/tmp/envplane-pvc-copy.lsMO9m/go.work`,
`GOTOOLCHAIN=go1.26.9`, and
`GOCACHE=/private/tmp/envplane-pvccopy-go-cache`.

| Check | Actual result |
| --- | --- |
| `go test ./... -count=1` | PASS, all runner packages |
| `go test -race ./... -count=1` | PASS, all runner packages |
| `go vet ./...` | Exit 0 |
| `/Users/alex/go/bin/golangci-lint run --timeout=5m` | Exit 0, 0 issues |
| `ENVPLANE_MYSQLCOPY_DOCKER_TEST=1 go test -race ./internal/mysqlcopy -run TestMySQL8EngineConsistentFixture -count=1 -v -timeout=3m` | PASS, 18.259 seconds package time |

The final real MySQL8 fixture restored **116 transactional snapshot rows** from
**8,043 raw dump bytes**; the already-active independent AUTO_INCREMENT writer
advanced to **229 source rows**. Logical schema/ordered-row proof matched after
restore. The source backup connection held LOCK INSTANCE FOR BACKUP, DDL was
refused while inserts continued, and source dump used a SELECT-only principal.

The fixture additionally exercised actual SHOW GRANTS catalog authority and
catalog filtering: a fixture-only trigger was invisible to the SELECT-only dump
principal (count 0), visible to the approved metadata admin (count 1), then removed
before copying. Zero counts from a blind principal are not accepted as absence
proof. No existing database or application was used.

A final image compatibility check discovered that official MySQL8 has **no
external `kill` executable**. Native shutdown now uses fixed argv
`sh -c 'kill -TERM 1'`, without interpolation/passwords/extra capabilities. The
real target fixture executed this as UID/GID999 and Docker wait returned exit 0.
Completion receipts still require the Kubernetes container's clean exit proof.

Pinned fixture image:
`mysql@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b`.
Fixture servers were newly created, networkless, with RAM-backed datadirs/configs,
no host data mounts, no persisted dump, and cleanup of both owned containers.
Only metadata/counts were printed. Fixture bootstrap grants affect only these
new disposable servers, not a live source or Kubernetes RBAC.

Regression checks cover exact internal Service TLS hostnames, VERIFY_IDENTITY
arguments, explicit full catalog visibility, dynamic table AUTO_INCREMENT-only
normalization, literal/column/index/constraint changes, binary-mode/no-LOCAL-INFILE
restore, whole-statement-before-forward validation, EOF consumption, independent
target app/root credentials, lifecycle UTC binding, UID/spec drift, admission
negative/positive protocol, partial refusal and completed restart re-verification.
The Linux target-only ownership fixture separately passed exclusive claim and
actual UID/GID999/mode0700 datadir assertions in a new bounded root container.

## Remaining acceptance boundary

Docker engine success is **not live Kubernetes TLS/admission/RBAC/PodSecurity,
WFFC/storage, persisted restart or application-publication proof**. Native driver
restart/receipt/admission operations have unit/protocol coverage; Singer/Aquinas
own the separate fresh isolated Kubernetes acceptance run. Their CEL quantity
projection and CRI index/platform alias blockers cannot be waived by this package.

Initial support remains nonempty MySQL8 base-table databases with primary keys,
no views/triggers/routines/events/cross-schema foreign keys, direct catalog grants,
verified internal-Service TLS, compatible mysql UID/GID999 image and trusted root
init opt-in. Unsupported/custom datadir/runtime/SQL shapes refuse; Restricted
PodSecurity may refuse helpers. Target exclusivity needs the parent's owned
namespace and prepublication lease, not target-wide admission isolation.

Main owns compiled-domain permission adaptation, independently generated Secret
UID/app/root key proof, sealed target subPath=mysql/runtime layout, publication and
readiness. No environment Ready, full-filesystem/CSI support, Kubernetes SQL PASS
or database-aware-volume ticket closure is claimed here. No push.
