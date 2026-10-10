# PVC copy permission and ownership defect

Status: identified during implementation; fixed and tested in this iteration.

Problem: a fresh local-path PVC may be root:root 0755, preventing UID 10001
helpers from claiming it. Creating all payload files as 0600 also loses original
ownership and permission semantics, so byte-only success is not a usable copy.

Codex implementation prompt: In runner/internal/pvccopy only, preserve numeric
UID/GID and regular permission bits for the root directory, directories, and
regular files. Include these fields in the canonical tree hash. Reject links,
special files, and setuid/setgid/sticky permissions. Restore target metadata
without changing source metadata. Support an explicitly trusted root-helper
policy with ALL capabilities dropped and only the minimum role-specific additions;
never set fsGroup or weaken PodSecurity/RBAC automatically. Test metadata round
trips and permission denial before completion. Keep runtime wiring separate.

Acceptance: actual temporary-directory bytes and ownership/mode agree, hash
changes on metadata tampering, source metadata is unchanged, and a missing root
permission or admission denial fails closed with no completed marker.
