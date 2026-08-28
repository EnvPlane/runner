# Missing rendered namespace became a nil string

Helm commonly omits `metadata.namespace` from namespaced manifests and applies
the release namespace out of band. Runner release-plan rendering converted the
missing map value with `fmt.Sprint`, producing the literal string `<nil>`.
That bypassed the intended release-namespace fallback and caused the control
plane to reject an otherwise scoped plan as a namespace escape.

## Resolution

Normalize absent and nil manifest identity values to an empty string before
applying the release-name and release-namespace defaults.
