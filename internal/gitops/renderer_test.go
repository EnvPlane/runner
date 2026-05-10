package gitops

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"envpilot/internal/domain"
)

func TestFluxRendererUsesObservedFeatureEnvironmentPattern(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		FluxNamespace:   "flux-system",
		SourceRefName:   "apps",
		DependsOnName:   "flux.automation",
		ProductBasePath: "common/apps",
		HealthCheckName: "nginx",
		AppChartVersion: "1.1.10",
		InfraVersion:    "0.4.0",
		NginxVersion:    "0.8.11",
	})

	content, err := renderer.Render(domain.Environment{
		ID:        "kan-1701",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "kan-1701-cms",
		Mode:      domain.ModeHybrid,
		Domain:    "kan-1701.feature.int",
		Base: domain.BaseEnvironment{
			EnvironmentID: "feature",
			Namespace:     "feature",
			Domain:        "feature.int",
			Services: []domain.BaseServiceRef{
				{Name: "mysql", Namespace: "feature"},
				{Name: "redis", Namespace: "feature-shared"},
			},
		},
		Infrastructure: domain.Infrastructure{
			MySQL:     true,
			RabbitMQ:  true,
			Redis:     true,
			Memcached: true,
			MongoDB:   true,
			Zone:      "ca-central-1a",
			Capacity:  "spot",
		},
		Services: []domain.ServiceOverride{
			{Name: "backend", Tag: "dev-1.2.3", Replace: true},
			{Name: "api", Tag: "dev-4.5.6"},
			{Name: "frontend", Tag: "dev-7.8.9"},
		},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	yaml := string(content)
	assertContains(t, yaml, "name: kan-1701.bethunder")
	assertContains(t, yaml, "namespace: kan-1701-cms")
	assertContains(t, yaml, "path: common/apps/bethunder")
	assertContains(t, yaml, "dependsOn:")
	assertContains(t, yaml, "- name: flux.automation")
	assertContains(t, yaml, "cmsBackendTag: dev-1.2.3")
	assertContains(t, yaml, "cmsApiTag: dev-4.5.6")
	assertContains(t, yaml, "ZONE: ca-central-1a")
	assertContains(t, yaml, "mysqlEnabled: 'true'")
	assertContains(t, yaml, "ingressEnabled: 'true'")
	assertContains(t, yaml, "ingressHost: kan-1701.feature.int")
	assertContains(t, yaml, "previewUrl: https://kan-1701.feature.int")
	assertContains(t, yaml, "root_env: feature")
	assertContains(t, yaml, "baseNamespace: feature")
	assertContains(t, yaml, "baseServices: mysql=feature,redis=feature-shared")
	assertContains(t, yaml, "deployServices: backend")
	assertContains(t, yaml, "baseRoutedServices: api=feature,frontend=feature,mysql=feature,redis=feature-shared")
	assertContains(t, yaml, "routingStrategy: hybrid-ingress")
	assertContains(t, yaml, "overrideRoutes: backend=kan-1701-cms")
	assertContains(t, yaml, "fallbackRoutes: api=feature,frontend=feature,mysql=feature,redis=feature-shared")
	assertContains(t, yaml, "replacedServices: backend")
	assertContains(t, yaml, "apiDeployEnabled: 'false'")
	assertContains(t, yaml, "apiRouteNamespace: feature")
	assertContains(t, yaml, "apiRouteTarget: base")
	assertContains(t, yaml, "backendDeployEnabled: 'true'")
	assertContains(t, yaml, "frontendDeployEnabled: 'false'")
	assertContains(t, yaml, "frontendBaseNamespace: feature")
	assertContains(t, yaml, "frontendRouteNamespace: feature")
	assertContains(t, yaml, "frontendRouteTarget: base")
}

func TestFluxRendererHybridIngressRoutesOverridesToPreviewAndOthersToBase(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		FluxNamespace:   "flux-system",
		SourceRefName:   "apps",
		ProductBasePath: "common/apps",
	})

	content, err := renderer.Render(domain.Environment{
		ID:        "kan-1804",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "kan-1804-cms",
		Mode:      domain.ModeHybrid,
		Domain:    "kan-1804.feature.int",
		Base: domain.BaseEnvironment{
			EnvironmentID: "feature",
			Namespace:     "feature",
			Services: []domain.BaseServiceRef{
				{Name: "api", Namespace: "feature"},
				{Name: "backend", Namespace: "feature"},
				{Name: "frontend", Namespace: "feature-shared"},
			},
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "api-base"},
			{Name: "backend", Tag: "backend-pr", Replace: true},
			{Name: "frontend", Tag: "frontend-base"},
		},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	yaml := string(content)
	assertContains(t, yaml, "routingStrategy: hybrid-ingress")
	assertContains(t, yaml, "overrideRoutes: backend=kan-1804-cms")
	assertContains(t, yaml, "fallbackRoutes: api=feature,frontend=feature-shared")
	assertContains(t, yaml, "backendDeployEnabled: 'true'")
	assertContains(t, yaml, "backendRouteNamespace: kan-1804-cms")
	assertContains(t, yaml, "backendRouteTarget: override")
	assertContains(t, yaml, "apiDeployEnabled: 'false'")
	assertContains(t, yaml, "apiRouteNamespace: feature")
	assertContains(t, yaml, "apiRouteTarget: base")
	assertContains(t, yaml, "frontendDeployEnabled: 'false'")
	assertContains(t, yaml, "frontendRouteNamespace: feature-shared")
	assertContains(t, yaml, "frontendRouteTarget: base")
}

