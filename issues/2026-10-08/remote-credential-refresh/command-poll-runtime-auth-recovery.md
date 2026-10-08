# Command polling failed to request normal runtime auth recovery

Status: reproduced and fixed locally; no live Runner image update or push.

Command polling typed only token-is-not-issued; invalid/missing cases fell through
legacy string classification. A reproduced regression showed HTTP403 with body
text mentioning401 incorrectly requested recovery. The fix shares the initial
heartbeat narrow detail classifier for actual HTTP401 and rejects typed command
authentication errors before the legacy string fallback. Existing persisted-token
clearing and normal bootstrap registration remain the only recovery path.
Generic401/403 and expired bootstrap remain denied;
no minted token, skipped authentication, broadened permissions or auth-PVC deletion.

## Codex implementation prompt / acceptance

Preserve the HTTP401-only, explicit runtime-auth rejection allowlist. Test invalid,
missing and unissued cases and non-recovery for403/generic401/expired bootstrap.
Keep bootstrap expiry/stale identity repair under normal reconciler desired Helm
values; never use this recovery as an authorization bypass. Release/update the
Runner through its normal artifact flow after parent approves. In migrated
candidate confirm all four authenticated heartbeats advance and remote phase
healthy, not merely Pod Ready. Source remains paused for rollback.

Live snapshot during investigation (2026-10-08 UTC): the originally reported
failing Pod no longer existed. Its replacement had no restart and master/project
Agent/Runner bootstrap sessions were online with fresh server-accepted timestamps;
remote bethunder-local phase healthy. Parent normal rotation had already recovered
this live incident before this worker acted. No unnecessary second rotation made.

Repeated projected read-only management status confirmed progress, not just Ready:
master Agent 15:30:41->15:34:41 UTC, master Runner 15:30:46->15:34:46;
project app Agent 15:30:37->15:34:37, Runner 15:30:42->15:34:42.
Both sessions online and remote phase healthy. No token/hash/raw session payload
was printed; query projected only identity/status/timestamps. Normal registration
and heartbeat authentication had accepted all four runtimes. No database writes,
source unpause, PV deletion, management credential bypass or second live rollout.
Full Runner suite and internal/runner race suite passed after allowing local
httptest loopback listeners. Local fix remains unshipped, separate from parent
normal bootstrap/desired-Helm recovery that resolved the migrated live incident.
