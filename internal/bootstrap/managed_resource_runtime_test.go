package bootstrap

import (
	"errors"
	"testing"

	"github.com/envpilot/contracts/domain"
)

func TestManagedResourceRuntimeBlocksUnlabeledApplyUpdateAndDeleteForAllRunnerKinds(t *testing.T) {
	kinds := []string{
		"Deployment",
		"Service",
		"Ingress",
		"ConfigMap",
		"Secret",
		"NetworkPolicy",
		"HelmRelease",
		"Kustomization",
		"GitRepository",
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			config := DefaultCleanupSafetyConfig()
			runtime := NewManagedResourceRuntime(
				[]domain.ResourceSnapshot{resourceSnapshot(kind, "manual", nil)},
				config,
				"checkout",
				"pr-123",
			)

			if err := runtime.Apply(resourceSnapshot(kind, "new-manual", nil)); !errors.Is(err, ErrResourceNotEnvPlaneManaged) {
				t.Fatalf("unlabeled %s create should be rejected, got %v", kind, err)
			}
			if _, ok := runtime.Get(kind, "envpilot-pr-123", "new-manual"); ok {
				t.Fatalf("unlabeled %s was created", kind)
			}

			if err := runtime.Apply(resourceSnapshot(kind, "manual", envPilotLabels())); !errors.Is(err, ErrResourceNotEnvPlaneManaged) {
				t.Fatalf("unlabeled existing %s update should be rejected, got %v", kind, err)
			}
			existing, ok := runtime.Get(kind, "envpilot-pr-123", "manual")
			if !ok {
				t.Fatalf("manual %s disappeared", kind)
			}
			if existing.Labels[EnvPlaneManagedLabel] == "true" {
				t.Fatalf("unlabeled existing %s was modified", kind)
			}

			if err := runtime.Delete(kind, "envpilot-pr-123", "manual"); !errors.Is(err, ErrResourceNotEnvPlaneManaged) {
				t.Fatalf("unlabeled existing %s delete should be rejected, got %v", kind, err)
			}
			if _, ok := runtime.Get(kind, "envpilot-pr-123", "manual"); !ok {
				t.Fatalf("unlabeled existing %s was deleted", kind)
			}
		})
	}
}

func TestManagedResourceRuntimeAllowsEnvPlaneLabeledApplyUpdateAndDeleteForAllRunnerKinds(t *testing.T) {
	kinds := []string{
		"Deployment",
		"Service",
		"Ingress",
		"ConfigMap",
		"Secret",
		"NetworkPolicy",
		"HelmRelease",
		"Kustomization",
		"GitRepository",
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			config := DefaultCleanupSafetyConfig()
			runtime := NewManagedResourceRuntime(nil, config, "checkout", "pr-123")

			if err := runtime.Apply(resourceSnapshot(kind, "orders", envPilotLabels())); err != nil {
				t.Fatalf("EnvPlane-labeled %s create should be allowed: %v", kind, err)
			}
			if _, ok := runtime.Get(kind, "envpilot-pr-123", "orders"); !ok {
				t.Fatalf("EnvPlane-labeled %s was not created", kind)
			}

			updated := resourceSnapshot(kind, "orders", envPilotLabels())
			updated.Annotations = map[string]string{"envpilot.io/test-update": "true"}
			if err := runtime.Apply(updated); err != nil {
				t.Fatalf("EnvPlane-labeled %s update should be allowed: %v", kind, err)
			}
			existing, ok := runtime.Get(kind, "envpilot-pr-123", "orders")
			if !ok {
				t.Fatalf("EnvPlane-labeled %s disappeared", kind)
			}
			if existing.Annotations["envpilot.io/test-update"] != "true" {
				t.Fatalf("EnvPlane-labeled %s was not updated: %+v", kind, existing)
			}

			if err := runtime.Delete(kind, "envpilot-pr-123", "orders"); err != nil {
				t.Fatalf("EnvPlane-labeled %s delete should be allowed: %v", kind, err)
			}
			if _, ok := runtime.Get(kind, "envpilot-pr-123", "orders"); ok {
				t.Fatalf("EnvPlane-labeled %s was not deleted", kind)
			}
		})
	}
}

func resourceSnapshot(kind string, name string, labels map[string]string) domain.ResourceSnapshot {
	return domain.ResourceSnapshot{
		Kind:      kind,
		Namespace: "envpilot-pr-123",
		Name:      name,
		Labels:    labels,
	}
}

func envPilotLabels() map[string]string {
	return map[string]string{
		EnvPlaneManagedByLabel:     "envpilot",
		EnvPlaneManagedLabel:       "true",
		EnvPlaneProjectLabel:       "checkout",
		EnvPlaneEnvironmentIDLabel: "pr-123",
	}
}
