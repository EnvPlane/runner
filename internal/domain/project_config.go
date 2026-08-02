package domain

import (
	"fmt"
	"strings"
	"time"
)

const (
	DeploymentBackendHelmDirect     = "helm_direct"
	DeploymentBackendFluxCD         = "fluxcd"
	DeploymentBackendGitOpsManifest = "gitops_manifest"
)

type DeploymentBackend string

type ProjectDeploymentConfig struct {
	Backend    DeploymentBackend        `json:"backend"`
	HelmDirect *ProjectHelmDirectConfig `json:"helmDirect,omitempty"`
	FluxCD     *ProjectFluxCDConfig     `json:"fluxcd,omitempty"`
}

type ProjectHelmDirectConfig struct {
	NamespaceMode          string `json:"namespaceMode"`
	NamespacePattern       string `json:"namespacePattern"`
	ReleaseNamePattern     string `json:"releaseNamePattern"`
	ChartRef               string `json:"chartRef"`
	ChartVersion           string `json:"chartVersion,omitempty"`
	Timeout                int    `json:"timeout"`
	Wait                   bool   `json:"wait"`
	CreateNamespace        bool   `json:"createNamespace"`
	ValuesOverrideStrategy string `json:"valuesOverrideStrategy"`
	ImageTagValuePath      string `json:"imageTagValuePath"`
}

type ProjectFluxCDConfig struct {
	GitopsRepo        string `json:"gitopsRepo"`
	GitopsPath        string `json:"gitopsPath"`
	FluxNamespace     string `json:"fluxNamespace"`
	KustomizationName string `json:"kustomizationName"`
	CommitMode        string `json:"commitMode"`
}

// InferDeploymentBackend normalizes and infers the effective deployment backend.
// If backend is missing and Flux/GitOps-specific fields are present, it falls back
// to fluxcd; otherwise it defaults to helm_direct.
func InferDeploymentBackend(rawBackend any, rawDeployment map[string]any, legacy map[string]any) DeploymentBackend {
	value := strings.ToLower(strings.TrimSpace(asString(rawBackend)))
	switch value {
	case "", string(DeploymentBackendHelmDirect), "helm-direct":
		if value == "" && hasFluxDeploymentFields(rawDeployment, legacy) {
			return DeploymentBackendFluxCD
		}
		return DeploymentBackendHelmDirect
	case string(DeploymentBackendFluxCD), "flux", "flux_cd":
		return DeploymentBackendFluxCD
	case string(DeploymentBackendGitOpsManifest), "gitops-manifest":
		return DeploymentBackendGitOpsManifest
	default:
		return DeploymentBackend(value)
	}
}

func hasFluxDeploymentFields(rawDeployment map[string]any, legacy map[string]any) bool {
	if fluxcd, ok := rawDeployment["fluxcd"].(map[string]any); ok && len(fluxcd) > 0 {
		return true
	}
	return hasAnyNonEmptyValues(rawDeployment, "gitopsPath", "fluxNamespace", "kustomizationName", "commitMode", "fluxcd") ||
		hasAnyNonEmptyValues(rawDeployment, "gitOpsOutputPath", "fluxKustomizationRef") ||
		hasAnyNonEmptyValues(legacy, "gitOpsOutputPath", "gitopsPath", "fluxNamespace", "fluxKustomizationRef", "kustomizationName", "gitOpsCommitMode", "commitMode")
}

func hasAnyNonEmptyValues(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := values[key]; ok && strings.TrimSpace(asString(value)) != "" {
			return true
		}
	}
	return false
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case nil:
		return ""
	default:
		return ""
	}
}

type ProjectConfig struct {
	ID            string         `json:"id" db:"id"`
	ProjectID     string         `json:"project_id" db:"project_id"`
	Version       int            `json:"version" db:"version"`
	Config        map[string]any `json:"config,omitempty" db:"config"`
	Sensitive     map[string]any `json:"sensitive,omitempty" db:"sensitive"`
	SensitiveRefs map[string]any `json:"sensitive_refs,omitempty"`
	CreatedBy     string         `json:"created_by" db:"created_by"`
	CreatedAt     time.Time      `json:"created_at" db:"created_at"`
}
