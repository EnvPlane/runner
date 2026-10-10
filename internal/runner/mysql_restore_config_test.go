package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/envplane/contracts/domain"
	"github.com/envplane/runner/internal/mysqlcopy"
)

func TestMySQLRestoreResultNeverExposesRawFailure(t *testing.T) {
	message := mysqlRestoreFailureMessage(errors.New("password=private-value SELECT customer-data; kubectl body"))
	for _, sensitive := range []string{"private-value", "SELECT", "customer-data", "kubectl body"} {
		if strings.Contains(message, sensitive) {
			t.Fatal("raw failure escaped diagnostic boundary")
		}
	}
	if !strings.Contains(message, "Stages: unknown; code: operation_failed") {
		t.Fatal("missing bounded constant diagnostic")
	}
}

func TestMySQLRestoreTrustedConfigRejectsTrailingJSON(t *testing.T) {
	t.Setenv("ENVPLANE_MYSQL_RESTORE_SOURCE_FENCES", `[{"Namespace":"source","PolicyName":"fence"}] {}`)
	t.Setenv("ENVPLANE_PVC_COPY_APPROVED_PRODUCTION_SOURCES", `[{"Namespace":"source","Name":"data","UID":"uid"}] {}`)
	cfg := runnerConfigFromEnv()
	if len(cfg.MySQLRestoreSourceFences) != 0 || len(cfg.PVCCopyApprovedProductionSources) != 0 {
		t.Fatal("trusted configuration with trailing JSON admitted")
	}
}

func TestMySQLRestoreTrustedConfigDecodesSingleDocument(t *testing.T) {
	t.Setenv("ENVPLANE_MYSQL_RESTORE_SOURCE_FENCES", `[{"Namespace":"source","PolicyName":"fence"}]`)
	t.Setenv("ENVPLANE_PVC_COPY_APPROVED_PRODUCTION_SOURCES", `[{"namespace":"source","name":"data","uid":"uid"}]`)
	cfg := runnerConfigFromEnv()
	if len(cfg.MySQLRestoreSourceFences) != 1 || len(cfg.PVCCopyApprovedProductionSources) != 1 {
		t.Fatal("single trusted configuration document not decoded")
	}
}

func TestMySQLRestoreCapabilityRequiresReviewedAuthority(t *testing.T) {
	digest := strings.Repeat("a", 64)
	cfg := runnerConfig{
		MySQLRestoreHelperImage:      "ghcr.io/envplane/runner@sha256:" + digest,
		PVCCopyRunnerServiceAccount:  "project-runner",
		MySQLRestoreSourceNamespaces: []string{"source"},
		MySQLRestoreTargetImages:     []string{"mysql@sha256:" + digest},
		MySQLRestoreAllowRootInit:    true,
		MySQLRestoreSourceFences:     []mysqlcopy.AdmissionFenceRef{{Namespace: "source", PolicyUID: "policy-uid", BindingUID: "binding-uid", PolicySpecSHA256: digest, BindingSpecSHA256: digest}},
	}
	if mysqlRestoreRunnerContractVersion(cfg) != domain.MySQLRestoreContractVersion {
		t.Fatal("reviewed configuration not advertised")
	}
	for _, scenario := range []string{"no-fence", "duplicate-fence", "root-denied", "no-service-account", "mutable-image"} {
		t.Run(scenario, func(t *testing.T) {
			invalid := cfg
			switch scenario {
			case "no-fence":
				invalid.MySQLRestoreSourceFences = nil
			case "duplicate-fence":
				invalid.MySQLRestoreSourceFences = append([]mysqlcopy.AdmissionFenceRef{}, cfg.MySQLRestoreSourceFences...)
				invalid.MySQLRestoreSourceFences = append(invalid.MySQLRestoreSourceFences, cfg.MySQLRestoreSourceFences[0])
			case "root-denied":
				invalid.MySQLRestoreAllowRootInit = false
			case "no-service-account":
				invalid.PVCCopyRunnerServiceAccount = ""
			case "mutable-image":
				invalid.MySQLRestoreTargetImages = []string{"mysql:8"}
			}
			if mysqlRestoreRunnerContractVersion(invalid) != "" {
				t.Fatal("unreviewed capability advertised")
			}
		})
	}
	if executeRunnerMySQLRestore(context.Background(), cfg, domain.RunnerCommand{}, func(string) bool { return true }) == nil {
		t.Fatal("missing sealed plan admitted")
	}
}
