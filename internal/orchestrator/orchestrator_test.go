package orchestrator

import (
	"context"
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"envpilot/internal/domain"
	"envpilot/internal/gitops"
	"envpilot/internal/store"
)

func TestCreateTriggersRendererAndPersistsCreatingStatus(t *testing.T) {
	envStore, orch, backend, writer := newTestOrchestrator(t)
	env := testEnvironment("kan-2501")

	created, err := orch.Create(context.Background(), env)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if backend.renderCalls != 1 {
		t.Fatalf("backend render calls = %d", backend.renderCalls)
	}
	if backend.applyCalls != 1 {
		t.Fatalf("backend apply calls = %d", backend.applyCalls)
	}
	if writer.writes != 3 {
		t.Fatalf("writer writes = %d", writer.writes)
	}
	if created.Status != domain.StatusCreating {
		t.Fatalf("status = %q", created.Status)
	}
	persisted, err := envStore.Get("kan-2501")
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if persisted.Status != domain.StatusCreating {
		t.Fatalf("persisted status = %q", persisted.Status)
	}
}

func TestDeleteTriggersCleanupAndTerminatesEnvironment(t *testing.T) {
	envStore, orch, backend, writer := newTestOrchestrator(t)
	env := testEnvironment("kan-2502")
	env.ManifestPath = "/tmp/apps/kan-2502/manifest.yaml"
	env.NamespaceManifestPath = "/tmp/apps/kan-2502/namespace.yaml"
	env.KustomizationManifestPath = "/tmp/apps/kan-2502/kustomization.yaml"
	if err := envStore.Save(env); err != nil {
		t.Fatalf("save env: %v", err)
	}

	deleted, err := orch.Delete(context.Background(), "kan-2502")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if writer.removePaths != 1 {
		t.Fatalf("writer remove paths = %d", writer.removePaths)
	}
	if backend.deleteCalls != 1 {
		t.Fatalf("backend delete calls = %d", backend.deleteCalls)
	}
	if deleted.Status != domain.StatusTerminated {
		t.Fatalf("status = %q", deleted.Status)
	}
	if deleted.ManifestPath != "" || deleted.NamespaceManifestPath != "" || deleted.KustomizationManifestPath != "" {
		t.Fatalf("expected manifest paths to be cleared after cleanup, got manifest=%q namespace=%q kustomization=%q", deleted.ManifestPath, deleted.NamespaceManifestPath, deleted.KustomizationManifestPath)
	}
	persisted, err := envStore.Get("kan-2502")
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if persisted.Status != domain.StatusTerminated {
		t.Fatalf("persisted status = %q", persisted.Status)
	}
}

func TestDeleteTransitionsToDeleteFailedWhenGitOpsDeleteFails(t *testing.T) {
	envStore, orch, backend, writer := newTestOrchestrator(t)
	env := testEnvironment("kan-2504")
	if err := envStore.Save(env); err != nil {
		t.Fatalf("save env: %v", err)
	}
	writer.removeErr = context.DeadlineExceeded
	deleted, err := orch.Delete(context.Background(), "kan-2504")
	if err == nil {
		t.Fatalf("expected delete error")
	}
	if backend.deleteCalls != 1 {
		t.Fatalf("backend delete calls = %d", backend.deleteCalls)
	}
	if deleted.Status != domain.StatusDeleteFailed {
		t.Fatalf("status = %q", deleted.Status)
	}
	persisted, err := envStore.Get("kan-2504")
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if persisted.Status != domain.StatusDeleteFailed {
		t.Fatalf("persisted status = %q", persisted.Status)
	}
}

func TestDeleteRetriesFromDeleteFailedIdempotently(t *testing.T) {
	envStore, orch, backend, writer := newTestOrchestrator(t)
	env := testEnvironment("kan-2505")
	env.Status = domain.StatusDeleteFailed
	if err := envStore.Save(env); err != nil {
		t.Fatalf("save env: %v", err)
	}
	deleted, err := orch.Delete(context.Background(), "kan-2505")
	if err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if deleted.Status != domain.StatusTerminated {
		t.Fatalf("status = %q", deleted.Status)
	}
	if writer.removePaths != 1 || writer.commits != 1 {
		t.Fatalf("unexpected writer calls removePaths=%d commits=%d", writer.removePaths, writer.commits)
	}
	if backend.deleteCalls != 1 {
		t.Fatalf("backend delete calls = %d", backend.deleteCalls)
	}
}

func TestDeleteIsIdempotentForTerminatingOrTerminated(t *testing.T) {
	cases := []struct {
		status      domain.EnvironmentStatus
		wantStatus  domain.EnvironmentStatus
		wantCleanup bool
	}{
		{status: domain.StatusTerminating, wantStatus: domain.StatusTerminated, wantCleanup: true},
		{status: domain.StatusTerminated, wantStatus: domain.StatusTerminated, wantCleanup: false},
	}
	for _, status := range cases {
		t.Run(string(status.status), func(t *testing.T) {
			envStore, orch, backend, writer := newTestOrchestrator(t)
			env := testEnvironment("kan-2506-" + string(status.status))
			env.Status = status.status
			if err := envStore.Save(env); err != nil {
				t.Fatalf("save env: %v", err)
			}
			deleted, err := orch.Delete(context.Background(), env.ID)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if deleted.Status != status.wantStatus {
				t.Fatalf("status = %q", deleted.Status)
			}
			wantCalls := 0
			if status.wantCleanup {
				wantCalls = 1
			}
			if writer.removePaths != wantCalls || writer.commits != wantCalls {
				t.Fatalf("writer calls removePaths=%d commits=%d, want %d", writer.removePaths, writer.commits, wantCalls)
			}
			if backend.deleteCalls != wantCalls {
				t.Fatalf("backend delete calls=%d, want %d", backend.deleteCalls, wantCalls)
			}
		})
	}
}