func TestFluxRendererFullModeDeploysAllServicesLocally(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		FluxNamespace:   "flux-system",
		SourceRefName:   "apps",
		DependsOnName:   "flux.automation",
		ProductBasePath: "common/apps",
		HealthCheckName: "nginx",
	})

	content, err := renderer.Render(domain.Environment{
		ID:        "kan-1803",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "kan-1803-cms",
		Mode:      domain.ModeFull,
		Base: domain.BaseEnvironment{
			Namespace: "feature",
		},
		Services: []domain.ServiceOverride{
			{Name: "backend", Tag: "dev-1.2.3"},
			{Name: "frontend", Tag: "dev-7.8.9"},
		},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	yaml := string(content)
	assertContains(t, yaml, "deployServices: backend,frontend")
	assertContains(t, yaml, "baseRoutedServices: ")
	assertContains(t, yaml, "routingStrategy: full-ingress")
	assertContains(t, yaml, "overrideRoutes: backend=kan-1803-cms,frontend=kan-1803-cms")
	assertContains(t, yaml, "fallbackRoutes: ")
	assertContains(t, yaml, "backendDeployEnabled: 'true'")
	assertContains(t, yaml, "backendRouteNamespace: kan-1803-cms")
	assertContains(t, yaml, "backendRouteTarget: override")
	assertContains(t, yaml, "frontendDeployEnabled: 'true'")
	assertNotContains(t, yaml, "backendBaseNamespace:")
	assertNotContains(t, yaml, "frontendBaseNamespace:")
}

func TestFluxRendererGeneratesNamespaceManifest(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{})

	content, err := renderer.RenderNamespace(domain.Environment{
		ID:      "feature-checkout",
		Project: "cms",
		Product: "bethunder",
		Source: domain.SCMSource{
			PullRequestID: "123",
		},
	})
	if err != nil {
		t.Fatalf("render namespace failed: %v", err)
	}

	yaml := string(content)
	assertContains(t, yaml, "apiVersion: v1")
	assertContains(t, yaml, "kind: Namespace")
	assertContains(t, yaml, "metadata:\n  name: envpilot-pr-123")
	assertContains(t, yaml, "envpilot.io/environment-id: feature-checkout")
	assertContains(t, yaml, "kind: ResourceQuota")
	assertContains(t, yaml, "name: envpilot-preview-quota")
	assertContains(t, yaml, "namespace: envpilot-pr-123")
	assertContains(t, yaml, `requests.cpu: "2"`)
	assertContains(t, yaml, "requests.memory: 4Gi")
	assertContains(t, yaml, `limits.cpu: "4"`)
	assertContains(t, yaml, "limits.memory: 8Gi")
	assertContains(t, yaml, "kind: LimitRange")
	assertContains(t, yaml, "name: envpilot-preview-limits")
	assertContains(t, yaml, "default:")
	assertContains(t, yaml, "cpu: 500m")
	assertContains(t, yaml, "memory: 512Mi")
	assertContains(t, yaml, "defaultRequest:")
	assertContains(t, yaml, "cpu: 100m")
	assertContains(t, yaml, "memory: 128Mi")
}

func TestFluxRendererAppliesConfigurableOverrides(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		FluxNamespace:   "flux-system",
		SourceRefName:   "apps",
		DependsOnName:   "flux.automation",
		ProductBasePath: "common/apps",
		HealthCheckName: "nginx",
	})

	content, err := renderer.Render(domain.Environment{
		ID:        "kan-1704",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "kan-1704-cms",
		Mode:      domain.ModeHybrid,
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "abc1234"},
		},
		Overrides: map[string]string{
			"cmsApiTag":     "override-api-tag",
			"featureToggle": "'true'",
		},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	yaml := string(content)
	assertContains(t, yaml, "cmsApiTag: override-api-tag")
	assertContains(t, yaml, "featureToggle: 'true'")
}

