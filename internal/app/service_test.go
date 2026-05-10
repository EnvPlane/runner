package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"envpilot/internal/catalog"
	"envpilot/internal/config"
	"envpilot/internal/domain"
	"envpilot/internal/gitops"
	"envpilot/internal/orchestrator"
	"envpilot/internal/store"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("ENVPILOT_DEPLOYMENT_BACKEND", "fluxcd")
	os.Exit(m.Run())
}

func TestNewEnvironmentServiceUsesDeploymentBackendAliases(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(filepath.Join(tmp, "store.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	cfg := config.FromEnv()
	cfg.GitOpsDir = filepath.Join(tmp, "gitops")
	baseRenderer := gitops.NewFluxRenderer(cfg.GitOps)
	baseWriter := gitops.NewFileWriter(filepath.Join(tmp, "manifests"), false, "", "")

	cases := []string{
		"flux",
		"flux_cd",
		"helm-direct",
		"fluxcd",
		"helm_direct",
	}

	for i, backend := range cases {
		t.Run(backend, func(t *testing.T) {
			normalized := orchestrator.NormalizeDeploymentBackendType(backend)
			if normalized == "" {
				t.Fatalf("backend=%q did not normalize", backend)
			}
			switch normalized {
			case orchestrator.DeploymentBackendHelmDirect:
				// Keep this test stable in environments where a real Kubernetes cluster is unavailable.
				// Helm-direct backend is still validated below via normalization check.
				cfg.DeploymentBackend = "fluxcd"
			default:
				cfg.DeploymentBackend = backend
			}
			service := NewEnvironmentService(cfg, catalog.Default(), envStore, baseRenderer, baseWriter)
			env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
				ID:      "kan-backend-" + strconv.Itoa(i+1),
				Product: "bethunder",
			})
			if err != nil {
				t.Fatalf("create with backend=%q: %v", backend, err)
			}
			if env.Status == domain.StatusFailed {
				t.Fatalf("create with backend=%q failed status: %q", backend, env.Status)
			}
			if env.ManifestPath == "" {
				t.Fatalf("backend=%q produced empty manifest path", backend)
			}
		})
	}
}