func TestStatusReadsBackendStatusAndPersistsStatus(t *testing.T) {
	envStore, orch, backend, _ := newTestOrchestrator(t)
	env := testEnvironment("kan-2507")
	env.Status = domain.StatusCreating
	if err := envStore.Save(env); err != nil {
		t.Fatalf("save env: %v", err)
	}
	backend.status = domain.StatusReady

	updated, err := orch.Status(context.Background(), "kan-2507", domain.ProjectConfig{ID: "pc-1"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if updated.Status != domain.StatusReady {
		t.Fatalf("status = %q", updated.Status)
	}
	if backend.statusCalls != 1 {
		t.Fatalf("backend status calls = %d", backend.statusCalls)
	}
	if backend.lastProjectConfig.ID != "pc-1" {
		t.Fatalf("project config ID = %q", backend.lastProjectConfig.ID)
	}
	persisted, err := envStore.Get("kan-2507")
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if persisted.Status != domain.StatusReady {
		t.Fatalf("persisted status = %q", persisted.Status)
	}
}

func TestNormalizeDeploymentBackendType(t *testing.T) {
	cases := []struct {
		name string
		in   string
		out  DeploymentBackendType
	}{
		{name: "gitops manifest", in: "gitops_manifest", out: DeploymentBackendGitOpsManifest},
		{name: "fluxcd", in: "fluxcd", out: DeploymentBackendFluxCD},
		{name: "flux legacy", in: "flux", out: DeploymentBackendFluxCD},
		{name: "flux legacy underscore", in: "flux_cd", out: DeploymentBackendFluxCD},
		{name: "helm direct", in: "helm_direct", out: DeploymentBackendHelmDirect},
		{name: "helm alias", in: "helm-direct", out: DeploymentBackendHelmDirect},
		{name: "unknown value", in: "custom", out: DeploymentBackendType("custom")},
		{name: "trim spaces", in: "  fluxcd  ", out: DeploymentBackendFluxCD},
		{name: "case", in: "HeLm_DiReCt", out: DeploymentBackendHelmDirect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeDeploymentBackendType(tc.in)
			if got != tc.out {
				t.Fatalf("NormalizeDeploymentBackendType(%q) = %q, want %q", tc.in, got, tc.out)
			}
		})
	}
}

func TestNewDeploymentBackendReturnsConcreteBackendsForKnownTypes(t *testing.T) {
	for _, in := range []DeploymentBackendType{
		DeploymentBackendGitOpsManifest,
		DeploymentBackendFluxCD,
		DeploymentBackendHelmDirect,
		DeploymentBackendType("custom"),
	} {
		t.Run(string(in), func(t *testing.T) {
			backend := NewDeploymentBackend(in, nil)
			switch in {
			case DeploymentBackendFluxCD:
				if _, ok := backend.(*FluxBackend); !ok {
					t.Fatalf("expected FluxBackend, got %T", backend)
				}
			case DeploymentBackendHelmDirect:
				if _, ok := backend.(*HelmDirectBackend); !ok {
					t.Fatalf("expected HelmDirectBackend, got %T", backend)
				}
			case DeploymentBackendType("custom"):
				if _, ok := backend.(*GitOpsManifestBackend); !ok {
					t.Fatalf("expected GitOpsManifestBackend fallback, got %T", backend)
				}
			default:
				if _, ok := backend.(*GitOpsManifestBackend); !ok {
					t.Fatalf("expected GitOpsManifestBackend, got %T", backend)
				}
			}
		})
	}
}

func TestResolveDeploymentBackendReturnsTypedBackendForEachConfig(t *testing.T) {
	cases := []struct {
		name        string
		backendType DeploymentBackendType
	}{
		{name: "helm direct", backendType: DeploymentBackendHelmDirect},
		{name: "fluxcd", backendType: DeploymentBackendFluxCD},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := ResolveDeploymentBackend(tc.backendType, nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			switch tc.backendType {
			case DeploymentBackendHelmDirect:
				if _, ok := backend.(*HelmDirectBackend); !ok {
					t.Fatalf("expected HelmDirectBackend, got %T", backend)
				}
			case DeploymentBackendFluxCD:
				if _, ok := backend.(*FluxBackend); !ok {
					t.Fatalf("expected FluxBackend, got %T", backend)
				}
			}
		})
	}
}

func TestResolveDeploymentBackendFromProjectConfig(t *testing.T) {
	backend, err := ResolveDeploymentBackendFromProjectConfig(domain.ProjectConfig{
		Config: map[string]any{},
	}, nil)
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if _, ok := backend.(*HelmDirectBackend); !ok {
		t.Fatalf("expected default HelmDirectBackend, got %T", backend)
	}

	backend, err = ResolveDeploymentBackendFromProjectConfig(domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "fluxcd",
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := backend.(*FluxBackend); !ok {
		t.Fatalf("expected FluxBackend, got %T", backend)
	}

	backend, err = ResolveDeploymentBackendFromProjectConfig(domain.ProjectConfig{
		Config: map[string]any{
			"gitOpsRepoUrl":    "https://github.com/acme/gitops",
			"gitOpsOutputPath": "feature-envs",
		},
	}, nil)
	if err != nil {
		t.Fatalf("resolve legacy: %v", err)
	}
	if _, ok := backend.(*FluxBackend); !ok {
		t.Fatalf("expected legacy-inferred FluxBackend, got %T", backend)
	}
}

func TestResolveDeploymentBackendRejectsUnknownBackend(t *testing.T) {
	if _, err := ResolveDeploymentBackend("custom", nil); err == nil {
		t.Fatal("expected error")
	}
	backend, err := ResolveDeploymentBackendFromProjectConfig(domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "custom",
			},
		},
	}, nil)
	if err == nil {
		t.Fatalf("expected error for project config backend, got nil with %T", backend)
	}
}

