package app

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"envpilot/internal/domain"
	"envpilot/internal/store"
)

type ProjectService struct {
	store    store.ProjectStore
	settings settingsProvider
	now      func() time.Time
}

func NewProjectService(projectStore store.ProjectStore) *ProjectService {
	return &ProjectService{
		store: projectStore,
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func (s *ProjectService) SetSettingsProvider(provider settingsProvider) {
	s.settings = provider
}

func (s *ProjectService) ListProjects() ([]domain.Project, error) {
	return s.store.List()
}

func (s *ProjectService) GetProject(id string) (domain.Project, error) {
	return s.store.Get(id)
}

func (s *ProjectService) ResolveProjectByRepository(provider string, repo string) (domain.Project, bool, error) {
	repo = normalizeRepositoryIdentity(repo)
	if repo == "" {
		return domain.Project{}, false, nil
	}
	projects, err := s.store.List()
	if err != nil {
		return domain.Project{}, false, err
	}
	for _, project := range projects {
		if s.repositoryMatches(project, repo, provider) {
			return project, true, nil
		}
	}
	return domain.Project{}, false, nil
}

func (s *ProjectService) SaveProject(project domain.Project) (domain.Project, error) {
	project.ID = strings.TrimSpace(project.ID)
	if project.ID == "" {
		return domain.Project{}, ValidationError{Message: "project id is required"}
	}
	project.Name = strings.TrimSpace(project.Name)
	if project.Name == "" {
		project.Name = project.ID
	}
	project.ProductID = normalizeID(project.ProductID)
	if project.ProductID == "" {
		return domain.Project{}, ValidationError{Message: "product_id is required"}
	}
	project.AppRepositoryID = normalizeRepositoryIdentity(project.AppRepositoryID)
	project.GitOpsRepositoryID = normalizeRepositoryIdentity(project.GitOpsRepositoryID)
	project.ClusterID = normalizeID(project.ClusterID)
	project.WebhookBranchFilters = normalizeWebhookFilterList(project.WebhookBranchFilters)
	project.WebhookLabels = normalizeWebhookLabelList(project.WebhookLabels)
	project.GitHubInstallationIDs = normalizeWebhookIdentityList(project.GitHubInstallationIDs)
	project.GitLabProjectIDs = normalizeWebhookIdentityList(project.GitLabProjectIDs)
	project.AccessUsers = normalizeAccessUsers(project.AccessUsers)
	project.AccessOrganizations = normalizeAccessOrganizations(project.AccessOrganizations)
	project.SecretRefs = normalizeStringIDs(project.SecretRefs)
	project.GitRepo = normalizeRepositoryRef(project.GitRepo)
	project.GitOpsRepo = normalizeRepositoryRef(project.GitOpsRepo)
	project.CostPolicy = normalizeProjectCostPolicy(project.CostPolicy)
	if project.AppRepositoryID == "" && strings.TrimSpace(project.GitRepo.URL) == "" {
		return domain.Project{}, ValidationError{Message: "app_repository_id or git_repo.url is required"}
	}
	if project.GitOpsRepositoryID == "" && strings.TrimSpace(project.GitOpsRepo.URL) == "" {
		return domain.Project{}, ValidationError{Message: "gitops_repository_id or gitops_repo.url is required"}
	}
	existing, err := s.store.Get(project.ID)
	if err == nil && project.BaseEnvConfig.HybridOverrides == nil {
		project.BaseEnvConfig.HybridOverrides = existing.BaseEnvConfig.HybridOverrides
	} else if err != nil && err != store.ErrProjectNotFound {
		return domain.Project{}, fmt.Errorf("load project: %w", err)
	}
	if err := validateAndNormalizeBaseConfig(&project.BaseEnvConfig); err != nil {
		return domain.Project{}, err
	}

	now := s.now()
	existing, err = s.store.Get(project.ID)
	if err == nil {
		project.CreatedAt = existing.CreatedAt
	} else if err == store.ErrProjectNotFound {
		project.CreatedAt = now
	} else {
		return domain.Project{}, fmt.Errorf("load project: %w", err)
	}
	project.UpdatedAt = now
	if err := s.store.Save(project); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func (s *ProjectService) SaveProjectHybridConfig(id string, overrides map[string]bool) (domain.Project, error) {
	project, err := s.store.Get(id)
	if err != nil {
		return domain.Project{}, err
	}
	if overrides == nil {
		overrides = map[string]bool{}
	}
	project.BaseEnvConfig.HybridOverrides = normalizeHybridOverrides(overrides)
	if err := validateAndNormalizeBaseConfig(&project.BaseEnvConfig); err != nil {
		return domain.Project{}, err
	}
	return s.SaveProject(project)
}

func (s *ProjectService) SaveProjectCostPolicy(id string, policy domain.ProjectCostPolicy) (domain.Project, error) {
	project, err := s.store.Get(id)
	if err != nil {
		return domain.Project{}, err
	}
	project.CostPolicy = normalizeProjectCostPolicy(policy)
	return s.SaveProject(project)
}

type repositoryCandidate struct {
	identity string
	provider string
}

func (s *ProjectService) repositoryMatches(project domain.Project, repo string, provider string) bool {
	repo = normalizeRepositoryIdentity(repo)
	if repo == "" {
		return false
	}
	projectProvider := normalizeRepositoryProvider(project.GitRepo.Provider)
	candidates := []repositoryCandidate{
		{identity: project.GitRepo.URL, provider: projectProvider},
		{identity: project.GitRepo.Path, provider: projectProvider},
	}
	if repository, ok := s.repositoryFromSettings(project.AppRepositoryID); ok {
		repositoryProvider := normalizeRepositoryProvider(repository.Provider)
		candidates = append(candidates, repositoryCandidate{identity: repository.URL, provider: repositoryProvider})
		if strings.TrimSpace(repository.Path) != "" {
			candidates = append(candidates, repositoryCandidate{identity: repository.Path, provider: repositoryProvider})
		}
	} else if project.AppRepositoryID != "" {
		candidates = append(candidates, repositoryCandidate{identity: project.AppRepositoryID, provider: projectProvider})
	}

	targetProvider := normalizeRepositoryProvider(provider)
	for _, candidate := range candidates {
		normalizedCandidate := normalizeRepositoryIdentity(candidate.identity)
		if normalizedCandidate == "" {
			continue
		}
		candidateProvider := normalizeRepositoryProvider(candidate.provider)
		if targetProvider != "" && candidateProvider != "" && targetProvider != candidateProvider {
			continue
		}
		if normalizedCandidate == repo {
			return true
		}
	}
	return false
}

func (s *ProjectService) ResolveProjectByIntegrationID(provider string, integrationID string) (domain.Project, bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	integrationID = normalizeWebhookIdentity(integrationID)
	if integrationID == "" {
		return domain.Project{}, false, nil
	}
	projects, err := s.store.List()
	if err != nil {
		return domain.Project{}, false, err
	}
	for _, project := range projects {
		targets := project.GitHubInstallationIDs
		switch provider {
		case "github":
			// no-op
		case "gitlab":
			targets = project.GitLabProjectIDs
		default:
			continue
		}
		for _, candidate := range targets {
			if normalizeWebhookIdentity(candidate) == integrationID {
				return project, true, nil
			}
		}
	}
	return domain.Project{}, false, nil
}

func normalizeRepositoryProvider(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeAccessUsers(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}

func normalizeAccessOrganizations(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}

func (s *ProjectService) repositoryFromSettings(id string) (domain.ConfiguredRepository, bool) {
	id = normalizeID(id)
	if id == "" || s.settings == nil {
		return domain.ConfiguredRepository{}, false
	}
	settings, err := s.settings.GetSettings()
	if err != nil {
		return domain.ConfiguredRepository{}, false
	}
	for _, repository := range settings.Repositories {
		if normalizeID(repository.ID) == id {
			repository.Provider = normalizeRepositoryProvider(repository.Provider)
			if repository.Provider == "" {
				repository.Provider = inferRepositoryProvider(repository.URL)
			}
			repository.URL = strings.TrimSpace(repository.URL)
			repository.Path = strings.TrimSpace(repository.Path)
			return repository, true
		}
	}
	return domain.ConfiguredRepository{}, false
}

func normalizeRepositoryIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, ".git")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Path
	}
	value = strings.TrimPrefix(value, "git@")
	value = strings.TrimPrefix(value, "ssh://")
	value = strings.TrimPrefix(value, "git+ssh://")
	value = strings.TrimPrefix(value, "git://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "www.") {
		value = strings.TrimPrefix(value, "www.")
	}
	if idx := strings.Index(value, ":"); idx > 0 && !strings.Contains(value, "/") {
		value = value[idx+1:]
	}
	if idx := strings.Index(value, ":"); idx > 0 && !strings.Contains(value[:idx], "/") && strings.Contains(value[idx+1:], "/") {
		value = value[idx+1:]
	}
	value = strings.ReplaceAll(value, "\\", "/")
	value = strings.Trim(value, "/")
	if value == "" {
		return ""
	}

	parts := strings.Split(value, "/")
	if len(parts) > 0 {
		for len(parts) > 0 && parts[0] == "" {
			parts = parts[1:]
		}
		if len(parts) > 0 {
			if strings.Contains(parts[0], ".") {
				parts = parts[1:]
			}
		}
		if len(parts) > 0 {
			if strings.HasPrefix(parts[0], "www.") {
				parts[0] = strings.TrimPrefix(parts[0], "www.")
			}
		}
	}
	for i := 0; i < len(parts); i++ {
		parts[i] = strings.TrimSpace(strings.ToLower(parts[i]))
	}
	for len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "/")
}

