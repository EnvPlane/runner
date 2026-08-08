package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/envpilot/runner/internal/config"
	"github.com/envpilot/runner/internal/domain"
	"github.com/envpilot/runner/internal/store"
)

type SettingsService struct {
	store store.SettingsStore
	now   func() time.Time
}

type AgentBootstrapClaimFunc func(now time.Time) (string, error)

func NewSettingsService(settingsStore store.SettingsStore) *SettingsService {
	return &SettingsService{
		store: settingsStore,
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func DefaultControlPlaneSettings(cfg config.Config) domain.ControlPlaneSettings {
	return domain.ControlPlaneSettings{
		SchemaVersion: "v1",
		Runtime: domain.RuntimeSettings{
			DefaultProduct:          "generic",
			DefaultProject:          "default",
			DefaultMode:             domain.ModeFull,
			DomainRoot:              cfg.DefaultDomainRoot,
			NamespacePrefix:         "envpilot-pr",
			DefaultTTLHours:         int(cfg.DefaultTTL.Hours()),
			MaxCPUPerEnv:            0,
			MaxMemoryPerEnv:         0,
			MaxActiveEnvsPerProject: 0,
			AutoDeleteIdleEnvs:      ptrBool(true),
			IdleThresholdHours:      int(cfg.IdleThreshold.Hours()),
			TTLCheckSeconds:         int(cfg.TTLCheckInterval.Seconds()),
			JobRetrySeconds:         int(cfg.JobRetryDelay.Seconds()),
			JobMaxAttempts:          cfg.JobMaxAttempts,
			GitOpsDir:               cfg.GitOpsDir,
			ProductBasePath:         cfg.GitOps.ProductBasePath,
			FluxNamespace:           cfg.GitOps.FluxNamespace,
			SourceRefName:           cfg.GitOps.SourceRefName,
			DependsOnName:           cfg.GitOps.DependsOnName,
			HealthCheckName:         cfg.GitOps.HealthCheckName,
			EnableGitCommit:         cfg.EnableGitCommit,
			EnableGitPush:           cfg.EnableGitPush,
			GitPushRemote:           cfg.GitPushRemote,
			GitPushBranch:           cfg.GitPushBranch,
			CatalogPath:             cfg.CatalogPath,
			DatabaseURLConfigured:   cfg.DatabaseURL != "",
			RedisURLConfigured:      cfg.RedisURL != "",
		},
		ManifestSources: []domain.ManifestSource{
			{
				ID:      "generic",
				Name:    "Generic app template",
				Kind:    "flux-kustomization",
				Path:    strings.TrimSuffix(cfg.GitOps.ProductBasePath, "/") + "/generic",
				Enabled: true,
			},
		},
	}
}

func (s *SettingsService) GetSettings() (domain.ControlPlaneSettings, error) {
	return s.store.Get()
}

func (s *SettingsService) SaveSettings(settings domain.ControlPlaneSettings, updatedBy string) (domain.ControlPlaneSettings, error) {
	normalized, err := normalizeSettings(settings)
	if err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	normalized.UpdatedAt = s.now()
	normalized.UpdatedBy = strings.TrimSpace(updatedBy)
	if err := s.store.Save(normalized); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	return normalized, nil
}

// RegisterAgent records the side effects of an already authenticated bootstrap
// exchange. Callers should validate and claim the one-time registration token
// before invoking this method, then persist the issued agent auth token and the
// consumed bootstrap state atomically with the registration state when possible.
// The operation is idempotent for the same clusterId/agentId pair: retries update
// the existing cluster target instead of creating a second logical agent.
func (s *SettingsService) RegisterAgent(req domain.AgentRegistrationRequest) (domain.ClusterTarget, error) {
	cluster, err := s.registerAgent(req, "online")
	if err != nil {
		return domain.ClusterTarget{}, err
	}
	return cluster, nil
}

// RegisterAgentWithBootstrapClaim records the settings-side agent registration
// as registration_pending and only then executes the bootstrap claim callback
// that consumes the one-time token and persists the agent auth token hash.
//
// This service cannot provide a cross-store database transaction for every store
// implementation. The guarantee is therefore one-way and deliberate:
//   - if settings registration fails, the bootstrap token is not consumed;
//   - if the bootstrap claim fails after settings registration, the cluster is
//     visible only as registration_pending, not online. Retrying the same
//     projectId+agentId+clusterId is safe because registerAgent is idempotent and
//     updates the existing cluster target instead of creating duplicates.
//
// A successful authenticated heartbeat is responsible for moving the cluster to
// online after the auth token hash exists. This avoids presenting untrusted
// settings registration state as a connected agent when claim finalization fails.
//
// Store-level bootstrap claim CAS, not a process-local caller lock, is the
// authoritative race-protection boundary for the one-time token. Callers may
// still serialize locally to reduce duplicate work and preserve in-process
// retry token reuse.
func (s *SettingsService) RegisterAgentWithBootstrapClaim(req domain.AgentRegistrationRequest, claim AgentBootstrapClaimFunc) (domain.ClusterTarget, string, error) {
	if claim == nil {
		return domain.ClusterTarget{}, "", ValidationError{Message: "bootstrap claim callback is required"}
	}
	cluster, err := s.registerAgent(req, "registration_pending")
	if err != nil {
		return domain.ClusterTarget{}, "", err
	}
	now := req.ObservedAt
	if now.IsZero() {
		now = s.now()
	}
	agentAuthToken, err := claim(now)
	if err != nil {
		return domain.ClusterTarget{}, "", err
	}
	return cluster, agentAuthToken, nil
}

func (s *SettingsService) registerAgent(req domain.AgentRegistrationRequest, status string) (domain.ClusterTarget, error) {
	clusterID := normalizeID(req.ClusterID)
	if clusterID == "" {
		return domain.ClusterTarget{}, ValidationError{Message: "clusterId is required"}
	}
	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		return domain.ClusterTarget{}, ValidationError{Message: "agentId is required"}
	}
	settings, err := s.store.Get()
	if err != nil {
		return domain.ClusterTarget{}, err
	}
	now := req.ObservedAt
	if now.IsZero() {
		now = s.now()
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = "registration_pending"
	}
	cluster := upsertClusterAgent(settings.Clusters, clusterID, func(cluster *domain.ClusterTarget) {
		if cluster.Name == "" {
			cluster.Name = clusterID
		}
		if cluster.Provider == "" {
			cluster.Provider = "kubernetes"
		}
		cluster.AgentID = agentID
		cluster.AgentVersion = strings.TrimSpace(req.AgentVersion)
		cluster.AgentNamespace = strings.TrimSpace(req.AgentNamespace)
		cluster.AgentStatus = status
		cluster.AgentError = ""
		cluster.KubernetesVersion = strings.TrimSpace(req.KubernetesVersion)
		cluster.FluxNamespace = strings.TrimSpace(req.FluxNamespace)
		cluster.NamespaceSelector = strings.TrimSpace(req.NamespaceSelector)
		cluster.Capabilities = normalizeCapabilities(req.Capabilities)
		cluster.HeartbeatIntervalSeconds = req.HeartbeatIntervalSeconds
		cluster.LastHeartbeatAt = &now
		cluster.Enabled = true
	})
	settings.Clusters = cluster.items
	settings.UpdatedAt = now
	settings.UpdatedBy = "agent:" + agentID
	if err := s.store.Save(settings); err != nil {
		return domain.ClusterTarget{}, err
	}
	return cluster.value, nil
}

func (s *SettingsService) RecordAgentHeartbeat(req domain.AgentHeartbeatRequest) (domain.ClusterTarget, error) {
	clusterID := normalizeID(req.ClusterID)
	if clusterID == "" {
		return domain.ClusterTarget{}, ValidationError{Message: "clusterId is required"}
	}
	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		return domain.ClusterTarget{}, ValidationError{Message: "agentId is required"}
	}
	settings, err := s.store.Get()
	if err != nil {
		return domain.ClusterTarget{}, err
	}
	now := req.ObservedAt
	if now.IsZero() {
		now = s.now()
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "online"
	}
	cluster := upsertClusterAgent(settings.Clusters, clusterID, func(cluster *domain.ClusterTarget) {
		if cluster.Name == "" {
			cluster.Name = clusterID
		}
		if cluster.Provider == "" {
			cluster.Provider = "kubernetes"
		}
		cluster.AgentID = agentID
		cluster.AgentStatus = status
		cluster.AgentError = strings.TrimSpace(req.Error)
		cluster.LastHeartbeatAt = &now
		if strings.TrimSpace(req.AgentVersion) != "" {
			cluster.AgentVersion = strings.TrimSpace(req.AgentVersion)
		}
		if strings.TrimSpace(req.KubernetesVersion) != "" {
			cluster.KubernetesVersion = strings.TrimSpace(req.KubernetesVersion)
		}
		if len(req.Capabilities) > 0 {
			cluster.Capabilities = normalizeCapabilities(req.Capabilities)
		}
	})
	settings.Clusters = cluster.items
	settings.UpdatedAt = now
	settings.UpdatedBy = "agent:" + agentID
	if err := s.store.Save(settings); err != nil {
		return domain.ClusterTarget{}, err
	}
	return cluster.value, nil
}

