package sandbox

import (
	"strings"
	"testing"
	"time"

	"github.com/envplane/contracts/domain"
)

func validSpec() domain.AISandboxSpec {
	return domain.AISandboxSpec{SchemaVersion: domain.AISandboxSchemaVersion, RunID: "run-1", TenantID: "tenant-a", ProjectID: "project-a", Transform: domain.AISandboxExtractMetrics, Image: "registry.example/analysis@sha256:" + strings.Repeat("a", 64), Inputs: []domain.AISignedInputRef{{ID: "input-1", Digest: "sha256:" + strings.Repeat("b", 64), Signature: "signature", KeyID: "key-1"}}, NetworkDenied: true, Resources: domain.AISandboxResourceLimits{CPUMillis: 100, MemoryMiB: 64, PIDs: 8, TimeoutSeconds: 30, MaxOutputBytes: 1024}, CreatedAt: time.Now().UTC()}
}

func TestValidateLaunchRequiresSandboxSecurityBoundary(t *testing.T) {
	spec := validSpec()
	policy := RuntimePolicy{RunAsNonRoot: true, ReadOnlyRootFilesystem: true, SeccompProfile: "RuntimeDefault", MaxPIDs: 32, MaxTimeoutSeconds: 60}
	if err := ValidateLaunch(spec, policy); err != nil {
		t.Fatal(err)
	}
	policy.AllowPrivilegeEscalation = true
	if err := ValidateLaunch(spec, policy); err == nil {
		t.Fatal("privilege escalation accepted")
	}
	policy.AllowPrivilegeEscalation = false
	spec.Resources.PIDs = 33
	if err := ValidateLaunch(spec, policy); err == nil {
		t.Fatal("fork-bomb resource limit exceeded")
	}
}
