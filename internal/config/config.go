package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/envpilot/runner/internal/gitops"
)

type Config struct {
	// EnvDiagnostics contains variable names only; values are never retained.
	EnvDiagnostics               []string
	Addr                         string
	DataDir                      string
	DatabaseURL                  string
	RedisURL                     string
	PostgresMigrationsDir        string
	GitOpsDir                    string
	EnableGitCommit              bool
	EnableGitPush                bool
	GitPushRemote                string
	GitPushBranch                string
	GitAuthorName                string
	GitAuthorEmail               string
	GitHubToken                  string
	GitHubAPI                    string
	GitLabToken                  string
	GitLabAPI                    string
	CatalogPath                  string
	WebhookSecret                string
	GitLabWebhookSecret          string
	GitHubWebhookSecret          string
	GitHubWebhookDebugPayloadLog bool
	TTLCheckInterval             time.Duration
	JobRetryDelay                time.Duration
	JobMaxAttempts               int
	DefaultTTL                   time.Duration
	IdleThreshold                time.Duration
	DefaultDomainRoot            string
	GitOps                       gitops.FluxOptions
	DeploymentBackend            string
	APIReadToken                 string
	APIWriteToken                string
	APITokenRoles                map[string]string
	OAuthSessionSecret           string
	GitHubOAuthClientID          string
	GitHubOAuthSecret            string
	GitHubOAuthAuthURL           string
	GitHubOAuthTokenURL          string
	GitHubOAuthUserURL           string
	GitLabOAuthClientID          string
	GitLabOAuthSecret            string
	GitLabOAuthAuthURL           string
	GitLabOAuthTokenURL          string
	GitLabOAuthUserURL           string
	OIDCOAuthClientID            string
	OIDCOAuthSecret              string
	OIDCOAuthAuthURL             string
	OIDCOAuthTokenURL            string
	OIDCOAuthUserURL             string
	OIDCOAuthScopes              string
	FrontendURL                  string
	AuditLogPath                 string
	CredentialEncryptionKey      string
	AgentRegistrationTokenTTL    time.Duration
	PendingRegistrationTokenTTL  time.Duration
	PendingRegistrationTokenMax  int
	RateLimitRequests            int
	RateLimitWindow              time.Duration
	BootstrapRateLimitRequests   int
	BootstrapRateLimitWindow     time.Duration
	DependencyWaitTimeout        time.Duration
	DependencyWaitInterval       time.Duration
	AllowUnauthenticatedAgents   bool
	CleanupProtectedNamespaces   []string
	CleanupRequireEnvPlaneLabels bool
}

type runtimeConfigFile struct {
	WebhookSecret                      *string `json:"webhook_secret"`
	GitLabWebhookSecret                *string `json:"gitlab_webhook_secret"`
	GitHubWebhookSecret                *string `json:"github_webhook_secret"`
	GitHubWebhookDebugPayloadLog       *bool   `json:"github_webhook_debug_payload_log"`
	APIReadToken                       *string `json:"api_read_token"`
	APIWriteToken                      *string `json:"api_write_token"`
	APITokenRoles                      *string `json:"api_token_roles"`
	OAuthSessionSecret                 *string `json:"oauth_session_secret"`
	GitHubOAuthClientID                *string `json:"github_oauth_client_id"`
	GitHubOAuthSecret                  *string `json:"github_oauth_secret"`
	GitLabOAuthClientID                *string `json:"gitlab_oauth_client_id"`
	GitLabOAuthSecret                  *string `json:"gitlab_oauth_secret"`
	OIDCOAuthClientID                  *string `json:"oidc_oauth_client_id"`
	OIDCOAuthSecret                    *string `json:"oidc_oauth_secret"`
	OIDCOAuthAuthURL                   *string `json:"oidc_oauth_auth_url"`
	OIDCOAuthTokenURL                  *string `json:"oidc_oauth_token_url"`
	OIDCOAuthUserURL                   *string `json:"oidc_oauth_user_url"`
	OIDCOAuthScopes                    *string `json:"oidc_oauth_scopes"`
	FrontendURL                        *string `json:"frontend_url"`
	AuditLogPath                       *string `json:"audit_log_path"`
	CredentialEncryptionKey            *string `json:"credential_encryption_key"`
	AgentRegistrationTokenTTLSeconds   *int    `json:"agent_registration_token_ttl_seconds"`
	PendingRegistrationTokenTTLSeconds *int    `json:"pending_registration_token_ttl_seconds"`
	PendingRegistrationTokenMax        *int    `json:"pending_registration_token_max"`
	RateLimitRequests                  *int    `json:"rate_limit_requests"`
	RateLimitSeconds                   *int    `json:"rate_limit_seconds"`
	RateLimitWindowSecond              *int    `json:"rate_limit_window_seconds"`
	BootstrapRateLimitRequests         *int    `json:"bootstrap_rate_limit_requests"`
	BootstrapRateLimitSeconds          *int    `json:"bootstrap_rate_limit_seconds"`
	AllowUnauthenticatedAgents         *bool   `json:"allow_unauthenticated_agents"`
}