func TestHelmDirectBackendRenderMinimal(t *testing.T) {
	backend := NewHelmDirectBackend(nil)
	environment := domain.Environment{
		ID:        "pr-100",
		Project:   "proj-100",
		Product:   "payments",
		Namespace: "envpilot-pr-100",
		GitOps:    domain.GitOpsTarget{},
		Source: domain.SCMSource{
			PullRequestID: "77",
			Branch:        "feature/demo",
			Commit:        "abc123",
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "v1"},
		},
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}

	manifests, err := backend.Render(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests))
	}

	type renderedHelmDirect struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Release struct {
				Name      string       `yaml:"name"`
				Namespace string       `yaml:"namespace"`
				Labels    []helmTestKV `yaml:"labels"`
			} `yaml:"release"`
			Namespace struct {
				Name   string       `yaml:"name"`
				Labels []helmTestKV `yaml:"labels"`
			} `yaml:"namespace"`
			Identity []helmTestKV `yaml:"identity"`
			Values   struct {
				ImageTags []helmTestKV `yaml:"imageTags"`
			} `yaml:"values"`
			ValuesObj  []helmTestKV `yaml:"valuesObject"`
			ValuesFile string       `yaml:"valuesFile"`
		} `yaml:"spec"`
		Metadata struct {
			Labels      []helmTestKV `yaml:"labels"`
			Annotations []helmTestKV `yaml:"annotations"`
		} `yaml:"metadata"`
	}

	var decoded renderedHelmDirect
	if err := yaml.Unmarshal(manifests[0].Content, &decoded); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if decoded.Kind != "HelmDirectDeployment" {
		t.Fatalf("kind = %q", decoded.Kind)
	}
	if decoded.Spec.Release.Name != "proj-100-pr-100" {
		t.Fatalf("release name = %q", decoded.Spec.Release.Name)
	}
	if decoded.Spec.Release.Namespace != "envpilot-pr-100" {
		t.Fatalf("release namespace = %q", decoded.Spec.Release.Namespace)
	}
	if !hasLabel(decoded.Metadata.Labels, "envpilot.io/project-id", "proj-100") {
		t.Fatalf("missing top metadata label")
	}
	if !hasLabel(decoded.Spec.Namespace.Labels, "envpilot.io/managed", "true") {
		t.Fatalf("missing namespace label")
	}
	if !hasLabel(decoded.Spec.Release.Labels, "envpilot.io/environment-id", "pr-100") {
		t.Fatalf("missing release label")
	}

	imageValues := helmValuesMap(decoded.Spec.ValuesObj)
	if imageValues["cmsApiTag"] != "v1" {
		t.Fatalf("cmsApiTag = %q", imageValues["cmsApiTag"])
	}
	identity := helmValuesMap(decoded.Spec.Identity)
	if identity["environmentId"] != "pr-100" || identity["projectId"] != "proj-100" {
		t.Fatalf("identity = %#v", identity)
	}
	if decoded.Spec.ValuesFile != "" {
		t.Fatalf("valuesFile should be empty for value object mode")
	}
	if len(decoded.Spec.Values.ImageTags) != 0 {
		t.Fatalf("expected values image tags to be empty when valuesFile is empty")
	}

	repeat, err := backend.Render(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if string(repeat[0].Content) != string(manifests[0].Content) {
		t.Fatal("render output is not deterministic")
	}
}

func TestHelmDirectBackendDeploymentTargetMatchesRenderedRelease(t *testing.T) {
	backend := NewHelmDirectBackend(nil)
	environment := domain.Environment{
		ID: "envpilot-e2e-full-01", Project: "bethunder-e2e-20260729", Namespace: "envpilot-e2e-full-01",
	}
	projectConfig := domain.ProjectConfig{Config: map[string]any{
		"deployment": map[string]any{
			"backend":             "helm_direct",
			"releaseNameTemplate": "{{ .project.id }}-{{ .environment.id }}",
		},
	}}

	release, namespace, err := backend.DeploymentTarget(environment, projectConfig)
	if err != nil {
		t.Fatalf("deployment target: %v", err)
	}
	if release != "bethunder-e2e-20260729-envpilot-e2e-full-01" {
		t.Fatalf("release = %q", release)
	}
	if namespace != "envpilot-e2e-full-01" {
		t.Fatalf("namespace = %q", namespace)
	}
}

func TestHelmDirectBackendRenderCustomEnvironmentMetadataAndValues(t *testing.T) {
	backend := NewHelmDirectBackend(nil)
	environment := domain.Environment{
		ID:        "pr-custom-1",
		Project:   "custom-project",
		Product:   "cms",
		Namespace: "custom-ns",
		GitOps: domain.GitOpsTarget{
			Path:       "charts/cms",
			ValuesPath: "values-preview.yaml",
		},
		Charts: domain.ChartVersions{
			App: "",
		},
		Source: domain.SCMSource{
			PullRequestID: "1234",
			Branch:        "feature/custom",
			Commit:        "deadbeef",
		},
		Services: []domain.ServiceOverride{
			{Name: "nginx", Tag: "nginx-1"},
			{Name: "api", Tag: "api-2"},
		},
		Overrides: map[string]string{
			"cmsApiTag": "overridden",
		},
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"releaseNamePattern": "{{ .project.id }}-{{ .environment.id }}-{{ .source.branch }}-{{ .source.pr }}-{{ .source.commit }}",
				},
			},
		},
	}

	manifests, err := backend.Render(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests))
	}

	type renderedHelmDirect struct {
		Spec struct {
			Release struct {
				Name      string       `yaml:"name"`
				Namespace string       `yaml:"namespace"`
				ChartPath string       `yaml:"chartPath"`
				ChartRef  string       `yaml:"chartRef"`
				Labels    []helmTestKV `yaml:"labels"`
			} `yaml:"release"`
			ValuesFile string       `yaml:"valuesFile"`
			Identity   []helmTestKV `yaml:"identity"`
		} `yaml:"spec"`
	}

	var decoded renderedHelmDirect
	if err := yaml.Unmarshal(manifests[0].Content, &decoded); err != nil {
		t.Fatalf("yaml: %v", err)
	}

	wantName := "custom-project-pr-custom-1-feature/custom-1234-deadbeef"
	if decoded.Spec.Release.Name != wantName {
		t.Fatalf("release name = %q, want %q", decoded.Spec.Release.Name, wantName)
	}
	if decoded.Spec.Release.ChartPath != "charts/cms" {
		t.Fatalf("chartPath = %q", decoded.Spec.Release.ChartPath)
	}
	if decoded.Spec.Release.ChartRef != "charts/cms" {
		t.Fatalf("chartRef = %q", decoded.Spec.Release.ChartRef)
	}
	if decoded.Spec.Release.Namespace != "custom-ns" {
		t.Fatalf("namespace = %q", decoded.Spec.Release.Namespace)
	}
	if decoded.Spec.ValuesFile != "values-preview.yaml" {
		t.Fatalf("valuesFile = %q", decoded.Spec.ValuesFile)
	}
	identity := helmValuesMap(decoded.Spec.Identity)
	if identity["environmentId"] != "pr-custom-1" || identity["projectId"] != "custom-project" {
		t.Fatalf("identity map = %#v", identity)
	}
	if identity["prNumber"] != "1234" || identity["branch"] != "feature/custom" || identity["commitSHA"] != "deadbeef" {
		t.Fatalf("identity map = %#v", identity)
	}
	if !hasLabel(decoded.Spec.Release.Labels, "envpilot.io/managed", "true") {
		t.Fatalf("missing managed label")
	}
}

