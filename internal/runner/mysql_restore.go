package runner

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/envplane/contracts/domain"
	"github.com/envplane/runner/internal/mysqlcopy"
)

func mysqlRestoreFailureMessage(err error) string {
	stage, code := mysqlcopy.FailureInfo(err)
	stages := mysqlcopy.FailureStages(err)
	labels := make([]string, 0, len(stages))
	for _, value := range stages {
		labels = append(labels, string(value))
	}
	if len(labels) == 0 {
		labels = append(labels, string(stage))
	}
	return fmt.Sprintf("MySQL restore was not verified; source preserved. Stages: %s; code: %s. Review protected backup, TLS, source identity, helper permissions and generated target credentials.", strings.Join(labels, ","), code)
}

func mysqlRestoreRunnerContractVersion(cfg runnerConfig) string {
	parts := strings.Split(cfg.MySQLRestoreHelperImage, "@sha256:")
	if len(parts) != 2 || parts[0] != "ghcr.io/envplane/runner" || len(parts[1]) != 64 || cfg.PVCCopyRunnerServiceAccount == "" || !cfg.MySQLRestoreAllowRootInit {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	if len(cfg.MySQLRestoreSourceNamespaces) == 0 || len(cfg.MySQLRestoreTargetImages) == 0 {
		return ""
	}
	for _, namespace := range cfg.MySQLRestoreSourceNamespaces {
		if strings.TrimSpace(namespace) == "" {
			return ""
		}
		matches := 0
		for _, fence := range cfg.MySQLRestoreSourceFences {
			if fence.Namespace == namespace && fence.PolicyUID != "" && fence.BindingUID != "" && len(fence.PolicySpecSHA256) == 64 && len(fence.BindingSpecSHA256) == 64 {
				matches++
			}
		}
		if matches != 1 {
			return ""
		}
	}
	for _, image := range cfg.MySQLRestoreTargetImages {
		parts := strings.Split(image, "@sha256:")
		if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 {
			return ""
		}
		if _, err := hex.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	return domain.MySQLRestoreContractVersion
}

func executeRunnerMySQLRestore(ctx context.Context, cfg runnerConfig, command domain.RunnerCommand, namespaceAllowed func(string) bool) error {
	plan := command.MySQLRestorePlan
	if plan == nil || plan.Validate() != nil || mysqlRestoreRunnerContractVersion(cfg) != domain.MySQLRestoreContractVersion {
		return fmt.Errorf("MySQL restore contract/configuration unavailable")
	}
	tenant := domain.NormalizeTenantID(command.Environment.TenantID)
	if tenant != domain.DefaultTenantID || plan.TenantID != tenant || domain.NormalizeTenantID(command.ProjectConfig.TenantID) != tenant || command.ProjectID != cfg.ProjectID || command.Environment.Project != cfg.ProjectID || plan.ProjectID != cfg.ProjectID || plan.EnvironmentID != command.Environment.ID || !plan.EnvironmentCreatedAt.Equal(command.Environment.CreatedAt) || plan.TargetNamespace != command.Environment.Namespace || command.Environment.DeploymentBackend != domain.DeploymentBackendFluxCD || command.ProjectConfigVersion != command.ProjectConfig.Version {
		return fmt.Errorf("MySQL restore runtime identity mismatch")
	}
	if namespaceAllowed == nil || !namespaceAllowed(plan.TargetNamespace) {
		return fmt.Errorf("MySQL restore target namespace denied")
	}
	if plan.HelperImage != cfg.MySQLRestoreHelperImage || !containsMySQLRestoreValue(cfg.MySQLRestoreTargetImages, plan.TargetImage) || plan.MaxBytes > 1<<40 || plan.StorageQuotaBytes > 1<<40 || plan.TimeoutSeconds > 300 {
		return fmt.Errorf("MySQL restore images/bounds denied")
	}
	for _, namespace := range plan.AllowedSourceNamespaces {
		if !containsMySQLRestoreValue(cfg.MySQLRestoreSourceNamespaces, namespace) {
			return fmt.Errorf("MySQL restore source namespace denied")
		}
	}
	session, _ := command.ProjectConfig.Config["bootstrapSessionData"].(map[string]any)
	encoded, err := json.Marshal(session["mysqlRestoreSources"])
	if err != nil {
		return err
	}
	var sources []domain.MySQLRestoreSource
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sources); err != nil {
		return fmt.Errorf("MySQL restore source projection unavailable")
	}
	permission := domain.MySQLRestorePermission{PlanID: plan.PlanID, TenantID: tenant, ProjectID: cfg.ProjectID, EnvironmentID: command.Environment.ID, EnvironmentCreatedAt: command.Environment.CreatedAt, TemplateRevisionID: command.ProjectConfig.TemplateRevisionID, TemplateDigest: command.ProjectConfig.TemplateDigest, TargetNamespace: command.Environment.Namespace, AllowedSourceNamespaces: plan.AllowedSourceNamespaces, AllowedHelperImages: []string{cfg.MySQLRestoreHelperImage}, AllowedTargetImages: cfg.MySQLRestoreTargetImages, MaxBytes: 1 << 40, TimeoutSeconds: 300, StorageQuotaBytes: 1 << 40}
	permission.ApprovedProductionSources = cfg.PVCCopyApprovedProductionSources
	for _, source := range sources {
		if source.BackupAdminSecretRef != nil {
			permission.ApprovedBackupAdminSecrets = append(permission.ApprovedBackupAdminSecrets, *source.BackupAdminSecretRef)
		}
	}
	for _, item := range plan.Items {
		if item.TargetRootPasswordKey != "MYSQL_ROOT_PASSWORD" {
			return fmt.Errorf("MySQL restore requires independently generated administrator credentials")
		}
		permission.TargetSecrets = append(permission.TargetSecrets, domain.MySQLRestoreTargetSecret{Name: item.TargetSecretName, UID: item.TargetSecretUID, PasswordKey: item.TargetPasswordKey, Username: item.TargetUsername, SecretMaterializationPlanDigest: item.SecretMaterializationPlanDigest})
		permission.TargetSecrets[len(permission.TargetSecrets)-1].RootPasswordKey = item.TargetRootPasswordKey
	}
	if err := domain.ValidateMySQLRestorePlan(*plan, sources, permission); err != nil {
		return err
	}
	for _, item := range plan.Items {
		if item.DDLMode != "backup_lock" || item.Source.BackupAdminSecretRef == nil {
			return fmt.Errorf("MySQL restore requires a protected online backup, not a job-provided quiescence flag")
		}
		credential := func(ref domain.MySQLRestoreCredentialRef) mysqlcopy.CredentialRef {
			return mysqlcopy.CredentialRef{Ref: mysqlcopy.Ref{Namespace: ref.Namespace, Name: ref.Name, UID: ref.UID}, Username: ref.Username, UsernameKey: ref.UsernameKey, PasswordKey: ref.PasswordKey}
		}
		p := mysqlcopy.Plan{Binding: plan.PlanID + "/" + item.ID, Tenant: tenant, Project: cfg.ProjectID, Environment: command.Environment.ID, EnvironmentCreatedAt: plan.EnvironmentCreatedAt, DomainPlanDigest: plan.Digest, SecretMaterializationPlanDigest: item.SecretMaterializationPlanDigest, ConfigImage: plan.HelperImage, DDLMode: item.DDLMode, MaxBytes: item.MaxBytes, MaxStatementBytes: item.MaxStatementBytes, MaxTables: item.MaxTables, MaxRows: item.MaxRows, Timeout: time.Duration(item.TimeoutSeconds) * time.Second,
			Source: mysqlcopy.Source{PVC: mysqlcopy.Ref{Namespace: item.Source.Namespace, Name: item.Source.PVCName, UID: item.Source.PVCUID}, StatefulSet: mysqlcopy.Ref{Namespace: item.Source.Namespace, Name: item.Source.WorkloadName, UID: item.Source.WorkloadUID}, Container: item.Source.Container, Image: item.Source.SourceImage, Service: item.Source.Service, ServiceUID: item.Source.ServiceUID, Database: item.Source.Database, Port: item.Source.Port, Credentials: credential(domain.MySQLRestoreCredentialRef{Namespace: item.Source.Namespace, Name: item.Source.SecretName, UID: item.Source.SecretUID, Username: item.Source.Username, UsernameKey: item.Source.UsernameKey, PasswordKey: item.Source.PasswordKey}), BackupAdminSecretRef: credential(*item.Source.BackupAdminSecretRef), TLSCASecret: mysqlcopy.Ref{Namespace: item.Source.Namespace, Name: item.Source.TLS.CASecretName, UID: item.Source.TLS.CASecretUID}, TLSCAKey: item.Source.TLS.CAKey, TLSServerName: item.Source.TLS.ServerName},
			Target: mysqlcopy.Target{Namespace: plan.TargetNamespace, PVCName: item.TargetPVCName, StorageClass: item.StorageClass, Image: plan.TargetImage, Database: item.TargetDatabase, RequestedBytes: item.RequestedBytes, Credentials: credential(domain.MySQLRestoreCredentialRef{Namespace: plan.TargetNamespace, Name: item.TargetSecretName, UID: item.TargetSecretUID, Username: item.TargetUsername, PasswordKey: item.TargetPasswordKey})}}
		p.Target.RootPasswordKey = item.TargetRootPasswordKey
		authority := func(ctx context.Context, _ mysqlcopy.Plan, _ mysqlcopy.Stage) error {
			return checkPVCCopyNamespaceOwner(ctx, command.Environment)
		}
		driver := mysqlcopy.NewProofDriver(mysqlcopy.DriverConfig{RunnerImage: cfg.MySQLRestoreHelperImage, MySQLImage: plan.TargetImage, ServiceAccount: "default", AllowRootInit: cfg.MySQLRestoreAllowRootInit, SourceAdmissionFences: cfg.MySQLRestoreSourceFences, Authorize: authority})
		transport := &mysqlcopy.KubeTransport{Driver: driver, SourceNamespaces: cfg.MySQLRestoreSourceNamespaces, RunnerImage: cfg.MySQLRestoreHelperImage, MySQLImage: plan.TargetImage}
		if _, err := (mysqlcopy.DomainExecutor{Transport: transport, Authority: authority}).ExecuteDomain(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func containsMySQLRestoreValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