func TestFluxRendererGeneratesPathBasedManifestSet(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		FluxNamespace:   "flux-system",
		SourceRefName:   "apps",
		DependsOnName:   "flux.automation",
		ProductBasePath: "common/apps",
		HealthCheckName: "nginx",
	})

	manifests, err := renderer.RenderManifestSet(domain.Environment{
		ID:        "kan-1705",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "kan-1705-cms",
		Mode:      domain.ModeHybrid,
	})
	if err != nil {
		t.Fatalf("render manifest set failed: %v", err)
	}
	if len(manifests) != 3 {
		t.Fatalf("expected 3 manifests, got %d", len(manifests))
	}

	byPath := map[string]string{}
	for _, manifest := range manifests {
		byPath[manifest.Path] = string(manifest.Content)
	}
	assertContains(t, byPath["feature-envs/cms/kan-1705/namespace.yaml"], "kind: Namespace")
	assertContains(t, byPath["feature-envs/cms/kan-1705/flux-kustomization.yaml"], "apiVersion: kustomize.toolkit.fluxcd.io/v1")
	assertContains(t, byPath["feature-envs/cms/kan-1705/flux-kustomization.yaml"], "kind: Kustomization")
	assertContains(t, byPath["feature-envs/cms/kan-1705/flux-kustomization.yaml"], "path: common/apps/bethunder")
	assertContains(t, byPath["feature-envs/cms/kan-1705/kustomization.yaml"], "apiVersion: kustomize.config.k8s.io/v1beta1")
	assertContains(t, byPath["feature-envs/cms/kan-1705/kustomization.yaml"], "- namespace.yaml")
	assertContains(t, byPath["feature-envs/cms/kan-1705/kustomization.yaml"], "- flux-kustomization.yaml")
}

func TestFluxRendererGeneratesHelmReleaseManifestSet(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		FluxNamespace:   "flux-system",
		SourceRefName:   "apps",
		ProductBasePath: "charts",
	})

	manifests, err := renderer.RenderManifestSet(domain.Environment{
		ID:        "pr-42",
		Project:   "checkout",
		Product:   "payments",
		Namespace: "envpilot-pr-42",
		Mode:      domain.ModeFull,
		GitOps: domain.GitOpsTarget{
			Renderer:      "helm",
			Path:          "deploy/helm/payments",
			ValuesPath:    "values-preview.yaml",
			SourceRefName: "tenant-apps",
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "abc123"},
		},
		Overrides: map[string]string{
			"apiImageTag": "abc123",
		},
	})
	if err != nil {
		t.Fatalf("render manifest set failed: %v", err)
	}

	byPath := map[string]string{}
	for _, manifest := range manifests {
		byPath[manifest.Path] = string(manifest.Content)
	}
	assertContains(t, byPath["feature-envs/checkout/pr-42/namespace.yaml"], "kind: Namespace")
	assertContains(t, byPath["feature-envs/checkout/pr-42/helm-release.yaml"], "apiVersion: helm.toolkit.fluxcd.io/v2")
	assertContains(t, byPath["feature-envs/checkout/pr-42/helm-release.yaml"], "kind: HelmRelease")
	assertContains(t, byPath["feature-envs/checkout/pr-42/helm-release.yaml"], "chart: deploy/helm/payments")
	assertContains(t, byPath["feature-envs/checkout/pr-42/helm-release.yaml"], "name: tenant-apps")
	assertContains(t, byPath["feature-envs/checkout/pr-42/helm-release.yaml"], "- values-preview.yaml")
	assertContains(t, byPath["feature-envs/checkout/pr-42/helm-release.yaml"], "apiImageTag: abc123")
	assertContains(t, byPath["feature-envs/checkout/pr-42/kustomization.yaml"], "- namespace.yaml")
	assertContains(t, byPath["feature-envs/checkout/pr-42/kustomization.yaml"], "- helm-release.yaml")
}

