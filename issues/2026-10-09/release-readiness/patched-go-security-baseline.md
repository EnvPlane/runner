# Patched Go security baseline

Status: implemented locally; hosted CI pending.

Current security scans detect HTTP/2, TLS and other standard-library advisories in older Go versions. The module now requires Go 1.26.9, and any container builder used here is pinned to a verified Go 1.27.2 image digest. x/net consumers use v0.60.0.

Validation: isolated unit tests passed on Go 1.26.9. Govulncheck reports no reachable vulnerabilities; this does not claim all transitively required modules are advisory-free. Keep release security gates enabled.

Implementation prompt: use the patched module toolchain consistently in CI, preserve immutable builder digests, verify tests and vet/lint after dependency updates, and only deploy successfully scanned published artifacts.

