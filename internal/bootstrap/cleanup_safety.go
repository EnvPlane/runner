package bootstrap

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"envpilot/internal/domain"
)

type CleanupSafetyConfig struct {
	ProtectedNamespaces       []string
	DeleteEnvPilotLabeledOnly bool
	FinalizerStrategy         string
}

const (
	EnvPilotManagedByLabel     = "app.kubernetes.io/managed-by"
	EnvPilotManagedLabel       = "envpilot.io/managed"
	EnvPilotProjectLabel       = "envpilot.io/project"
	EnvPilotEnvironmentIDLabel = "envpilot.io/environment-id"
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
		DeleteEnvPilotLabeledOnly: true,
		FinalizerStrategy:         "foreground",
	}
}

func ValidateCleanupSafetyConfig(config CleanupSafetyConfig, targetNamespaces []string) error {
	protected := normalizeNamespaceList(config.ProtectedNamespaces)
	if len(protected) == 0 {
		return fmt.Errorf("protected namespaces list must not be empty")
	}
	if !config.DeleteEnvPilotLabeledOnly {
		return fmt.Errorf("cleanup must delete only resources with EnvPilot labels")
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
		if config.DeleteEnvPilotLabeledOnly && !IsEnvPilotManaged(resource, projectID, environmentID) {
			continue
		}
		filtered = append(filtered, resource)
	}
	return filtered
}

func IsEnvPilotManaged(resource domain.ResourceSnapshot, projectID string, environmentID string) bool {
	labels := resource.Labels
	if !hasEnvPilotManagedLabel(labels) {
		return false
	}
	projectID = strings.TrimSpace(projectID)
	if projectID != "" && strings.TrimSpace(labels[EnvPilotProjectLabel]) != projectID {
		return false
	}
	environmentID = strings.TrimSpace(environmentID)
	if environmentID != "" && strings.TrimSpace(labels[EnvPilotEnvironmentIDLabel]) != environmentID {
		return false
	}
	return true
}

func ValidateDeleteManagedResource(resource domain.ResourceSnapshot, config CleanupSafetyConfig, projectID string, environmentID string) error {
	if IsProtectedNamespace(resource.Namespace, config.ProtectedNamespaces) {
		return fmt.Errorf("protected namespace %q cannot be targeted by cleanup", strings.TrimSpace(resource.Namespace))
	}
	if !IsEnvPilotManaged(resource, projectID, environmentID) {
		return fmt.Errorf("%w: %s %s/%s", ErrResourceNotEnvPilotManaged, strings.TrimSpace(resource.Kind), strings.TrimSpace(resource.Namespace), strings.TrimSpace(resource.Name))
	}
	return nil
}

func ValidateModifyManagedResource(existing domain.ResourceSnapshot, projectID string, environmentID string) error {
	if !IsEnvPilotManaged(existing, projectID, environmentID) {
		return fmt.Errorf("%w: %s %s/%s", ErrResourceNotEnvPilotManaged, strings.TrimSpace(existing.Kind), strings.TrimSpace(existing.Namespace), strings.TrimSpace(existing.Name))
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

func hasEnvPilotManagedLabel(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(labels[EnvPilotManagedByLabel]), "envpilot") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(labels[EnvPilotManagedLabel]), "true") {
		return true
	}
	return strings.TrimSpace(labels[EnvPilotEnvironmentIDLabel]) != ""
}

var ErrResourceNotEnvPilotManaged = errors.New("resource is not managed by EnvPilot")
