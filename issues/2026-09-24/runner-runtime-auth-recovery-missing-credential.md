# Runner does not recover when runtime auth credential is absent

## Status

Fixed locally; live verification requires a Runner image containing the fix.

## Evidence

During the isolated `0.4.407` upgrade, the Runner started with a persisted
runtime token and an available bootstrap registration token. Its initial
heartbeat returned HTTP 401 with `missing runtime auth credential: runner auth
token is not configured`. The process retained the persisted token instead of
clearing it and registering again, leaving the Pod in CrashLoopBackOff and the
Helm upgrade pending.

## Impact

A control-plane restart, reset, or migration that removes a server-side Runner
credential can block zero-setup recovery even though the Runner has valid
bootstrap registration credentials.

## Implementation prompt

Extend the narrowly scoped Runner runtime-auth recovery classifier to recognise
the missing-runtime-credential 401 response. Clear and re-register only when a
bootstrap registration token is present. Keep unrelated authentication failures
non-recoverable, and add a regression test for the exact response.