func TestHelmDirectBackendApplyUsesHelmUpgradeInstall(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:        "pr-100",
		Project:   "proj-100",
		Product:   "payments",
		Namespace: "envpilot-pr-100",
		Charts: domain.ChartVersions{
			App: "payments-chart",
		},
		GitOps: domain.GitOpsTarget{
			ValuesPath: "values-preview.yaml",
		},
		Source: domain.SCMSource{
			PullRequestID: "77",
			Branch:        "feature/demo",
			Commit:        "abc123",
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "v1"},
		},
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"namespaceMode": "dedicated",
					"wait":          true,
					"timeout":       120,
				},
			},
		},
	}
	if err := backend.Apply(context.Background(), environment, projectConfig); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 helm call, got %d", len(executor.calls))
	}
	call := executor.calls[0]
	if call.Options.ReleaseName != "proj-100-pr-100" {
		t.Fatalf("release name = %q", call.Options.ReleaseName)
	}
	if call.Options.ChartRef != "payments-chart" {
		t.Fatalf("chart = %q", call.Options.ChartRef)
	}
	if call.Options.Namespace != "envpilot-pr-100" {
		t.Fatalf("namespace = %q", call.Options.Namespace)
	}
	if call.Options.Wait != true {
		t.Fatalf("expected wait=true")
	}
	if call.Options.Timeout != 120 {
		t.Fatalf("timeout = %d", call.Options.Timeout)
	}
	if call.Options.ValuesFile != "values-preview.yaml" {
		t.Fatalf("values file = %q", call.Options.ValuesFile)
	}
}

func TestHelmDirectBackendApplyPreservesCustomEnvironmentAndProjectIds(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:        "feature/custom-environment",
		Project:   "acme-payment-service",
		Product:   "payments",
		Namespace: "custom-ns",
		Charts: domain.ChartVersions{
			App: "payments-chart",
		},
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"releaseNamePattern": "{{ .project.id }}::{{ .environment.id }}",
				},
			},
		},
	}
	if err := backend.Apply(context.Background(), environment, projectConfig); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 helm call, got %d", len(executor.calls))
	}
	call := executor.calls[0]
	if call.Options.ReleaseName != "acme-payment-service::feature/custom-environment" {
		t.Fatalf("release name = %q", call.Options.ReleaseName)
	}
	if call.Options.Namespace != "custom-ns" {
		t.Fatalf("namespace = %q", call.Options.Namespace)
	}
}

func TestHelmDirectBackendApplyGeneratesValuesFile(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:        "pr-101",
		Project:   "proj-101",
		Product:   "payments",
		Namespace: "envpilot-pr-101",
		GitOps:    domain.GitOpsTarget{},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "v1"},
			{Name: "cms-api", Tag: "v2"},
		},
	}
	if err := backend.Apply(context.Background(), environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 helm call, got %d", len(executor.calls))
	}
	call := executor.calls[0]
	if call.Options.ValuesFile == "" {
		t.Fatalf("expected generated values file")
	}
	content := call.Content
	if !strings.Contains(string(content), "cmsApiTag") || !strings.Contains(string(content), "cmsApiTag: v2") {
		t.Fatalf("generated values missing tag data: %s", string(content))
	}
}

func TestHelmDirectBackendApplyIdempotentUpgradeKeepsRelease(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:      "pr-102",
		Project: "proj-102",
		GitOps: domain.GitOpsTarget{
			Path: "charts/payments",
		},
	}
	if err := backend.Apply(context.Background(), environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := backend.Apply(context.Background(), environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("expected 2 helm calls, got %d", len(executor.calls))
	}
	if executor.calls[0].Options.ReleaseName != executor.calls[1].Options.ReleaseName {
		t.Fatalf("release name changed between applies: %q vs %q", executor.calls[0].Options.ReleaseName, executor.calls[1].Options.ReleaseName)
	}
}

func TestHelmDirectBackendApplyMapsErrorAsReadableReason(t *testing.T) {
	executor := &CLIHelmExecutor{
		runCommand: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("chart pull failed"), errors.New("exit status 1")
		},
	}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:      "pr-103",
		Project: "proj-103",
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}
	err := backend.Apply(context.Background(), environment, projectConfig)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "helm apply failed for release") {
		t.Fatalf("unexpected error: %q", err)
	}
}

func TestHelmDirectBackendDeleteUninstallsRelease(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:        "pr-104",
		Project:   "proj-104",
		Namespace: "envpilot-pr-104",
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"namespaceMode": "dedicated",
				},
			},
		},
	}
	if err := backend.Delete(context.Background(), environment, projectConfig); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(executor.uninstallCalls) != 1 {
		t.Fatalf("expected 1 helm uninstall call, got %d", len(executor.uninstallCalls))
	}
	call := executor.uninstallCalls[0]
	if call.Options.ReleaseName != "proj-104-pr-104" {
		t.Fatalf("release name = %q", call.Options.ReleaseName)
	}
	if call.Options.Namespace != "envpilot-pr-104" {
		t.Fatalf("namespace = %q", call.Options.Namespace)
	}
	if executor.deleteNamespaceCalls != 1 {
		t.Fatalf("expected namespace delete call when dedicated mode, got %d", executor.deleteNamespaceCalls)
	}
}

func TestHelmDirectBackendDeleteMissingReleaseIsNotFatal(t *testing.T) {
	executor := &fakeHelmExecutor{uninstallErr: errors.New("uninstall: release not found")}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:      "pr-105",
		Project: "proj-105",
	}
	if err := backend.Delete(context.Background(), environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}); err != nil {
		t.Fatalf("expected missing release to be non-fatal, got %v", err)
	}
	if len(executor.uninstallCalls) != 1 {
		t.Fatalf("expected uninstall call, got %d", len(executor.uninstallCalls))
	}
}