func normalizeSettings(settings domain.ControlPlaneSettings) (domain.ControlPlaneSettings, error) {
	settings.SchemaVersion = "v1"
	settings.Runtime.DefaultProduct = defaultString(settings.Runtime.DefaultProduct, "generic")
	settings.Runtime.DefaultProject = defaultString(settings.Runtime.DefaultProject, "default")
	if settings.Runtime.DefaultMode == "" {
		settings.Runtime.DefaultMode = domain.ModeFull
	}
	if settings.Runtime.DefaultMode != domain.ModeFull && settings.Runtime.DefaultMode != domain.ModeHybrid {
		return domain.ControlPlaneSettings{}, ValidationError{Message: fmt.Sprintf("unsupported default mode %q", settings.Runtime.DefaultMode)}
	}
	settings.Runtime.NamespacePrefix = defaultString(settings.Runtime.NamespacePrefix, "envpilot-pr")
	settings.Runtime.DomainRoot = defaultString(settings.Runtime.DomainRoot, "feature.int")
	settings.Runtime.DefaultTTLHours = defaultPositive(settings.Runtime.DefaultTTLHours, 48)
	if settings.Runtime.MaxCPUPerEnv < 0 {
		settings.Runtime.MaxCPUPerEnv = 0
	}
	if settings.Runtime.MaxMemoryPerEnv < 0 {
		settings.Runtime.MaxMemoryPerEnv = 0
	}
	if settings.Runtime.MaxActiveEnvsPerProject < 0 {
		settings.Runtime.MaxActiveEnvsPerProject = 0
	}
	settings.Runtime.AutoDeleteIdleEnvs = normalizeBool(settings.Runtime.AutoDeleteIdleEnvs, true)
	if settings.Runtime.IdleThresholdHours < 0 {
		settings.Runtime.IdleThresholdHours = 0
	}
	settings.Runtime.TTLCheckSeconds = defaultPositive(settings.Runtime.TTLCheckSeconds, 60)
	settings.Runtime.JobRetrySeconds = defaultPositive(settings.Runtime.JobRetrySeconds, 5)
	settings.Runtime.JobMaxAttempts = defaultPositive(settings.Runtime.JobMaxAttempts, 3)
	settings.Runtime.ProductBasePath = defaultString(settings.Runtime.ProductBasePath, "apps")
	settings.Runtime.FluxNamespace = defaultString(settings.Runtime.FluxNamespace, "flux-system")
	settings.Runtime.SourceRefName = defaultString(settings.Runtime.SourceRefName, "apps")
	settings.Runtime.HealthCheckName = defaultString(settings.Runtime.HealthCheckName, "app")
	settings.Runtime.GitPushRemote = defaultString(settings.Runtime.GitPushRemote, "origin")
	settings.Runtime.GitPushBranch = defaultString(settings.Runtime.GitPushBranch, "main")

	if err := normalizeRepositoryIDs(settings.Repositories); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	if err := normalizeSecretIDs(settings.SecretRefs); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	if err := normalizeManifestSourceIDs(settings.ManifestSources); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	if err := normalizeClusterIDs(settings.Clusters); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	if err := normalizeNotificationIDs(settings.Notifications); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	return settings, nil
}

