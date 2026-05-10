package domain

import "time"

type Project struct {
	ID                    string            `json:"id" db:"id"`
	Name                  string            `json:"name" db:"name"`
	ProductID             string            `json:"product_id" db:"product_id"`
	AppRepositoryID       string            `json:"app_repository_id,omitempty" db:"app_repository_id"`
	GitOpsRepositoryID    string            `json:"gitops_repository_id,omitempty" db:"gitops_repository_id"`
	WebhookBranchFilters  []string          `json:"branch_filters,omitempty" db:"branch_filters"`
	WebhookLabels         []string          `json:"labels,omitempty" db:"webhook_labels"`
	WebhookAllowDraftPRs  bool              `json:"allow_draft_prs,omitempty" db:"webhook_allow_draft_prs"`
	GitHubInstallationIDs []string          `json:"github_installation_ids,omitempty" db:"github_installation_ids"`
	GitLabProjectIDs      []string          `json:"gitlab_project_ids,omitempty" db:"gitlab_project_ids"`
	ClusterID             string            `json:"cluster_id,omitempty" db:"cluster_id"`
	AccessUsers           []string          `json:"access_users,omitempty" db:"access_users"`
	AccessOrganizations   []string          `json:"access_organizations,omitempty" db:"access_organizations"`
	SecretRefs            []string          `json:"secret_refs,omitempty" db:"secret_refs"`
	GitRepo               RepositoryRef     `json:"git_repo" db:"git_repo"`
	GitOpsRepo            RepositoryRef     `json:"gitops_repo" db:"gitops_repo"`
	BaseEnvConfig         BaseEnvConfig     `json:"base_env_config" db:"base_env_config"`
	CostPolicy            ProjectCostPolicy `json:"cost_policy,omitempty" db:"cost_policy"`
	CreatedAt             time.Time         `json:"created_at" db:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at" db:"updated_at"`
}

type RepositoryRef struct {
	Provider      string `json:"provider"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch"`
	Path          string `json:"path,omitempty"`
}

type BaseEnvConfig struct {
	EnvironmentID   string            `json:"environment_id"`
	Namespace       string            `json:"namespace"`
	Domain          string            `json:"domain"`
	ConfigPath      string            `json:"config_path"`
	Services        []BaseServiceRef  `json:"services,omitempty"`
	Values          map[string]string `json:"values,omitempty"`
	HybridOverrides map[string]bool   `json:"hybrid_overrides,omitempty"`
}

type BaseServiceRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type ProjectCostPolicy struct {
	DefaultTTLHours         int   `json:"default_ttl_hours,omitempty"`
	MaxActiveEnvsPerProject int   `json:"max_active_envs_per_project,omitempty"`
	MaxCPUPerEnv            int   `json:"max_cpu_per_env,omitempty"`
	MaxMemoryPerEnv         int   `json:"max_memory_per_env,omitempty"`
	IdleTimeoutHours        int   `json:"idle_timeout_hours,omitempty"`
	AutoDeleteIdleEnvs      *bool `json:"auto_delete_idle_envs,omitempty"`
}
