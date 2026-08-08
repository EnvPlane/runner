package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/envpilot/contracts/domain"
)

func TestNamespaceWatcherReportsEnvNamespaceStatus(t *testing.T) {
	source := &fakeNamespaceSource{
		namespaces: []Namespace{
			{
				Metadata: NamespaceMetadata{
					Name:   "envpilot-pr-kan-402",
					Labels: map[string]string{environmentIDLabel: "kan-402"},
				},
				Status: NamespaceStatus{Phase: "Active"},
			},
		},
	}
	reporter := &fakeStatusReporter{}
	watcher := NewNamespaceWatcher(source, reporter, time.Second, nil)

	if err := watcher.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync once: %v", err)
	}

	if len(reporter.reports) != 1 {
		t.Fatalf("reports = %d", len(reporter.reports))
	}
	report := reporter.reports[0]
	if report.EnvironmentID != "kan-402" {
		t.Fatalf("environment id = %q", report.EnvironmentID)
	}
	if report.Status != domain.StatusReady {
		t.Fatalf("status = %q", report.Status)
	}
}

func TestNamespaceWatcherReportsDeploymentCollectorStatus(t *testing.T) {
	source := &fakeNamespaceSource{
		namespaces: []Namespace{
			{
				Metadata: NamespaceMetadata{
					Name:   "envpilot-pr-kan-404",
					Labels: map[string]string{environmentIDLabel: "kan-404"},
				},
				Status: NamespaceStatus{Phase: "Active"},
			},
		},
	}
	workloads := &fakeWorkloadSource{
		deployments: []Deployment{
			{
				Metadata: DeploymentMetadata{Name: "cms-api"},
				Status: DeploymentStatus{
					ReadyReplicas:     0,
					AvailableReplicas: 0,
				},
			},
		},
		pods: []Pod{
			{
				Metadata: PodMetadata{Name: "cms-api-abc"},
				Status: PodStatus{
					Phase: "Running",
					ContainerStatuses: []ContainerStatus{{
						Name: "api",
						State: ContainerState{
							Waiting: &ContainerStateWaiting{Reason: "ImagePullBackOff"},
						},
					}},
				},
			},
		},
	}
	reporter := &fakeStatusReporter{}
	watcher := NewNamespaceWatcherWithCollector(source, reporter, NewDeploymentStatusCollector(workloads), time.Second, nil)

	if err := watcher.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync once: %v", err)
	}

	if len(reporter.reports) != 1 {
		t.Fatalf("reports = %d", len(reporter.reports))
	}
	report := reporter.reports[0]
	if report.Status != domain.StatusFailed {
		t.Fatalf("status = %q", report.Status)
	}
	if !strings.Contains(report.Message, "pod/cms-api-abc") || !strings.Contains(report.Message, "ImagePullBackOff") {
		t.Fatalf("message does not contain pod failure: %s", report.Message)
	}
}

func TestNamespaceWatcherReportsKubernetesEvents(t *testing.T) {
	source := &fakeNamespaceSource{
		namespaces: []Namespace{
			{
				Metadata: NamespaceMetadata{
					Name:   "envpilot-pr-kan-405",
					Labels: map[string]string{environmentIDLabel: "kan-405"},
				},
				Status: NamespaceStatus{Phase: "Active"},
			},
		},
	}
	events := &fakeEventSource{
		events: []KubernetesEvent{
			{
				Metadata: EventMetadata{UID: "event-1", Namespace: "envpilot-pr-kan-405"},
				Type:     "Warning",
				Reason:   "FailedScheduling",
				Message:  "0/3 nodes are available",
				InvolvedObject: InvolvedObject{
					Kind: "Pod",
					Name: "cms-api-abc",
				},
				Count:         2,
				LastTimestamp: "2026-05-01T10:00:00Z",
			},
		},
	}
	reporter := &fakeStatusReporter{}
	watcher := NewNamespaceWatcherWithCollectors(source, reporter, nil, NewEventCollector(events), nil, time.Second, nil)

	if err := watcher.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync once: %v", err)
	}

	if len(reporter.eventReports) != 1 {
		t.Fatalf("event reports = %d", len(reporter.eventReports))
	}
	report := reporter.eventReports[0]
	if report.environmentID != "kan-405" {
		t.Fatalf("environment id = %q", report.environmentID)
	}
	if len(report.events) != 1 || report.events[0].Reason != "FailedScheduling" {
		t.Fatalf("events = %#v", report.events)
	}
}

