# Service-name prefix did not constrain TLS connection destination

Status: resolved locally; external suffix regression added.

Prefix matching allowed service.namespace.svc.attacker.example even though the
TLS server name is also the actual mysql --host destination. The worker now
accepts only exact service.namespace.svc or service.namespace.svc.cluster.local.
CA trust and a Service UID cannot authorize an external DNS suffix.

Codex prompt: preserve exact internal-host validation. Add custom cluster-domain
support only through reviewed trusted deployment configuration, never job flags.
