# Published contracts alignment

Status: implemented locally; hosted CI pending.

The repository pinned contracts v0.1.106 while v0.1.107 is published. Local workspace replacements concealed the mismatch; consumers requiring new domain types failed registry-resolved builds.

Fix: pin github.com/envplane/contracts v0.1.107 and tidy the module. GOWORK=off go test ./... and GOWORK=off go vet ./... passed. Keep unrelated untracked files untouched.

Implementation prompt: preserve published module pins, verify isolated builds rather than workspace-only tests, wait for successful hosted CI and any image publication before including this component in an umbrella release.