func normalizeRepositoryIDs(items []domain.ConfiguredRepository) error {
	seen := map[string]struct{}{}
	for index := range items {
		items[index].ID = normalizeID(items[index].ID)
		if items[index].ID == "" {
			return ValidationError{Message: "repository id is required"}
		}
		if _, ok := seen[items[index].ID]; ok {
			return ValidationError{Message: fmt.Sprintf("duplicate repository id %q", items[index].ID)}
		}
		seen[items[index].ID] = struct{}{}
		items[index].BranchStrategy = normalizeBranchStrategy(items[index].BranchStrategy)
	}
	return nil
}

func normalizeBranchStrategy(value string) string {
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

func normalizeSecretIDs(items []domain.SecretReference) error {
	seen := map[string]struct{}{}
	for index := range items {
		items[index].ID = normalizeID(items[index].ID)
		if items[index].ID == "" {
			return ValidationError{Message: "secret reference id is required"}
		}
		if _, ok := seen[items[index].ID]; ok {
			return ValidationError{Message: fmt.Sprintf("duplicate secret reference id %q", items[index].ID)}
		}
		seen[items[index].ID] = struct{}{}
	}
	return nil
}

func normalizeManifestSourceIDs(items []domain.ManifestSource) error {
	seen := map[string]struct{}{}
	for index := range items {
		items[index].ID = normalizeID(items[index].ID)
		if items[index].ID == "" {
			return ValidationError{Message: "manifest source id is required"}
		}
		if _, ok := seen[items[index].ID]; ok {
			return ValidationError{Message: fmt.Sprintf("duplicate manifest source id %q", items[index].ID)}
		}
		seen[items[index].ID] = struct{}{}
		items[index].Kind = normalizeManifestSourceKind(items[index].Kind)
		items[index].Path = strings.TrimSpace(items[index].Path)
		items[index].ValuesPath = strings.TrimSpace(items[index].ValuesPath)
		if items[index].Enabled && items[index].Path == "" {
			return ValidationError{Message: fmt.Sprintf("manifest source %q path is required", items[index].ID)}
		}
	}
	return nil
}

func normalizeManifestSourceKind(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "flux", "flux-kustomization", "kustomization":
		return "flux-kustomization"
	case "helm", "helm-release":
		return "helm"
	case "raw", "raw-manifests", "raw-manifest":
		return "raw"
	case "kustomize", "kustomize-overlay", "overlay":
		return "kustomize-overlay"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeClusterIDs(items []domain.ClusterTarget) error {
	seen := map[string]struct{}{}
	for index := range items {
		items[index].ID = normalizeID(items[index].ID)
		if items[index].ID == "" {
			return ValidationError{Message: "cluster id is required"}
		}
		if _, ok := seen[items[index].ID]; ok {
			return ValidationError{Message: fmt.Sprintf("duplicate cluster id %q", items[index].ID)}
		}
		seen[items[index].ID] = struct{}{}
		items[index].Capabilities = normalizeCapabilities(items[index].Capabilities)
	}
	return nil
}

type clusterAgentUpsert struct {
	items []domain.ClusterTarget
	value domain.ClusterTarget
}

func upsertClusterAgent(items []domain.ClusterTarget, clusterID string, mutate func(*domain.ClusterTarget)) clusterAgentUpsert {
	updated := append([]domain.ClusterTarget(nil), items...)
	for index := range updated {
		if normalizeID(updated[index].ID) == clusterID {
			updated[index].ID = clusterID
			mutate(&updated[index])
			return clusterAgentUpsert{items: updated, value: updated[index]}
		}
	}
	cluster := domain.ClusterTarget{ID: clusterID, Name: clusterID}
	mutate(&cluster)
	updated = append(updated, cluster)
	return clusterAgentUpsert{items: updated, value: cluster}
}

func normalizeCapabilities(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		item := strings.ToLower(strings.TrimSpace(value))
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		normalized = append(normalized, item)
	}
	return normalized
}

func normalizeNotificationIDs(items []domain.NotificationTarget) error {
	seen := map[string]struct{}{}
	for index := range items {
		items[index].ID = normalizeID(items[index].ID)
		if items[index].ID == "" {
			return ValidationError{Message: "notification target id is required"}
		}
		if _, ok := seen[items[index].ID]; ok {
			return ValidationError{Message: fmt.Sprintf("duplicate notification target id %q", items[index].ID)}
		}
		seen[items[index].ID] = struct{}{}
	}
	return nil
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func defaultPositive(value int, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func normalizeBool(value *bool, fallback bool) *bool {
	if value != nil {
		return value
	}
	fallbackValue := fallback
	return &fallbackValue
}

func ptrBool(value bool) *bool {
	copy := value
	return &copy
}