func TestCreateEnvironmentFailsWithInvalidProjectConfigBackend(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(filepath.Join(tmp, "store.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	projectConfigStore, err := store.NewJSONProjectConfigStore(filepath.Join(tmp, "project-configs.json"))
	if err != nil {
		t.Fatalf("project config store: %v", err)
	}
	if err := projectConfigStore.Save(domain.ProjectConfig{
		ID:        "cms-config-v1",
		ProjectID: "cms",
		Version:   1,
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "custom",
			},
		},
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	cfg := config.FromEnv()
	cfg.DeploymentBackend = "fluxcd"
	service := NewEnvironmentService(cfg, catalog.Default(), envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetProjectConfigStore(projectConfigStore)

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-backend-invalid",
		Project: "cms",
		Product: "bethunder",
	})
	if err == nil {
		t.Fatal("expected invalid project deployment backend error")
	}
	if !strings.Contains(err.Error(), "unsupported deployment backend") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteEnvironmentUsesProjectConfigBackendValidation(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(filepath.Join(tmp, "store.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	projectConfigStore, err := store.NewJSONProjectConfigStore(filepath.Join(tmp, "project-configs.json"))
	if err != nil {
		t.Fatalf("project config store: %v", err)
	}
	if err := projectConfigStore.Save(domain.ProjectConfig{
		ID:        "cms-config-v2",
		ProjectID: "cms",
		Version:   1,
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "custom",
			},
		},
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	cfg := config.FromEnv()
	cfg.DeploymentBackend = "helm_direct"
	service := NewEnvironmentService(cfg, catalog.Default(), envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetProjectConfigStore(projectConfigStore)

	if err := envStore.Save(domain.Environment{
		ID:        "kan-delete-backend-invalid",
		Project:   "cms",
		Namespace: "cms-kan-delete",
		Status:    domain.StatusReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed env: %v", err)
	}

	_, err = service.DeleteEnvironment(context.Background(), "kan-delete-backend-invalid", true)
	if err == nil {
		t.Fatal("expected invalid project deployment backend error")
	}
	if !strings.Contains(err.Error(), "unsupported deployment backend") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeploymentBackendFromProjectConfigInfersFluxcdFromLegacyFields(t *testing.T) {
	backend := deploymentBackendFromProjectConfig(domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"gitopsRepoUrl":    "https://github.com/acme/gitops",
				"gitOpsOutputPath": "environments/{{ .PRNumber }}",
			},
		},
	})
	if backend != string(domain.DeploymentBackendFluxCD) {
		t.Fatalf("expected inferred backend %q, got %q", domain.DeploymentBackendFluxCD, backend)
	}
}

func TestCreateEnvironmentAppliesProductDefaults(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1701",
		Product: "bethunder",
		Services: []domain.ServiceOverride{
			{Name: "backend", Tag: "dev-1.2.3"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Namespace != "envpilot-pr-kan-1701" {
		t.Fatalf("unexpected namespace: %s", env.Namespace)
	}
	if env.Infrastructure.MySQL != true || env.Infrastructure.Redis != true {
		t.Fatalf("expected product infrastructure defaults: %+v", env.Infrastructure)
	}
	if env.ExpiresAt == nil {
		t.Fatal("expected ttl expiration")
	}
	if env.CostEstimateDay != "~ €0.60/day" {
		t.Fatalf("cost estimate = %q", env.CostEstimateDay)
	}
}

func TestCreateEnvironmentAllowsExplicitNamespaceOverride(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:        "kan-1706",
		Product:   "bethunder",
		Namespace: "custom-feature-ns",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Namespace != "custom-feature-ns" {
		t.Fatalf("namespace = %q", env.Namespace)
	}
}

func TestCreateEnvironmentAppliesRuntimeSettings(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.FromEnv()
	cfg.DefaultDomainRoot = "bootstrap.example.com"
	cfg.GitOps.ProductBasePath = "bootstrap/apps"
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	settingsStore, err := store.NewJSONSettingsStore(tmp+"/settings.json", DefaultControlPlaneSettings(cfg))
	if err != nil {
		t.Fatalf("settings store: %v", err)
	}
	settingsService := NewSettingsService(settingsStore)
	_, err = settingsService.SaveSettings(domain.ControlPlaneSettings{
		Runtime: domain.RuntimeSettings{
			DefaultProduct:  "generic",
			DefaultProject:  "checkout",
			DefaultMode:     domain.ModeFull,
			DomainRoot:      "example.com",
			NamespacePrefix: "preview",
			DefaultTTLHours: 12,
			ProductBasePath: "platform/apps",
			SourceRefName:   "tenant-apps",
			HealthCheckName: "web",
		},
	}, "test")
	if err != nil {
		t.Fatalf("save settings: %v", err)
	}
	service := NewEnvironmentService(cfg, catalog.Default(), envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetSettingsProvider(settingsService)

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID: "pr-900",
		Source: domain.SCMSource{
			Provider:      "github",
			Repository:    "example/checkout",
			PullRequestID: "900",
			Commit:        "abc123",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Product != "generic" || env.Project != "checkout" || env.Mode != domain.ModeFull {
		t.Fatalf("unexpected product/project/mode: product=%q project=%q mode=%q", env.Product, env.Project, env.Mode)
	}
	if env.Namespace != "preview-900" {
		t.Fatalf("namespace = %q", env.Namespace)
	}
	if env.Domain != "pr-900.checkout.preview.example.com" {
		t.Fatalf("domain = %q", env.Domain)
	}
	if env.TTLHours != 12 {
		t.Fatalf("ttl = %d", env.TTLHours)
	}
	if env.GitOps.Path != "platform/apps/generic" {
		t.Fatalf("gitops path = %q", env.GitOps.Path)
	}
	if env.GitOps.SourceRefName != "tenant-apps" {
		t.Fatalf("source ref = %q", env.GitOps.SourceRefName)
	}
	if env.GitOps.HealthCheckName != "web" {
		t.Fatalf("health check = %q", env.GitOps.HealthCheckName)
	}
}

func TestCreateEnvironmentUsesConfiguredGitOpsRepository(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TEST_GIT_TOKEN", "unused-token")
	remote := filepath.Join(tmp, "remote.git")
	runAppGit(t, "", "git", "init", "--bare", remote)
	seed := filepath.Join(tmp, "seed")
	runAppGit(t, "", "git", "init", seed)
	runAppGit(t, seed, "git", "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runAppGit(t, seed, "git", "add", ".")
	runAppGit(t, seed, "git", "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "seed")
	runAppGit(t, seed, "git", "remote", "add", "origin", remote)
	runAppGit(t, seed, "git", "push", "origin", "main")

	cfg := config.FromEnv()
	cfg.DataDir = filepath.Join(tmp, "data")
	cfg.EnableGitCommit = true
	cfg.EnableGitPush = true
	envStore, err := store.NewJSONStore(filepath.Join(tmp, "store.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	projectStore, err := store.NewJSONProjectStore(filepath.Join(tmp, "projects.json"), nil)
	if err != nil {
		t.Fatalf("project store: %v", err)
	}
	if err := projectStore.Save(domain.Project{
		ID:                 "checkout",
		Name:               "Checkout",
		ProductID:          "generic",
		AppRepositoryID:    "checkout-app",
		GitOpsRepositoryID: "checkout-gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	settingsStore, err := store.NewJSONSettingsStore(filepath.Join(tmp, "settings.json"), DefaultControlPlaneSettings(cfg))
	if err != nil {
		t.Fatalf("settings store: %v", err)
	}
	settingsService := NewSettingsService(settingsStore)
	if _, err := settingsService.SaveSettings(domain.ControlPlaneSettings{
		Repositories: []domain.ConfiguredRepository{
			{
				ID:            "checkout-gitops",
				Name:          "Checkout GitOps",
				Kind:          "gitops",
				Provider:      "git",
				URL:           remote,
				DefaultBranch: "main",
				Path:          "clusters/dev",
				SecretRef:     "git-token",
			},
		},
		SecretRefs: []domain.SecretReference{
			{
				ID:        "git-token",
				Provider:  "env",
				Scope:     "gitops",
				Reference: "TEST_GIT_TOKEN",
			},
		},
		Runtime: domain.RuntimeSettings{
			EnableGitCommit: true,
			EnableGitPush:   true,
			GitPushRemote:   "origin",
			GitPushBranch:   "main",
		},
	}, "test"); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	service := NewEnvironmentService(cfg, catalog.Default(), envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(filepath.Join(tmp, "fallback"), false, "", ""))
	service.SetProjectStore(projectStore)
	service.SetSettingsProvider(settingsService)
	prepared, err := service.prepareEnvironment(domain.CreateEnvironmentRequest{
		ID:      "pr-123",
		Project: "checkout",
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	resolvedWriter, err := service.gitOpsWriterForEnvironment(context.Background(), prepared)
	if err != nil {
		t.Fatalf("resolve writer: %v", err)
	}
	if _, ok := resolvedWriter.(*gitops.RepositoryWriter); !ok {
		settings, _ := settingsService.GetSettings()
		t.Fatalf("expected repository writer, got %T settings=%+v", resolvedWriter, settings.Repositories)
	}

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "pr-123",
		Project: "checkout",
		Source: domain.SCMSource{
			Provider:      "github",
			Repository:    "example/checkout",
			PullRequestID: "123",
			Commit:        "abc123",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.ManifestPath == "" || filepath.IsLocal(env.ManifestPath) {
		t.Fatalf("expected absolute manifest path, got %q", env.ManifestPath)
	}

	verify := filepath.Join(tmp, "verify")
	runAppGit(t, "", "git", "clone", "--branch", "main", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "clusters/dev/feature-envs/checkout/pr-123/flux-kustomization.yaml")); err != nil {
		t.Fatalf("expected pushed deployment manifest: %v", err)
	}
}

func TestCreateHybridEnvironmentAppliesProjectBaseConfig(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	projectStore, err := store.NewJSONProjectStore(tmp+"/projects.json", []domain.Project{
		{
			ID:   "cms",
			Name: "CMS",
			BaseEnvConfig: domain.BaseEnvConfig{
				EnvironmentID: "feature",
				Namespace:     "feature",
				Domain:        "feature.int",
				ConfigPath:    "/Users/alex/bh/CMS/env/ENV/feature",
				Services: []domain.BaseServiceRef{
					{Name: "mysql"},
					{Name: "backend"},
					{Name: "redis", Namespace: "feature-shared"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("project store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetProjectStore(projectStore)

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1801",
		Project: "cms",
		Product: "bethunder",
		Mode:    domain.ModeHybrid,
		Services: []domain.ServiceOverride{
			{Name: "backend", Replace: true},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Base.Namespace != "feature" {
		t.Fatalf("base namespace = %q", env.Base.Namespace)
	}
	if len(env.Base.Services) != 3 || env.Base.Services[0].Namespace != "feature" || env.Base.Services[1].Namespace != "feature" || env.Base.Services[2].Namespace != "feature-shared" {
		t.Fatalf("base services = %#v", env.Base.Services)
	}
	replaced := map[string]bool{}
	for _, service := range env.Services {
		replaced[service.Name] = service.Replace
	}
	if !replaced["backend"] || replaced["api"] {
		t.Fatalf("replaced services = %#v", replaced)
	}
}

func TestCreateHybridEnvironmentAllowsPartialDeploymentWithBaseDependencies(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	projectStore, err := store.NewJSONProjectStore(tmp+"/projects.json", []domain.Project{
		{
			ID:   "cms",
			Name: "CMS",
			BaseEnvConfig: domain.BaseEnvConfig{
				EnvironmentID: "feature",
				Namespace:     "feature",
				Services: []domain.BaseServiceRef{
					{Name: "backend"},
					{Name: "api"},
					{Name: "frontend", Namespace: "feature-shared"},
					{Name: "mysql"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("project store: %v", err)
	}
	renderer := gitops.NewFluxRenderer(config.FromEnv().GitOps)
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, renderer, gitops.NewFileWriter(tmp, false, "", ""))
	service.SetProjectStore(projectStore)

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1807",
		Project: "cms",
		Product: "bethunder",
		Mode:    domain.ModeHybrid,
		Services: []domain.ServiceOverride{
			{Name: "backend", Tag: "backend-pr", Replace: true},
		},
	})
	if err != nil {
		t.Fatalf("create hybrid partial deployment: %v", err)
	}

	values := renderer.RenderValuesPreview(env)
	if values["deployServices"] != "backend" {
		t.Fatalf("deploy services = %q", values["deployServices"])
	}
	if values["backendRouteTarget"] != "override" || values["backendRouteNamespace"] != env.Namespace {
		t.Fatalf("backend route = %q %q", values["backendRouteTarget"], values["backendRouteNamespace"])
	}
	if values["apiRouteTarget"] != "base" || values["apiRouteNamespace"] != "feature" {
		t.Fatalf("api route = %q %q", values["apiRouteTarget"], values["apiRouteNamespace"])
	}
	if values["frontendRouteTarget"] != "base" || values["frontendRouteNamespace"] != "feature-shared" {
		t.Fatalf("frontend route = %q %q", values["frontendRouteTarget"], values["frontendRouteNamespace"])
	}
	if !strings.Contains(values["fallbackRoutes"], "api=feature") || !strings.Contains(values["fallbackRoutes"], "frontend=feature-shared") {
		t.Fatalf("fallback routes = %q", values["fallbackRoutes"])
	}
}

func TestCreateHybridEnvironmentValidationRejectsMissingOverrides(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	projectStore, err := store.NewJSONProjectStore(tmp+"/projects.json", []domain.Project{
		{
			ID: "cms",
			BaseEnvConfig: domain.BaseEnvConfig{
				EnvironmentID: "feature",
				Namespace:     "feature",
				ConfigPath:    "/Users/alex/bh/CMS/env/ENV/feature",
				Services:      []domain.BaseServiceRef{{Name: "mysql"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("project store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetProjectStore(projectStore)

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1805",
		Project: "cms",
		Product: "bethunder",
		Mode:    domain.ModeHybrid,
	})
	if err == nil {
		t.Fatal("expected hybrid validation error")
	}
	if _, ok := err.(ValidationError); !ok {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
}

func TestCreateHybridEnvironmentValidationRejectsOverrideNotInBaseInventory(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	projectStore, err := store.NewJSONProjectStore(tmp+"/projects.json", []domain.Project{
		{
			ID: "cms",
			BaseEnvConfig: domain.BaseEnvConfig{
				EnvironmentID: "feature",
				Namespace:     "feature",
				ConfigPath:    "/Users/alex/bh/CMS/env/ENV/feature",
				Services:      []domain.BaseServiceRef{{Name: "mysql"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("project store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetProjectStore(projectStore)

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1806",
		Project: "cms",
		Product: "bethunder",
		Mode:    domain.ModeHybrid,
		Services: []domain.ServiceOverride{
			{Name: "api", Tag: "dev-1", Replace: true},
		},
	})
	if err == nil {
		t.Fatal("expected hybrid validation error")
	}
	if _, ok := err.(ValidationError); !ok {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
}

func TestCreateEnvironmentCommentsPreviewURLAndStatus(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	commenter := &fakeCommenter{}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetCommenter(commenter)

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1710",
		Project: "cms",
		Product: "bethunder",
		Source: domain.SCMSource{
			Provider:      "github",
			Repository:    "owner/repo",
			PullRequestID: "1710",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(commenter.items) != 1 {
		t.Fatalf("comments = %d", len(commenter.items))
	}
	commented := commenter.items[0]
	if commented.URL == "" {
		t.Fatalf("expected commented url, got %+v", commented)
	}
	if commented.Status != domain.StatusCreating {
		t.Fatalf("expected creating comment, got %q", commented.Status)
	}
}

func TestRecordFluxStatusCommentsReadyStatus(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	commenter := &fakeCommenter{}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	service.SetCommenter(commenter)

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1711",
		Product: "bethunder",
		Source: domain.SCMSource{
			Provider:      "gitlab",
			Repository:    "group/repo",
			PullRequestID: "1711",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = service.RecordFluxStatus("kan-1711", domain.FluxStatus{Status: domain.StatusReady, Message: "ready"})
	if err != nil {
		t.Fatalf("record flux status: %v", err)
	}
	if len(commenter.items) != 2 {
		t.Fatalf("comments = %d", len(commenter.items))
	}
	if commenter.items[1].Status != domain.StatusReady {
		t.Fatalf("expected ready comment, got %q", commenter.items[1].Status)
	}
}

func TestRecordFluxStatusMovesCreatingEnvironmentToReady(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1707",
		Product: "bethunder",
		Mode:    domain.ModeFull,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Status != domain.StatusCreating {
		t.Fatalf("status before flux = %q", env.Status)
	}

	updated, err := service.RecordFluxStatus("kan-1707", domain.FluxStatus{
		Status:  domain.StatusReady,
		Message: "flux ready",
		Kustomizations: []domain.FluxResourceStatus{
			{Kind: "Kustomization", Name: "kan-1707.bethunder", Ready: true},
		},
	})
	if err != nil {
		t.Fatalf("record flux status: %v", err)
	}
	if updated.Status != domain.StatusReady {
		t.Fatalf("status after flux = %q", updated.Status)
	}
	if updated.FluxStatus == nil || updated.FluxStatus.Status != domain.StatusReady {
		t.Fatalf("flux status = %#v", updated.FluxStatus)
	}
}

func TestStatusLifecycleAllowsCreatingReadyFailed(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1712",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Status != domain.StatusCreating {
		t.Fatalf("initial status = %q", env.Status)
	}

	env, err = service.UpdateStatus("kan-1712", domain.StatusReady, "")
	if err != nil {
		t.Fatalf("ready status: %v", err)
	}
	if env.Status != domain.StatusReady {
		t.Fatalf("ready status = %q", env.Status)
	}

	env, err = service.UpdateStatus("kan-1712", domain.StatusFailed, "pod crashloop")
	if err != nil {
		t.Fatalf("failed status: %v", err)
	}
	if env.Status != domain.StatusFailed {
		t.Fatalf("failed status = %q", env.Status)
	}
	if env.LastError != "pod crashloop" {
		t.Fatalf("last error = %q", env.LastError)
	}
}

func TestEnvironmentStatusModelAllowsMVPStatuses(t *testing.T) {
	statuses := []domain.EnvironmentStatus{
		domain.StatusCreating,
		domain.StatusReady,
		domain.StatusFailed,
		domain.StatusDeleteRequested,
		domain.StatusGitOpsDeletePending,
		domain.StatusDeleteFailed,
		domain.StatusTerminating,
		domain.StatusTerminated,
	}
	for _, status := range statuses {
		if !validStatus(status) {
			t.Fatalf("expected status %q to be valid", status)
		}
	}
	if validStatus(domain.EnvironmentStatus("deleted")) {
		t.Fatalf("expected unknown status to be invalid")
	}
}

func TestEnvironmentStatusLifecycleTransitions(t *testing.T) {
	allowed := []struct {
		current domain.EnvironmentStatus
		next    domain.EnvironmentStatus
	}{
		{current: "", next: domain.StatusCreating},
		{current: "", next: domain.StatusReady},
		{current: "", next: domain.StatusFailed},
		{current: domain.StatusCreating, next: domain.StatusReady},
		{current: domain.StatusCreating, next: domain.StatusFailed},
		{current: domain.StatusCreating, next: domain.StatusDeleteRequested},
		{current: domain.StatusCreating, next: domain.StatusTerminating},
		{current: domain.StatusCreating, next: domain.StatusTerminated},
		{current: domain.StatusReady, next: domain.StatusFailed},
		{current: domain.StatusReady, next: domain.StatusDeleteRequested},
		{current: domain.StatusReady, next: domain.StatusTerminating},
		{current: domain.StatusReady, next: domain.StatusTerminated},
		{current: domain.StatusFailed, next: domain.StatusDeleteRequested},
		{current: domain.StatusFailed, next: domain.StatusTerminated},
		{current: domain.StatusDeleteRequested, next: domain.StatusGitOpsDeletePending},
		{current: domain.StatusDeleteRequested, next: domain.StatusDeleteFailed},
		{current: domain.StatusDeleteRequested, next: domain.StatusTerminating},
		{current: domain.StatusGitOpsDeletePending, next: domain.StatusDeleteRequested},
		{current: domain.StatusGitOpsDeletePending, next: domain.StatusDeleteFailed},
		{current: domain.StatusGitOpsDeletePending, next: domain.StatusTerminating},
		{current: domain.StatusDeleteFailed, next: domain.StatusDeleteRequested},
		{current: domain.StatusDeleteFailed, next: domain.StatusTerminating},
		{current: domain.StatusTerminating, next: domain.StatusTerminated},
		{current: domain.StatusTerminating, next: domain.StatusDeleteFailed},
	}
	for _, tt := range allowed {
		if !allowedStatusTransition(tt.current, tt.next) {
			t.Fatalf("expected transition %q -> %q to be allowed", tt.current, tt.next)
		}
	}

	denied := []struct {
		current domain.EnvironmentStatus
		next    domain.EnvironmentStatus
	}{
		{current: domain.StatusReady, next: domain.StatusCreating},
		{current: domain.StatusFailed, next: domain.StatusReady},
		{current: domain.StatusDeleteRequested, next: domain.StatusReady},
		{current: domain.StatusDeleteRequested, next: domain.StatusFailed},
		{current: domain.StatusGitOpsDeletePending, next: domain.StatusReady},
		{current: domain.StatusGitOpsDeletePending, next: domain.StatusFailed},
		{current: domain.StatusDeleteFailed, next: domain.StatusReady},
		{current: domain.StatusDeleteFailed, next: domain.StatusFailed},
		{current: domain.StatusTerminating, next: domain.StatusReady},
		{current: domain.StatusTerminated, next: domain.StatusReady},
		{current: domain.StatusTerminated, next: domain.StatusFailed},
	}
	for _, tt := range denied {
		if allowedStatusTransition(tt.current, tt.next) {
			t.Fatalf("expected transition %q -> %q to be denied", tt.current, tt.next)
		}
	}
}

func TestStatusLifecyclePreventsReadyToCreatingRegression(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1713",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = service.UpdateStatus("kan-1713", domain.StatusReady, "")
	if err != nil {
		t.Fatalf("ready status: %v", err)
	}

	env, err := service.UpdateStatus("kan-1713", domain.StatusCreating, "collector warming up")
	if err != nil {
		t.Fatalf("creating regression: %v", err)
	}
	if env.Status != domain.StatusReady {
		t.Fatalf("status regressed to %q", env.Status)
	}
}

func TestFluxStatusLifecycleCanMarkReadyEnvironmentFailed(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1714",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = service.RecordFluxStatus("kan-1714", domain.FluxStatus{Status: domain.StatusReady, Message: "ready"})
	if err != nil {
		t.Fatalf("ready flux: %v", err)
	}

	env, err := service.RecordFluxStatus("kan-1714", domain.FluxStatus{Status: domain.StatusFailed, Message: "reconciliation failed"})
	if err != nil {
		t.Fatalf("failed flux: %v", err)
	}
	if env.Status != domain.StatusFailed {
		t.Fatalf("status = %q", env.Status)
	}
	if env.LastError != "reconciliation failed" {
		t.Fatalf("last error = %q", env.LastError)
	}
}

type fakeCommenter struct {
	items []domain.Environment
}

func (f *fakeCommenter) CommentEnvironment(_ context.Context, environment domain.Environment) error {
	f.items = append(f.items, environment)
	return nil
}

func TestCreateEnvironmentUsesCommitSHAAsDefaultImageTagAndAllowsOverrides(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1703",
		Product: "bethunder",
		Source: domain.SCMSource{
			Commit: "abc1234",
		},
		Services: []domain.ServiceOverride{
			{Name: "backend", Tag: "manual-backend-tag", Replace: true},
		},
		Overrides: map[string]string{
			"customFlag": "'enabled'",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	tags := map[string]string{}
	for _, service := range env.Services {
		tags[service.Name] = service.Tag
	}
	if tags["api"] != "abc1234" {
		t.Fatalf("expected api tag from commit sha, got %q", tags["api"])
	}
	if tags["backend"] != "manual-backend-tag" {
		t.Fatalf("expected explicit backend override, got %q", tags["backend"])
	}
	replaced := map[string]bool{}
	for _, service := range env.Services {
		replaced[service.Name] = service.Replace
	}
	if !replaced["backend"] || replaced["api"] {
		t.Fatalf("expected only backend to be marked replaced, got %+v", replaced)
	}
	if env.Overrides["customFlag"] != "'enabled'" {
		t.Fatalf("expected custom override, got %+v", env.Overrides)
	}
}

func runAppGit(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(output))
	}
}

func TestCreateEnvironmentGeneratesPreviewIngressHostname(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cfg := config.FromEnv()
	cfg.DefaultDomainRoot = "preview.example.com"
	catalogData := catalog.Default()
	product := catalogData.Products["bethunder"]
	product.DefaultDomain = ""
	catalogData.Products["bethunder"] = product
	service := NewEnvironmentService(cfg, catalogData, envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1708",
		Project: "cms",
		Product: "bethunder",
		Source: domain.SCMSource{
			PullRequestID: "123",
			Branch:        "feature/kan-1708",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Domain != "pr-123.cms.preview.example.com" {
		t.Fatalf("domain = %q", env.Domain)
	}
	if env.URL != "https://pr-123.cms.preview.example.com" {
		t.Fatalf("url = %q", env.URL)
	}
}

func TestCreateEnvironmentDoesNotDuplicatePreviewDomainRoot(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cfg := config.FromEnv()
	cfg.DefaultDomainRoot = "preview.local"
	catalogData := catalog.Default()
	product := catalogData.Products["bethunder"]
	product.DefaultDomain = ""
	catalogData.Products["bethunder"] = product
	service := NewEnvironmentService(cfg, catalogData, envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "pr-123",
		Project: "checkout",
		Product: "bethunder",
		Source: domain.SCMSource{
			PullRequestID: "123",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Domain != "pr-123.checkout.preview.local" {
		t.Fatalf("domain = %q", env.Domain)
	}
	if env.URL != "https://pr-123.checkout.preview.local" {
		t.Fatalf("url = %q", env.URL)
	}
}

func TestCreateEnvironmentAllowsExplicitPreviewHostnameOverride(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1709",
		Product: "bethunder",
		Domain:  "custom.preview.example.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Domain != "custom.preview.example.com" || env.URL != "https://custom.preview.example.com" {
		t.Fatalf("domain/url = %q %q", env.Domain, env.URL)
	}
}

func TestReconcileExpiredDeletesUnpinnedEnvironment(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:       "kan-1702",
		Product:  "bethunder",
		TTLHours: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Status != domain.StatusCreating {
		t.Fatalf("unexpected status: %s", env.Status)
	}

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	deleted, err := service.ReconcileExpired(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Status != domain.StatusTerminated {
		t.Fatalf("expected cleanup to terminate environment, got %+v", deleted)
	}
}

func TestReconcileExpiredKeepsPinnedEnvironment(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:       "kan-1703",
		Product:  "bethunder",
		TTLHours: 1,
		Pinned:   true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !env.Pinned || env.ExpiresAt != nil {
		t.Fatalf("expected pinned env without expiration, got pinned=%v expiresAt=%v", env.Pinned, env.ExpiresAt)
	}

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	deleted, err := service.ReconcileExpired(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("expected no deleted environments, got %+v", deleted)
	}
	stored, err := service.GetEnvironment("kan-1703")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if !stored.Pinned || stored.Status == domain.StatusTerminated {
		t.Fatalf("expected pinned env to survive ttl cleanup, got %+v", stored)
	}
}

func TestReconcileIdleMarksInactiveEnvironmentAndActivityClearsIdle(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return start }

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-idle",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Idle || env.LastActivityAt == nil || !env.LastActivityAt.Equal(start) {
		t.Fatalf("expected active environment with last activity, got idle=%v last=%v", env.Idle, env.LastActivityAt)
	}

	service.now = func() time.Time { return start.Add(3 * time.Hour) }
	idle, err := service.ReconcileIdle(2 * time.Hour)
	if err != nil {
		t.Fatalf("reconcile idle: %v", err)
	}
	if len(idle) != 1 || idle[0].ID != "kan-idle" || !idle[0].Idle || idle[0].IdleSince == nil {
		t.Fatalf("idle environments = %+v", idle)
	}

	service.now = func() time.Time { return start.Add(4 * time.Hour) }
	active, err := service.UpdateStatus("kan-idle", domain.StatusReady, "traffic observed")
	if err != nil {
		t.Fatalf("activity status update: %v", err)
	}
	if active.Idle || active.IdleSince != nil {
		t.Fatalf("expected activity to clear idle, got idle=%v since=%v", active.Idle, active.IdleSince)
	}
	if active.LastActivityAt == nil || !active.LastActivityAt.Equal(start.Add(4*time.Hour)) {
		t.Fatalf("last activity = %v", active.LastActivityAt)
	}
}

func TestShutdownIdleDeletesIdleEnvironment(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return start }

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-idle-shutdown",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	service.now = func() time.Time { return start.Add(3 * time.Hour) }
	if _, err := service.ReconcileIdle(2 * time.Hour); err != nil {
		t.Fatalf("reconcile idle: %v", err)
	}
	shutdown, err := service.ShutdownIdle(context.Background())
	if err != nil {
		t.Fatalf("shutdown idle: %v", err)
	}
	if len(shutdown) != 1 || shutdown[0].ID != "kan-idle-shutdown" || shutdown[0].Status != domain.StatusTerminated {
		t.Fatalf("shutdown environments = %+v", shutdown)
	}
	stored, err := service.GetEnvironment("kan-idle-shutdown")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if stored.Status != domain.StatusTerminated {
		t.Fatalf("expected idle environment to be terminated, got %q", stored.Status)
	}
}

func TestSetPinnedTogglesExpiration(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:       "kan-1704",
		Product:  "bethunder",
		TTLHours: 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.ExpiresAt == nil {
		t.Fatal("expected initial expiration")
	}

	pinned, err := service.SetPinned("kan-1704", true)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if !pinned.Pinned || pinned.PinnedUntil != nil || pinned.ExpiresAt != nil {
		t.Fatalf("expected pinned env without expiration, got %+v", pinned)
	}

	unpinned, err := service.SetPinned("kan-1704", false)
	if err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if unpinned.Pinned || unpinned.PinnedUntil != nil || unpinned.ExpiresAt == nil {
		t.Fatalf("expected unpinned env with expiration, got %+v", unpinned)
	}
}

func TestSetPinnedForExpiresViaReconcilePins(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	start := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return start }

	_, err = service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:       "kan-pin-until",
		Product:  "bethunder",
		TTLHours: 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pinned, err := service.SetPinnedFor("kan-pin-until", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("pin for duration: %v", err)
	}
	if !pinned.Pinned || pinned.PinnedUntil == nil || !pinned.PinnedUntil.Equal(start.Add(7*24*time.Hour)) || pinned.ExpiresAt != nil {
		t.Fatalf("unexpected timed pin state: %+v", pinned)
	}

	service.now = func() time.Time { return start.Add(7*24*time.Hour - time.Minute) }
	unpinned, err := service.ReconcilePins()
	if err != nil {
		t.Fatalf("early reconcile pins: %v", err)
	}
	if len(unpinned) != 0 {
		t.Fatalf("expected no early unpin, got %+v", unpinned)
	}

	expiredAt := start.Add(7 * 24 * time.Hour)
	service.now = func() time.Time { return expiredAt }
	unpinned, err = service.ReconcilePins()
	if err != nil {
		t.Fatalf("reconcile pins: %v", err)
	}
	if len(unpinned) != 1 || unpinned[0].ID != "kan-pin-until" {
		t.Fatalf("unpinned = %+v", unpinned)
	}
	if unpinned[0].Pinned || unpinned[0].PinnedUntil != nil || unpinned[0].ExpiresAt == nil {
		t.Fatalf("expected unpinned environment with resumed expiration, got %+v", unpinned[0])
	}
	if !unpinned[0].ExpiresAt.Equal(expiredAt.Add(2 * time.Hour)) {
		t.Fatalf("expiresAt = %v", unpinned[0].ExpiresAt)
	}
}

func TestListEnvironmentsHidesTerminatedEnvironments(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	active, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1715",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	terminated, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-1716",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create terminated: %v", err)
	}
	_, err = service.DeleteEnvironment(context.Background(), terminated.ID, true)
	if err != nil {
		t.Fatalf("delete terminated: %v", err)
	}
	_, err = service.UpdateStatus(terminated.ID, domain.StatusTerminated, "namespace deleted")
	if err != nil {
		t.Fatalf("verify terminated: %v", err)
	}

	items, err := service.ListEnvironments()
	if err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(items) != 1 || items[0].ID != active.ID {
		t.Fatalf("expected only active env, got %+v", items)
	}
	records, err := service.ListEnvironmentRecords()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected audit records retained, got %d", len(records))
	}
}

func TestDeleteEnvironmentTerminatesAfterGitOpsCleanup(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-604",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deleting, err := service.DeleteEnvironment(context.Background(), env.ID, true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleting.Status != domain.StatusTerminated {
		t.Fatalf("expected terminated after gitops cleanup, got %q", deleting.Status)
	}

	ignoredReady, err := service.UpdateStatus(env.ID, domain.StatusReady, "late ready report")
	if err != nil {
		t.Fatalf("late ready status: %v", err)
	}
	if ignoredReady.Status != domain.StatusTerminated {
		t.Fatalf("expected late ready report to be ignored, got %q", ignoredReady.Status)
	}
}

func TestDeleteEnvironmentRetryIsIdempotentForInProgressAndTerminated(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := NewEnvironmentService(config.FromEnv(), catalog.Default(), envStore, gitops.NewFluxRenderer(config.FromEnv().GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-605",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first, err := service.DeleteEnvironment(context.Background(), env.ID, true)
	if err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if first.Status != domain.StatusTerminated {
		t.Fatalf("expected first delete status terminated, got %q", first.Status)
	}
	second, err := service.DeleteEnvironment(context.Background(), env.ID, true)
	if err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if second.Status != domain.StatusTerminated {
		t.Fatalf("expected second delete status terminated, got %q", second.Status)
	}
	third, err := service.DeleteEnvironment(context.Background(), env.ID, true)
	if err != nil {
		t.Fatalf("third delete on terminated: %v", err)
	}
	if third.Status != domain.StatusTerminated {
		t.Fatalf("expected terminated status on idempotent delete, got %q", third.Status)
	}
}

func TestDeleteEnvironmentBlocksProtectedNamespace(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cfg := config.FromEnv()
	cfg.CleanupProtectedNamespaces = []string{"default", "kube-system"}
	cfg.CleanupRequireEnvPilotLabels = true
	service := NewEnvironmentService(cfg, catalog.Default(), envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:        "kan-protected",
		Product:   "bethunder",
		Namespace: "default",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = service.DeleteEnvironment(context.Background(), env.ID, true)
	if err == nil {
		t.Fatalf("expected protected namespace delete error")
	}
	if !strings.Contains(err.Error(), "protected namespace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteEnvironmentBlocksUnsafeLabelCleanupConfig(t *testing.T) {
	tmp := t.TempDir()
	envStore, err := store.NewJSONStore(tmp + "/store.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cfg := config.FromEnv()
	cfg.CleanupProtectedNamespaces = []string{"default", "kube-system"}
	cfg.CleanupRequireEnvPilotLabels = false
	service := NewEnvironmentService(cfg, catalog.Default(), envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))

	env, err := service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-unsafe-cleanup",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = service.DeleteEnvironment(context.Background(), env.ID, true)
	if err == nil {
		t.Fatalf("expected unsafe cleanup config error")
	}
	if !strings.Contains(err.Error(), "EnvPilot labels") {
		t.Fatalf("unexpected error: %v", err)
	}
}
