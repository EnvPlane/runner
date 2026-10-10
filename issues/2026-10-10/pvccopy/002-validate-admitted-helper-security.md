# Validate admitted helper security settings

Status: fixed in the hardening iteration; negative tests added.

Problem: checking only the submitted Pod manifest does not prove a mutating
admission webhook left it intact. A changed fsGroup could cause source metadata
changes during mount; modified root/capabilities/mount settings could violate
the trusted helper policy. UID/ownership checks alone are insufficient for exec.

Codex implementation prompt: In runner/internal/pvccopy, verify the actual create
response and Pod readback before wait/exec against the exact helper image, SA,
tokenless setting, one-container/no-sidecar shape, read-only source PVC and
mount, no fsGroup/SELinux overrides, UID/GID/root opt-in, capabilities, seccomp,
bounded sleep/deadline and no host namespace. Fail on mutation. Preserve known
UID ownership for cleanup even when shape validation fails, and delete only with
UID precondition. Add negative admitted-manifest tests without live operations.

Integration prerequisite: the administrator-owned source admission fence should
also reject fsGroup/SELinux additions before scheduling; post-admission readback
is a guard, not protection against kubelet already acting on a mutated manifest.
