package agent

import (
	"context"
	"strings"
	"testing"

	"envpilot/internal/domain"
)

func TestFluxStatusCollectorReportsReadyReconciliation(t *testing.T) {
	status := BuildFluxStatus("kan-405", Namespace{
		Metadata: NamespaceMetadata{
			Name: "envpilot-pr-kan-405",
			Labels: map[string]string{
				"envpilot.io/product": "bethunder",
			},
		},
	}, []FluxKustomization{
		{
			Metadata: FluxMetadata{Name: "kan-405.bethunder", Namespace: "flux-system"},
			Status: FluxStatus{
				ObservedGeneration:  2,
				LastAppliedRevision: "main/abc123",
				Conditions: []FluxCondition{
					{Type: "Ready", Status: "True", Reason: "ReconciliationSucceeded", LastTransitionTime: "2026-05-01T10:00:00Z"},
				},
			},
		},
	}, []HelmRelease{
		{
			Metadata: FluxMetadata{Name: "nginx", Namespace: "envpilot-pr-kan-405"},
			Status: FluxStatus{
				ObservedGeneration: 1,
				Conditions: []FluxCondition{
					{Type: "Ready", Status: "True", Reason: "InstallSucceeded"},
				},
			},
		},
	})

	if status.Status != domain.StatusReady {
		t.Fatalf("status = %q", status.Status)
	}
	if len(status.Kustomizations) != 1 || status.Kustomizations[0].LastAppliedRevision != "main/abc123" {
		t.Fatalf("kustomizations = %#v", status.Kustomizations)
	}
	if !strings.Contains(status.Message, "Kustomization/kan-405.bethunder ready=true") {
		t.Fatalf("message = %s", status.Message)
	}
}

func TestFluxStatusCollectorReportsFailedReconciliation(t *testing.T) {
	status := BuildFluxStatus("kan-405", Namespace{
		Metadata: NamespaceMetadata{Name: "envpilot-pr-kan-405"},
	}, []FluxKustomization{
		{
			Metadata: FluxMetadata{Name: "kan-405.bethunder", Namespace: "flux-system"},
			Status: FluxStatus{
				Conditions: []FluxCondition{
					{Type: "Ready", Status: "False", Reason: "BuildFailed", Message: "kustomize build failed"},
				},
			},
		},
	}, nil)

	if status.Status != domain.StatusFailed {
		t.Fatalf("status = %q", status.Status)
	}
	if !status.Kustomizations[0].Failed || status.Kustomizations[0].Reason != "BuildFailed" {
		t.Fatalf("kustomization status = %#v", status.Kustomizations[0])
	}
}

func TestFluxStatusCollectorUsesFluxSource(t *testing.T) {
	source := &fakeFluxSource{
		kustomizations: []FluxKustomization{
			{
				Metadata: FluxMetadata{Name: "kan-406.bethunder", Namespace: "flux-system"},
				Status:   FluxStatus{Conditions: []FluxCondition{{Type: "Ready", Status: "True"}}},
			},
		},
		helmReleases: []HelmRelease{
			{
				Metadata: FluxMetadata{Name: "nginx", Namespace: "envpilot-pr-kan-406"},
				Status:   FluxStatus{Conditions: []FluxCondition{{Type: "Ready", Status: "True"}}},
			},
		},
	}
	collector := NewFluxStatusCollector(source)

	status, err := collector.Collect(context.Background(), "kan-406", Namespace{
		Metadata: NamespaceMetadata{
			Name:   "envpilot-pr-kan-406",
			Labels: map[string]string{"envpilot.io/product": "bethunder"},
		},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if status.Status != domain.StatusReady {
		t.Fatalf("status = %q", status.Status)
	}
	if source.kustomizationNamespace != "flux-system" {
		t.Fatalf("kustomization namespace = %q", source.kustomizationNamespace)
	}
	if source.helmReleaseNamespace != "envpilot-pr-kan-406" {
		t.Fatalf("helm release namespace = %q", source.helmReleaseNamespace)
	}
}
