# Runner result callback hid API error details

The Runner reported command results through the shared SDK. Its API error type
preserved status and code but discarded the bounded `error` detail returned by
the control plane. Release-plan publication failures therefore appeared in
runtime diagnostics only as HTTP 422, preventing actionable diagnosis of the
failed contract guard.

## Resolution

Use the Runner's existing bounded JSON transport for result callbacks. Keep the
same bearer authentication and command API version header while retaining the
redacted status, code, and error detail in diagnostics.