func TestFluxRendererPassesImageTagValuesToHelmTemplate(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		SourceRefName: "apps",
	})

	manifests, err := renderer.RenderManifestSet(domain.Environment{
		ID:        "pr-43",
		Project:   "checkout",
		Product:   "payments",
		Namespace: "envpilot-pr-43",
		GitOps: domain.GitOpsTarget{
			Renderer: "helm",
			Path:     "charts/payments",
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "commit-sha-123"},
		},
		Overrides: map[string]string{
			"apiImageTag": "commit-sha-123",
		},
	})
	if err != nil {
		t.Fatalf("render manifest set failed: %v", err)
	}

	byPath := map[string]string{}
	for _, manifest := range manifests {
		byPath[manifest.Path] = string(manifest.Content)
	}
	helmRelease := byPath["feature-envs/checkout/pr-43/helm-release.yaml"]
	assertContains(t, helmRelease, "values:")
	assertContains(t, helmRelease, "apiImageTag: commit-sha-123")
}

func TestFluxRendererGeneratesRawManifestSet(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{})
	manifests, err := renderer.RenderManifestSet(domain.Environment{
		ID:        "pr-77",
		Project:   "checkout",
		Product:   "payments",
		Namespace: "envpilot-pr-77",
		Domain:    "pr-77.checkout.preview.example.com",
		Mode:      domain.ModeFull,
		GitOps: domain.GitOpsTarget{
			Renderer: "raw",
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "abc123"},
		},
	})
	if err != nil {
		t.Fatalf("render manifest set: %v", err)
	}
	byPath := map[string]string{}
	for _, manifest := range manifests {
		byPath[manifest.Path] = string(manifest.Content)
	}
	assertContains(t, byPath["feature-envs/checkout/pr-77/raw-manifests.yaml"], "kind: Deployment")
	assertContains(t, byPath["feature-envs/checkout/pr-77/raw-manifests.yaml"], "image: api:abc123")
	assertContains(t, byPath["feature-envs/checkout/pr-77/raw-manifests.yaml"], "kind: Ingress")
	assertContains(t, byPath["feature-envs/checkout/pr-77/kustomization.yaml"], "- raw-manifests.yaml")
}

func TestFluxRendererRawManifestIngressUsesPreviewHost(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{})
	manifest, err := renderer.RenderRawManifests(domain.Environment{
		ID:        "pr-123",
		Project:   "checkout",
		Product:   "generic",
		Namespace: "envpilot-pr-123",
		Domain:    "pr-123.checkout.preview.local",
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "abc123"},
		},
	})
	if err != nil {
		t.Fatalf("render raw manifests: %v", err)
	}
	assertContains(t, string(manifest), "kind: Ingress")
	assertContains(t, string(manifest), "host: pr-123.checkout.preview.local")
}

func TestFluxRendererRawHybridManifestsDeployOnlyOverrideServices(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{})
	manifest, err := renderer.RenderRawManifests(domain.Environment{
		ID:        "pr-124",
		Project:   "checkout",
		Product:   "generic",
		Namespace: "envpilot-pr-124",
		Domain:    "pr-124.checkout.preview.local",
		Mode:      domain.ModeHybrid,
		Base: domain.BaseEnvironment{
			Namespace: "feature",
			Services: []domain.BaseServiceRef{
				{Name: "api", Namespace: "feature"},
				{Name: "backend", Namespace: "feature"},
			},
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "abc123"},
			{Name: "backend", Tag: "def456", Replace: true},
		},
	})
	if err != nil {
		t.Fatalf("render raw manifests: %v", err)
	}
	yaml := string(manifest)
	assertContains(t, yaml, "name: backend")
	assertContains(t, yaml, "image: backend:def456")
	assertContains(t, yaml, "service:\n                name: backend")
	assertNotContains(t, yaml, "name: api")
	assertNotContains(t, yaml, "image: api:abc123")
}

func TestFluxRendererGeneratesKustomizeOverlayManifestSet(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{ProductBasePath: "apps"})
	manifests, err := renderer.RenderManifestSet(domain.Environment{
		ID:        "pr-78",
		Project:   "checkout",
		Product:   "payments",
		Namespace: "envpilot-pr-78",
		Domain:    "pr-78.checkout.preview.example.com",
		Mode:      domain.ModeFull,
		GitOps: domain.GitOpsTarget{
			Renderer: "kustomize-overlay",
			Path:     "apps/payments/base",
		},
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "def456"},
		},
	})
	if err != nil {
		t.Fatalf("render manifest set: %v", err)
	}
	byPath := map[string]string{}
	for _, manifest := range manifests {
		byPath[manifest.Path] = string(manifest.Content)
	}
	assertContains(t, byPath["feature-envs/checkout/pr-78/overlay/kustomization.yaml"], "- ../../../apps/payments/base")
	assertContains(t, byPath["feature-envs/checkout/pr-78/overlay/kustomization.yaml"], "newTag: def456")
	assertContains(t, byPath["feature-envs/checkout/pr-78/overlay/kustomization.yaml"], "value: \"pr-78.checkout.preview.example.com\"")
	assertContains(t, byPath["feature-envs/checkout/pr-78/kustomization.yaml"], "- overlay")
}

