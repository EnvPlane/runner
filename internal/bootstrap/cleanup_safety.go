package bootstrap

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/envpilot/contracts/domain"
)

type CleanupSafetyConfig struct {
	ProtectedNamespaces       []string
	DeleteEnvPlaneLabeledOnly bool
	FinalizerStrategy         string
}

const (
	EnvPlaneManagedByLabel     = "app.kubernetes.io/managed-by"
	EnvPlaneManagedLabel       = "envpilot.io/managed"
	EnvPlaneProjectLabel       = "envpilot.io/project"
	EnvPlaneEnvironmentIDLabel = "envpilot.io/environment-id"
)

var defaultProtectedNamespaces = []string{
	"default",
	"kube-node-lease",
	"kube-public",
	"kube-system",
	"flux-system",
	"cert-manager",
}

func DefaultCleanupSafetyConfig() CleanupSafetyConfig {
	return CleanupSafetyConfig{
		ProtectedNamespaces:       append([]string{}, defaultProtectedNamespaces...),
		DeleteEnvPlaneLabeledOnly: true,
		FinalizerStrategy:         "foreground",
	}
}

func ValidateCleanupSafetyConfig(config CleanupSafetyConfig, targetNamespaces []string) error {
	protected := normalizeNamespaceList(config.ProtectedNamespaces)
	if len(protected) == 0 {
		return fmt.Errorf("protected namespaces list must not be empty")
	}
	if !config.DeleteEnvPlaneLabeledOnly {
		return fmt.Errorf("cleanup must delete only resources with EnvPlane labels")
	}
	switch normalizeFinalizerStrategy(config.FinalizerStrategy) {
	case "none", "foreground", "orphan":
	default:
		return fmt.Errorf("finalizer strategy must be one of: none, foreground, orphan")
	}

	protectedSet := make(map[string]struct{}, len(protected))
	for _, namespace := range protected {
		protectedSet[namespace] = struct{}{}
	}
	for _, namespace := range normalizeNamespaceList(targetNamespaces) {
		if _, ok := protectedSet[namespace]; ok {
			return fmt.Errorf("protected namespace %q cannot be targeted by cleanup", namespace)
		}
	}
	return nil
}

func FilterCleanupEligibleResources(resources []domain.ResourceSnapshot, config CleanupSafetyConfig) []domain.ResourceSnapshot {
	return FilterCleanupEligibleResourcesForEnvironment(resources, config, "", "")
}

func FilterCleanupEligibleResourcesForEnvironment(resources []domain.ResourceSnapshot, config CleanupSafetyConfig, projectID string, environmentID string) []domain.ResourceSnapshot {
	protected := make(map[string]struct{}, len(config.ProtectedNamespaces))
	for _, namespace := range normalizeNamespaceList(config.ProtectedNamespaces) {
		protected[namespace] = struct{}{}
	}
	filtered := make([]domain.ResourceSnapshot, 0, len(resources))
	for _, resource := range resources {
		if _, ok := protected[strings.TrimSpace(resource.Namespace)]; ok {
			continue
		}
		if config.DeleteEnvPlaneLabeledOnly && !IsEnvPlaneManaged(resource, projectID, environmentID) {
			continue
		}
		filtered = append(filtered, resource)
	}
	return filtered
}

func IsEnvPlaneManaged(resource domain.ResourceSnapshot, projectID string, environmentID string) bool {
	labels := resource.Labels
	if !hasEnvPlaneManagedLabel(labels) {
		return false
	}
	projectID = strings.TrimSpace(projectID)
	if projectID != "" && strings.TrimSpace(labels[EnvPlaneProjectLabel]) != projectID {
		return false
	}
	environmentID = strings.TrimSpace(environmentID)
	if environmentID != "" && strings.TrimSpace(labels[EnvPlaneEnvironmentIDLabel]) != environmentID {
		return false
	}
	return true
}

func ValidateDeleteManagedResource(resource domain.ResourceSnapshot, config CleanupSafetyConfig, projectID string, environmentID string) error {
	if IsProtectedNamespace(resource.Namespace, config.ProtectedNamespaces) {
		return fmt.Errorf("protected namespace %q cannot be targeted by cleanup", strings.TrimSpace(resource.Namespace))
	}
	if !IsEnvPlaneManaged(resource, projectID, environmentID) {
		return fmt.Errorf("%w: %s %s/%s", ErrResourceNotEnvPlaneManaged, strings.TrimSpace(resource.Kind), strings.TrimSpace(resource.Namespace), strings.TrimSpace(resource.Name))
	}
	return nil
}

func ValidateModifyManagedResource(existing domain.ResourceSnapshot, projectID string, environmentID string) error {
	if !IsEnvPlaneManaged(existing, projectID, environmentID) {
		return fmt.Errorf("%w: %s %s/%s", ErrResourceNotEnvPlaneManaged, strings.TrimSpace(existing.Kind), strings.TrimSpace(existing.Namespace), strings.TrimSpace(existing.Name))
	}
	return nil
}

func ShouldDeleteManagedResource(resource domain.ResourceSnapshot, config CleanupSafetyConfig, projectID string, environmentID string) bool {
	return ValidateDeleteManagedResource(resource, config, projectID, environmentID) == nil
}

func ShouldModifyManagedResource(existing domain.ResourceSnapshot, projectID string, environmentID string) bool {
	return ValidateModifyManagedResource(existing, projectID, environmentID) == nil
}

func IsProtectedNamespace(namespace string, protectedNamespaces []string) bool {
	normalized := strings.TrimSpace(namespace)
	if normalized == "" {
		return false
	}
	for _, protected := range normalizeNamespaceList(protectedNamespaces) {
		if normalized == protected {
			return true
		}
	}
	return false
}

func normalizeNamespaceList(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	sort.Strings(result)
	return result
}

func normalizeFinalizerStrategy(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return "foreground"
	}
	return trimmed
}

func hasEnvPlaneManagedLabel(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(labels[EnvPlaneManagedByLabel]), "envpilot") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(labels[EnvPlaneManagedLabel]), "true") {
		return true
	}
	return strings.TrimSpace(labels[EnvPlaneEnvironmentIDLabel]) != ""
}

var ErrResourceNotEnvPlaneManaged = errors.New("resource is not managed by EnvPlane")