func TestBuildNamespaceStatusReportMapsDeletionEvent(t *testing.T) {
	report, ok := BuildNamespaceStatusReport("DELETED", Namespace{
		Metadata: NamespaceMetadata{
			Name:   "envpilot-pr-kan-403",
			Labels: map[string]string{environmentIDLabel: "kan-403"},
		},
		Status: NamespaceStatus{Phase: "Terminating"},
	})
	if !ok {
		t.Fatal("expected report")
	}
	if report.EnvironmentID != "kan-403" {
		t.Fatalf("environment id = %q", report.EnvironmentID)
	}
	if report.Status != domain.StatusTerminated {
		t.Fatalf("status = %q", report.Status)
	}
}

func TestNamespaceWatcherReportsFluxStatus(t *testing.T) {
	source := &fakeNamespaceSource{
		namespaces: []Namespace{
			{
				Metadata: NamespaceMetadata{
					Name: "envpilot-pr-kan-406",
					Labels: map[string]string{
						environmentIDLabel:    "kan-406",
						"envpilot.io/product": "bethunder",
					},
				},
				Status: NamespaceStatus{Phase: "Active"},
			},
		},
	}
	flux := &fakeFluxSource{
		kustomizations: []FluxKustomization{
			{
				Metadata: FluxMetadata{Name: "kan-406.bethunder", Namespace: "flux-system"},
				Status: FluxStatus{
					Conditions:          []FluxCondition{{Type: "Ready", Status: "True", Reason: "ReconciliationSucceeded"}},
					LastAppliedRevision: "main/abc123",
				},
			},
		},
		helmReleases: []HelmRelease{
			{
				Metadata: FluxMetadata{Name: "nginx", Namespace: "envpilot-pr-kan-406"},
				Status:   FluxStatus{Conditions: []FluxCondition{{Type: "Ready", Status: "True", Reason: "InstallSucceeded"}}},
			},
		},
	}
	reporter := &fakeStatusReporter{}
	watcher := NewNamespaceWatcherWithCollectors(source, reporter, nil, nil, NewFluxStatusCollector(flux), time.Second, nil)

	if err := watcher.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync once: %v", err)
	}

	if len(reporter.fluxReports) != 1 {
		t.Fatalf("flux reports = %d", len(reporter.fluxReports))
	}
	report := reporter.fluxReports[0]
	if report.environmentID != "kan-406" {
		t.Fatalf("environment id = %q", report.environmentID)
	}
	if report.status.Status != domain.StatusReady {
		t.Fatalf("flux status = %q", report.status.Status)
	}
}

type fakeNamespaceSource struct {
	namespaces []Namespace
}

func (f *fakeNamespaceSource) ListNamespaces(context.Context) ([]Namespace, error) {
	return f.namespaces, nil
}

func (f *fakeNamespaceSource) WatchNamespaces(context.Context, func(NamespaceEvent) error) error {
	return nil
}

type fakeStatusReporter struct {
	reports      []NamespaceStatusReport
	eventReports []fakeEventReport
	fluxReports  []fakeFluxReport
}

func (f *fakeStatusReporter) ReportNamespaceStatus(_ context.Context, report NamespaceStatusReport) error {
	f.reports = append(f.reports, report)
	return nil
}

func (f *fakeStatusReporter) ReportEvents(_ context.Context, environmentID string, events []domain.KubernetesEvent) error {
	f.eventReports = append(f.eventReports, fakeEventReport{environmentID: environmentID, events: events})
	return nil
}

func (f *fakeStatusReporter) ReportFluxStatus(_ context.Context, environmentID string, status domain.FluxStatus) error {
	f.fluxReports = append(f.fluxReports, fakeFluxReport{environmentID: environmentID, status: status})
	return nil
}

type fakeEventReport struct {
	environmentID string
	events        []domain.KubernetesEvent
}

type fakeFluxReport struct {
	environmentID string
	status        domain.FluxStatus
}

type fakeEventSource struct {
	namespace string
	events    []KubernetesEvent
}

func (f *fakeEventSource) ListEvents(_ context.Context, namespace string) ([]KubernetesEvent, error) {
	f.namespace = namespace
	return f.events, nil
}

type fakeFluxSource struct {
	kustomizationNamespace string
	helmReleaseNamespace   string
	kustomizations         []FluxKustomization
	helmReleases           []HelmRelease
}

func (f *fakeFluxSource) ListFluxKustomizations(_ context.Context, namespace string) ([]FluxKustomization, error) {
	f.kustomizationNamespace = namespace
	return f.kustomizations, nil
}

func (f *fakeFluxSource) ListHelmReleases(_ context.Context, namespace string) ([]HelmRelease, error) {
	f.helmReleaseNamespace = namespace
	return f.helmReleases, nil
}

func (f *fakeFluxSource) FluxNamespace() string {
	return "flux-system"
}
