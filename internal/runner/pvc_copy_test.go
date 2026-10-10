package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/envplane/contracts/domain"
)

func TestPVCCopyCapabilityRequiresExplicitLocalConfiguration(t *testing.T) {
	for _, cfg := range []runnerConfig{{}, {PVCCopyHelperImage: "ghcr.io/envplane/runner:latest", PVCCopySourceNamespaces: []string{"base"}}, {PVCCopyHelperImage: "ghcr.io/envplane/runner@sha256:" + strings.Repeat("a", 64)}} {
		if pvcCopyRunnerContractVersion(cfg) != "" {
			t.Fatal("unconfigured copy advertised")
		}
	}
	cfg := runnerConfig{PVCCopyHelperImage: "ghcr.io/envplane/runner@sha256:" + strings.Repeat("a", 64), PVCCopySourceNamespaces: []string{"base"}, PVCCopyRunnerServiceAccount: "runner"}
	if pvcCopyRunnerContractVersion(cfg) != domain.PVCCopyContractVersion {
		t.Fatal("configured capability absent")
	}
}

func TestPVCCopyCommandWithoutImmutablePayloadIsRejected(t *testing.T) {
	cfg := runnerConfig{ProjectID: "project"}
	command := domain.RunnerCommand{Operation: domain.RunnerOperationMaterializePVC, ProjectID: "project", Environment: domain.Environment{ID: "env", Project: "project", Namespace: "feature"}}
	if err := executeRunnerPVCCopy(context.Background(), cfg, command, func(string) bool { return true }); err == nil {
		t.Fatal("missing payload accepted")
	}
}