func normalizeRepositoryRef(repo domain.RepositoryRef) domain.RepositoryRef {
	repo.Provider = strings.ToLower(strings.TrimSpace(repo.Provider))
	if repo.Provider == "" {
		repo.Provider = inferRepositoryProvider(repo.URL)
	}
	repo.URL = strings.TrimSpace(repo.URL)
	repo.DefaultBranch = strings.TrimSpace(repo.DefaultBranch)
	repo.Path = strings.TrimSpace(repo.Path)
	return repo
}

func inferRepositoryProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		return inferRepositoryProviderByHost(strings.TrimPrefix(parsed.Host, "www."))
	}

	if strings.HasPrefix(value, "git@") {
		value = strings.TrimPrefix(value, "git@")
		if at := strings.Index(value, ":"); at > 0 {
			return inferRepositoryProviderByHost(value[:at])
		}
		if slash := strings.Index(value, "/"); slash > 0 {
			return inferRepositoryProviderByHost(value[:slash])
		}
	}

	return ""
}

func inferRepositoryProviderByHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "www.")
	switch {
	case strings.Contains(host, "github.com"):
		return "github"
	case strings.Contains(host, "gitlab.com"):
		return "gitlab"
	default:
		return ""
	}
}

func normalizeWebhookIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeWebhookIdentityList(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		id := normalizeWebhookIdentity(value)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	return normalized
}