func TestHelmDirectBackendDeleteRepeatedCallsAreIdempotent(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:      "pr-106",
		Project: "proj-106",
	}
	if err := backend.Delete(context.Background(), environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := backend.Delete(context.Background(), environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if len(executor.uninstallCalls) != 2 {
		t.Fatalf("expected 2 uninstall calls, got %d", len(executor.uninstallCalls))
	}
}

func TestHelmDirectBackendDeleteSkipsUnmanagedNamespace(t *testing.T) {
	executor := &fakeHelmExecutor{
		namespaceManagedSet: true,
		namespaceManaged:    false,
	}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:        "pr-106",
		Project:   "proj-106",
		Namespace: "envpilot-pr-106",
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"namespaceMode": "dedicated",
				},
			},
		},
	}
	if err := backend.Delete(context.Background(), environment, projectConfig); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if executor.deleteNamespaceCalls != 0 {
		t.Fatalf("expected unmanaged namespace to stay untouched, got calls=%d", executor.deleteNamespaceCalls)
	}
}

func TestHelmDirectBackendDeleteSkipsSharedNamespaceDeletion(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:        "pr-107",
		Project:   "proj-107",
		Namespace: "shared-environments",
	}
	if err := backend.Delete(context.Background(), environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"namespaceMode": "shared",
				},
			},
		},
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if executor.deleteNamespaceCalls != 0 {
		t.Fatalf("expected shared namespace to stay untouched, got calls=%d", executor.deleteNamespaceCalls)
	}
}

func TestCLIHelmExecutorBuildsUpgradeInstallCommand(t *testing.T) {
	capturedName := ""
	var capturedArgs []string
	executor := &CLIHelmExecutor{
		runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			capturedName = name
			capturedArgs = append([]string(nil), args...)
			return nil, nil
		},
	}
	err := executor.UpgradeInstall(context.Background(), HelmUpgradeOptions{
		ReleaseName: "test-release",
		ChartRef:    "charts/test",
		Namespace:   "feature-ns",
		ValuesFile:  "values.yaml",
		Wait:        true,
		Timeout:     45,
	})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if capturedName != "helm" {
		t.Fatalf("command = %q", capturedName)
	}
	requireArg := map[string]bool{}
	for _, arg := range capturedArgs {
		requireArg[arg] = true
	}
	for _, required := range []string{"upgrade", "--install", "test-release", "charts/test", "--namespace", "feature-ns", "--create-namespace", "-f", "values.yaml", "--wait", "--timeout", "45s"} {
		if !requireArg[required] {
			t.Fatalf("missing arg %q in %v", required, capturedArgs)
		}
	}
}

func TestCLIHelmExecutorBuildsUninstallCommand(t *testing.T) {
	capturedName := ""
	var capturedArgs []string
	executor := &CLIHelmExecutor{
		runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			capturedName = name
			capturedArgs = append([]string(nil), args...)
			return nil, nil
		},
	}
	err := executor.Uninstall(context.Background(), HelmUninstallOptions{
		ReleaseName: "test-release",
		Namespace:   "feature-ns",
	})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if capturedName != "helm" {
		t.Fatalf("command = %q", capturedName)
	}
	requireArg := map[string]bool{}
	for _, arg := range capturedArgs {
		requireArg[arg] = true
	}
	for _, required := range []string{"uninstall", "test-release", "--namespace", "feature-ns"} {
		if !requireArg[required] {
			t.Fatalf("missing arg %q in %v", required, capturedArgs)
		}
	}
}

func TestHelmDirectBackendStatusMapsHelmStates(t *testing.T) {
	executor := &fakeHelmExecutor{}
	backend := NewHelmDirectBackendWithExecutor(nil, executor)
	environment := domain.Environment{
		ID:        "pr-108",
		Project:   "proj-108",
		Namespace: "envpilot-pr-108",
		Status:    domain.StatusCreating,
	}
	projectConfig := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"wait": true,
				},
			},
		},
	}

	executor.statusResult = HelmStatus{Found: true, Status: "deployed"}
	executor.readinessResult = true
	status, err := backend.Status(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("status deployed: %v", err)
	}
	if status != domain.StatusReady {
		t.Fatalf("deployed status = %q", status)
	}
	if len(executor.readinessCalls) != 1 {
		t.Fatalf("expected readiness check, got %d", len(executor.readinessCalls))
	}

	executor.readinessResult = false
	status, err = backend.Status(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("status deployed (not ready): %v", err)
	}
	if status != domain.StatusCreating {
		t.Fatalf("deployed-not-ready status = %q", status)
	}

	executor.statusResult = HelmStatus{Found: true, Status: "pending-install"}
	status, err = backend.Status(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("status pending-install: %v", err)
	}
	if status != domain.StatusCreating {
		t.Fatalf("pending status = %q", status)
	}

	executor.statusResult = HelmStatus{Found: true, Status: "pending-upgrade"}
	status, err = backend.Status(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("status pending-upgrade: %v", err)
	}
	if status != domain.StatusCreating {
		t.Fatalf("pending-upgrade status = %q", status)
	}

	executor.statusResult = HelmStatus{Found: true, Status: "failed"}
	status, err = backend.Status(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if status != domain.StatusFailed {
		t.Fatalf("failed status = %q", status)
	}

	executor.statusResult = HelmStatus{Found: false}
	environment.Status = domain.StatusCreating
	status, err = backend.Status(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("status not found: %v", err)
	}
	if status != domain.EnvironmentStatus("not_found") {
		t.Fatalf("not found status = %q", status)
	}

	environment.Status = domain.StatusTerminating
	status, err = backend.Status(context.Background(), environment, projectConfig)
	if err != nil {
		t.Fatalf("status not found for terminating: %v", err)
	}
	if status != domain.StatusTerminated {
		t.Fatalf("terminating lifecycle not found -> %q", status)
	}

	projectConfigNoWait := domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"wait": false,
				},
			},
		},
	}
	executor.readinessCalls = nil
	executor.statusResult = HelmStatus{Found: true, Status: "deployed"}
	status, err = backend.Status(context.Background(), environment, projectConfigNoWait)
	if err != nil {
		t.Fatalf("status deployed with wait=false: %v", err)
	}
	if status != domain.StatusReady {
		t.Fatalf("deployed no-wait status = %q", status)
	}
	if len(executor.readinessCalls) != 0 {
		t.Fatalf("expected no readiness check with wait=false, got %d", len(executor.readinessCalls))
	}
}

