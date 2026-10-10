# Official MySQL image has no external kill executable

Status: fixed locally; real same-UID shutdown regression required.

The native driver attempted `kubectl exec ... -- kill -TERM 1`, but the pinned
official MySQL8 image returns executable-not-found. The target could never reach
verified shutdown or a completion receipt. Use the image's POSIX shell builtin
with a fixed command, no interpolation, credentials or extra capabilities.

Codex prompt: keep the real engine fixture signaling PID1 as UID/GID999 and
asserting exit code zero. Never replace clean shutdown proof with force deletion
or create a receipt after an unknown/failed shutdown.
