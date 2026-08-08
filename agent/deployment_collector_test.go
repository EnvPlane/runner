package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/envpilot/contracts/domain"
)

func TestDeploymentStatusCollectorReportsReadyWhenDeploymentsAndPodsReady(t *testing.T) {
	replicas := int32(2)
	report := BuildWorkloadStatusReport([]Deployment{
		{
			Metadata: DeploymentMetadata{Name: "cms-api"},
			Spec:     DeploymentSpec{Replicas: &replicas},
			Status: DeploymentStatus{
				ReadyReplicas:     2,
				AvailableReplicas: 2,
			},
		},
	}, []Pod{
		{
			Metadata: PodMetadata{Name: "cms-api-abc"},
			Status: PodStatus{
				Phase:      "Running",
				Conditions: []PodCondition{{Type: "Ready", Status: "True"}},
			},
		},
	}, []Ingress{
		{
			Metadata: IngressMetadata{Name: "preview"},
			Spec:     IngressSpec{Rules: []IngressRule{{Host: "kan-403.preview.local"}}},
			Status:   IngressStatus{LoadBalancer: IngressLoadBalancerStatus{Ingress: []LoadBalancerIngress{{Hostname: "lb.local"}}}},
		},
	})

	if report.Status != domain.StatusReady {
		t.Fatalf("status = %q", report.Status)
	}
	if !strings.Contains(report.Message, "deployment/cms-api ready=2/2") {
		t.Fatalf("message does not include deployment readiness: %s", report.Message)
	}
	if !strings.Contains(report.Message, "pod/cms-api-abc phase=Running ready=true") {
		t.Fatalf("message does not include pod readiness: %s", report.Message)
	}
	if !strings.Contains(report.Message, "ingress/preview available=true") {
		t.Fatalf("message does not include ingress readiness: %s", report.Message)
	}
}

func TestDeploymentStatusCollectorRequiresEveryPodReadyAndIngressAvailable(t *testing.T) {
	replicas := int32(2)
	report := BuildWorkloadStatusReport([]Deployment{
		{
			Metadata: DeploymentMetadata{Name: "cms-api"},
			Spec:     DeploymentSpec{Replicas: &replicas},
			Status: DeploymentStatus{
				ReadyReplicas:     2,
				AvailableReplicas: 2,
			},
		},
	}, []Pod{
		{
			Metadata: PodMetadata{Name: "cms-api-ready"},
			Status: PodStatus{
				Phase:      "Running",
				Conditions: []PodCondition{{Type: "Ready", Status: "True"}},
			},
		},
		{
			Metadata: PodMetadata{Name: "cms-api-pending"},
			Status: PodStatus{
				Phase:      "Running",
				Conditions: []PodCondition{{Type: "Ready", Status: "False"}},
			},
		},
	}, []Ingress{
		{
			Metadata: IngressMetadata{Name: "preview"},
			Spec:     IngressSpec{Rules: []IngressRule{{Host: "kan-403.preview.local"}}},
			Status:   IngressStatus{LoadBalancer: IngressLoadBalancerStatus{}},
		},
	})

	if report.Status != domain.StatusCreating {
		t.Fatalf("status = %q", report.Status)
	}
	if !strings.Contains(report.Message, "pod/cms-api-pending phase=Running ready=false") {
		t.Fatalf("message does not include unready pod: %s", report.Message)
	}
	if !strings.Contains(report.Message, "ingress/preview available=false") {
		t.Fatalf("message does not include unavailable ingress: %s", report.Message)
	}
}

func TestDeploymentStatusCollectorReportsFailedOnPodCrashLoop(t *testing.T) {
	replicas := int32(1)
	report := BuildWorkloadStatusReport([]Deployment{
		{
			Metadata: DeploymentMetadata{Name: "cms-api"},
			Spec:     DeploymentSpec{Replicas: &replicas},
			Status: DeploymentStatus{
				ReadyReplicas:     0,
				AvailableReplicas: 0,
			},
		},
	}, []Pod{
		{
			Metadata: PodMetadata{Name: "cms-api-abc"},
			Status: PodStatus{
				Phase: "Running",
				ContainerStatuses: []ContainerStatus{
					{
						Name: "api",
						State: ContainerState{
							Waiting: &ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off restarting failed container"},
						},
					},
				},
			},
		},
	}, []Ingress{{Metadata: IngressMetadata{Name: "preview"}}})

	if report.Status != domain.StatusFailed {
		t.Fatalf("status = %q", report.Status)
	}
	if !strings.Contains(report.Message, "pod/cms-api-abc") || !strings.Contains(report.Message, "CrashLoopBackOff") {
		t.Fatalf("message does not include failed pod status: %s", report.Message)
	}
}

func TestDeploymentStatusCollectorUsesKubernetesSource(t *testing.T) {
	source := &fakeWorkloadSource{
		deployments: []Deployment{{Metadata: DeploymentMetadata{Name: "cms-api"}}},
		pods:        []Pod{{Metadata: PodMetadata{Name: "cms-api-abc"}, Status: PodStatus{Phase: "Pending"}}},
		ingresses:   []Ingress{{Metadata: IngressMetadata{Name: "preview"}}},
	}
	collector := NewDeploymentStatusCollector(source)

	report, err := collector.Collect(context.Background(), "envpilot-pr-kan-403")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if source.namespace != "envpilot-pr-kan-403" {
		t.Fatalf("namespace = %q", source.namespace)
	}
	if report.Status != domain.StatusCreating {
		t.Fatalf("status = %q", report.Status)
	}
}

type fakeWorkloadSource struct {
	namespace   string
	deployments []Deployment
	pods        []Pod
	ingresses   []Ingress
}

func (f *fakeWorkloadSource) ListDeployments(_ context.Context, namespace string) ([]Deployment, error) {
	f.namespace = namespace
	return f.deployments, nil
}

func (f *fakeWorkloadSource) ListPods(_ context.Context, namespace string) ([]Pod, error) {
	f.namespace = namespace
	return f.pods, nil
}

func (f *fakeWorkloadSource) ListIngresses(_ context.Context, namespace string) ([]Ingress, error) {
	f.namespace = namespace
	return f.ingresses, nil
}