func TestFluxRendererValuesPreviewMergesSubstitutions(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{})
	values := renderer.RenderValuesPreview(domain.Environment{
		ID:        "pr-79",
		Project:   "checkout",
		Product:   "payments",
		Namespace: "envpilot-pr-79",
		Domain:    "pr-79.checkout.preview.example.com",
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "abc123"},
		},
		Overrides: map[string]string{"featureToggle": "'true'"},
	})
	if values["cmsApiTag"] != "abc123" {
		t.Fatalf("cmsApiTag = %q", values["cmsApiTag"])
	}
	if values["featureToggle"] != "'true'" {
		t.Fatalf("featureToggle = %q", values["featureToggle"])
	}
	yaml := ValuesYAML(values)
	assertContains(t, yaml, "cmsApiTag: abc123")
	assertContains(t, yaml, "featureToggle: 'true'")
}

func TestFluxRendererManifestTemplatesAreValidYAML(t *testing.T) {
	renderer := NewFluxRenderer(FluxOptions{
		ProductBasePath: "apps",
		SourceRefName:   "apps",
	})
	cases := []struct {
		name string
		env  domain.Environment
	}{
		{
			name: "flux-kustomization",
			env: domain.Environment{
				ID:        "pr-90",
				Project:   "checkout",
				Product:   "payments",
				Namespace: "envpilot-pr-90",
				Domain:    "pr-90.checkout.preview.example.com",
				Mode:      domain.ModeHybrid,
				GitOps: domain.GitOpsTarget{
					Path: "apps/payments",
				},
				Services: []domain.ServiceOverride{
					{Name: "api", Tag: "abc123"},
				},
			},
		},
		{
			name: "helm",
			env: domain.Environment{
				ID:        "pr-91",
				Project:   "checkout",
				Product:   "payments",
				Namespace: "envpilot-pr-91",
				Domain:    "pr-91.checkout.preview.example.com",
				Mode:      domain.ModeFull,
				GitOps: domain.GitOpsTarget{
					Renderer:   "helm",
					Path:       "apps/payments",
					ValuesPath: "values.yaml",
				},
				Services: []domain.ServiceOverride{
					{Name: "api", Tag: "abc123"},
				},
			},
		},
		{
			name: "raw",
			env: domain.Environment{
				ID:        "pr-92",
				Project:   "checkout",
				Product:   "payments",
				Namespace: "envpilot-pr-92",
				Domain:    "pr-92.checkout.preview.example.com",
				Mode:      domain.ModeFull,
				GitOps: domain.GitOpsTarget{
					Renderer: "raw",
				},
				Services: []domain.ServiceOverride{
					{Name: "api", Tag: "abc123"},
				},
			},
		},
		{
			name: "kustomize-overlay",
			env: domain.Environment{
				ID:        "pr-93",
				Project:   "checkout",
				Product:   "payments",
				Namespace: "envpilot-pr-93",
				Domain:    "pr-93.checkout.preview.example.com",
				Mode:      domain.ModeFull,
				GitOps: domain.GitOpsTarget{
					Renderer: "kustomize-overlay",
					Path:     "apps/payments",
				},
				Services: []domain.ServiceOverride{
					{Name: "api", Tag: "abc123"},
				},
			},
		},
	}
	for _, item := range cases {
		manifests, err := renderer.RenderManifestSet(item.env)
		if err != nil {
			t.Fatalf("[%s] render manifest set: %v", item.name, err)
		}
		for _, manifest := range manifests {
			if err := validateRenderedYAMLDocuments(manifest.Content, manifest.Path); err != nil {
				t.Fatalf("[%s] %s: %v", item.name, manifest.Path, err)
			}
		}
	}
}

func validateRenderedYAMLDocuments(content []byte, path string) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var document interface{}
		err := decoder.Decode(&document)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if document == nil {
			continue
		}
	}
}

func assertContains(t *testing.T, value string, expected string) {
	t.Helper()
	if !strings.Contains(value, expected) {
		t.Fatalf("expected rendered manifest to contain %q\n%s", expected, value)
	}
}

func assertNotContains(t *testing.T, value string, expected string) {
	t.Helper()
	if strings.Contains(value, expected) {
		t.Fatalf("expected rendered manifest not to contain %q\n%s", expected, value)
	}
}
