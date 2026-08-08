package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/envpilot/contracts/domain"
	"github.com/envpilot/runner/internal/bootstrap"
	"github.com/envpilot/runner/internal/catalog"
	"github.com/envpilot/runner/internal/config"
	"github.com/envpilot/runner/internal/gitops"
	"github.com/envpilot/runner/internal/orchestrator"
	scmcomment "github.com/envpilot/runner/internal/scm/comment"
	"github.com/envpilot/runner/internal/secrets"
	"github.com/envpilot/runner/internal/store"
)

type EnvironmentService struct {
	cfg            config.Config
	catalog        catalog.Catalog
	store          store.EnvironmentStore
	projects       store.ProjectStore
	projectConfigs store.ProjectConfigStore
	products       productProvider
	settings       settingsProvider
	renderer       gitops.Renderer
	writer         gitops.Writer
	orch           *orchestrator.EnvironmentOrchestrator
	comment        scmcomment.Commenter
	notifier       environmentNotifier
	now            func() time.Time
}

type productProvider interface {
	ListProducts() ([]domain.ProductTemplate, error)
	GetProduct(name string) (domain.ProductTemplate, error)
}

type settingsProvider interface {
	GetSettings() (domain.ControlPlaneSettings, error)
}

type environmentNotifier interface {
	NotifyEnvironment(ctx context.Context, environment domain.Environment) error
}

