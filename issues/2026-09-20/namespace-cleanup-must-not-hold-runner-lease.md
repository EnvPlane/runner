# Namespace cleanup must not hold the Runner command lease

## Observed

In isolated E2E, a Helm Direct delete command successfully removed the
preview namespace but stayed `claimed` because `kubectl delete namespace`
waited for Kubernetes finalizers. This prevented the Runner from reporting
the command result and delayed all later commands.

## Expected

The Runner initiates deletion of a verified envplane-owned preview namespace
and returns immediately. The control plane continues to observe the cleanup
asynchronously; a namespace's finalizers must not hold the Runner lease.

## Implementation prompt

Add `--wait=false` to the Runner's `kubectl delete namespace` invocation.
Keep `--ignore-not-found=true`, namespace ownership validation, and error
handling. Add a unit test that asserts the exact kubectl invocation includes
both flags.
