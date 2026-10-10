package runner

import "testing"

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