func TestFluxBackendStatusUsesFluxStatusAndResources(t *testing.T) {
	backend := NewFluxBackend(nil)

	environment := testEnvironment("kan-flux-status")
	environment.Status = domain.StatusCreating

	status, err := backend.Status(context.Background(), environment, domain.ProjectConfig{})
	if err != nil {
		t.Fatalf("status without flux status: %v", err)
	}
	if status != domain.StatusCreating {
		t.Fatalf("expected creating when no flux status, got %q", status)
	}

	environment.FluxStatus = &domain.FluxStatus{Status: domain.StatusReady}
	status, err = backend.Status(context.Background(), environment, domain.ProjectConfig{})
	if err != nil {
		t.Fatalf("status explicit flux: %v", err)
	}
	if status != domain.StatusReady {
		t.Fatalf("expected ready from explicit flux status, got %q", status)
	}

	environment.FluxStatus = &domain.FluxStatus{
		Kustomizations: []domain.FluxResourceStatus{{Kind: "Kustomization", Ready: false}},
	}
	status, err = backend.Status(context.Background(), environment, domain.ProjectConfig{})
	if err != nil {
		t.Fatalf("status from creating resource: %v", err)
	}
	if status != domain.StatusCreating {
		t.Fatalf("expected creating from unready flux resource, got %q", status)
	}

	environment.FluxStatus = &domain.FluxStatus{
		HelmReleases: []domain.FluxResourceStatus{{Kind: "HelmRelease", Failed: true}},
	}
	status, err = backend.Status(context.Background(), environment, domain.ProjectConfig{})
	if err != nil {
		t.Fatalf("status from failed resource: %v", err)
	}
	if status != domain.StatusFailed {
		t.Fatalf("expected failed from failed flux resource, got %q", status)
	}
}

func TestFluxBackendApplyWithWriterCommitsAndReturnsPRURL(t *testing.T) {
	backend := NewFluxBackend(nil)
	writer := &fakeWriter{commitPullRequestURL: "https://example.com/pr/flux-create"}

	environment := testEnvironment("kan-flux-apply")
	commit, err := backend.ApplyWithWriter(context.Background(), environment, domain.ProjectConfig{}, writer)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if commit.PullRequestURL != "https://example.com/pr/flux-create" {
		t.Fatalf("pull request url = %q", commit.PullRequestURL)
	}
	if writer.commits != 1 {
		t.Fatalf("writer commits = %d", writer.commits)
	}
	if writer.lastCommitMessage != "envpilot: create kan-flux-apply" {
		t.Fatalf("commit message = %q", writer.lastCommitMessage)
	}
}

func TestFluxBackendDeleteWithWriterRemovesPathAndCommits(t *testing.T) {
	backend := NewFluxBackend(nil)
	writer := &fakeWriter{commitPullRequestURL: "https://example.com/pr/flux-delete"}
	environment := testEnvironment("kan-flux-delete")

	commit, err := backend.DeleteWithWriter(context.Background(), environment, domain.ProjectConfig{}, writer)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if commit.PullRequestURL != "https://example.com/pr/flux-delete" {
		t.Fatalf("pull request url = %q", commit.PullRequestURL)
	}
	if writer.removePaths != 1 {
		t.Fatalf("writer remove paths = %d", writer.removePaths)
	}
	if writer.lastRemovedPath != environment.GitOpsDirectory() {
		t.Fatalf("remove path = %q", writer.lastRemovedPath)
	}
	if writer.lastCommitMessage != "envpilot: delete kan-flux-delete" {
		t.Fatalf("commit message = %q", writer.lastCommitMessage)
	}
}

