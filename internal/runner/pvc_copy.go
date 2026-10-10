package runner

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/envplane/contracts/domain"
	"github.com/envplane/runner/internal/pvccopy"
)

func pvcCopyRunnerContractVersion(cfg runnerConfig) string {
	parts := strings.Split(cfg.PVCCopyHelperImage, "@sha256:")
	if len(parts) != 2 || parts[0] != "ghcr.io/envplane/runner" || len(parts[1]) != 64 || cfg.PVCCopyRunnerServiceAccount == "" {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	for _, ns := range cfg.PVCCopySourceNamespaces {
		if strings.TrimSpace(ns) != "" {
			return domain.PVCCopyContractVersion
		}
	}
	return ""
}

func executeRunnerPVCCopy(ctx context.Context, cfg runnerConfig, command domain.RunnerCommand, namespaceAllowed func(string) bool) error {
	plan := command.PVCCopyPlan
	if plan == nil || plan.Validate() != nil || pvcCopyRunnerContractVersion(cfg) != domain.PVCCopyContractVersion {
		return fmt.Errorf("PVC copy contract/configuration unavailable")
	}
	tenant := domain.NormalizeTenantID(command.Environment.TenantID)
	if tenant != domain.DefaultTenantID || plan.TenantID != tenant || domain.NormalizeTenantID(command.ProjectConfig.TenantID) != tenant || command.ProjectID != cfg.ProjectID || command.Environment.Project != cfg.ProjectID || plan.ProjectID != cfg.ProjectID || plan.EnvironmentID != command.Environment.ID || plan.TargetNamespace != command.Environment.Namespace || command.Environment.DeploymentBackend != domain.DeploymentBackendFluxCD {
		return fmt.Errorf("PVC copy runtime identity mismatch")
	}
	if namespaceAllowed == nil || !namespaceAllowed(plan.TargetNamespace) {
		return fmt.Errorf("PVC copy target namespace denied")
	}
	for _, ns := range plan.AllowedSourceNamespaces {
		allowed := false
		for _, grant := range cfg.PVCCopySourceNamespaces {
			if strings.TrimSpace(grant) == ns {
				allowed = true
			}
		}
		if !allowed {
			return fmt.Errorf("PVC copy source namespace denied")
		}
	}
	if plan.Image != cfg.PVCCopyHelperImage || plan.MaxBytes > 1<<40 || plan.TimeoutSeconds > 300 || plan.StorageQuotaBytes > 1<<40 {
		return fmt.Errorf("PVC copy image/bounds denied")
	}
	session, ok := command.ProjectConfig.Config["bootstrapSessionData"].(map[string]any)
	if !ok {
		return fmt.Errorf("PVC copy compiled sources unavailable")
	}
	encoded, err := json.Marshal(session["pvcCopySources"])
	if err != nil {
		return err
	}
	var sources []domain.PVCCopySource
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sources); err != nil {
		return err
	}
	permission := domain.PVCCopyPermission{PlanID: plan.PlanID, TenantID: tenant, ProjectID: cfg.ProjectID, EnvironmentID: command.Environment.ID, TemplateRevisionID: command.ProjectConfig.TemplateRevisionID, TemplateDigest: command.ProjectConfig.TemplateDigest, TargetNamespace: command.Environment.Namespace, AllowedSourceNamespaces: plan.AllowedSourceNamespaces, AllowedImages: []string{cfg.PVCCopyHelperImage}, MaxBytes: 1 << 40, StorageQuotaBytes: 1 << 40, TimeoutSeconds: 300}
	if err := domain.ValidatePVCCopyPlan(*plan, sources, permission); err != nil {
		return err
	}
	transport := &pvccopy.Kubectl{RunnerImage: cfg.PVCCopyHelperImage, ServiceAccount: "default", AllowRootHelpers: cfg.PVCCopyAllowRootHelpers}
	executor := pvccopy.DomainExecutor{Kubectl: transport, Permission: permission, Sources: sources, Authorize: func(ctx context.Context, plan domain.PVCCopyPlan, _ []domain.PVCCopySource) error {
		if err := checkPVCCopyNamespaceOwner(ctx, command.Environment); err != nil {
			return err
		}
		return transport.RequireAdmissionFence(ctx, plan, cfg.RunnerNamespace, cfg.PVCCopyRunnerServiceAccount)
	}}
	markers, err := executor.ExecuteDomain(ctx, *plan, plan.AllowedSourceNamespaces, cfg.PVCCopyHelperImage)
	if err != nil {
		return err
	}
	if len(markers) != len(plan.Items) {
		return fmt.Errorf("PVC copy incomplete")
	}
	return nil
}

func checkPVCCopyNamespaceOwner(ctx context.Context, env domain.Environment) error {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "namespace", env.Namespace, "-o", "json", "--request-timeout=30s")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	raw, readErr := io.ReadAll(io.LimitReader(pipe, (1<<20)+1))
	if readErr != nil || len(raw) > 1<<20 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("namespace response unavailable")
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("namespace ownership unavailable")
	}
	var namespace struct {
		Metadata struct {
			Name              string            `json:"name"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
			Labels            map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &namespace); err != nil {
		return err
	}
	labels := namespace.Metadata.Labels
	managed := labels["envplane.io/managed-by"] == "same-cluster-project-reconciler" || labels["envplane.io/managed-by"] == "remote-project-reconciler" || labels["envplane.io/managed"] == "true"
	if namespace.Metadata.Name != env.Namespace || namespace.Metadata.DeletionTimestamp != nil || labels["envplane.io/project-id"] != env.Project || labels["envplane.io/environment-id"] != env.ID || !managed {
		return fmt.Errorf("PVC copy namespace not exclusively environment owned")
	}
	return nil
}