func FromEnv() Config {
	dataDir := getenv("ENVPILOT_DATA_DIR", ".envpilot")
	domainRoot := getenv("ENVPILOT_DOMAIN_ROOT", "feature.int")

	cfg := Config{
		Addr:                         getenv("ENVPILOT_ADDR", ":8080"),
		DataDir:                      dataDir,
		DatabaseURL:                  getenv("ENVPILOT_DATABASE_URL", ""),
		RedisURL:                     getenv("ENVPILOT_REDIS_URL", ""),
		PostgresMigrationsDir:        getenv("ENVPILOT_POSTGRES_MIGRATIONS_DIR", "migrations/postgres"),
		GitOpsDir:                    getenv("ENVPILOT_GITOPS_DIR", filepath.Join(dataDir, "gitops")),
		EnableGitCommit:              getenvBool("ENVPILOT_ENABLE_GIT_COMMIT", false),
		EnableGitPush:                getenvBool("ENVPILOT_ENABLE_GIT_PUSH", false),
		GitPushRemote:                getenv("ENVPILOT_GIT_PUSH_REMOTE", "origin"),
		GitPushBranch:                getenv("ENVPILOT_GIT_PUSH_BRANCH", "main"),
		GitAuthorName:                getenv("ENVPILOT_GIT_AUTHOR_NAME", "envpilot"),
		GitAuthorEmail:               getenv("ENVPILOT_GIT_AUTHOR_EMAIL", "envpilot@example.com"),
		GitHubToken:                  getenv("ENVPILOT_GITHUB_TOKEN", ""),
		GitHubAPI:                    getenv("ENVPILOT_GITHUB_API", "https://api.github.com"),
		GitLabToken:                  getenv("ENVPILOT_GITLAB_TOKEN", ""),
		GitLabAPI:                    getenv("ENVPILOT_GITLAB_API", "https://gitlab.com/api/v4"),
		CatalogPath:                  getenv("ENVPILOT_CATALOG_PATH", ""),
		WebhookSecret:                getenv("ENVPILOT_WEBHOOK_SECRET", ""),
		GitLabWebhookSecret:          getenv("ENVPILOT_GITLAB_WEBHOOK_SECRET", getenv("ENVPILOT_WEBHOOK_SECRET", "")),
		GitHubWebhookSecret:          getenv("ENVPILOT_GITHUB_WEBHOOK_SECRET", getenv("ENVPILOT_WEBHOOK_SECRET", "")),
		GitHubWebhookDebugPayloadLog: getenvBool("ENVPILOT_GITHUB_WEBHOOK_DEBUG_PAYLOAD_LOG", false),
		APIReadToken:                 getenv("ENVPILOT_API_READ_TOKEN", ""),
		APIWriteToken:                getenv("ENVPILOT_API_WRITE_TOKEN", getenv("ENVPILOT_AGENT_TOKEN", "")),
		APITokenRoles:                map[string]string{},
		OAuthSessionSecret:           getenv("ENVPILOT_OAUTH_SESSION_SECRET", ""),
		GitHubOAuthClientID:          getenv("ENVPILOT_GITHUB_OAUTH_CLIENT_ID", ""),
		GitHubOAuthSecret:            getenv("ENVPILOT_GITHUB_OAUTH_CLIENT_SECRET", ""),
		GitHubOAuthAuthURL:           getenv("ENVPILOT_GITHUB_OAUTH_AUTH_URL", "https://github.com/login/oauth/authorize"),
		GitHubOAuthTokenURL:          getenv("ENVPILOT_GITHUB_OAUTH_TOKEN_URL", "https://github.com/login/oauth/access_token"),
		GitHubOAuthUserURL:           getenv("ENVPILOT_GITHUB_OAUTH_USER_URL", "https://api.github.com/user"),
		GitLabOAuthClientID:          getenv("ENVPILOT_GITLAB_OAUTH_CLIENT_ID", ""),
		GitLabOAuthSecret:            getenv("ENVPILOT_GITLAB_OAUTH_CLIENT_SECRET", ""),
		GitLabOAuthAuthURL:           getenv("ENVPILOT_GITLAB_OAUTH_AUTH_URL", "https://gitlab.com/oauth/authorize"),
		GitLabOAuthTokenURL:          getenv("ENVPILOT_GITLAB_OAUTH_TOKEN_URL", "https://gitlab.com/oauth/token"),
		GitLabOAuthUserURL:           getenv("ENVPILOT_GITLAB_OAUTH_USER_URL", "https://gitlab.com/api/v4/user"),
		OIDCOAuthClientID:            getenv("ENVPILOT_OIDC_CLIENT_ID", ""),
		OIDCOAuthSecret:              getenv("ENVPILOT_OIDC_CLIENT_SECRET", ""),
		OIDCOAuthAuthURL:             getenv("ENVPILOT_OIDC_AUTH_URL", ""),
		OIDCOAuthTokenURL:            getenv("ENVPILOT_OIDC_TOKEN_URL", ""),
		OIDCOAuthUserURL:             getenv("ENVPILOT_OIDC_USER_URL", ""),
		OIDCOAuthScopes:              getenv("ENVPILOT_OIDC_SCOPES", ""),
		FrontendURL:                  getenv("ENVPILOT_FRONTEND_URL", "/"),
		AuditLogPath:                 getenv("ENVPILOT_AUDIT_LOG_PATH", ""),
		CredentialEncryptionKey:      getenv("ENVPILOT_CREDENTIAL_ENCRYPTION_KEY", "envpilot-local-development-key"),
		AgentRegistrationTokenTTL:    time.Duration(getenvInt("ENVPILOT_AGENT_REGISTRATION_TOKEN_TTL_SECONDS", 900)) * time.Second,
		PendingRegistrationTokenTTL:  time.Duration(getenvInt("ENVPILOT_PENDING_REGISTRATION_TOKEN_TTL_SECONDS", 60)) * time.Second,
		PendingRegistrationTokenMax:  getenvInt("ENVPILOT_PENDING_REGISTRATION_TOKEN_MAX", 1024),
		RateLimitRequests:            getenvInt("ENVPILOT_RATE_LIMIT_REQUESTS", 0),
		RateLimitWindow:              time.Duration(getenvInt("ENVPILOT_RATE_LIMIT_SECONDS", 60)) * time.Second,
		BootstrapRateLimitRequests:   getenvInt("ENVPILOT_BOOTSTRAP_RATE_LIMIT_REQUESTS", 20),
		BootstrapRateLimitWindow:     time.Duration(getenvInt("ENVPILOT_BOOTSTRAP_RATE_LIMIT_SECONDS", 60)) * time.Second,
		DependencyWaitTimeout:        time.Duration(getenvInt("ENVPILOT_DEPENDENCY_WAIT_TIMEOUT_SECONDS", 120)) * time.Second,
		DependencyWaitInterval:       time.Duration(getenvInt("ENVPILOT_DEPENDENCY_WAIT_INTERVAL_SECONDS", 2)) * time.Second,
		AllowUnauthenticatedAgents:   getenvBool("ENVPILOT_ALLOW_UNAUTHENTICATED_AGENTS", false),
		CleanupProtectedNamespaces:   splitCSV(getenv("ENVPILOT_CLEANUP_PROTECTED_NAMESPACES", "default,kube-system,kube-public,kube-node-lease,flux-system,cert-manager")),
		CleanupRequireEnvPlaneLabels: getenvBool("ENVPILOT_CLEANUP_REQUIRE_ENVPILOT_LABELS", true),
		TTLCheckInterval:             time.Duration(getenvInt("ENVPILOT_TTL_CHECK_SECONDS", 60)) * time.Second,
		JobRetryDelay:                time.Duration(getenvInt("ENVPILOT_JOB_RETRY_SECONDS", 5)) * time.Second,
		JobMaxAttempts:               getenvInt("ENVPILOT_JOB_MAX_ATTEMPTS", 3),
		DefaultTTL:                   time.Duration(getenvInt("ENVPILOT_DEFAULT_TTL_HOURS", 48)) * time.Hour,
		IdleThreshold:                time.Duration(getenvInt("ENVPILOT_IDLE_THRESHOLD_HOURS", 0)) * time.Hour,
		DefaultDomainRoot:            domainRoot,
		GitOps: gitops.FluxOptions{
			FluxNamespace:   getenv("ENVPILOT_FLUX_NAMESPACE", "flux-system"),
			SourceRefName:   getenv("ENVPILOT_SOURCE_REF_NAME", "apps"),
			DependsOnName:   getenv("ENVPILOT_DEPENDS_ON", "flux.automation"),
			ProductBasePath: getenv("ENVPILOT_PRODUCT_BASE_PATH", "common/apps"),
			HealthCheckName: getenv("ENVPILOT_HEALTHCHECK_HELMRELEASE", "app"),
			AppChartVersion: getenv("ENVPILOT_APP_CHART_VERSION", "${appChartVersion}"),
			InfraVersion:    getenv("ENVPILOT_INFRA_CHART_VERSION", "${infraChartVersion}"),
			NginxVersion:    getenv("ENVPILOT_NGINX_CHART_VERSION", "${nginxChartVersion}"),
		},
		DeploymentBackend: DeploymentBackendFromEnv(),
	}
	cfg.APITokenRoles = parseAPITokenRoleBindings(
		getenv("ENVPILOT_API_TOKEN_ROLES", ""),
		cfg.APIReadToken,
		cfg.APIWriteToken,
	)
	cfg.EnvDiagnostics = legacyDiagnostics()
	return cfg
}