func TestOrchestratorUsesWriterAwareBackendForCreateAndDelete(t *testing.T) {
	envStore, err := store.NewJSONStore(filepath.Join(t.TempDir(), "environments.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	writer := &fakeWriter{path: "/tmp/manifest.yaml"}
	backend := &writerAwareBackend{
		manifestContent: []byte("manifest"),
	}
	orch := NewWithBackend(envStore, backend, writer)
	orch.now = func() time.Time {
		return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	}

	created, err := orch.Create(context.Background(), testEnvironment("kan-flux-writer"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != "kan-flux-writer" {
		t.Fatalf("created id = %q", created.ID)
	}
	if backend.applyWithWriterCalls != 1 {
		t.Fatalf("applyWithWriter calls = %d", backend.applyWithWriterCalls)
	}
	if backend.applyCalls != 0 {
		t.Fatalf("legacy apply calls = %d", backend.applyCalls)
	}
	if writer.commits != 1 {
		t.Fatalf("writer commits = %d", writer.commits)
	}

	if err := envStore.Save(created); err != nil {
		t.Fatalf("save: %v", err)
	}
	created.Status = domain.StatusGitOpsDeletePending
	if err := envStore.Save(created); err != nil {
		t.Fatalf("update save: %v", err)
	}

	deleted, err := orch.Delete(context.Background(), "kan-flux-writer")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.ID != "kan-flux-writer" {
		t.Fatalf("deleted id = %q", deleted.ID)
	}
	if backend.deleteWithWriterCalls != 1 {
		t.Fatalf("deleteWithWriter calls = %d", backend.deleteWithWriterCalls)
	}
	if backend.deleteCalls != 0 {
		t.Fatalf("legacy delete calls = %d", backend.deleteCalls)
	}
	if writer.commits != 2 {
		t.Fatalf("writer commits = %d", writer.commits)
	}
}

func TestCLIHelmExecutorBuildsStatusCommand(t *testing.T) {
	capturedName := ""
	var capturedArgs []string
	executor := &CLIHelmExecutor{
		runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			capturedName = name
			capturedArgs = append([]string(nil), args...)
			return []byte(`{"info":{"status":"deployed"},"chart":{"metadata":{"name":"test","app_version":"1.0"}},"namespace":"feature-ns"}`), nil
		},
	}
	_, err := executor.Status(context.Background(), HelmStatusOptions{
		ReleaseName: "test-release",
		Namespace:   "feature-ns",
	})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if capturedName != "helm" {
		t.Fatalf("command = %q", capturedName)
	}
	requireArg := map[string]bool{}
	for _, arg := range capturedArgs {
		requireArg[arg] = true
	}
	for _, required := range []string{"status", "test-release", "--namespace", "feature-ns", "-o", "json"} {
		if !requireArg[required] {
			t.Fatalf("missing arg %q in %v", required, capturedArgs)
		}
	}
}

func TestCLIHelmExecutorBuildsReadinessCommand(t *testing.T) {
	capturedName := ""
	var capturedArgs []string
	executor := &CLIHelmExecutor{
		runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			capturedName = name
			capturedArgs = append([]string(nil), args...)
			return []byte(`{"items":[]}`), nil
		},
	}
	ready, err := executor.Readiness(context.Background(), HelmReadinessOptions{
		Release:   "test-release",
		Namespace: "feature-ns",
	})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if ready {
		t.Fatalf("expected not ready for empty items list")
	}
	if capturedName != "kubectl" {
		t.Fatalf("command = %q", capturedName)
	}
	requireArg := map[string]bool{}
	for _, arg := range capturedArgs {
		requireArg[arg] = true
	}
	for _, required := range []string{"get", "pods", "--namespace", "feature-ns", "-l", "release=test-release", "-o", "json"} {
		if !requireArg[required] {
			t.Fatalf("missing arg %q in %v", required, capturedArgs)
		}
	}
}

func TestCLIHelmExecutorTreatsMissingReleaseAsNotFound(t *testing.T) {
	executor := &CLIHelmExecutor{
		runCommand: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("Error: release: not found"), errors.New("exit status 1")
		},
	}
	status, err := executor.Status(context.Background(), HelmStatusOptions{
		ReleaseName: "missing",
		Namespace:   "feature-ns",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Found {
		t.Fatalf("expected not found")
	}
}

func TestCLIHelmExecutorTreatsMissingReleaseAsNonFatal(t *testing.T) {
	executor := &CLIHelmExecutor{
		runCommand: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("Error: uninstall: release not found"), errors.New("exit status 1")
		},
	}
	err := executor.Uninstall(context.Background(), HelmUninstallOptions{
		ReleaseName: "missing",
		Namespace:   "feature-ns",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

type helmTestKV struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

func helmValuesMap(values []helmTestKV) map[string]string {
	result := make(map[string]string, len(values))
	for _, item := range values {
		result[item.Name] = item.Value
	}
	return result
}

func hasLabel(labels []helmTestKV, key, value string) bool {
	for _, item := range labels {
		if item.Name == key && item.Value == value {
			return true
		}
	}
	return false
}

func TestCreateWithProjectConfigInvalidDeploymentBackendReturnsError(t *testing.T) {
	envStore, _, _, writer := newTestOrchestrator(t)
	orch := NewWithBackendResolver(envStore, func(projectConfig domain.ProjectConfig) (DeploymentBackend, error) {
		return ResolveDeploymentBackendFromProjectConfig(projectConfig, nil)
	}, writer)
	orch.now = func() time.Time {
		return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	}

	created, err := orch.CreateWithWriterAndProjectConfig(context.Background(), testEnvironment("kan-2510"), writer, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "custom",
			},
		},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if created.ID != "kan-2510" {
		t.Fatalf("created id = %q", created.ID)
	}
	if created.Status != domain.StatusFailed {
		t.Fatalf("status = %q", created.Status)
	}
	if !strings.Contains(created.LastError, "unsupported deployment backend") {
		t.Fatalf("unexpected error = %q", created.LastError)
	}
}

func TestUpdateStatusPersistsStatus(t *testing.T) {
	envStore, orch, _, _ := newTestOrchestrator(t)
	env := testEnvironment("kan-2503")
	if err := envStore.Save(env); err != nil {
		t.Fatalf("save env: %v", err)
	}

	updated, err := orch.UpdateStatus("kan-2503", domain.StatusReady, "")
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if updated.Status != domain.StatusReady {
		t.Fatalf("status = %q", updated.Status)
	}
	persisted, err := envStore.Get("kan-2503")
	if err != nil {
		t.Fatalf("get persisted: %v", err)
	}
	if persisted.Status != domain.StatusReady {
		t.Fatalf("persisted status = %q", persisted.Status)
	}
}

func newTestOrchestrator(t *testing.T) (*store.JSONStore, *EnvironmentOrchestrator, *fakeBackend, *fakeWriter) {
	t.Helper()
	envStore, err := store.NewJSONStore(filepath.Join(t.TempDir(), "environments.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	backend := &fakeBackend{manifestContent: []byte("manifest")}
	writer := &fakeWriter{path: "/tmp/manifest.yaml"}
	orch := NewWithBackend(envStore, backend, writer)
	orch.now = func() time.Time {
		return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	}
	return envStore, orch, backend, writer
}

func testEnvironment(id string) domain.Environment {
	return domain.Environment{
		ID:        id,
		Project:   "cms",
		Product:   "bethunder",
		Namespace: id + "-cms",
		Mode:      domain.ModeHybrid,
		Status:    domain.StatusCreating,
		TTLHours:  48,
		CreatedAt: time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC),
	}
}

type fakeBackend struct {
	renderCalls       int
	applyCalls        int
	deleteCalls       int
	statusCalls       int
	status            domain.EnvironmentStatus
	manifestContent   []byte
	lastProjectConfig domain.ProjectConfig
	err               error
}

func (f *fakeBackend) Render(_ context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) ([]Manifest, error) {
	f.renderCalls++
	f.lastProjectConfig = projectConfig
	if f.err != nil {
		return nil, f.err
	}
	return []Manifest{
		{Path: environment.NamespaceManifestFilename(), Kind: "Namespace", Content: []byte("namespace")},
		{Path: environment.ManifestFilename(), Kind: "FluxKustomization", Content: f.manifestContent},
		{Path: environment.PathKustomizationFilename(), Kind: "Kustomization", Content: []byte("kustomization")},
	}, nil
}

func (f *fakeBackend) Apply(context.Context, domain.Environment, domain.ProjectConfig) error {
	f.applyCalls++
	return f.err
}

func (f *fakeBackend) Delete(context.Context, domain.Environment, domain.ProjectConfig) error {
	f.deleteCalls++
	return f.err
}

func (f *fakeBackend) Status(_ context.Context, _ domain.Environment, projectConfig domain.ProjectConfig) (domain.EnvironmentStatus, error) {
	f.lastProjectConfig = projectConfig
	f.statusCalls++
	if f.status != "" {
		return f.status, f.err
	}
	return domain.StatusReady, f.err
}

type writerAwareBackend struct {
	manifestContent       []byte
	renderCalls           int
	applyCalls            int
	applyWithWriterCalls  int
	deleteCalls           int
	deleteWithWriterCalls int
	lastProjectConfig     domain.ProjectConfig
	err                   error
}

func (b *writerAwareBackend) Render(_ context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) ([]Manifest, error) {
	b.renderCalls++
	b.lastProjectConfig = projectConfig
	if b.err != nil {
		return nil, b.err
	}
	return []Manifest{
		{Path: environment.ManifestFilename(), Kind: "HelmDirect", Content: b.manifestContent},
	}, nil
}

func (b *writerAwareBackend) Apply(context.Context, domain.Environment, domain.ProjectConfig) error {
	b.applyCalls++
	return b.err
}

func (b *writerAwareBackend) ApplyWithWriter(ctx context.Context, environment domain.Environment, _ domain.ProjectConfig, writer gitops.Writer) (gitops.CommitResult, error) {
	b.applyWithWriterCalls++
	if writer == nil {
		return gitops.CommitResult{}, errors.New("writer is nil")
	}
	if b.err != nil {
		return gitops.CommitResult{}, b.err
	}
	return writer.Commit(ctx, "envpilot: create "+environment.ID)
}

func (b *writerAwareBackend) Delete(context.Context, domain.Environment, domain.ProjectConfig) error {
	b.deleteCalls++
	return b.err
}

func (b *writerAwareBackend) DeleteWithWriter(_ context.Context, _ domain.Environment, _ domain.ProjectConfig, writer gitops.Writer) (gitops.CommitResult, error) {
	b.deleteWithWriterCalls++
	if writer == nil {
		return gitops.CommitResult{}, errors.New("writer is nil")
	}
	if b.err != nil {
		return gitops.CommitResult{}, b.err
	}
	if _, err := writer.Commit(context.TODO(), "envpilot: delete"); err != nil {
		return gitops.CommitResult{}, err
	}
	return gitops.CommitResult{Committed: true}, nil
}

func (b *writerAwareBackend) Status(_ context.Context, _ domain.Environment, projectConfig domain.ProjectConfig) (domain.EnvironmentStatus, error) {
	b.lastProjectConfig = projectConfig
	return domain.StatusReady, b.err
}

type fakeHelmExecutor struct {
	calls                []helmUpgradeCall
	uninstallCalls       []helmUninstallCall
	statusCalls          []helmStatusCall
	readinessCalls       []helmReadinessCall
	err                  error
	uninstallErr         error
	namespaceManaged     bool
	namespaceManagedSet  bool
	namespaceDeleteErr   error
	deleteNamespaceCalls int
	statusResult         HelmStatus
	readinessResult      bool
	readinessErr         error
}

type helmUpgradeCall struct {
	Options HelmUpgradeOptions
	Content string
}

type helmUninstallCall struct {
	Options HelmUninstallOptions
}

type helmStatusCall struct {
	Options HelmStatusOptions
}

type helmReadinessCall struct {
	Options HelmReadinessOptions
}

func (f *fakeHelmExecutor) UpgradeInstall(_ context.Context, options HelmUpgradeOptions) error {
	call := helmUpgradeCall{Options: options}
	if options.ValuesFile != "" {
		if data, err := os.ReadFile(options.ValuesFile); err == nil {
			call.Content = string(data)
		}
	}
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeHelmExecutor) Uninstall(_ context.Context, options HelmUninstallOptions) error {
	f.uninstallCalls = append(f.uninstallCalls, helmUninstallCall{Options: options})
	if f.uninstallErr != nil {
		return f.uninstallErr
	}
	return f.err
}

func (f *fakeHelmExecutor) DeleteNamespace(_ context.Context, namespace string) error {
	f.deleteNamespaceCalls++
	if strings.TrimSpace(namespace) == "" {
		return nil
	}
	if f.namespaceDeleteErr != nil {
		return f.namespaceDeleteErr
	}
	return f.err
}

func (f *fakeHelmExecutor) Status(_ context.Context, options HelmStatusOptions) (HelmStatus, error) {
	f.statusCalls = append(f.statusCalls, helmStatusCall{Options: options})
	if f.err != nil {
		return HelmStatus{}, f.err
	}
	return f.statusResult, nil
}

func (f *fakeHelmExecutor) Readiness(_ context.Context, options HelmReadinessOptions) (bool, error) {
	f.readinessCalls = append(f.readinessCalls, helmReadinessCall{Options: options})
	if f.readinessErr != nil {
		return false, f.readinessErr
	}
	return f.readinessResult, nil
}

func (f *fakeHelmExecutor) IsNamespaceManaged(_ context.Context, namespace string, projectID string, environmentID string) (bool, error) {
	if strings.TrimSpace(namespace) == "" {
		return false, nil
	}
	if f.namespaceManagedSet {
		return f.namespaceManaged, nil
	}
	_ = projectID
	_ = environmentID
	return true, nil
}

type fakeWriter struct {
	writes               int
	removes              int
	removePaths          int
	commits              int
	lastWriteMessage     string
	lastRemovedPath      string
	lastCommitMessage    string
	path                 string
	commitPullRequestURL string
	err                  error
	removeErr            error
	commitErr            error
}

func (f *fakeWriter) WriteManifest(_ context.Context, _ string, _ []byte, message string) (string, error) {
	f.lastWriteMessage = message
	f.writes++
	return f.path, f.err
}

func (f *fakeWriter) RemoveManifest(_ context.Context, _ string, message string) error {
	_ = message
	f.removes++
	return f.err
}

func (f *fakeWriter) RemovePath(_ context.Context, path string, message string) error {
	f.lastRemovedPath = path
	f.lastCommitMessage = message
	f.removePaths++
	if f.removeErr != nil {
		return f.removeErr
	}
	return f.err
}

func (f *fakeWriter) Commit(_ context.Context, message string) (gitops.CommitResult, error) {
	f.commits++
	f.lastCommitMessage = message
	if f.commitErr != nil {
		return gitops.CommitResult{}, f.commitErr
	}
	return gitops.CommitResult{Committed: true, PullRequestURL: f.commitPullRequestURL}, f.err
}
