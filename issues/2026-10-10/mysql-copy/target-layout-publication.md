# Generated workload must use restored MySQL subPath

Status: main integration/acceptance pending; worker protocol fixed.

Worker uses TargetDataSubPath=mysql under PVC root, leaving an exclusive durable
UID/plan claim outside the MySQL datadir. Existing source workload root mounts
are unchanged. An untransformed target root mount would make restored data unusable.

Codex prompt: seal matching generated target /var/lib/mysql subPath=mysql, pinned
platform image, and generated app/root Secret refs before template revision digest.
Test actual published application rows and clean target shutdown in an isolated
fixture; source workloads/PVCs must remain unchanged. Do not mark environment Ready
or database-aware-volume ticket closed from runner copy completion alone.