func normalizeWebhookFilterList(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}

func normalizeWebhookLabelList(values []string) []string {
	return normalizeWebhookFilterList(values)
}

func validateAndNormalizeBaseConfig(base *domain.BaseEnvConfig) error {
	base.EnvironmentID = strings.TrimSpace(base.EnvironmentID)
	base.Namespace = strings.TrimSpace(base.Namespace)
	base.Domain = strings.TrimSpace(base.Domain)
	base.ConfigPath = strings.TrimSpace(base.ConfigPath)
	hasBaseConfig := base.EnvironmentID != "" || base.Namespace != "" || base.Domain != "" || base.ConfigPath != "" || len(base.Services) > 0 || len(base.Values) > 0
	if !hasBaseConfig {
		return nil
	}
	if base.Namespace == "" {
		return ValidationError{Message: "base_env_config.namespace is required when base config is set"}
	}
	base.Services = normalizeBaseServices(base.Namespace, base.Services)
	base.HybridOverrides = normalizeHybridOverrides(base.HybridOverrides)
	return nil
}

func normalizeHybridOverrides(overrides map[string]bool) map[string]bool {
	if overrides == nil {
		return nil
	}
	normalized := make(map[string]bool, len(overrides))
	for service, enabled := range overrides {
		name := strings.ToLower(strings.TrimSpace(service))
		if name == "" {
			continue
		}
		normalized[name] = enabled
	}
	if len(normalized) == 0 {
		return map[string]bool{}
	}
	return normalized
}

func normalizeProjectCostPolicy(policy domain.ProjectCostPolicy) domain.ProjectCostPolicy {
	if policy.DefaultTTLHours < 0 {
		policy.DefaultTTLHours = 0
	}
	if policy.MaxActiveEnvsPerProject < 0 {
		policy.MaxActiveEnvsPerProject = 0
	}
	if policy.MaxCPUPerEnv < 0 {
		policy.MaxCPUPerEnv = 0
	}
	if policy.MaxMemoryPerEnv < 0 {
		policy.MaxMemoryPerEnv = 0
	}
	if policy.IdleTimeoutHours < 0 {
		policy.IdleTimeoutHours = 0
	}
	return policy
}

func normalizeStringIDs(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		id := normalizeID(value)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	return normalized
}

func normalizeBaseServices(defaultNamespace string, services []domain.BaseServiceRef) []domain.BaseServiceRef {
	defaultNamespace = strings.TrimSpace(defaultNamespace)
	normalized := make([]domain.BaseServiceRef, 0, len(services))
	seen := map[string]struct{}{}
	for _, service := range services {
		name := strings.ToLower(strings.TrimSpace(service.Name))
		if name == "" {
			continue
		}
		namespace := strings.TrimSpace(service.Namespace)
		if namespace == "" {
			namespace = defaultNamespace
		}
		key := name + "\x00" + namespace
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, domain.BaseServiceRef{Name: name, Namespace: namespace})
	}
	return normalized
}
