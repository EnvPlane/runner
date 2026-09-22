# Support disabled feature writer mode in Runner

## Evidence

Remote-runtime E2E with no allocated feature namespaces intentionally sets
`rbac.featureEnvWriter.enabled=false`. The chart previously still supplied
`preconfiguredNamespaces` with an empty namespace list, and Runner rejected
its configuration before registration.

## Implementation prompt

Add an explicit `disabled` feature writer mode. It must validate with an empty
namespace list and must expose no Helm target namespaces. Preserve existing
behaviour for `releaseNamespace`, `preconfiguredNamespaces`, and
`generatedFeatureNamespaces`. Add focused runtime tests.

## Acceptance criteria

- A Runner with mode `disabled` starts with no feature writer namespaces.
- It cannot select a Helm target namespace through the feature writer path.
- Existing writer modes retain their current validation and target behaviour.