func NewEnvironmentService(cfg config.Config, catalog catalog.Catalog, envStore store.EnvironmentStore, renderer gitops.Renderer, writer gitops.Writer) *EnvironmentService {
	defaultBackend := orchestrator.NormalizeDeploymentBackendType(cfg.DeploymentBackend)
	return &EnvironmentService{
		cfg:      cfg,
		catalog:  catalog,
		store:    envStore,
		renderer: renderer,
		writer:   writer,
		orch: orchestrator.NewWithBackendResolver(envStore, func(projectConfig domain.ProjectConfig) (orchestrator.DeploymentBackend, error) {
			backendType := defaultBackend
			if projectBackend := deploymentBackendFromProjectConfig(projectConfig); projectBackend != "" {
				backendType = orchestrator.NormalizeDeploymentBackendType(projectBackend)
			}
			return orchestrator.ResolveDeploymentBackend(backendType, renderer)
		}, writer),
		comment: noopCommenter{},
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func (s *EnvironmentService) SetCommenter(commenter scmcomment.Commenter) {
	if commenter == nil {
		s.comment = noopCommenter{}
		return
	}
	s.comment = commenter
}

func (s *EnvironmentService) SetProjectStore(projectStore store.ProjectStore) {
	s.projects = projectStore
}

func (s *EnvironmentService) SetProjectConfigStore(projectConfigStore store.ProjectConfigStore) {
	s.projectConfigs = projectConfigStore
}

func (s *EnvironmentService) SetProductProvider(provider productProvider) {
	s.products = provider
}

func (s *EnvironmentService) SetSettingsProvider(provider settingsProvider) {
	s.settings = provider
}

func (s *EnvironmentService) SetNotifier(notifier environmentNotifier) {
	s.notifier = notifier
}

func (s *EnvironmentService) ListEnvironments() ([]domain.Environment, error) {
	items, err := s.store.List()
	if err != nil {
		return nil, err
	}
	active := make([]domain.Environment, 0, len(items))
	for _, item := range items {
		if item.Status == domain.StatusTerminated {
			continue
		}
		active = append(active, item)
	}
	return active, nil
}

func (s *EnvironmentService) ListEnvironmentRecords() ([]domain.EnvironmentRecord, error) {
	return s.store.ListRecords()
}

func (s *EnvironmentService) GetEnvironment(id string) (domain.Environment, error) {
	return s.store.Get(id)
}

func (s *EnvironmentService) GetEnvironmentRecord(id string) (domain.EnvironmentRecord, error) {
	return s.store.GetRecord(id)
}

func (s *EnvironmentService) ListProducts() ([]domain.ProductTemplate, error) {
	if s.products != nil {
		products, err := s.products.ListProducts()
		if err == nil {
			return products, nil
		}
	}
	return s.catalog.List(), nil
}

func (s *EnvironmentService) PreviewEnvironment(req domain.CreateEnvironmentRequest) (domain.RenderPreview, error) {
	env, err := s.prepareEnvironment(req)
	if err != nil {
		return domain.RenderPreview{}, err
	}
	manifests, err := s.renderer.RenderManifestSet(env)
	if err != nil {
		return domain.RenderPreview{}, err
	}
	values := map[string]string{}
	if previewer, ok := s.renderer.(interface {
		RenderValuesPreview(domain.Environment) map[string]string
	}); ok {
		values = previewer.RenderValuesPreview(env)
	}
	outputs := make([]domain.RenderOutput, 0, len(manifests))
	for _, manifest := range manifests {
		outputs = append(outputs, domain.RenderOutput{Path: manifest.Path, Kind: manifest.Kind})
	}
	return domain.RenderPreview{
		Environment: env,
		Values:      values,
		ValuesYAML:  gitops.ValuesYAML(values),
		Manifests:   outputs,
	}, nil
}

func (s *EnvironmentService) ReserveEnvironment(_ context.Context, req domain.CreateEnvironmentRequest) (domain.Environment, error) {
	env, err := s.prepareEnvironment(req)
	if err != nil {
		return domain.Environment{}, err
	}
	existing, err := s.store.Get(env.ID)
	if err == nil {
		if existing.Status != domain.StatusTerminated {
			return existing, nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Environment{}, err
	}
	if err := s.checkActiveEnvironmentLimit(env.Project, env.ID); err != nil {
		return domain.Environment{}, err
	}
	if err := s.store.Save(env); err != nil {
		return domain.Environment{}, err
	}
	return env, nil
}

func (s *EnvironmentService) CreateEnvironment(ctx context.Context, req domain.CreateEnvironmentRequest) (domain.Environment, error) {
	env, err := s.prepareEnvironment(req)
	if err != nil {
		return domain.Environment{}, err
	}
	existingID := ""
	if existing, err := s.store.Get(env.ID); err == nil {
		if existing.Status != domain.StatusTerminated {
			if !isReservedForCreate(existing) {
				return domain.Environment{}, ConflictError{Message: fmt.Sprintf("environment %q already exists", env.ID)}
			}
			existingID = existing.ID
			env.CreatedAt = existing.CreatedAt
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Environment{}, err
	}
	if existingID == "" {
		if err := s.checkActiveEnvironmentLimit(env.Project, ""); err != nil {
			return domain.Environment{}, err
		}
	}

	writer, err := s.gitOpsWriterForEnvironment(ctx, env)
	if err != nil {
		return domain.Environment{}, err
	}
	projectConfig, err := s.projectConfigForProject(env.Project)
	if err != nil {
		return domain.Environment{}, err
	}
	created, err := s.orch.CreateWithWriterAndProjectConfig(ctx, env, writer, projectConfig)
	if err == nil {
		s.commentEnvironment(ctx, created)
		s.notifyEnvironment(ctx, created)
	}
	return created, err
}

func (s *EnvironmentService) DeleteEnvironment(ctx context.Context, id string, force bool) (domain.Environment, error) {
	env, err := s.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	if env.Status == domain.StatusTerminated {
		return env, nil
	}
	if env.Pinned && !force {
		return domain.Environment{}, ConflictError{Message: fmt.Sprintf("environment %q is pinned", id)}
	}
	if err := s.validateCleanupSafety(env); err != nil {
		return domain.Environment{}, err
	}

	writer, err := s.gitOpsWriterForEnvironment(ctx, env)
	if err != nil {
		return domain.Environment{}, err
	}
	projectConfig, err := s.projectConfigForProject(env.Project)
	if err != nil {
		return domain.Environment{}, err
	}
	return s.orch.DeleteWithWriterAndProjectConfig(ctx, id, writer, projectConfig)
}

func (s *EnvironmentService) projectConfigForProject(projectID string) (domain.ProjectConfig, error) {
	if s.projectConfigs == nil {
		return domain.ProjectConfig{}, nil
	}
	rawConfig, err := s.projectConfigs.Latest(projectID)
	if errors.Is(err, store.ErrProjectConfigNotFound) {
		return domain.ProjectConfig{}, nil
	}
	if err != nil {
		return domain.ProjectConfig{}, err
	}
	return rawConfig, nil
}

func deploymentBackendFromProjectConfig(projectConfig domain.ProjectConfig) string {
	if len(projectConfig.Config) == 0 {
		return ""
	}
	rawDeployment, ok := projectConfig.Config["deployment"]
	if !ok {
		backend := domain.InferDeploymentBackend("", map[string]any{}, projectConfig.Config)
		if backend == domain.DeploymentBackendHelmDirect {
			return ""
		}
		return string(backend)
	}
	rawDeploymentMap, ok := rawDeployment.(map[string]any)
	if !ok {
		return ""
	}
	if len(rawDeploymentMap) == 0 {
		return ""
	}
	return string(domain.InferDeploymentBackend(rawDeploymentMap["backend"], rawDeploymentMap, projectConfig.Config))
}

func (s *EnvironmentService) validateCleanupSafety(env domain.Environment) error {
	config := bootstrap.DefaultCleanupSafetyConfig()
	if len(s.cfg.CleanupProtectedNamespaces) > 0 {
		config.ProtectedNamespaces = s.cfg.CleanupProtectedNamespaces
		config.DeleteEnvPilotLabeledOnly = s.cfg.CleanupRequireEnvPilotLabels
	}
	if err := bootstrap.ValidateCleanupSafetyConfig(config, []string{env.Namespace}); err != nil {
		return ValidationError{Message: fmt.Sprintf("cleanup safety validation failed: %v", err)}
	}
	return nil
}

func (s *EnvironmentService) SetPinned(id string, pinned bool) (domain.Environment, error) {
	env, err := s.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	env.Pinned = pinned
	env.PinnedUntil = nil
	env = s.markActive(env)
	if pinned {
		env.ExpiresAt = nil
	} else if env.TTLHours > 0 {
		expiresAt := env.UpdatedAt.Add(time.Duration(env.TTLHours) * time.Hour)
		env.ExpiresAt = &expiresAt
	}
	if err := s.store.Save(env); err != nil {
		return domain.Environment{}, err
	}
	return env, nil
}

func (s *EnvironmentService) SetPinnedFor(id string, duration time.Duration) (domain.Environment, error) {
	if duration <= 0 {
		return domain.Environment{}, ValidationError{Message: "pin duration must be positive"}
	}
	env, err := s.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	env.Pinned = true
	pinnedUntil := s.now().Add(duration)
	env.PinnedUntil = &pinnedUntil
	env = s.markActive(env)
	env.ExpiresAt = nil
	if err := s.store.Save(env); err != nil {
		return domain.Environment{}, err
	}
	return env, nil
}

func (s *EnvironmentService) ExtendTTL(id string) (domain.Environment, error) {
	env, err := s.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	if env.Pinned {
		return domain.Environment{}, ValidationError{Message: fmt.Sprintf("environment %q is pinned", id)}
	}
	ttlHours := env.TTLHours
	if ttlHours <= 0 {
		ttlHours = 48
	}
	duration := time.Duration(ttlHours) * time.Hour
	now := s.now()
	expiresAt := now.Add(duration)
	if env.ExpiresAt != nil && env.ExpiresAt.After(now) {
		expiresAt = env.ExpiresAt.Add(duration)
	}
	env.ExpiresAt = &expiresAt
	env = s.markActive(env)
	if err := s.store.Save(env); err != nil {
		return domain.Environment{}, err
	}
	return env, nil
}

func (s *EnvironmentService) UpdateStatus(id string, status domain.EnvironmentStatus, message string) (domain.Environment, error) {
	return s.UpdateStatusFromCluster(id, status, message, "")
}

func (s *EnvironmentService) UpdateStatusFromCluster(id string, status domain.EnvironmentStatus, message string, clusterID string) (domain.Environment, error) {
	if !validStatus(status) {
		return domain.Environment{}, ValidationError{Message: fmt.Sprintf("unsupported status %q", status)}
	}
	env, err := s.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	if err := ensureClusterRoute(env, clusterID); err != nil {
		return domain.Environment{}, err
	}
	updated := s.applyStatusLifecycle(env, status, message)
	updated = s.markActive(updated)
	if err := s.store.Save(updated); err != nil {
		return domain.Environment{}, err
	}
	s.commentEnvironment(context.Background(), updated)
	s.notifyEnvironment(context.Background(), updated)
	return updated, nil
}

func (s *EnvironmentService) RecordEvents(id string, events []domain.KubernetesEvent) (domain.Environment, error) {
	return s.RecordEventsFromCluster(id, events, "")
}

func (s *EnvironmentService) RecordEventsFromCluster(id string, events []domain.KubernetesEvent, clusterID string) (domain.Environment, error) {
	env, err := s.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	if err := ensureClusterRoute(env, clusterID); err != nil {
		return domain.Environment{}, err
	}
	env.Events = normalizeEvents(events)
	env = s.markActive(env)
	if err := s.store.Save(env); err != nil {
		return domain.Environment{}, err
	}
	return env, nil
}

func (s *EnvironmentService) ListEvents(id string) ([]domain.KubernetesEvent, error) {
	env, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	return env.Events, nil
}

func (s *EnvironmentService) RecordFluxStatus(id string, status domain.FluxStatus) (domain.Environment, error) {
	return s.RecordFluxStatusFromCluster(id, status, "")
}

func (s *EnvironmentService) RecordFluxStatusFromCluster(id string, status domain.FluxStatus, clusterID string) (domain.Environment, error) {
	env, err := s.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	if err := ensureClusterRoute(env, clusterID); err != nil {
		return domain.Environment{}, err
	}
	if status.UpdatedAt.IsZero() {
		status.UpdatedAt = s.now()
	}
	env.FluxStatus = &status
	env = s.applyStatusLifecycle(env, status.Status, status.Message)
	env = s.markActive(env)
	if err := s.store.Save(env); err != nil {
		return domain.Environment{}, err
	}
	s.commentEnvironment(context.Background(), env)
	s.notifyEnvironment(context.Background(), env)
	return env, nil
}

func (s *EnvironmentService) GetFluxStatus(id string) (*domain.FluxStatus, error) {
	env, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	return env.FluxStatus, nil
}

func (s *EnvironmentService) ReconcileExpired(ctx context.Context) ([]domain.Environment, error) {
	items, err := s.store.List()
	if err != nil {
		return nil, err
	}
	now := s.now()
	deleted := make([]domain.Environment, 0)
	for _, env := range items {
		if env.Status == domain.StatusTerminated || env.Status == domain.StatusTerminating {
			continue
		}
		if env.Pinned {
			if _, expired, err := s.expirePinnedUntil(env, now); err != nil {
				return deleted, err
			} else if !expired {
				continue
			}
			continue
		}
		if env.ExpiresAt == nil {
			continue
		}
		if now.Before(*env.ExpiresAt) {
			continue
		}
		deletedEnv, err := s.DeleteEnvironment(ctx, env.ID, true)
		if err != nil {
			return deleted, err
		}
		deleted = append(deleted, deletedEnv)
	}
	return deleted, nil
}

func (s *EnvironmentService) ReconcilePins() ([]domain.Environment, error) {
	items, err := s.store.List()
	if err != nil {
		return nil, err
	}
	now := s.now()
	unpinned := make([]domain.Environment, 0)
	for _, env := range items {
		updated, expired, err := s.expirePinnedUntil(env, now)
		if err != nil {
			return unpinned, err
		}
		if !expired {
			continue
		}
		unpinned = append(unpinned, updated)
	}
	return unpinned, nil
}

func (s *EnvironmentService) ReconcileIdle(threshold time.Duration) ([]domain.Environment, error) {
	if threshold <= 0 {
		return nil, nil
	}
	items, err := s.store.List()
	if err != nil {
		return nil, err
	}
	now := s.now()
	idle := make([]domain.Environment, 0)
	for _, env := range items {
		if env.Idle || env.Status == domain.StatusTerminated || env.Status == domain.StatusTerminating {
			continue
		}
		lastActivity := env.UpdatedAt
		if env.LastActivityAt != nil && !env.LastActivityAt.IsZero() {
			lastActivity = *env.LastActivityAt
		} else if lastActivity.IsZero() {
			lastActivity = env.CreatedAt
		}
		if lastActivity.IsZero() || now.Sub(lastActivity) < threshold {
			continue
		}
		env.Idle = true
		idleSince := now
		env.IdleSince = &idleSince
		env.UpdatedAt = now
		if err := s.store.Save(env); err != nil {
			return idle, err
		}
		idle = append(idle, env)
	}
	return idle, nil
}

func (s *EnvironmentService) ShutdownIdle(ctx context.Context) ([]domain.Environment, error) {
	items, err := s.store.List()
	if err != nil {
		return nil, err
	}
	shutdown := make([]domain.Environment, 0)
	for _, env := range items {
		if !env.Idle || env.Pinned || env.Status == domain.StatusTerminated || env.Status == domain.StatusTerminating {
			continue
		}
		deletedEnv, err := s.DeleteEnvironment(ctx, env.ID, true)
		if err != nil {
			return shutdown, err
		}
		shutdown = append(shutdown, deletedEnv)
	}
	return shutdown, nil
}

func (s *EnvironmentService) expirePinnedUntil(env domain.Environment, now time.Time) (domain.Environment, bool, error) {
	if !env.Pinned || env.PinnedUntil == nil || now.Before(*env.PinnedUntil) {
		return env, false, nil
	}
	env.Pinned = false
	env.PinnedUntil = nil
	env.UpdatedAt = now
	if env.TTLHours > 0 {
		expiresAt := now.Add(time.Duration(env.TTLHours) * time.Hour)
		env.ExpiresAt = &expiresAt
	}
	if err := s.store.Save(env); err != nil {
		return env, false, err
	}
	return env, true, nil
}

func (s *EnvironmentService) prepareEnvironment(req domain.CreateEnvironmentRequest) (domain.Environment, error) {
	now := s.now()
	runtime := s.runtimeSettings()
	id := normalizeID(req.ID)
	if id == "" {
		id = environmentIDFromSource(req.Source)
	}
	if id == "" {
		return domain.Environment{}, ValidationError{Message: "id, source.pullRequestId, or source.branch is required"}
	}
	if !validID.MatchString(id) {
		return domain.Environment{}, ValidationError{Message: "id must use lowercase letters, numbers, and dashes"}
	}

	productName := strings.ToLower(strings.TrimSpace(req.Product))
	if productName == "" {
		productName = strings.ToLower(strings.TrimSpace(runtime.DefaultProduct))
	}
	if productName == "" {
		productName = "generic"
	}
	product, ok := s.productTemplate(productName)
	if !ok {
		return domain.Environment{}, ValidationError{Message: fmt.Sprintf("unknown product %q", productName)}
	}

	project := strings.TrimSpace(req.Project)
	if project == "" {
		project = product.Project
	}
	if project == "" {
		project = runtime.DefaultProject
	}
	if project == "" {
		project = "default"
	}
	clusterID := normalizeID(req.ClusterID)
	if clusterID == "" {
		clusterID = s.clusterIDForProject(project)
	}

	mode := req.Mode
	if mode == "" {
		mode = product.DefaultMode
	}
	if mode == "" {
		mode = runtime.DefaultMode
	}
	if mode == "" {
		mode = domain.ModeFull
	}
	if mode != domain.ModeFull && mode != domain.ModeHybrid {
		return domain.Environment{}, ValidationError{Message: fmt.Sprintf("unsupported environment mode %q", mode)}
	}
	base, err := s.baseEnvironmentFor(project, req.Base)
	if err != nil {
		return domain.Environment{}, err
	}

	namespace := normalizeID(req.Namespace)
	if namespace == "" {
		namespace = namespaceName(runtime.NamespacePrefix, namespaceIDFromSource(req.Source, id))
	}

	domainRoot := strings.TrimSpace(product.DefaultDomain)
	if domainRoot == "" {
		domainRoot = runtime.DomainRoot
	}
	domainName := strings.TrimSpace(req.Domain)
	if domainName == "" {
		domainName = previewHostname(id, project, req.Source, domainRoot)
	}

	ttlHours := req.TTLHours
	if ttlHours <= 0 {
		ttlHours = runtime.DefaultTTLHours
	}
	if ttlHours <= 0 {
		ttlHours = 48
	}

	charts := mergeCharts(product.DefaultCharts, req.Charts)
	infra := mergeInfrastructure(product.Infrastructure, req.Infrastructure)
	services := mergeServices(product.Services, req.Services, req.Source.Commit)
	if req.Mode == domain.ModeHybrid {
		projectOverrides := s.projectHybridOverrides(project)
		explicit := normalizeHybridOverrideFlags(req.Services)
		services = mergeHybridOverrides(services, projectOverrides, explicit)
		if err := validateHybridEnvironment(namespace, base, services); err != nil {
			return domain.Environment{}, err
		}
	}
	manifestSource := s.manifestSourceFor(product)
	overrides := mergeOverrides(productTemplateSubstitutions(product, manifestSource, services), req.Overrides)
	gitOpsPath := manifestSource.Path
	if gitOpsPath == "" {
		gitOpsPath = product.BasePath
	}
	if gitOpsPath == "" {
		gitOpsPath = strings.TrimSuffix(runtime.ProductBasePath, "/") + "/" + product.Name
	}
	healthCheck := product.HealthCheck
	if healthCheck == "" {
		healthCheck = runtime.HealthCheckName
	}
	renderer := strings.TrimSpace(manifestSource.Kind)
	valuesPath := strings.TrimSpace(manifestSource.ValuesPath)
	if valuesPath == "" {
		valuesPath = strings.TrimSpace(product.ValuesPath)
	}
	targetNamespace := resolveProductTargetNamespace(product.TargetNamespace, namespace)

	var expiresAt *time.Time
	if !req.Pinned {
		value := now.Add(time.Duration(ttlHours) * time.Hour)
		expiresAt = &value
	}

	return domain.Environment{
		ID:        id,
		Project:   project,
		Product:   product.Name,
		ClusterID: clusterID,
		Namespace: namespace,
		Mode:      mode,
		Status:    domain.StatusCreating,
		Domain:    domainName,
		URL:       "https://" + domainName,
		Source:    req.Source,
		Base:      base,
		GitOps: domain.GitOpsTarget{
			Path:            gitOpsPath,
			Renderer:        renderer,
			ValuesPath:      valuesPath,
			SourceRefName:   runtime.SourceRefName,
			TargetNamespace: targetNamespace,
			HealthCheckName: healthCheck,
		},
		Charts:          charts,
		Infrastructure:  infra,
		Services:        services,
		Overrides:       overrides,
		Pinned:          req.Pinned,
		TTLHours:        ttlHours,
		CostEstimateDay: costEstimateDay(mode),
		LastActivityAt:  &now,
		ExpiresAt:       expiresAt,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func resolveProductTargetNamespace(value, environmentNamespace string) string {
	target := strings.TrimSpace(value)
	switch strings.ToLower(target) {
	case "", "default":
		return ""
	case "environment", "env", "namespace", "$namespace", "${namespace}", "{{namespace}}":
		return environmentNamespace
	default:
		return target
	}
}

func (s *EnvironmentService) projectHybridOverrides(projectID string) map[string]bool {
	if s.projects == nil {
		return nil
	}
	project, err := s.projects.Get(projectID)
	if err != nil {
		return nil
	}
	return normalizeHybridOverrides(project.BaseEnvConfig.HybridOverrides)
}

func (s *EnvironmentService) markActive(env domain.Environment) domain.Environment {
	now := s.now()
	env.LastActivityAt = &now
	env.Idle = false
	env.IdleSince = nil
	env.UpdatedAt = now
	return env
}

func costEstimateDay(mode domain.EnvironmentMode) string {
	if mode == domain.ModeHybrid {
		return "~ €0.60/day"
	}
	return "~ €1.20/day"
}

func isReservedForCreate(environment domain.Environment) bool {
	return environment.Status == domain.StatusCreating &&
		environment.ManifestPath == "" &&
		environment.NamespaceManifestPath == "" &&
		environment.KustomizationManifestPath == ""
}

func (s *EnvironmentService) runtimeSettings() domain.RuntimeSettings {
	runtime := domain.RuntimeSettings{
		DefaultProduct:          "generic",
		DefaultProject:          "default",
		DefaultMode:             domain.ModeFull,
		DomainRoot:              s.cfg.DefaultDomainRoot,
		NamespacePrefix:         "envpilot-pr",
		DefaultTTLHours:         int(s.cfg.DefaultTTL.Hours()),
		MaxCPUPerEnv:            0,
		MaxMemoryPerEnv:         0,
		MaxActiveEnvsPerProject: 0,
		AutoDeleteIdleEnvs:      boolPtr(true),
		ProductBasePath:         s.cfg.GitOps.ProductBasePath,
		SourceRefName:           s.cfg.GitOps.SourceRefName,
		HealthCheckName:         s.cfg.GitOps.HealthCheckName,
	}
	if runtime.DefaultTTLHours <= 0 {
		runtime.DefaultTTLHours = 48
	}
	if runtime.DomainRoot == "" {
		runtime.DomainRoot = "feature.int"
	}
	if runtime.ProductBasePath == "" {
		runtime.ProductBasePath = "apps"
	}
	if runtime.SourceRefName == "" {
		runtime.SourceRefName = "apps"
	}
	if runtime.HealthCheckName == "" {
		runtime.HealthCheckName = "app"
	}
	if s.settings == nil {
		return runtime
	}
	settings, err := s.settings.GetSettings()
	if err != nil {
		return runtime
	}
	return mergeRuntimeSettings(runtime, settings.Runtime)
}

func (s *EnvironmentService) checkActiveEnvironmentLimit(project string, excludeID string) error {
	runtime := s.runtimeSettings()
	if runtime.MaxActiveEnvsPerProject <= 0 {
		return nil
	}
	environments, err := s.store.List()
	if err != nil {
		return err
	}
	active := 0
	for _, env := range environments {
		if env.ID == excludeID {
			continue
		}
		if env.Project != project {
			continue
		}
		if env.Status != domain.StatusCreating && env.Status != domain.StatusReady {
			continue
		}
		active++
	}
	if active >= runtime.MaxActiveEnvsPerProject {
		return ValidationError{Message: fmt.Sprintf("project %q reached active environment limit (%d)", project, runtime.MaxActiveEnvsPerProject)}
	}
	return nil
}

func (s *EnvironmentService) productTemplate(name string) (domain.ProductTemplate, bool) {
	if s.products != nil {
		product, err := s.products.GetProduct(name)
		if err == nil {
			return product, true
		}
	}
	return s.catalog.Get(name)
}

func (s *EnvironmentService) manifestSourceFor(product domain.ProductTemplate) domain.ManifestSource {
	sourceID := normalizeID(product.ManifestSourceID)
	if sourceID == "" || s.settings == nil {
		return domain.ManifestSource{}
	}
	settings, err := s.settings.GetSettings()
	if err != nil {
		return domain.ManifestSource{}
	}
	for _, source := range settings.ManifestSources {
		if normalizeID(source.ID) == sourceID && source.Enabled {
			return source
		}
	}
	return domain.ManifestSource{}
}

func (s *EnvironmentService) clusterIDForProject(projectID string) string {
	if s.projects == nil {
		return ""
	}
	project, err := s.projects.Get(projectID)
	if err != nil {
		return ""
	}
	return normalizeID(project.ClusterID)
}

func (s *EnvironmentService) gitOpsWriterForEnvironment(ctx context.Context, environment domain.Environment) (gitops.Writer, error) {
	if s.projects == nil || s.settings == nil {
		return s.writer, nil
	}
	project, err := s.projects.Get(environment.Project)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			return s.writer, nil
		}
		return nil, err
	}
	settings, err := s.settings.GetSettings()
	if err != nil {
		return nil, err
	}
	repository, ok := gitOpsRepositoryForProject(project, settings)
	if !ok {
		return s.writer, nil
	}
	secretValue, err := gitSecretValue(ctx, repository.SecretRef, project.SecretRefs, settings.SecretRefs)
	if err != nil {
		return nil, err
	}
	runtime := mergeRuntimeSettings(domain.RuntimeSettings{
		GitOpsDir:       s.cfg.GitOpsDir,
		EnableGitCommit: s.cfg.EnableGitCommit,
		EnableGitPush:   s.cfg.EnableGitPush,
		GitPushRemote:   s.cfg.GitPushRemote,
		GitPushBranch:   s.cfg.GitPushBranch,
	}, settings.Runtime)
	branch := strings.TrimSpace(repository.DefaultBranch)
	if branch == "" {
		branch = strings.TrimSpace(runtime.GitPushBranch)
	}
	if branch == "" {
		branch = "main"
	}
	branchStrategy := normalizeGitOpsBranchStrategy(repository.BranchStrategy)
	pushBranch := branch
	if branchStrategy == "branch" || branchStrategy == "pull-request" {
		pushBranch = "github.com/envpilot/runner/" + environment.ID
	}
	workspaceRoot := filepath.Join(s.cfg.DataDir, "gitops-repositories")
	workspace := gitops.RepositoryWorkspace(workspaceRoot, repository.URL, branch)
	return gitops.NewRepositoryWriter(gitops.RepositoryTarget{
		URL:               repository.URL,
		Provider:          repository.Provider,
		Branch:            branch,
		BranchStrategy:    branchStrategy,
		Path:              repository.Path,
		SecretValue:       secretValue,
		Workspace:         workspace,
		Commit:            runtime.EnableGitCommit,
		Push:              runtime.EnableGitPush,
		PushRemote:        runtime.GitPushRemote,
		PushBranch:        pushBranch,
		AuthorName:        s.cfg.GitAuthorName,
		AuthorEmail:       s.cfg.GitAuthorEmail,
		CreatePullRequest: branchStrategy == "pull-request",
		PullRequestTitle:  "EnvPilot " + environment.ID,
		PullRequestBody:   "Generated GitOps manifests for " + environment.ID + ".",
	})
}

func gitOpsRepositoryForProject(project domain.Project, settings domain.ControlPlaneSettings) (domain.ConfiguredRepository, bool) {
	repositoryID := normalizeID(project.GitOpsRepositoryID)
	if repositoryID != "" {
		for _, repository := range settings.Repositories {
			if normalizeID(repository.ID) == repositoryID {
				repository.URL = strings.TrimSpace(repository.URL)
				if repository.URL != "" {
					return repository, true
				}
				return domain.ConfiguredRepository{}, false
			}
		}
	}
	if strings.TrimSpace(project.GitOpsRepo.URL) == "" {
		return domain.ConfiguredRepository{}, false
	}
	return domain.ConfiguredRepository{
		ID:             repositoryID,
		Name:           project.GitOpsRepo.URL,
		Kind:           "gitops",
		Provider:       project.GitOpsRepo.Provider,
		URL:            project.GitOpsRepo.URL,
		DefaultBranch:  project.GitOpsRepo.DefaultBranch,
		Path:           project.GitOpsRepo.Path,
		BranchStrategy: "direct",
	}, true
}

func normalizeGitOpsBranchStrategy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "direct", "main":
		return "direct"
	case "branch", "environment-branch", "env-branch":
		return "branch"
	case "pull-request", "pr", "merge-request", "mr":
		return "pull-request"
	default:
		return "direct"
	}
}

func ensureClusterRoute(environment domain.Environment, reportedClusterID string) error {
	expected := normalizeID(environment.ClusterID)
	reported := normalizeID(reportedClusterID)
	if expected == "" {
		return nil
	}
	if reported == "" {
		return ConflictError{Message: fmt.Sprintf("environment %q belongs to cluster %q, missing report clusterId", environment.ID, expected)}
	}
	if expected == reported {
		return nil
	}
	return ConflictError{Message: fmt.Sprintf("environment %q belongs to cluster %q, got report from %q", environment.ID, expected, reported)}
}

func gitSecretValue(ctx context.Context, repositorySecretRef string, projectSecretRefs []string, secretRefs []domain.SecretReference) (string, error) {
	candidates := make([]string, 0, 1+len(projectSecretRefs))
	if repositorySecretRef != "" {
		candidates = append(candidates, repositorySecretRef)
	}
	candidates = append(candidates, projectSecretRefs...)
	resolver := secrets.NewResolver()
	for index, candidate := range candidates {
		secret, ok := findSecretReference(candidate, secretRefs)
		if !ok {
			continue
		}
		isRepositorySecret := repositorySecretRef != "" && index == 0
		if !isRepositorySecret && !isGitSecretScope(secret.Scope) {
			continue
		}
		return resolver.Resolve(ctx, secret)
	}
	return "", nil
}

func findSecretReference(id string, secrets []domain.SecretReference) (domain.SecretReference, bool) {
	id = normalizeID(id)
	if id == "" {
		return domain.SecretReference{}, false
	}
	for _, secret := range secrets {
		if normalizeID(secret.ID) == id {
			return secret, true
		}
	}
	return domain.SecretReference{}, false
}

func isGitSecretScope(scope string) bool {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "", "git", "gitops", "github", "gitlab", "scm", "repository", "repo":
		return true
	default:
		return false
	}
}

func mergeRuntimeSettings(base domain.RuntimeSettings, override domain.RuntimeSettings) domain.RuntimeSettings {
	if strings.TrimSpace(override.DefaultProduct) != "" {
		base.DefaultProduct = strings.TrimSpace(override.DefaultProduct)
	}
	if strings.TrimSpace(override.DefaultProject) != "" {
		base.DefaultProject = strings.TrimSpace(override.DefaultProject)
	}
	if override.DefaultMode != "" {
		base.DefaultMode = override.DefaultMode
	}
	if strings.TrimSpace(override.DomainRoot) != "" {
		base.DomainRoot = strings.TrimSpace(override.DomainRoot)
	}
	if strings.TrimSpace(override.NamespacePrefix) != "" {
		base.NamespacePrefix = strings.TrimSpace(override.NamespacePrefix)
	}
	if override.DefaultTTLHours > 0 {
		base.DefaultTTLHours = override.DefaultTTLHours
	}
	if strings.TrimSpace(override.ProductBasePath) != "" {
		base.ProductBasePath = strings.TrimSpace(override.ProductBasePath)
	}
	if strings.TrimSpace(override.SourceRefName) != "" {
		base.SourceRefName = strings.TrimSpace(override.SourceRefName)
	}
	if strings.TrimSpace(override.HealthCheckName) != "" {
		base.HealthCheckName = strings.TrimSpace(override.HealthCheckName)
	}
	if override.MaxCPUPerEnv >= 0 {
		base.MaxCPUPerEnv = override.MaxCPUPerEnv
	}
	if override.MaxMemoryPerEnv >= 0 {
		base.MaxMemoryPerEnv = override.MaxMemoryPerEnv
	}
	if override.MaxActiveEnvsPerProject >= 0 {
		base.MaxActiveEnvsPerProject = override.MaxActiveEnvsPerProject
	}
	if override.AutoDeleteIdleEnvs != nil {
		base.AutoDeleteIdleEnvs = override.AutoDeleteIdleEnvs
	}
	if strings.TrimSpace(override.GitOpsDir) != "" {
		base.GitOpsDir = strings.TrimSpace(override.GitOpsDir)
	}
	if override.EnableGitCommit {
		base.EnableGitCommit = true
	}
	if override.EnableGitPush {
		base.EnableGitPush = true
	}
	if strings.TrimSpace(override.GitPushRemote) != "" {
		base.GitPushRemote = strings.TrimSpace(override.GitPushRemote)
	}
	if strings.TrimSpace(override.GitPushBranch) != "" {
		base.GitPushBranch = strings.TrimSpace(override.GitPushBranch)
	}
	return base
}

func mergeCharts(base domain.ChartVersions, override domain.ChartVersions) domain.ChartVersions {
	if override.App != "" {
		base.App = override.App
	}
	if override.Infra != "" {
		base.Infra = override.Infra
	}
	if override.Nginx != "" {
		base.Nginx = override.Nginx
	}
	return base
}

func boolPtr(value bool) *bool {
	valueCopy := value
	return &valueCopy
}

func mergeInfrastructure(base domain.Infrastructure, override domain.Infrastructure) domain.Infrastructure {
	if override.MySQL || override.Postgres || override.RabbitMQ || override.Redis || override.Memcached || override.MongoDB {
		base.MySQL = override.MySQL
		base.Postgres = override.Postgres
		base.RabbitMQ = override.RabbitMQ
		base.Redis = override.Redis
		base.Memcached = override.Memcached
		base.MongoDB = override.MongoDB
	}
	if override.Zone != "" {
		base.Zone = override.Zone
	}
	if override.Capacity != "" {
		base.Capacity = override.Capacity
	}
	if base.Zone == "" {
		base.Zone = "ca-central-1b"
	}
	if base.Capacity == "" {
		base.Capacity = "spot"
	}
	return base
}

func (s *EnvironmentService) baseEnvironmentFor(projectID string, override domain.BaseEnvironment) (domain.BaseEnvironment, error) {
	base := override
	if s.projects != nil {
		project, err := s.projects.Get(projectID)
		if err != nil && !errors.Is(err, store.ErrProjectNotFound) {
			return domain.BaseEnvironment{}, fmt.Errorf("load project base environment: %w", err)
		}
		if err == nil {
			base = domain.BaseEnvironment{
				EnvironmentID: project.BaseEnvConfig.EnvironmentID,
				Namespace:     project.BaseEnvConfig.Namespace,
				Domain:        project.BaseEnvConfig.Domain,
				Services:      append([]domain.BaseServiceRef(nil), project.BaseEnvConfig.Services...),
			}
			base = mergeBaseEnvironment(base, override)
		}
	}
	base.EnvironmentID = strings.TrimSpace(base.EnvironmentID)
	if base.EnvironmentID == "" {
		base.EnvironmentID = "feature"
	}
	base.Namespace = strings.TrimSpace(base.Namespace)
	if base.Namespace == "" {
		base.Namespace = base.EnvironmentID
	}
	base.Domain = strings.TrimSpace(base.Domain)
	base.Services = normalizeBaseServices(base.Namespace, base.Services)
	return base, nil
}

func mergeBaseEnvironment(base domain.BaseEnvironment, override domain.BaseEnvironment) domain.BaseEnvironment {
	if strings.TrimSpace(override.EnvironmentID) != "" {
		base.EnvironmentID = strings.TrimSpace(override.EnvironmentID)
	}
	if strings.TrimSpace(override.Namespace) != "" {
		base.Namespace = strings.TrimSpace(override.Namespace)
	}
	if strings.TrimSpace(override.Domain) != "" {
		base.Domain = strings.TrimSpace(override.Domain)
	}
	if len(override.Services) > 0 {
		base.Services = append([]domain.BaseServiceRef(nil), override.Services...)
	}
	return base
}

func validateHybridEnvironment(namespace string, base domain.BaseEnvironment, services []domain.ServiceOverride) error {
	if strings.TrimSpace(base.Namespace) == "" {
		return ValidationError{Message: "hybrid environment requires base.namespace"}
	}
	if strings.TrimSpace(base.Namespace) == strings.TrimSpace(namespace) {
		return ValidationError{Message: "hybrid base.namespace must be different from the feature namespace"}
	}
	if len(base.Services) == 0 {
		return ValidationError{Message: "hybrid environment requires base services"}
	}
	baseServiceNames := make(map[string]struct{}, len(base.Services))
	for _, service := range base.Services {
		name := strings.ToLower(strings.TrimSpace(service.Name))
		if name == "" {
			continue
		}
		baseServiceNames[name] = struct{}{}
	}
	if len(services) == 0 {
		return ValidationError{Message: "hybrid environment requires product services"}
	}

	replaced := 0
	for _, service := range services {
		name := strings.ToLower(strings.TrimSpace(service.Name))
		if name == "" {
			continue
		}
		if service.Replace {
			if _, ok := baseServiceNames[name]; !ok {
				return ValidationError{Message: fmt.Sprintf("hybrid environment requires replaced service %q in base services", name)}
			}
			replaced++
		}
	}
	if replaced == 0 {
		return ValidationError{Message: "hybrid environment requires at least one replaced service"}
	}
	if replaced == len(services) {
		return ValidationError{Message: "hybrid environment must keep at least one service on the base environment"}
	}
	return nil
}

func normalizeHybridOverrideFlags(overrides []domain.ServiceOverride) map[string]bool {
	normalized := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		name := strings.ToLower(strings.TrimSpace(override.Name))
		if name == "" {
			continue
		}
		normalized[name] = override.Replace
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func mergeHybridOverrides(services []domain.ServiceOverride, policy map[string]bool, explicit map[string]bool) []domain.ServiceOverride {
	if len(services) == 0 {
		return services
	}
	if len(policy) == 0 && len(explicit) == 0 {
		return services
	}
	merged := make([]domain.ServiceOverride, len(services))
	for index, service := range services {
		name := strings.ToLower(strings.TrimSpace(service.Name))
		if name == "" {
			merged[index] = service
			continue
		}
		if value, ok := explicit[name]; ok {
			service.Replace = value
		} else if value, ok := policy[name]; ok {
			service.Replace = value
		}
		merged[index] = service
	}
	return merged
}

func mergeServices(defaults []domain.ServiceTemplate, overrides []domain.ServiceOverride, commitSHA string) []domain.ServiceOverride {
	services := make([]domain.ServiceOverride, 0, len(defaults)+len(overrides))
	seen := map[string]int{}
	for _, service := range defaults {
		tag := service.DefaultTag
		if strings.TrimSpace(commitSHA) != "" {
			tag = strings.TrimSpace(commitSHA)
		}
		if tag == "" {
			continue
		}
		seen[strings.ToLower(service.Name)] = len(services)
		services = append(services, domain.ServiceOverride{Name: service.Name, Tag: tag})
	}
	for _, override := range overrides {
		name := strings.ToLower(strings.TrimSpace(override.Name))
		tag := strings.TrimSpace(override.Tag)
		if name == "" {
			continue
		}
		if tag == "" && !override.Replace {
			continue
		}
		if index, ok := seen[name]; ok {
			if tag != "" {
				services[index].Tag = tag
			}
			services[index].Replace = override.Replace
			continue
		}
		seen[name] = len(services)
		services = append(services, domain.ServiceOverride{Name: name, Tag: tag, Replace: override.Replace})
	}
	return services
}

func mergeOverrides(defaults map[string]string, overrides map[string]string) map[string]string {
	if len(defaults) == 0 && len(overrides) == 0 {
		return nil
	}
	merged := make(map[string]string, len(defaults)+len(overrides))
	for key, value := range defaults {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		merged[key] = strings.TrimSpace(value)
	}
	for key, value := range overrides {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		merged[key] = strings.TrimSpace(value)
	}
	return merged
}

func mergeServiceTagSubstitutions(base map[string]string, templates []domain.ServiceTemplate, services []domain.ServiceOverride) map[string]string {
	if len(templates) == 0 || len(services) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(services))
	for key, value := range base {
		merged[key] = value
	}
	tagKeys := map[string]string{}
	for _, template := range templates {
		name := strings.ToLower(strings.TrimSpace(template.Name))
		tagKey := strings.TrimSpace(template.TagKey)
		if name == "" || tagKey == "" {
			continue
		}
		tagKeys[name] = tagKey
	}
	for _, service := range services {
		name := strings.ToLower(strings.TrimSpace(service.Name))
		tag := strings.TrimSpace(service.Tag)
		if name == "" || tag == "" {
			continue
		}
		if tagKey := tagKeys[name]; tagKey != "" {
			merged[tagKey] = tag
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func productTemplateSubstitutions(product domain.ProductTemplate, source domain.ManifestSource, services []domain.ServiceOverride) map[string]string {
	values := mergeServiceTagSubstitutions(product.Substitutions, product.Services, services)
	if strings.TrimSpace(product.ValuesPath) != "" {
		values = setSubstitution(values, "valuesPath", product.ValuesPath)
	}
	if strings.TrimSpace(source.ID) != "" {
		values = setSubstitution(values, "manifestSourceId", source.ID)
	}
	if strings.TrimSpace(source.Kind) != "" {
		values = setSubstitution(values, "manifestSourceKind", source.Kind)
	}
	if strings.TrimSpace(source.ValuesPath) != "" {
		values = setSubstitution(values, "valuesPath", source.ValuesPath)
	}
	if strings.TrimSpace(source.Version) != "" {
		values = setSubstitution(values, "manifestSourceVersion", source.Version)
	}
	return values
}

func setSubstitution(values map[string]string, key string, value string) map[string]string {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return values
	}
	if values == nil {
		values = map[string]string{}
	}
	values[key] = value
	return values
}

func environmentIDFromSource(source domain.SCMSource) string {
	if source.PullRequestID != "" {
		id := normalizeID(source.PullRequestID)
		if id != "" {
			return id
		}
	}
	return branchToEnvironmentID(source.Branch)
}

func namespaceIDFromSource(source domain.SCMSource, fallbackID string) string {
	if source.PullRequestID != "" {
		if id := normalizeID(source.PullRequestID); id != "" {
			return id
		}
	}
	return fallbackID
}

func namespaceName(prefix string, id string) string {
	prefix = normalizeID(prefix)
	id = normalizeID(id)
	if prefix == "" {
		return gitops.NamespaceName(id)
	}
	if id == "" {
		return prefix
	}
	return prefix + "-" + id
}

func branchToEnvironmentID(branch string) string {
	branch = strings.ToLower(strings.TrimSpace(branch))
	if branch == "" {
		return ""
	}
	return normalizeID(branch)
}

func previewHostname(id string, project string, source domain.SCMSource, root string) string {
	changeID := normalizeID(source.PullRequestID)
	if changeID == "" {
		changeID = normalizeID(id)
	}
	prefix := changeID
	if !strings.HasPrefix(prefix, "pr-") {
		prefix = "pr-" + prefix
	}
	project = normalizeID(project)
	if project == "" {
		project = "default"
	}
	root = strings.Trim(strings.ToLower(strings.TrimSpace(root)), ".")
	if root == "" {
		root = "feature.int"
	}
	if strings.HasPrefix(root, "preview.") {
		return prefix + "." + project + "." + root
	}
	return prefix + "." + project + ".preview." + root
}

var validID = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var invalidIDChars = regexp.MustCompile(`[^a-z0-9-]+`)

func normalizeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = invalidIDChars.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func validStatus(status domain.EnvironmentStatus) bool {
	switch status {
	case domain.StatusCreating, domain.StatusReady, domain.StatusFailed, domain.StatusDeleteRequested, domain.StatusGitOpsDeletePending, domain.StatusDeleteFailed, domain.StatusTerminating, domain.StatusTerminated:
		return true
	default:
		return false
	}
}

func normalizeEvents(events []domain.KubernetesEvent) []domain.KubernetesEvent {
	items := make([]domain.KubernetesEvent, 0, len(events))
	for _, event := range events {
		if strings.TrimSpace(event.Message) == "" && strings.TrimSpace(event.Reason) == "" {
			continue
		}
		items = append(items, event)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return eventLastSeen(items[i]).After(eventLastSeen(items[j]))
	})
	if len(items) > 100 {
		items = items[:100]
	}
	return items
}

func eventLastSeen(event domain.KubernetesEvent) time.Time {
	if !event.LastSeen.IsZero() {
		return event.LastSeen
	}
	return event.FirstSeen
}

func (s *EnvironmentService) applyStatusLifecycle(env domain.Environment, next domain.EnvironmentStatus, message string) domain.Environment {
	if next == "" || env.Status == next || !allowedStatusTransition(env.Status, next) {
		env.UpdatedAt = s.now()
		return env
	}
	env.Status = next
	env.LastError = ""
	if next == domain.StatusFailed || next == domain.StatusDeleteFailed {
		env.LastError = message
	}
	env.UpdatedAt = s.now()
	return env
}

func allowedStatusTransition(current domain.EnvironmentStatus, next domain.EnvironmentStatus) bool {
	if current == "" {
		return next == domain.StatusCreating || next == domain.StatusReady || next == domain.StatusFailed
	}
	if current == next {
		return true
	}
	switch current {
	case domain.StatusCreating:
		return next == domain.StatusReady || next == domain.StatusFailed || next == domain.StatusDeleteRequested || next == domain.StatusTerminating || next == domain.StatusTerminated
	case domain.StatusReady:
		return next == domain.StatusFailed || next == domain.StatusDeleteRequested || next == domain.StatusTerminating || next == domain.StatusTerminated
	case domain.StatusFailed:
		return next == domain.StatusDeleteRequested || next == domain.StatusTerminating || next == domain.StatusTerminated
	case domain.StatusDeleteRequested:
		return next == domain.StatusGitOpsDeletePending || next == domain.StatusDeleteFailed || next == domain.StatusTerminating || next == domain.StatusTerminated
	case domain.StatusGitOpsDeletePending:
		return next == domain.StatusDeleteRequested || next == domain.StatusDeleteFailed || next == domain.StatusTerminating || next == domain.StatusTerminated
	case domain.StatusDeleteFailed:
		return next == domain.StatusDeleteRequested || next == domain.StatusTerminating || next == domain.StatusTerminated
	case domain.StatusTerminating:
		return next == domain.StatusTerminated || next == domain.StatusDeleteFailed
	case domain.StatusTerminated:
		return false
	default:
		return false
	}
}

func (s *EnvironmentService) commentEnvironment(ctx context.Context, environment domain.Environment) {
	if s.comment == nil {
		return
	}
	_ = s.comment.CommentEnvironment(ctx, environment)
}

func (s *EnvironmentService) notifyEnvironment(ctx context.Context, environment domain.Environment) {
	if s.notifier == nil {
		return
	}
	_ = s.notifier.NotifyEnvironment(ctx, environment)
}

type noopCommenter struct{}

func (noopCommenter) CommentEnvironment(context.Context, domain.Environment) error {
	return nil
}

type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string {
	return e.Message
}

type ConflictError struct {
	Message string
}

func (e ConflictError) Error() string {
	return e.Message
}
