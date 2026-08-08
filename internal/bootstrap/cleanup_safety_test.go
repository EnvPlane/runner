package bootstrap

import (
	"strings"
	"testing"

	"github.com/envpilot/runner/internal/domain"
)

func TestValidateCleanupSafetyBlocksProtectedNamespaceTarget(t *testing.T) {
	config := DefaultCleanupSafetyConfig()
	err := ValidateCleanupSafetyConfig(config, []string{"kube-system"})
	if err == nil {
		t.Fatalf("expected protected namespace error")
	}
	if !strings.Contains(err.Error(), "protected namespace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateCleanupSafetyRejectsDangerousLabelConfig(t *testing.T) {
	config := DefaultCleanupSafetyConfig()
	config.DeleteEnvPilotLabeledOnly = false
	err := ValidateCleanupSafetyConfig(config, []string{"envpilot-pr-123"})
	if err == nil {
		t.Fatalf("expected labels-only validation error")
	}
	if !strings.Contains(err.Error(), "EnvPilot labels") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFilterCleanupEligibleResourcesIgnoresProtectedAndUnlabeled(t *testing.T) {
	config := DefaultCleanupSafetyConfig()
	resources := []domain.ResourceSnapshot{
		{
			Kind:      "Deployment",
			Namespace: "envpilot-pr-123",
			Name:      "orders",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "envpilot",
			},
		},
		{
			Kind:      "Service",
			Namespace: "envpilot-pr-123",
			Name:      "manual-service",
			Labels:    map[string]string{"app": "manual"},
		},
		{
			Kind:      "ConfigMap",
			Namespace: "kube-system",
			Name:      "coredns",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "envpilot",
			},
		},
	}

	filtered := FilterCleanupEligibleResources(resources, config)
	if len(filtered) != 1 {
		t.Fatalf("expected one eligible resource, got %+v", filtered)
	}
	if filtered[0].Name != "orders" {
		t.Fatalf("expected orders resource, got %+v", filtered[0])
	}
}

func TestIsEnvPilotManagedRequiresOwnershipLabels(t *testing.T) {
	resource := domain.ResourceSnapshot{
		Kind:      "Deployment",
		Namespace: "envpilot-pr-123",
		Name:      "orders",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "envpilot",
			"envpilot.io/managed":          "true",
			"envpilot.io/project":          "checkout",
			"envpilot.io/environment-id":   "pr-123",
		},
	}
	if !IsEnvPilotManaged(resource, "checkout", "pr-123") {
		t.Fatalf("expected resource to be EnvPilot-managed")
	}
	if IsEnvPilotManaged(resource, "payments", "pr-123") {
		t.Fatalf("expected project mismatch to reject resource")
	}
	if IsEnvPilotManaged(resource, "checkout", "pr-999") {
		t.Fatalf("expected environment mismatch to reject resource")
	}
	if IsEnvPilotManaged(domain.ResourceSnapshot{Kind: "Deployment", Namespace: "envpilot-pr-123", Name: "manual"}, "checkout", "pr-123") {
		t.Fatalf("expected unlabeled resource to be rejected")
	}
}

func TestCleanupDeleteGuardSkipsUnlabeledDeploymentAndSecret(t *testing.T) {
	config := DefaultCleanupSafetyConfig()
	resources := []domain.ResourceSnapshot{
		{
			Kind:      "Deployment",
			Namespace: "envpilot-pr-123",
			Name:      "manual-orders",
			Labels:    map[string]string{"app": "orders"},
		},
		{
			Kind:      "Secret",
			Namespace: "envpilot-pr-123",
			Name:      "manual-secret",
			Labels:    map[string]string{"app": "orders"},
		},
		{
			Kind:      "Deployment",
			Namespace: "envpilot-pr-123",
			Name:      "orders",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "envpilot",
				"envpilot.io/managed":          "true",
				"envpilot.io/project":          "checkout",
				"envpilot.io/environment-id":   "pr-123",
			},
		},
		{
			Kind:      "Secret",
			Namespace: "envpilot-pr-123",
			Name:      "orders-secret",
			Labels: map[string]string{
				"envpilot.io/managed":        "true",
				"envpilot.io/project":        "checkout",
				"envpilot.io/environment-id": "pr-123",
			},
		},
	}

	filtered := FilterCleanupEligibleResourcesForEnvironment(resources, config, "checkout", "pr-123")
	if len(filtered) != 2 {
		t.Fatalf("expected only labeled resources to be cleanup eligible, got %+v", filtered)
	}
	for _, item := range filtered {
		if item.Name == "manual-orders" || item.Name == "manual-secret" {
			t.Fatalf("unlabeled resource was cleanup eligible: %+v", item)
		}
	}
	if ShouldDeleteManagedResource(resources[0], config, "checkout", "pr-123") {
		t.Fatalf("unlabeled Deployment must not be deleted")
	}
	if ShouldDeleteManagedResource(resources[1], config, "checkout", "pr-123") {
		t.Fatalf("unlabeled Secret must not be deleted")
	}
	if !ShouldDeleteManagedResource(resources[2], config, "checkout", "pr-123") {
		t.Fatalf("EnvPilot-labeled Deployment should be deleted")
	}
	if !ShouldDeleteManagedResource(resources[3], config, "checkout", "pr-123") {
		t.Fatalf("EnvPilot-labeled Secret should be deleted")
	}
}

func TestApplyGuardRejectsUnlabeledIngressAndAllowsEnvPilotLabeled(t *testing.T) {
	unlabeled := domain.ResourceSnapshot{
		Kind:      "Ingress",
		Namespace: "envpilot-pr-123",
		Name:      "orders",
		Labels:    map[string]string{"app": "orders"},
	}
	labeled := domain.ResourceSnapshot{
		Kind:      "Ingress",
		Namespace: "envpilot-pr-123",
		Name:      "orders",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "envpilot",
			"envpilot.io/project":          "checkout",
			"envpilot.io/environment-id":   "pr-123",
		},
	}

	if ShouldModifyManagedResource(unlabeled, "checkout", "pr-123") {
		t.Fatalf("unlabeled Ingress must not be modified")
	}
	if err := ValidateModifyManagedResource(unlabeled, "checkout", "pr-123"); err == nil {
		t.Fatalf("expected unlabeled Ingress modify error")
	}
	if !ShouldModifyManagedResource(labeled, "checkout", "pr-123") {
		t.Fatalf("EnvPilot-labeled Ingress should be modified")
	}
	if err := ValidateModifyManagedResource(labeled, "checkout", "pr-123"); err != nil {
		t.Fatalf("labeled Ingress should pass modify guard: %v", err)
	}
}