func DeploymentBackendFromEnv() string {
	raw := strings.ToLower(strings.TrimSpace(getenv("ENVPILOT_DEPLOYMENT_BACKEND", "helm_direct")))
	switch raw {
	case "gitops_manifest", "fluxcd", "helm_direct":
		return raw
	case "flux_cd":
		return "fluxcd"
	case "flux":
		return "fluxcd"
	case "helm-direct":
		return "helm_direct"
	case "gitops":
		return "gitops_manifest"
	case "":
		return "helm_direct"
	default:
		return raw
	}
}

func FromEnvWithRuntimeFile(path string) (Config, error) {
	cfg := FromEnv()
	return applyRuntimeConfigFile(cfg, path)
}

func (c Config) StorePath() string {
	return filepath.Join(c.DataDir, "environments.json")
}

func (c Config) ProjectStorePath() string {
	return filepath.Join(c.DataDir, "projects.json")
}

func (c Config) ProductStorePath() string {
	return filepath.Join(c.DataDir, "products.json")
}

func (c Config) SettingsStorePath() string {
	return filepath.Join(c.DataDir, "settings.json")
}

func (c Config) BootstrapSessionStorePath() string {
	return filepath.Join(c.DataDir, "bootstrap_sessions.json")
}

func (c Config) ProjectConfigStorePath() string {
	return filepath.Join(c.DataDir, "project_config_versions.json")
}

func parseAPITokenRoleBindings(raw, readToken, writeToken string) map[string]string {
	roles := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		rawEntry := strings.TrimSpace(entry)
		if rawEntry == "" {
			continue
		}
		parts := strings.SplitN(rawEntry, ":", 2)
		if len(parts) != 2 {
			continue
		}
		token := strings.TrimSpace(parts[0])
		role := normalizeAPITokenRole(parts[1])
		if token == "" || role == "" {
			continue
		}
		roles[token] = role
	}

	readToken = strings.TrimSpace(readToken)
	if readToken != "" {
		roles[readToken] = APITokenRoleReader
	}
	writeToken = strings.TrimSpace(writeToken)
	if writeToken != "" {
		roles[writeToken] = APITokenRoleAdmin
	}
	return roles
}

func normalizeAPITokenRole(rawRole string) string {
	switch strings.ToLower(strings.TrimSpace(rawRole)) {
	case "admin", "write", "writer", "maintain", "maintainer":
		return APITokenRoleAdmin
	case "read", "read-only", "readonly", "reader", "view", "viewer":
		return APITokenRoleReader
	default:
		return ""
	}
}

const (
	APITokenRoleAdmin  = "admin"
	APITokenRoleReader = "reader"
)

func getenv(key, fallback string) string {
	if strings.HasPrefix(key, "ENVPILOT_") {
		canonical := "ENVPLANE_" + strings.TrimPrefix(key, "ENVPILOT_")
		if value, set := os.LookupEnv(canonical); set {
			return strings.TrimSpace(value)
		}
	}
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	value := getenv(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvInt(key string, fallback int) int {
	value := getenv(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func legacyDiagnostics() []string {
	seen := map[string]bool{}
	result := []string{}
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, "ENVPILOT_") {
			continue
		}
		item := "deprecated:" + name
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func applyRuntimeConfigFile(cfg Config, path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return cfg, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	var fileCfg runtimeConfigFile
	if err := json.Unmarshal(raw, &fileCfg); err != nil {
		return cfg, err
	}
	if fileCfg.WebhookSecret != nil {
		cfg.WebhookSecret = strings.TrimSpace(*fileCfg.WebhookSecret)
	}
	if fileCfg.GitLabWebhookSecret != nil {
		cfg.GitLabWebhookSecret = strings.TrimSpace(*fileCfg.GitLabWebhookSecret)
	}
	if fileCfg.GitHubWebhookSecret != nil {
		cfg.GitHubWebhookSecret = strings.TrimSpace(*fileCfg.GitHubWebhookSecret)
	}
	if fileCfg.GitHubWebhookDebugPayloadLog != nil {
		cfg.GitHubWebhookDebugPayloadLog = *fileCfg.GitHubWebhookDebugPayloadLog
	}
	if fileCfg.WebhookSecret != nil {
		cfg.GitLabWebhookSecret = cfg.WebhookSecret
		cfg.GitHubWebhookSecret = cfg.WebhookSecret
	}
	if fileCfg.APIReadToken != nil {
		cfg.APIReadToken = strings.TrimSpace(*fileCfg.APIReadToken)
	}
	if fileCfg.APIWriteToken != nil {
		cfg.APIWriteToken = strings.TrimSpace(*fileCfg.APIWriteToken)
	}
	if fileCfg.OAuthSessionSecret != nil {
		cfg.OAuthSessionSecret = strings.TrimSpace(*fileCfg.OAuthSessionSecret)
	}
	if fileCfg.GitHubOAuthClientID != nil {
		cfg.GitHubOAuthClientID = strings.TrimSpace(*fileCfg.GitHubOAuthClientID)
	}
	if fileCfg.GitHubOAuthSecret != nil {
		cfg.GitHubOAuthSecret = strings.TrimSpace(*fileCfg.GitHubOAuthSecret)
	}
	if fileCfg.GitLabOAuthClientID != nil {
		cfg.GitLabOAuthClientID = strings.TrimSpace(*fileCfg.GitLabOAuthClientID)
	}
	if fileCfg.GitLabOAuthSecret != nil {
		cfg.GitLabOAuthSecret = strings.TrimSpace(*fileCfg.GitLabOAuthSecret)
	}
	if fileCfg.OIDCOAuthClientID != nil {
		cfg.OIDCOAuthClientID = strings.TrimSpace(*fileCfg.OIDCOAuthClientID)
	}
	if fileCfg.OIDCOAuthSecret != nil {
		cfg.OIDCOAuthSecret = strings.TrimSpace(*fileCfg.OIDCOAuthSecret)
	}
	if fileCfg.OIDCOAuthAuthURL != nil {
		cfg.OIDCOAuthAuthURL = strings.TrimSpace(*fileCfg.OIDCOAuthAuthURL)
	}
	if fileCfg.OIDCOAuthTokenURL != nil {
		cfg.OIDCOAuthTokenURL = strings.TrimSpace(*fileCfg.OIDCOAuthTokenURL)
	}
	if fileCfg.OIDCOAuthUserURL != nil {
		cfg.OIDCOAuthUserURL = strings.TrimSpace(*fileCfg.OIDCOAuthUserURL)
	}
	if fileCfg.OIDCOAuthScopes != nil {
		cfg.OIDCOAuthScopes = strings.TrimSpace(*fileCfg.OIDCOAuthScopes)
	}
	if fileCfg.FrontendURL != nil {
		cfg.FrontendURL = strings.TrimSpace(*fileCfg.FrontendURL)
	}
	if fileCfg.AuditLogPath != nil {
		cfg.AuditLogPath = strings.TrimSpace(*fileCfg.AuditLogPath)
	}
	if fileCfg.CredentialEncryptionKey != nil {
		cfg.CredentialEncryptionKey = strings.TrimSpace(*fileCfg.CredentialEncryptionKey)
	}
	if fileCfg.AgentRegistrationTokenTTLSeconds != nil && *fileCfg.AgentRegistrationTokenTTLSeconds > 0 {
		cfg.AgentRegistrationTokenTTL = time.Duration(*fileCfg.AgentRegistrationTokenTTLSeconds) * time.Second
	}
	if fileCfg.PendingRegistrationTokenTTLSeconds != nil && *fileCfg.PendingRegistrationTokenTTLSeconds > 0 {
		cfg.PendingRegistrationTokenTTL = time.Duration(*fileCfg.PendingRegistrationTokenTTLSeconds) * time.Second
	}
	if fileCfg.PendingRegistrationTokenMax != nil && *fileCfg.PendingRegistrationTokenMax > 0 {
		cfg.PendingRegistrationTokenMax = *fileCfg.PendingRegistrationTokenMax
	}
	if fileCfg.RateLimitRequests != nil {
		cfg.RateLimitRequests = *fileCfg.RateLimitRequests
	}
	if fileCfg.RateLimitWindowSecond != nil {
		cfg.RateLimitWindow = time.Duration(*fileCfg.RateLimitWindowSecond) * time.Second
	}
	if fileCfg.RateLimitSeconds != nil {
		cfg.RateLimitWindow = time.Duration(*fileCfg.RateLimitSeconds) * time.Second
	}
	if fileCfg.BootstrapRateLimitRequests != nil {
		cfg.BootstrapRateLimitRequests = *fileCfg.BootstrapRateLimitRequests
	}
	if fileCfg.BootstrapRateLimitSeconds != nil {
		cfg.BootstrapRateLimitWindow = time.Duration(*fileCfg.BootstrapRateLimitSeconds) * time.Second
	}
	if fileCfg.AllowUnauthenticatedAgents != nil {
		cfg.AllowUnauthenticatedAgents = *fileCfg.AllowUnauthenticatedAgents
	}

	rawRoles := getenv("ENVPILOT_API_TOKEN_ROLES", "")
	if fileCfg.APITokenRoles != nil {
		rawRoles = strings.TrimSpace(*fileCfg.APITokenRoles)
	}
	cfg.APITokenRoles = parseAPITokenRoleBindings(
		rawRoles,
		cfg.APIReadToken,
		cfg.APIWriteToken,
	)
	return cfg, nil
}
