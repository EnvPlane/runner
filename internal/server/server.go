package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"envpilot/internal/app"
	"envpilot/internal/bootstrap"
	"envpilot/internal/config"
	"envpilot/internal/domain"
	"envpilot/internal/jobs"
	"envpilot/internal/scm"
	"envpilot/internal/secrets"
	"envpilot/internal/store"
)

//go:embed openapi.json
var openAPIFiles embed.FS

type Dependencies struct {
	Config            config.Config
	Service           *app.EnvironmentService
	Products          *app.ProductService
	Projects          *app.ProjectService
	Settings          *app.SettingsService
	BootstrapSessions *app.BootstrapSessionService
	ProjectConfigs    *app.ProjectConfigService
	SCMValidation     *app.SCMValidationService
	Jobs              *jobs.Manager
	Logger            *slog.Logger
}

type Server struct {
	cfg                           atomic.Value
	service                       *app.EnvironmentService
	products                      *app.ProductService
	projects                      *app.ProjectService
	settings                      *app.SettingsService
	bootstrapSessions             *app.BootstrapSessionService
	projectConfigs                *app.ProjectConfigService
	scmValidation                 *app.SCMValidationService
	jobs                          *jobs.Manager
	logger                        *slog.Logger
	rateMu                        sync.Mutex
	rateMap                       map[string]*rateLimitState
	auditMu                       sync.Mutex
	metricMu                      sync.Mutex
	runnerDeploymentInstructionMu sync.Map
	runnerRegistrationMu          sync.Map
	agentRegistrationMu           sync.Map
	pendingAgentRegistrationMu    sync.Mutex
	pendingAgentRegistration      map[string]pendingRegistrationToken
	metrics                       *serverMetrics
	traceID                       uint64
}

var requestDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type apiEnvironmentResponse struct {
	domain.Environment
	DeploymentBackend string                          `json:"deploymentBackend"`
	DeploymentStatus  *apiEnvironmentDeploymentStatus `json:"deploymentStatus,omitempty"`
}

type apiEnvironmentDeploymentStatus struct {
	Backend string                    `json:"backend"`
	Flux    *domain.FluxStatus        `json:"fluxStatus,omitempty"`
	Helm    *apiEnvironmentHelmStatus `json:"helmDirectStatus,omitempty"`
}

type apiEnvironmentHelmStatus struct {
	Status string `json:"status"`
	Ready  bool   `json:"ready"`
}

var ErrBootstrapIdentityMismatch = store.ErrBootstrapIdentityMismatch

type pendingRegistrationToken struct {
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

const (
	auditActorAnonymous                        = "anonymous"
	auditActorToken                            = "api-token"
	auditActorUser                             = "user"
	auditEventAPIRequest                       = "api_request"
	auditEventAgentHeartbeatAuthFailed         = "agent_heartbeat_auth_failed"
	auditEventAgentHeartbeatSucceeded          = "agent_heartbeat_succeeded"
	auditEventAgentRegistrationAuthFailed      = "agent_registration_auth_failed"
	auditEventAgentRegistrationSucceeded       = "agent_registration_succeeded"
	auditEventBootstrapRateLimitHit            = "bootstrap_rate_limit_hit"
	auditEventBootstrapSCMCredentialsSaved     = "bootstrap_scm_credentials_saved"
	auditEventBootstrapSecretStrategiesSaved   = "bootstrap_secret_strategies_saved"
	auditEventBootstrapTokenValidationFail     = "bootstrap_token_validation_failed"
	auditEventProjectConfigSaved               = "project_config_saved"
	auditEventRunnerConfigFetchAuthFailed      = "runner_config_fetch_auth_failed"
	auditEventRunnerConfigFetchSucceeded       = "runner_config_fetch_succeeded"
	auditEventRunnerBootstrapTokenGenerated    = "runner_bootstrap_token_generated"
	auditEventRunnerHeartbeatAuthFailed        = "runner_heartbeat_auth_failed"
	auditEventRunnerHeartbeatSucceeded         = "runner_heartbeat_succeeded"
	auditEventRunnerRegistrationAuthFailed     = "runner_registration_auth_failed"
	auditEventRunnerRegistrationSucceeded      = "runner_registration_succeeded"
	auditEventWebhookAuthFailed                = "webhook_auth_failed"
	auditEventWebhookAuthSucceeded             = "webhook_auth_succeeded"
	auditEndpointAgentHeartbeat                = "agent_heartbeat"
	auditEndpointAgentRegistration             = "agent_registration"
	auditEndpointAgentResourceScanIngest       = "agent_resource_scan_ingest"
	auditEndpointAgentResourceScanNext         = "agent_resource_scan_next"
	auditEndpointBootstrapSessionCompile       = "bootstrap_session_compile"
	auditEndpointBootstrapSessionUpdate        = "bootstrap_session_update"
	auditEndpointGitHubWebhook                 = "github_webhook"
	auditEndpointGitLabWebhook                 = "gitlab_webhook"
	auditEndpointRunnerConfigFetch             = "runner_config_fetch"
	auditEndpointRunnerDeploymentInstructions  = "runner_deployment_instructions"
	auditEndpointRunnerHeartbeat               = "runner_heartbeat"
	auditEndpointRunnerRegistration            = "runner_registration"
	auditSubjectAgentID                        = "agent_id"
	auditSubjectRunnerID                       = "runner_id"
	traceParentVersion                         = "00"
	traceFlagsSampled                          = "01"
	bootstrapAgentTokenHashKey                 = "agentRegistrationTokenHash"
	bootstrapAgentTokenIssuedAtKey             = "agentRegistrationTokenIssuedAt"
	bootstrapAgentTokenExpiresAtKey            = "agentRegistrationTokenExpiresAt"
	bootstrapAgentTokenUsedAtKey               = "agentRegistrationTokenUsedAt"
	bootstrapAgentTokenProjectKey              = "agentRegistrationTokenProjectID"
	bootstrapAgentAuthTokenHashKey             = "agentAuthTokenHash"
	bootstrapAgentAuthTokenIssuedAtKey         = "agentAuthTokenIssuedAt"
	bootstrapAgentAuthTokenProjectKey          = "agentAuthTokenProjectID"
	bootstrapAgentStatusKey                    = "agentConnectionStatus"
	bootstrapAgentClusterIDKey                 = "agentClusterID"
	bootstrapAgentIDKey                        = "agentID"
	bootstrapAgentLastSeenAtKey                = "agentLastSeenAt"
	bootstrapAgentErrorKey                     = "agentConnectionError"
	bootstrapRunnerTokenHashKey                = "runnerRegistrationTokenHash"
	bootstrapRunnerTokenIssuedAtKey            = "runnerRegistrationTokenIssuedAt"
	bootstrapRunnerTokenExpiresAtKey           = "runnerRegistrationTokenExpiresAt"
	bootstrapRunnerTokenUsedAtKey              = "runnerRegistrationTokenUsedAt"
	bootstrapRunnerTokenProjectKey             = "runnerRegistrationTokenProjectID"
	bootstrapRunnerAuthTokenHashKey            = "runnerAuthTokenHash"
	bootstrapRunnerAuthTokenIssuedAtKey        = "runnerAuthTokenIssuedAt"
	bootstrapRunnerAuthTokenProjectKey         = "runnerAuthTokenProjectID"
	bootstrapRunnerConfigTokenHashKey          = "runnerConfigTokenHash"
	bootstrapRunnerConfigTokenIssuedAtKey      = "runnerConfigTokenIssuedAt"
	bootstrapRunnerConfigTokenExpiresAtKey     = "runnerConfigTokenExpiresAt"
	bootstrapRunnerConfigTokenUsedAtKey        = "runnerConfigTokenUsedAt"
	bootstrapRunnerConfigTokenProjectKey       = "runnerConfigTokenProjectID"
	bootstrapRunnerStatusKey                   = "runnerConnectionStatus"
	bootstrapRunnerClusterIDKey                = "runnerClusterID"
	bootstrapRunnerIDKey                       = "runnerID"
	bootstrapRunnerModeKey                     = "runnerDeploymentMode"
	bootstrapRunnerNamespaceKey                = "runnerNamespace"
	bootstrapRunnerReleaseNameKey              = "runnerReleaseName"
	bootstrapRunnerSecretCommandDisplayedAtKey = "runnerBootstrapSecretCommandDisplayedAt"
	bootstrapRunnerLastSeenAtKey               = "runnerLastSeenAt"
	bootstrapRunnerErrorKey                    = "runnerConnectionError"
	bootstrapCapabilityReportKey               = "clusterCapabilityReport"
	bootstrapSelectedNamespacesKey             = "selectedBaseNamespaces"
	bootstrapResourceScanStatusKey             = "resourceScanStatus"
	bootstrapResourceScanStartedAtKey          = "resourceScanStartedAt"
	bootstrapResourceScanCompletedAtKey        = "resourceScanCompletedAt"
	bootstrapResourceScanReportKey             = "resourceScanReport"
	bootstrapServiceGraphKey                   = "serviceGraph"
	bootstrapServiceEnvsKey                    = "serviceEnvs"
	bootstrapResourceScanTaskKey               = "resourceScanTask"
	bootstrapManifestTemplatesKey              = "manifestTemplates"
	bootstrapDeploymentConfigKey               = "deployment"
)

type serverMetrics struct {
	totalRequests          int64
	inFlightRequests       int64
	requestsByMethod       map[string]int64
	requestsByPath         map[string]int64
	requestsByStatus       map[int]int64
	requestDurationSum     float64
	requestDurationCount   int64
	requestDurationBuckets map[float64]int64
	requestDurationInf     int64
	requests4xxTotal       int64
	requests5xxTotal       int64
	pendingTokenEvictions  int64
}

type rateLimitState struct {
	windowStart time.Time
	count       int
}

func New(deps Dependencies) *Server {
	var cfgValue atomic.Value
	cfgValue.Store(deps.Config)
	if deps.Config.GitHubWebhookDebugPayloadLog && deps.Logger != nil {
		deps.Logger.Warn(
			"github webhook full payload debug logging is enabled; webhook payloads can expose repository metadata and user content",
			"config", "ENVPILOT_GITHUB_WEBHOOK_DEBUG_PAYLOAD_LOG",
		)
	}

	return &Server{
		cfg:                      cfgValue,
		service:                  deps.Service,
		products:                 deps.Products,
		projects:                 deps.Projects,
		settings:                 deps.Settings,
		bootstrapSessions:        deps.BootstrapSessions,
		projectConfigs:           deps.ProjectConfigs,
		scmValidation:            deps.SCMValidation,
		jobs:                     deps.Jobs,
		logger:                   deps.Logger,
		rateMap:                  make(map[string]*rateLimitState),
		pendingAgentRegistration: make(map[string]pendingRegistrationToken),
		metrics: &serverMetrics{
			requestsByMethod:       make(map[string]int64),
			requestsByPath:         make(map[string]int64),
			requestsByStatus:       make(map[int]int64),
			requestDurationBuckets: make(map[float64]int64),
		},
	}
}

func (s *Server) ReloadConfig(cfg config.Config) {
	s.cfg.Store(cfg)
}

func (s *Server) config() config.Config {
	v, ok := s.cfg.Load().(config.Config)
	if !ok {
		return config.Config{}
	}
	return v
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/dashboard/summary", s.getDashboardSummary)
	mux.HandleFunc("GET /api/v1/products", s.listProducts)
	mux.HandleFunc("POST /api/v1/products/validate", s.validateProduct)
	mux.HandleFunc("PUT /api/v1/products/{name}", s.saveProduct)
	mux.HandleFunc("GET /api/v1/products/{name}", s.getProduct)
	mux.HandleFunc("DELETE /api/v1/products/{name}", s.deleteProduct)
	mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("PUT /api/v1/projects/{id}", s.saveProject)
	mux.HandleFunc("PATCH /api/projects/{id}", s.patchProject)
	mux.HandleFunc("GET /api/v1/projects/{id}", s.getProject)
	mux.HandleFunc("GET /api/projects/{id}", s.getProject)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session/validate-scm", s.validateProjectBootstrapSCMConfig)
	mux.HandleFunc("GET /api/projects/{id}/environments", s.listProjectEnvironments)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session", s.createProjectBootstrapSession)
	mux.HandleFunc("GET /api/projects/{id}/bootstrap-session", s.getProjectBootstrapSession)
	mux.HandleFunc("GET /api/projects/{id}/config", s.getProjectConfig)
	mux.HandleFunc("GET /api/projects/{id}/bootstrap-session/manifest-templates", s.getProjectBootstrapManifestTemplates)
	mux.HandleFunc("PATCH /api/projects/{id}/bootstrap-session", s.updateProjectBootstrapSession)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session/compile", s.compileProjectBootstrapSession)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session/simulate-pr", s.simulateBootstrapSession)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session/agent-token", s.createBootstrapAgentToken)
	mux.HandleFunc("GET /api/projects/{id}/bootstrap-session/agent-status", s.getBootstrapAgentStatus)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session/runner-deployment-instructions", s.generateBootstrapRunnerDeploymentInstructions)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session/runner-deploy", s.generateBootstrapRunnerDeploymentInstructions)
	mux.HandleFunc("GET /api/projects/{id}/bootstrap-session/runner-status", s.getBootstrapRunnerStatus)
	mux.HandleFunc("POST /api/projects/{id}/bootstrap-session/resource-scan/start", s.startBootstrapResourceScan)
	mux.HandleFunc("POST /api/v1/projects/{id}/runner-config", s.getBootstrapRunnerConfig)
	mux.HandleFunc("PUT /api/v1/projects/{id}/hybrid-config", s.saveProjectHybridConfig)
	mux.HandleFunc("GET /api/projects/{id}/hybrid-config", s.getProjectHybridConfig)
	mux.HandleFunc("PATCH /api/projects/{id}/hybrid-config", s.saveProjectHybridConfig)
	mux.HandleFunc("GET /api/projects/{id}/cost-policy", s.getProjectCostPolicy)
	mux.HandleFunc("PATCH /api/projects/{id}/cost-policy", s.saveProjectCostPolicy)
	mux.HandleFunc("GET /api/projects/{id}/runtime-bundle", s.getProjectRuntimeBundle)
	mux.HandleFunc("GET /api/v1/settings", s.getSettings)
	mux.HandleFunc("PUT /api/v1/settings", s.saveSettings)
	mux.HandleFunc("POST /api/v1/settings/secret-refs/{id}/validate", s.validateSecretRef)
	mux.HandleFunc("GET /api/v1/settings/clusters/{id}/health", s.getClusterHealth)
	mux.HandleFunc("POST /api/v1/agents/register", s.registerAgent)
	mux.HandleFunc("POST /api/v1/agents/heartbeat", s.agentHeartbeat)
	mux.HandleFunc("POST /api/v1/agents/resource-scan", s.ingestAgentResourceScan)
	mux.HandleFunc("GET /api/v1/agents/resource-scan/next", s.nextAgentResourceScan)
	mux.HandleFunc("GET /api/v1/jobs", s.listJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /api/v1/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("GET /api/v1/environments", s.listEnvironments)
	mux.HandleFunc("GET /api/environments", s.listEnvironments)
	mux.HandleFunc("GET /api/v1/environment-records", s.listEnvironmentRecords)
	mux.HandleFunc("POST /api/v1/environments", s.createEnvironment)
	mux.HandleFunc("POST /api/v1/render/preview", s.previewRender)
	mux.HandleFunc("POST /api/v1/environments/reconcile", s.reconcileExpired)
	mux.HandleFunc("GET /api/v1/environments/{id}", s.getEnvironment)
	mux.HandleFunc("GET /api/environments/{id}", s.getEnvironment)
	mux.HandleFunc("DELETE /api/v1/environments/{id}", s.deleteEnvironment)
	mux.HandleFunc("DELETE /api/environments/{id}", s.deleteEnvironment)
	mux.HandleFunc("DELETE /environments/{id}", s.deleteEnvironment)
	mux.HandleFunc("POST /api/v1/environments/{id}/pin", s.pinEnvironment)
	mux.HandleFunc("POST /api/environments/{id}/pin", s.pinEnvironment)
	mux.HandleFunc("POST /api/v1/environments/{id}/unpin", s.unpinEnvironment)
	mux.HandleFunc("POST /api/environments/{id}/unpin", s.unpinEnvironment)
	mux.HandleFunc("POST /api/v1/environments/{id}/extend-ttl", s.extendEnvironmentTTL)
	mux.HandleFunc("POST /api/environments/{id}/extend-ttl", s.extendEnvironmentTTL)
	mux.HandleFunc("POST /api/v1/environments/{id}/recreate", s.recreateEnvironment)
	mux.HandleFunc("POST /api/environments/{id}/recreate", s.recreateEnvironment)
	mux.HandleFunc("POST /api/v1/environments/{id}/status", s.updateStatus)
	mux.HandleFunc("GET /api/v1/environments/{id}/events", s.listEnvironmentEvents)
	mux.HandleFunc("GET /api/environments/{id}/events", s.listEnvironmentEvents)
	mux.HandleFunc("GET /api/environments/{id}/kubernetes-events", s.listEnvironmentEvents)
	mux.HandleFunc("POST /api/v1/environments/{id}/events", s.ingestEnvironmentEvents)
	mux.HandleFunc("GET /api/v1/environments/{id}/flux-status", s.getFluxStatus)
	mux.HandleFunc("GET /api/environments/{id}/flux-status", s.getFluxStatus)
	mux.HandleFunc("POST /api/v1/environments/{id}/flux-status", s.ingestFluxStatus)
	mux.HandleFunc("POST /api/v1/webhooks/gitlab", s.gitlabWebhook)
	mux.HandleFunc("POST /api/v1/webhooks/github", s.githubWebhook)
	mux.HandleFunc("POST /webhook/github", s.githubWebhookPoC)
	mux.HandleFunc("GET /auth/github/login", s.githubOAuthLogin)
	mux.HandleFunc("GET /auth/github/callback", s.githubOAuthCallback)
	mux.HandleFunc("GET /auth/gitlab/login", s.gitlabOAuthLogin)
	mux.HandleFunc("GET /auth/gitlab/callback", s.gitlabOAuthCallback)
	mux.HandleFunc("GET /auth/oidc/login", s.oidcOAuthLogin)
	mux.HandleFunc("GET /auth/oidc/callback", s.oidcOAuthCallback)
	mux.HandleFunc("GET /openapi.json", s.serveOpenAPISpec)
	mux.HandleFunc("GET /api/v1/openapi.json", s.serveOpenAPISpec)
	mux.HandleFunc("GET /api/v1/metrics", s.metricsHandler)
	mux.HandleFunc("POST /api/v1/runners/register", s.registerRunner)
	mux.HandleFunc("POST /api/v1/runners/heartbeat", s.runnerHeartbeat)
	mux.HandleFunc("GET /api/v1/runners/health", s.runnersHealthcheck)
	handler := s.apiAccessControl(mux)
	handler = s.bootstrapPredecodeRateLimit(handler)
	handler = s.requestAudit(handler)
	handler = s.apiRateLimit(handler)
	handler = s.requestMetrics(handler)
	handler = requestLogger(s.logger, handler)
	handler = s.requestTracing(handler)
	return handler
}

type bootstrapSimulatePRRequest struct {
	DryRunCommit      bool `json:"dryRunCommit"`
	DryRunCommitSnake bool `json:"dry_run_commit"`
}

type bootstrapSimulatePRResponse struct {
	Validation        bootstrap.ManifestTemplateValidationResult `json:"validation"`
	ManifestTemplates []bootstrap.ManifestTemplate               `json:"manifestTemplates"`
	DryRun            *bootstrapSimulatePRDryRun                 `json:"dryRun,omitempty"`
}

type bootstrapSimulatePRDryRun struct {
	Enabled     bool     `json:"enabled"`
	Status      string   `json:"status"`
	Message     string   `json:"message"`
	CommitPath  string   `json:"commitPath"`
	FileCount   int      `json:"fileCount"`
	Files       []string `json:"files"`
	SimulatedAt string   `json:"simulatedAt"`
}

func bootstrapManifestTemplateFilePath(item bootstrap.ManifestTemplate) string {
	namespace := strings.TrimSpace(item.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	kind := strings.TrimSpace(item.Kind)
	if kind == "" {
		kind = "resource"
	}
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("%s/%s/%s.yaml", namespace, strings.ToLower(kind), name)
}

const (
	apiSessionCookieName   = "envpilot-token"
	oauthStateCookieName   = "envpilot-oauth-state"
	oauthOrgCookieName     = "envpilot-oauth-org"
	oauthSessionCookieTTL  = 24 * time.Hour
	oauthStateCookieMaxAge = 10 * 60
)

func (s *Server) requestTracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := requestTraceIDFromHeaders(r)
		if traceID == "" {
			traceID = s.generateTraceID()
		}
		spanID := s.generateSpanID()
		ctx := context.WithValue(r.Context(), apiRequestTraceIDKey, traceID)
		ctx = context.WithValue(ctx, apiRequestSpanIDKey, spanID)
		w.Header().Set("traceparent", buildTraceParent(traceID, spanID))
		w.Header().Set("X-Trace-Id", traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestTraceIDFromHeaders(r *http.Request) string {
	if traceID := strings.TrimSpace(r.Header.Get("X-Trace-Id")); traceID != "" {
		traceID = strings.ToLower(traceID)
		if isValidTraceID(traceID) {
			return traceID
		}
	}
	if traceParent := strings.TrimSpace(r.Header.Get("traceparent")); traceParent != "" {
		if parsed := parseTraceParent(traceParent); parsed != "" {
			return parsed
		}
	}
	return ""
}

func parseTraceParent(traceParent string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(traceParent)), "-")
	if len(parts) != 4 {
		return ""
	}
	if len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return ""
	}
	if !isHex(parts[1]) || !isHex(parts[2]) || !isHex(parts[3]) {
		return ""
	}
	if isZeroHex(parts[1]) || isZeroHex(parts[2]) {
		return ""
	}
	return parts[1]
}

func isValidTraceID(traceID string) bool {
	return len(traceID) == 32 && isHex(traceID) && !isZeroHex(traceID)
}

func isHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func isZeroHex(value string) bool {
	for _, c := range value {
		if c != '0' {
			return false
		}
	}
	return true
}

func buildTraceParent(traceID, spanID string) string {
	return fmt.Sprintf("%s-%s-%s-%s", traceParentVersion, strings.ToLower(traceID), strings.ToLower(spanID), traceFlagsSampled)
}

func (s *Server) generateTraceID() string {
	return s.generateHexID(16)
}

func (s *Server) generateSpanID() string {
	return s.generateHexID(8)
}

func (s *Server) generateHexID(byteCount int) string {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	seed := atomic.AddUint64(&s.traceID, 1)
	value := fmt.Sprintf("%0*x", byteCount*2, seed)
	if len(value) > byteCount*2 {
		return value[len(value)-(byteCount*2):]
	}
	return value
}

func (s *Server) apiAccessControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.requiresAPIAuthorization(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.apiAuthEnabled() {
			next.ServeHTTP(w, r)
			return
		}

		role, err := s.apiRequestRole(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		if !role.canMutate(r.Method) {
			writeError(w, http.StatusForbidden, fmt.Errorf("insufficient permissions for %q", r.Method))
			return
		}

		ctx := context.WithValue(r.Context(), apiRequestRoleKey, role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) apiRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.config()
		if cfg.RateLimitRequests <= 0 || cfg.RateLimitWindow <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		allowed, retryAfter := s.checkRateLimit(r)
		if !allowed {
			retryAfterSeconds := int64(math.Ceil(retryAfter.Seconds()))
			if retryAfterSeconds < 1 {
				retryAfterSeconds = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterSeconds))
			w.Header().Set("RateLimit-Limit", fmt.Sprintf("%d", cfg.RateLimitRequests))
			w.Header().Set("RateLimit-Remaining", "0")
			writeError(w, http.StatusTooManyRequests, fmt.Errorf("rate limit exceeded"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) bootstrapPredecodeRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint, projectID, ok := bootstrapPredecodeMetadata(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		allowed, retryAfter := s.recordBootstrapPredecodeAttempt(r, endpoint, projectID)
		if !allowed {
			s.writeBootstrapRateLimitAuditLog(r, endpoint, projectID, "", "", "", "malformed_or_excessive_bootstrap_requests")
			writeRateLimitError(w, s.config().BootstrapRateLimitRequests, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bootstrapPredecodeMetadata(r *http.Request) (string, string, bool) {
	if r == nil {
		return "", "", false
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/api/v1/agents/register":
		return auditEndpointAgentRegistration, "", true
	case r.Method == http.MethodPost && path == "/api/v1/agents/heartbeat":
		return auditEndpointAgentHeartbeat, "", true
	case r.Method == http.MethodPost && path == "/api/v1/agents/resource-scan":
		return auditEndpointAgentResourceScanIngest, "", true
	case r.Method == http.MethodPost && path == "/api/v1/runners/register":
		return auditEndpointRunnerRegistration, "", true
	case r.Method == http.MethodPost && path == "/api/v1/runners/heartbeat":
		return auditEndpointRunnerHeartbeat, "", true
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v1/projects/") && strings.HasSuffix(path, "/runner-config"):
		projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/projects/"), "/runner-config")
		return auditEndpointRunnerConfigFetch, normalizeSettingsID(projectID), true
	default:
		return "", "", false
	}
}

func (s *Server) requestAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseStatusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		role := apiRole("public")
		if value, ok := r.Context().Value(apiRequestRoleKey).(apiRole); ok && value != "" {
			role = value
		} else {
			role = s.apiRoleForToken(extractTokenFromRequest(r))
			if role == "" {
				role = apiRole("public")
			}
		}
		actorType, actorID, user := s.auditActor(r, role)
		outcome := requestAuditOutcome(status)
		reason := ""
		if outcome != "success" {
			reason = http.StatusText(status)
			if reason == "" {
				reason = "request_failed"
			}
		}
		s.writeAuditLog(map[string]any{
			"ts":            time.Now().UTC().Format(time.RFC3339Nano),
			"event":         auditEventAPIRequest,
			"endpoint":      r.URL.Path,
			"method":        r.Method,
			"path":          r.URL.Path,
			"query":         sanitizedRawQuery(r.URL.RawQuery),
			"status_code":   status,
			"status":        status,
			"outcome":       outcome,
			"reason":        reason,
			"project_id":    requestAuditProjectID(r.URL.Path),
			"user_id":       user,
			"duration_ms":   time.Since(start).Milliseconds(),
			"role":          string(role),
			"actor_type":    actorType,
			"actor":         actorID,
			"actor_user":    user,
			"remote_addr":   clientIP(r),
			"trace_id":      r.Context().Value(apiRequestTraceIDKey),
			"trace_span_id": r.Context().Value(apiRequestSpanIDKey),
		})
	})
}

func requestAuditOutcome(status int) string {
	if status == http.StatusTooManyRequests {
		return "rate_limited"
	}
	if status >= http.StatusBadRequest {
		return "failure"
	}
	return "success"
}

func requestAuditProjectID(path string) string {
	for _, prefix := range []string{"/api/v1/projects/", "/api/projects/"} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		projectID, _, _ := strings.Cut(rest, "/")
		return normalizeSettingsID(projectID)
	}
	return ""
}

func (s *Server) requestMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseStatusRecorder{ResponseWriter: w}
		start := time.Now()
		s.metricMu.Lock()
		s.metrics.inFlightRequests++
		s.metricMu.Unlock()
		next.ServeHTTP(rec, r)
		s.metricMu.Lock()
		s.metrics.inFlightRequests--
		s.metricMu.Unlock()
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		s.recordRequestMetrics(r.Method, normalizeMetricsPath(r.URL.Path), status, time.Since(start).Seconds())
	})
}

func (s *Server) recordRequestMetrics(method, path string, status int, durationSeconds float64) {
	s.metricMu.Lock()
	defer s.metricMu.Unlock()
	s.metrics.totalRequests++
	s.metrics.requestDurationCount++
	s.metrics.requestDurationSum += durationSeconds
	for _, bucket := range requestDurationBuckets {
		if durationSeconds <= bucket {
			s.metrics.requestDurationBuckets[bucket]++
		}
	}
	s.metrics.requestDurationInf++
	if status >= 400 && status < 500 {
		s.metrics.requests4xxTotal++
	}
	if status >= 500 {
		s.metrics.requests5xxTotal++
	}
	s.metrics.requestsByMethod[method]++
	s.metrics.requestsByPath[path]++
	s.metrics.requestsByStatus[status]++
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lines := s.renderMetrics()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(lines))
}

func (s *Server) renderMetrics() string {
	s.metricMu.Lock()
	defer s.metricMu.Unlock()

	var builder strings.Builder
	builder.WriteString("# HELP envpilot_http_requests_total Total number of HTTP requests\n")
	builder.WriteString("# TYPE envpilot_http_requests_total counter\n")
	builder.WriteString(fmt.Sprintf("envpilot_http_requests_total %d\n", s.metrics.totalRequests))

	builder.WriteString("# HELP envpilot_http_request_duration_seconds_sum Total request duration in seconds\n")
	builder.WriteString("# TYPE envpilot_http_request_duration_seconds_sum counter\n")
	builder.WriteString(fmt.Sprintf("envpilot_http_request_duration_seconds_sum %.6f\n", s.metrics.requestDurationSum))

	builder.WriteString("# HELP envpilot_http_request_duration_seconds Histogram of HTTP request duration in seconds\n")
	builder.WriteString("# TYPE envpilot_http_request_duration_seconds histogram\n")
	for _, bucket := range requestDurationBuckets {
		builder.WriteString(fmt.Sprintf(
			"envpilot_http_request_duration_seconds_bucket{le=%q} %d\n",
			strconv.FormatFloat(bucket, 'f', -1, 64),
			s.metrics.requestDurationBuckets[bucket],
		))
	}
	builder.WriteString(fmt.Sprintf("envpilot_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", s.metrics.requestDurationInf))
	builder.WriteString("# HELP envpilot_http_request_duration_seconds_count Total number of request duration observations\n")
	builder.WriteString("# TYPE envpilot_http_request_duration_seconds_count counter\n")
	builder.WriteString(fmt.Sprintf("envpilot_http_request_duration_seconds_count %d\n", s.metrics.requestDurationCount))

	builder.WriteString("# HELP envpilot_http_requests_in_flight HTTP requests currently in flight\n")
	builder.WriteString("# TYPE envpilot_http_requests_in_flight gauge\n")
	builder.WriteString(fmt.Sprintf("envpilot_http_requests_in_flight %d\n", s.metrics.inFlightRequests))

	builder.WriteString("# HELP envpilot_http_requests_4xx_total Total number of 4xx responses\n")
	builder.WriteString("# TYPE envpilot_http_requests_4xx_total counter\n")
	builder.WriteString(fmt.Sprintf("envpilot_http_requests_4xx_total %d\n", s.metrics.requests4xxTotal))

	builder.WriteString("# HELP envpilot_http_requests_5xx_total Total number of 5xx responses\n")
	builder.WriteString("# TYPE envpilot_http_requests_5xx_total counter\n")
	builder.WriteString(fmt.Sprintf("envpilot_http_requests_5xx_total %d\n", s.metrics.requests5xxTotal))

	builder.WriteString("# HELP envpilot_pending_registration_tokens Current pending in-memory registration token entries\n")
	builder.WriteString("# TYPE envpilot_pending_registration_tokens gauge\n")
	builder.WriteString(fmt.Sprintf("envpilot_pending_registration_tokens %d\n", s.pendingAgentRegistrationSize()))

	builder.WriteString("# HELP envpilot_pending_registration_token_evictions_total Total pending in-memory registration token evictions\n")
	builder.WriteString("# TYPE envpilot_pending_registration_token_evictions_total counter\n")
	builder.WriteString(fmt.Sprintf("envpilot_pending_registration_token_evictions_total %d\n", s.metrics.pendingTokenEvictions))

	builder.WriteString("# HELP envpilot_http_requests_by_method_total HTTP requests split by method\n")
	builder.WriteString("# TYPE envpilot_http_requests_by_method_total counter\n")
	methods := make([]string, 0, len(s.metrics.requestsByMethod))
	for method := range s.metrics.requestsByMethod {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	for _, method := range methods {
		builder.WriteString(fmt.Sprintf("envpilot_http_requests_by_method_total{method=%q} %d\n", labelEscape(method), s.metrics.requestsByMethod[method]))
	}

	builder.WriteString("# HELP envpilot_http_requests_by_status_total HTTP requests split by status code\n")
	builder.WriteString("# TYPE envpilot_http_requests_by_status_total counter\n")
	statuses := make([]int, 0, len(s.metrics.requestsByStatus))
	for status := range s.metrics.requestsByStatus {
		statuses = append(statuses, status)
	}
	sort.Ints(statuses)
	for _, status := range statuses {
		builder.WriteString(fmt.Sprintf("envpilot_http_requests_by_status_total{status=%q} %d\n", strconv.Itoa(status), s.metrics.requestsByStatus[status]))
	}

	builder.WriteString("# HELP envpilot_http_requests_by_path_total HTTP requests split by path template\n")
	builder.WriteString("# TYPE envpilot_http_requests_by_path_total counter\n")
	paths := make([]string, 0, len(s.metrics.requestsByPath))
	for path := range s.metrics.requestsByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		builder.WriteString(fmt.Sprintf("envpilot_http_requests_by_path_total{path=%q} %d\n", labelEscape(path), s.metrics.requestsByPath[path]))
	}
	return builder.String()
}

func labelEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func normalizeMetricsPath(path string) string {
	if strings.HasPrefix(path, "/api/projects/") {
		if strings.HasSuffix(path, "/bootstrap-session/validate-scm") {
			return "/api/projects/{id}/bootstrap-session/validate-scm"
		}
		if strings.HasSuffix(path, "/environments") {
			return "/api/projects/{id}/environments"
		}
		if strings.HasSuffix(path, "/hybrid-config") {
			return "/api/projects/{id}/hybrid-config"
		}
		if strings.HasSuffix(path, "/cost-policy") {
			return "/api/projects/{id}/cost-policy"
		}
		return "/api/projects/{id}"
	}
	if strings.HasPrefix(path, "/api/environments/") {
		if strings.HasSuffix(path, "/pin") {
			return "/api/environments/{id}/pin"
		}
		if strings.HasSuffix(path, "/unpin") {
			return "/api/environments/{id}/unpin"
		}
		if strings.HasSuffix(path, "/extend-ttl") {
			return "/api/environments/{id}/extend-ttl"
		}
		if strings.HasSuffix(path, "/recreate") {
			return "/api/environments/{id}/recreate"
		}
		if strings.HasSuffix(path, "/events") {
			return "/api/environments/{id}/events"
		}
		if strings.HasSuffix(path, "/kubernetes-events") {
			return "/api/environments/{id}/kubernetes-events"
		}
		if strings.HasSuffix(path, "/flux-status") {
			return "/api/environments/{id}/flux-status"
		}
		return "/api/environments/{id}"
	}
	if strings.HasPrefix(path, "/api/v1/products/") {
		return "/api/v1/products/{name}"
	}
	if strings.HasPrefix(path, "/api/v1/projects/") {
		return "/api/v1/projects/{id}"
	}
	if strings.HasPrefix(path, "/api/v1/jobs/") {
		if strings.HasSuffix(path, "/retry") {
			return "/api/v1/jobs/{id}/retry"
		}
		return "/api/v1/jobs/{id}"
	}
	if strings.HasPrefix(path, "/api/v1/environment-records/") {
		return "/api/v1/environment-records"
	}
	if strings.HasPrefix(path, "/api/v1/environments/") {
		if strings.HasSuffix(path, "/pin") {
			return "/api/v1/environments/{id}/pin"
		}
		if strings.HasSuffix(path, "/unpin") {
			return "/api/v1/environments/{id}/unpin"
		}
		if strings.HasSuffix(path, "/extend-ttl") {
			return "/api/v1/environments/{id}/extend-ttl"
		}
		if strings.HasSuffix(path, "/status") {
			return "/api/v1/environments/{id}/status"
		}
		if strings.HasSuffix(path, "/events") {
			return "/api/v1/environments/{id}/events"
		}
		if strings.HasSuffix(path, "/flux-status") {
			return "/api/v1/environments/{id}/flux-status"
		}
		return "/api/v1/environments/{id}"
	}
	if strings.HasPrefix(path, "/api/v1/settings/secret-refs/") && strings.HasSuffix(path, "/validate") {
		return "/api/v1/settings/secret-refs/{id}/validate"
	}
	if strings.HasPrefix(path, "/api/v1/settings/clusters/") {
		return "/api/v1/settings/clusters/{id}/health"
	}
	return path
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *Server) serveOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	spec, err := openAPIFiles.ReadFile("openapi.json")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec)
}

type apiRole string

const (
	apiRoleReader apiRole = "reader"
	apiRoleAdmin  apiRole = "admin"
)

type oauthProviderConfig struct {
	Name         string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserURL      string
	Scopes       string
}

type oauthSession struct {
	Provider string  `json:"provider"`
	User     string  `json:"user"`
	Org      string  `json:"org"`
	Role     apiRole `json:"role"`
	Expires  int64   `json:"expires"`
}

type responseStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *responseStatusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *responseStatusRecorder) Write(body []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(body)
}

type apiRoleContextKey struct{}

type apiRequestTraceIDContextKey struct{}
type apiRequestSpanIDContextKey struct{}

var (
	apiRequestRoleKey    apiRoleContextKey
	apiRequestTraceIDKey apiRequestTraceIDContextKey
	apiRequestSpanIDKey  apiRequestSpanIDContextKey
)

func (role apiRole) canMutate(method string) bool {
	if role == apiRoleAdmin {
		return true
	}
	if role != apiRoleReader {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	default:
		return false
	}
}

func (s *Server) requiresAPIAuthorization(path string) bool {
	if path == "/openapi.json" {
		return false
	}
	if strings.HasPrefix(path, "/api/v1/") {
		if strings.HasPrefix(path, "/api/v1/runners/") {
			return false
		}
		if strings.HasPrefix(path, "/api/v1/projects/") && strings.HasSuffix(path, "/runner-config") {
			return false
		}
		switch path {
		case "/api/v1/health", "/api/v1/openapi.json", "/api/v1/webhooks/github", "/api/v1/webhooks/gitlab", "/api/v1/agents/register", "/api/v1/agents/heartbeat", "/api/v1/agents/resource-scan", "/api/v1/agents/resource-scan/next":
			return false
		default:
			return true
		}
	}
	if strings.HasPrefix(path, "/api/") {
		return true
	}
	return false
}

func (s *Server) apiAuthEnabled() bool {
	cfg := s.config()
	if len(cfg.APITokenRoles) > 0 {
		return true
	}
	return strings.TrimSpace(cfg.APIReadToken) != "" || strings.TrimSpace(cfg.APIWriteToken) != "" || s.oauthEnabled(cfg)
}

func (s *Server) apiRequestRole(r *http.Request) (apiRole, error) {
	token := extractTokenFromRequest(r)
	if token == "" {
		return "", fmt.Errorf("api token is required")
	}
	if session, ok := s.parseOAuthSession(token); ok {
		return session.Role, nil
	}
	cfg := s.config()
	if role := s.resolveAPIRole(cfg, token); role != "" {
		return role, nil
	}
	if s.isAPIToken(token, cfg.APIWriteToken) {
		return apiRoleAdmin, nil
	}
	if s.isAPIToken(token, cfg.APIReadToken) {
		return apiRoleReader, nil
	}
	return "", fmt.Errorf("invalid api token")
}

func (s *Server) apiRoleForToken(token string) apiRole {
	if session, ok := s.parseOAuthSession(token); ok {
		return session.Role
	}
	cfg := s.config()
	if role := s.resolveAPIRole(cfg, token); role != "" {
		return role
	}
	if s.isAPIToken(token, cfg.APIWriteToken) {
		return apiRoleAdmin
	}
	if s.isAPIToken(token, cfg.APIReadToken) {
		return apiRoleReader
	}
	return ""
}

func (s *Server) filterProjectsForRequest(r *http.Request, projects []domain.Project) []domain.Project {
	filtered := make([]domain.Project, 0, len(projects))
	for _, project := range projects {
		if s.canAccessProject(r, project) {
			filtered = append(filtered, project)
		}
	}
	return filtered
}

func (s *Server) filterEnvironmentsForRequest(r *http.Request, environments []domain.Environment) []domain.Environment {
	filtered := make([]domain.Environment, 0, len(environments))
	for _, env := range environments {
		if s.canAccessProjectID(r, env.Project) {
			filtered = append(filtered, env)
		}
	}
	return filtered
}

func (s *Server) filterEnvironmentRecordsForRequest(r *http.Request, records []domain.EnvironmentRecord) []domain.EnvironmentRecord {
	filtered := make([]domain.EnvironmentRecord, 0, len(records))
	for _, record := range records {
		if s.canAccessProjectID(r, record.ProjectID) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func (s *Server) canAccessProjectID(r *http.Request, projectID string) bool {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || s.projects == nil {
		return true
	}
	project, err := s.projects.GetProject(projectID)
	if err != nil {
		return true
	}
	return s.canAccessProject(r, project)
}

func (s *Server) canAccessProject(r *http.Request, project domain.Project) bool {
	if len(project.AccessUsers) == 0 && len(project.AccessOrganizations) == 0 {
		return true
	}

	user := s.projectAccessUser(r)
	org := s.projectAccessOrganization(r)
	if user != "" && s.matchesAny(project.AccessUsers, user) {
		return true
	}
	if org != "" && s.matchesAny(project.AccessOrganizations, org) {
		return true
	}
	if user == "" && org == "" {
		return true
	}
	return false
}

func (s *Server) projectAccessUser(r *http.Request) string {
	if r == nil {
		return ""
	}
	if user := strings.TrimSpace(r.Header.Get("X-EnvPilot-User")); user != "" {
		return user
	}
	if session, ok := s.parseOAuthSession(extractTokenFromRequest(r)); ok {
		return session.User
	}
	return ""
}

func (s *Server) projectAccessOrganization(r *http.Request) string {
	if r == nil {
		return ""
	}
	if org := strings.TrimSpace(r.Header.Get("X-EnvPilot-Org")); org != "" {
		return org
	}
	if session, ok := s.parseOAuthSession(extractTokenFromRequest(r)); ok {
		return strings.ToLower(strings.TrimSpace(session.Org))
	}
	return ""
}

func (s *Server) matchesAny(allowed []string, candidate string) bool {
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	if candidate == "" {
		return false
	}
	for _, item := range allowed {
		if strings.ToLower(strings.TrimSpace(item)) == candidate {
			return true
		}
	}
	return false
}

func (s *Server) oauthEnabled(cfg config.Config) bool {
	return s.oauthProvider("github", cfg).ClientID != "" ||
		s.oauthProvider("gitlab", cfg).ClientID != "" ||
		s.oauthProvider("oidc", cfg).ClientID != ""
}

func (s *Server) oauthProvider(provider string, cfg config.Config) oauthProviderConfig {
	switch provider {
	case "github":
		return oauthProviderConfig{
			Name:         "github",
			ClientID:     strings.TrimSpace(cfg.GitHubOAuthClientID),
			ClientSecret: strings.TrimSpace(cfg.GitHubOAuthSecret),
			AuthURL:      strings.TrimSpace(cfg.GitHubOAuthAuthURL),
			TokenURL:     strings.TrimSpace(cfg.GitHubOAuthTokenURL),
			UserURL:      strings.TrimSpace(cfg.GitHubOAuthUserURL),
			Scopes:       "read:user user:email",
		}
	case "gitlab":
		return oauthProviderConfig{
			Name:         "gitlab",
			ClientID:     strings.TrimSpace(cfg.GitLabOAuthClientID),
			ClientSecret: strings.TrimSpace(cfg.GitLabOAuthSecret),
			AuthURL:      strings.TrimSpace(cfg.GitLabOAuthAuthURL),
			TokenURL:     strings.TrimSpace(cfg.GitLabOAuthTokenURL),
			UserURL:      strings.TrimSpace(cfg.GitLabOAuthUserURL),
			Scopes:       "read_user",
		}
	case "oidc":
		scopes := strings.TrimSpace(cfg.OIDCOAuthScopes)
		if scopes == "" {
			scopes = "openid profile email"
		}
		return oauthProviderConfig{
			Name:         "oidc",
			ClientID:     strings.TrimSpace(cfg.OIDCOAuthClientID),
			ClientSecret: strings.TrimSpace(cfg.OIDCOAuthSecret),
			AuthURL:      strings.TrimSpace(cfg.OIDCOAuthAuthURL),
			TokenURL:     strings.TrimSpace(cfg.OIDCOAuthTokenURL),
			UserURL:      strings.TrimSpace(cfg.OIDCOAuthUserURL),
			Scopes:       scopes,
		}
	default:
		return oauthProviderConfig{}
	}
}

func (s *Server) oauthSessionSecret() string {
	cfg := s.config()
	for _, candidate := range []string{
		cfg.OAuthSessionSecret,
		cfg.APIWriteToken,
		cfg.APIReadToken,
		cfg.GitHubOAuthSecret,
		cfg.GitLabOAuthSecret,
		cfg.OIDCOAuthSecret,
	} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return ""
}

func (s *Server) signOAuthPayload(payload []byte) string {
	secret := s.oauthSessionSecret()
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) buildOAuthSession(provider string, user string, role apiRole, org string) string {
	session := oauthSession{
		Provider: provider,
		User:     user,
		Org:      strings.TrimSpace(org),
		Role:     role,
		Expires:  time.Now().UTC().Add(oauthSessionCookieTTL).Unix(),
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return ""
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signature := s.signOAuthPayload([]byte(encodedPayload))
	if signature == "" {
		return ""
	}
	return "oauth." + encodedPayload + "." + signature
}

func (s *Server) parseOAuthSession(token string) (oauthSession, bool) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "oauth.") {
		return oauthSession{}, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return oauthSession{}, false
	}
	expected := s.signOAuthPayload([]byte(parts[1]))
	if expected == "" || !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return oauthSession{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return oauthSession{}, false
	}
	var session oauthSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return oauthSession{}, false
	}
	if session.Expires <= time.Now().UTC().Unix() || session.Role == "" {
		return oauthSession{}, false
	}
	return session, true
}

func (s *Server) auditActor(r *http.Request, role apiRole) (actorType, actorID, actorUser string) {
	if role == apiRole("") || role == "public" {
		return auditActorAnonymous, "", ""
	}
	user := strings.TrimSpace(r.Header.Get("X-EnvPilot-User"))
	if user != "" {
		return auditActorUser, user, user
	}
	token := extractTokenFromRequest(r)
	if session, ok := s.parseOAuthSession(token); ok {
		return auditActorUser, session.Provider + ":" + session.User, session.User
	}
	if token == "" {
		return auditActorAnonymous, "", ""
	}
	return auditActorToken, tokenFingerprint(token), ""
}

func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:8])
}

func (s *Server) isAPIToken(token string, configured string) bool {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(configured)) == 1
}

func (s *Server) resolveAPIRole(cfg config.Config, token string) apiRole {
	if token == "" {
		return ""
	}
	for configuredToken, role := range cfg.APITokenRoles {
		if role == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(configuredToken)) == 1 {
			return apiRole(role)
		}
	}
	return ""
}

func extractTokenFromRequest(r *http.Request) string {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization != "" {
		if token, ok := strings.CutPrefix(authorization, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
		if token, ok := strings.CutPrefix(authorization, "bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	if cookie, err := r.Cookie(apiSessionCookieName); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return strings.TrimSpace(r.Header.Get("X-EnvPilot-Token"))
}

func (s *Server) checkRateLimit(r *http.Request) (bool, time.Duration) {
	cfg := s.config()
	if r == nil {
		return true, 0
	}
	window := cfg.RateLimitWindow
	limit := cfg.RateLimitRequests
	now := time.Now()
	key := s.rateLimitKey(r)

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	state, ok := s.rateMap[key]
	if !ok {
		s.rateMap[key] = &rateLimitState{
			windowStart: now,
			count:       1,
		}
		return true, 0
	}
	if now.Sub(state.windowStart) >= window {
		state.windowStart = now
		state.count = 1
		return true, 0
	}
	if state.count >= limit {
		return false, window - now.Sub(state.windowStart)
	}
	state.count++
	return true, 0
}

func (s *Server) recordBootstrapAuthFailure(r *http.Request, endpoint string, projectID string, token string) (bool, time.Duration) {
	cfg := s.config()
	limit := cfg.BootstrapRateLimitRequests
	window := cfg.BootstrapRateLimitWindow
	if limit <= 0 || window <= 0 || r == nil {
		return true, 0
	}
	now := time.Now()
	keys := s.bootstrapRateLimitKeys(r, endpoint, projectID, token)

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	var retryAfter time.Duration
	for _, key := range keys {
		state, ok := s.rateMap[key]
		if !ok {
			continue
		}
		if now.Sub(state.windowStart) >= window {
			continue
		}
		if state.count >= limit {
			candidate := window - now.Sub(state.windowStart)
			if candidate > retryAfter {
				retryAfter = candidate
			}
		}
	}
	if retryAfter > 0 {
		return false, retryAfter
	}
	for _, key := range keys {
		state, ok := s.rateMap[key]
		if !ok {
			s.rateMap[key] = &rateLimitState{
				windowStart: now,
				count:       1,
			}
			continue
		}
		if now.Sub(state.windowStart) >= window {
			state.windowStart = now
			state.count = 1
			continue
		}
		state.count++
	}
	return true, 0
}

func (s *Server) recordBootstrapPredecodeAttempt(r *http.Request, endpoint string, projectID string) (bool, time.Duration) {
	cfg := s.config()
	limit := cfg.BootstrapRateLimitRequests
	window := cfg.BootstrapRateLimitWindow
	if limit <= 0 || window <= 0 || r == nil {
		return true, 0
	}
	now := time.Now()
	key := strings.Join([]string{
		"bootstrap_predecode",
		strings.TrimSpace(endpoint),
		"ip", clientIP(r),
		"project", normalizeSettingsID(projectID),
	}, ":")

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	state, ok := s.rateMap[key]
	if !ok {
		s.rateMap[key] = &rateLimitState{
			windowStart: now,
			count:       1,
		}
		return true, 0
	}
	if now.Sub(state.windowStart) >= window {
		state.windowStart = now
		state.count = 1
		return true, 0
	}
	if state.count >= limit {
		return false, window - now.Sub(state.windowStart)
	}
	state.count++
	return true, 0
}

func (s *Server) bootstrapRateLimitKeys(r *http.Request, endpoint string, projectID string, token string) []string {
	endpoint = strings.TrimSpace(endpoint)
	ip := clientIP(r)
	tokenPart := "none"
	if strings.TrimSpace(token) != "" {
		tokenPart = tokenFingerprint(token)
	}
	return []string{
		strings.Join([]string{
			"bootstrap",
			endpoint,
			"ip", ip,
		}, ":"),
		strings.Join([]string{
			"bootstrap",
			endpoint,
			"ip", ip,
			"project", normalizeSettingsID(projectID),
			"token", tokenPart,
		}, ":"),
	}
}

func (s *Server) bootstrapRateLimitKey(r *http.Request, endpoint string, projectID string, token string) string {
	keys := s.bootstrapRateLimitKeys(r, endpoint, projectID, token)
	if len(keys) == 0 {
		return strings.Join([]string{
			"bootstrap",
			strings.TrimSpace(endpoint),
			"ip", clientIP(r),
		}, ":")
	}
	if len(keys) == 1 {
		return keys[0]
	}
	return keys[1]
}

func (s *Server) bootstrapIPRateLimitKey(r *http.Request, endpoint string) string {
	return strings.Join([]string{
		"bootstrap",
		strings.TrimSpace(endpoint),
		"ip", clientIP(r),
	}, ":")
}

func (s *Server) rejectBootstrapAuthFailure(w http.ResponseWriter, r *http.Request, status int, event string, endpoint string, projectID string, subjectKey string, subjectID string, token string, cause error) {
	allowed, retryAfter := s.recordBootstrapAuthFailure(r, endpoint, projectID, token)
	if !allowed {
		s.writeBootstrapRateLimitAuditLog(r, endpoint, projectID, subjectKey, subjectID, token, bootstrapAuthAuditReason(cause))
		writeRateLimitError(w, s.config().BootstrapRateLimitRequests, retryAfter)
		return
	}
	s.writeBootstrapAuthAuditLog(r, event, endpoint, projectID, subjectKey, subjectID, token, bootstrapAuthAuditReason(cause))
	writeError(w, status, fmt.Errorf("%s", bootstrapAuthClientReason(endpoint, cause)))
}

func bootstrapAuthAuditReason(cause error) string {
	if errors.Is(cause, store.ErrBootstrapTokenAlreadyUsed) {
		return "already_used"
	}
	if cause == nil {
		return "authentication failed"
	}
	return cause.Error()
}

func bootstrapAuthClientReason(endpoint string, cause error) string {
	if errors.Is(cause, store.ErrBootstrapTokenAlreadyUsed) {
		if endpoint == auditEndpointRunnerConfigFetch {
			return "invalid config token"
		}
		return "invalid bootstrap token"
	}
	if cause == nil {
		return "authentication failed"
	}
	return cause.Error()
}

func writeRateLimitError(w http.ResponseWriter, limit int, retryAfter time.Duration) {
	retryAfterSeconds := int64(math.Ceil(retryAfter.Seconds()))
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterSeconds))
	if limit > 0 {
		w.Header().Set("RateLimit-Limit", fmt.Sprintf("%d", limit))
	}
	w.Header().Set("RateLimit-Remaining", "0")
	writeError(w, http.StatusTooManyRequests, fmt.Errorf("rate limit exceeded"))
}

func (s *Server) rateLimitKey(r *http.Request) string {
	if token := extractTokenFromRequest(r); token != "" {
		sum := sha256.Sum256([]byte(token))
		return "token:" + hex.EncodeToString(sum[:])
	}
	return "ip:" + clientIP(r)
}

func sanitizedRawQuery(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "[redacted-invalid-query]"
	}
	for key := range values {
		if isSensitiveQueryKey(key) {
			values.Set(key, "[redacted]")
		}
	}
	return values.Encode()
}

func isSensitiveQueryKey(key string) bool {
	return isSensitiveFieldName(key)
}

func (s *Server) writeAuditLog(entry map[string]any) {
	cfg := s.config()
	sanitized, _ := redactSecrets(normalizeAuditEntry(entry)).(map[string]any)
	if sanitized == nil {
		sanitized = map[string]any{}
	}
	payload, err := json.Marshal(sanitized)
	if err != nil {
		s.logger.Error("audit log serialization failed", "error", sanitizeErrorMessage(err))
		return
	}
	path := strings.TrimSpace(cfg.AuditLogPath)
	if path == "" {
		s.logger.Info("audit", "event", string(payload))
		return
	}
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		s.logger.Error("audit log write failed", "path", path, "error", sanitizeErrorMessage(err))
		return
	}
	defer file.Close()
	_, _ = file.Write(append(payload, '\n'))
}

func normalizeAuditEntry(entry map[string]any) map[string]any {
	normalized := make(map[string]any, len(entry)+4)
	for key, value := range entry {
		normalized[key] = value
	}
	event := strings.TrimSpace(fmt.Sprint(normalized["event"]))
	if event == "" || event == "<nil>" {
		event = "audit_event"
		normalized["event"] = event
	}
	outcome := strings.TrimSpace(fmt.Sprint(normalized["outcome"]))
	if outcome == "" || outcome == "<nil>" {
		outcome = auditOutcomeForEvent(event)
		normalized["outcome"] = outcome
	}
	if _, ok := normalized["reason"]; !ok {
		normalized["reason"] = ""
	}
	reason := strings.TrimSpace(fmt.Sprint(normalized["reason"]))
	if reason == "<nil>" {
		reason = ""
	}
	if reason == "" && (outcome == "failure" || outcome == "rate_limited") {
		reason = "unspecified"
	}
	normalized["reason"] = reason
	if strings.TrimSpace(fmt.Sprint(normalized["remote_addr"])) == "" || fmt.Sprint(normalized["remote_addr"]) == "<nil>" {
		normalized["remote_addr"] = "unknown"
	}
	if strings.TrimSpace(fmt.Sprint(normalized["trace_id"])) == "" || fmt.Sprint(normalized["trace_id"]) == "<nil>" {
		normalized["trace_id"] = "unknown"
	}
	return normalized
}

func auditOutcomeForEvent(event string) string {
	switch {
	case event == auditEventBootstrapRateLimitHit:
		return "rate_limited"
	case strings.Contains(event, "_failed") || strings.Contains(event, "validation_failed"):
		return "failure"
	default:
		return "success"
	}
}

var (
	sensitiveJSONFieldPattern = regexp.MustCompile(`(?i)("?(?:registrationToken|registration_token|projectConfigToken|project_config_token|configToken|config_token|agentAuthToken|agent_auth_token|runnerAuthToken|runner_auth_token|authorization|token|secret|password)"?\s*[:=]\s*"?)([^"\s,}&]+)`)
	bearerTokenPattern        = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)([^\s,"'}]+)`)
)

func redactSecrets(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			if isSensitiveFieldName(key) {
				redacted[key] = "[redacted]"
				continue
			}
			redacted[key] = redactSecrets(item)
		}
		return redacted
	case map[string]string:
		redacted := make(map[string]string, len(typed))
		for key, item := range typed {
			if isSensitiveFieldName(key) {
				redacted[key] = "[redacted]"
				continue
			}
			redacted[key] = redactSensitiveString(item)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, item := range typed {
			redacted[index] = redactSecrets(item)
		}
		return redacted
	case []string:
		redacted := make([]string, len(typed))
		for index, item := range typed {
			redacted[index] = redactSensitiveString(item)
		}
		return redacted
	case string:
		return redactSensitiveString(typed)
	case error:
		return sanitizeErrorMessage(typed)
	default:
		return typed
	}
}

func isSensitiveFieldName(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")
	switch normalized {
	case "authorization", "registrationtoken", "projectconfigtoken", "configtoken", "agentauthtoken", "runnerauthtoken", "token", "secret", "password", "signature":
		return true
	default:
		return strings.HasSuffix(normalized, "token") || strings.HasSuffix(normalized, "secret") || strings.HasSuffix(normalized, "password")
	}
}

func sanitizeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return redactSensitiveString(err.Error())
}

func redactSensitiveString(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	redacted := bearerTokenPattern.ReplaceAllString(value, "${1}[redacted]")
	redacted = sensitiveJSONFieldPattern.ReplaceAllString(redacted, "${1}[redacted]")
	return redacted
}

func clientIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return strings.TrimSpace(host)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listProducts(w http.ResponseWriter, _ *http.Request) {
	items, err := s.service.ListProducts()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	product, err := s.products.GetProduct(r.PathValue("name"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, product)
}

func (s *Server) saveProduct(w http.ResponseWriter, r *http.Request) {
	var product domain.ProductTemplate
	if err := json.NewDecoder(r.Body).Decode(&product); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	product.Name = r.PathValue("name")
	saved, err := s.products.SaveProduct(product)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) validateProduct(w http.ResponseWriter, r *http.Request) {
	var product domain.ProductTemplate
	if err := json.NewDecoder(r.Body).Decode(&product); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	normalized, err := app.ValidateProductTemplate(product)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "product": normalized})
}

func (s *Server) deleteProduct(w http.ResponseWriter, r *http.Request) {
	if err := s.products.DeleteProduct(r.PathValue("name")); err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	items, err := s.projects.ListProjects()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	items = s.filterProjectsForRequest(r, items)
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getDashboardSummary(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListEnvironments()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	items = s.filterEnvironmentsForRequest(r, items)
	active := 0
	failed := 0
	idle := 0
	estimatedDailyCost := 0.0
	for _, item := range items {
		if item.Status != domain.StatusTerminated && item.Status != domain.StatusTerminating {
			active++
			estimatedDailyCost += parseCostEstimateDay(item.CostEstimateDay)
		}
		if item.Status == domain.StatusFailed {
			failed++
		}
		if item.Idle {
			idle++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activeEnvironments": active,
		"failedEnvironments": failed,
		"idleEnvironments":   idle,
		"estimatedDailyCost": fmt.Sprintf("~ €%.2f/day", estimatedDailyCost),
	})
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) saveProject(w http.ResponseWriter, r *http.Request) {
	var project domain.Project
	if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	project.ID = r.PathValue("id")
	if existing, err := s.projects.GetProject(project.ID); err == nil && !s.canAccessProject(r, existing) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	saved, err := s.projects.SaveProject(project)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	var patch domain.Project
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("failed to read request body: %w", err))
		return
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	var aliases map[string]json.RawMessage
	if err := json.Unmarshal(body, &aliases); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	project = mergeProjectPatch(project, patch)
	project = mergeProjectPatchAliases(project, aliases)
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	saved, err := s.projects.SaveProject(project)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) listProjectEnvironments(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	items, err := s.service.ListEnvironments()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	items = s.filterEnvironmentsForRequest(r, items)
	filtered := make([]domain.Environment, 0, len(items))
	for _, item := range items {
		if item.Project == project.ID {
			filtered = append(filtered, item)
		}
	}
	writeJSON(w, http.StatusOK, filtered)
}

func (s *Server) getProjectBootstrapSession(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	session, err := s.bootstrapSessions.Get(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) getProjectBootstrapManifestTemplates(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	session, err := s.bootstrapSessions.Get(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}

	templates := asBootstrapManifestTemplates(session.Data[bootstrapManifestTemplatesKey])
	if len(templates) == 0 {
		session, err = s.refreshBootstrapManifestTemplates(project, session)
		if err != nil {
			writeMappedError(w, err)
			return
		}
		templates = asBootstrapManifestTemplates(session.Data[bootstrapManifestTemplatesKey])
	}
	if templates == nil {
		templates = []bootstrap.ManifestTemplate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projectId":          project.ID,
		"bootstrapSessionId": session.ID,
		"manifestTemplates":  templates,
	})
}

func (s *Server) validateProjectBootstrapSCMConfig(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.scmValidation == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("SCM validation is not configured"))
		return
	}

	var req struct {
		Provider           string `json:"provider"`
		ProviderSnake      string `json:"provider_name"`
		AppRepoURL         string `json:"appRepoUrl"`
		AppRepoURLSnake    string `json:"app_repo_url"`
		GitOpsRepoURL      string `json:"gitopsRepoUrl"`
		GitOpsRepoURLSnake string `json:"gitops_repo_url"`
		DefaultBranch      string `json:"defaultBranch"`
		DefaultBranchSnake string `json:"default_branch"`
		AuthMethod         string `json:"authMethod"`
		AuthMethodSnake    string `json:"auth_method"`
		OAuthToken         string `json:"oauthToken"`
		OAuthTokenSnake    string `json:"oauth_token"`
		AppToken           string `json:"appToken"`
		AppTokenSnake      string `json:"app_token"`
		DeployToken        string `json:"deployToken"`
		DeployTokenSnake   string `json:"deploy_token"`
		SSHPrivateKey      string `json:"sshPrivateKey"`
		SSHPrivateKeySnake string `json:"ssh_private_key"`
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("failed to read request body: %w", err))
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("request body is required"))
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	validationReq := app.SCMRepositoryValidationRequest{
		Provider:      firstNonEmpty(req.Provider, req.ProviderSnake),
		AppRepoURL:    firstNonEmpty(req.AppRepoURL, req.AppRepoURLSnake),
		GitOpsRepoURL: firstNonEmpty(req.GitOpsRepoURL, req.GitOpsRepoURLSnake),
		DefaultBranch: firstNonEmpty(req.DefaultBranch, req.DefaultBranchSnake),
		AuthMethod:    firstNonEmpty(req.AuthMethod, req.AuthMethodSnake),
		OAuthToken:    firstNonEmpty(req.OAuthToken, req.OAuthTokenSnake),
		AppToken:      firstNonEmpty(req.AppToken, req.AppTokenSnake),
		DeployToken:   firstNonEmpty(req.DeployToken, req.DeployTokenSnake),
		SSHPrivateKey: firstNonEmpty(req.SSHPrivateKey, req.SSHPrivateKeySnake),
	}

	result, err := s.scmValidation.ValidateSCMConfig(r.Context(), validationReq)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createProjectBootstrapSession(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req struct {
		CreatedBy    *string `json:"createdBy"`
		CreatedBySQL string  `json:"created_by"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("failed to read request body: %w", err))
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
			return
		}
	}
	createdBy := s.projectAccessUser(r)
	if req.CreatedBy != nil {
		createdBy = *req.CreatedBy
	} else if strings.TrimSpace(req.CreatedBySQL) != "" {
		createdBy = req.CreatedBySQL
	}
	session, err := s.bootstrapSessions.Create(project.ID, createdBy)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) updateProjectBootstrapSession(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req app.BootstrapSessionUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	compileRequested := bootstrapCompileRequested(req)
	updateReq := req
	if compileRequested {
		updateReq.Status = nil
	}
	if policy, ok, err := bootstrapResourcePolicyFromUpdate(req); err != nil {
		writeMappedError(w, app.ValidationError{Message: fmt.Sprintf("invalid resource policy: %v", err)})
		return
	} else if ok {
		if err := bootstrap.ValidateResourcePolicyConfig(policy); err != nil {
			writeMappedError(w, app.ValidationError{Message: fmt.Sprintf("invalid resource policy: %v", err)})
			return
		}
	}
	if networkPolicy, ok, err := bootstrapNetworkPolicyFromUpdate(req, bootstrapDefaultBaseNamespaces(project)); err != nil {
		writeMappedError(w, app.ValidationError{Message: fmt.Sprintf("invalid network policy: %v", err)})
		return
	} else if ok {
		if err := bootstrap.ValidateNetworkPolicyConfig(networkPolicy); err != nil {
			writeMappedError(w, app.ValidationError{Message: fmt.Sprintf("invalid network policy: %v", err)})
			return
		}
	}
	if cleanupSafety, ok, targetNamespaces, err := bootstrapCleanupSafetyFromUpdate(req); err != nil {
		writeMappedError(w, app.ValidationError{Message: fmt.Sprintf("invalid cleanup safety config: %v", err)})
		return
	} else if ok {
		if err := bootstrap.ValidateCleanupSafetyConfig(cleanupSafety, targetNamespaces); err != nil {
			writeMappedError(w, app.ValidationError{Message: fmt.Sprintf("invalid cleanup safety config: %v", err)})
			return
		}
	}
	session, err := s.bootstrapSessions.Update(project.ID, updateReq)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !bootstrapUpdateContainsManifestTemplates(req) {
		session, err = s.refreshBootstrapManifestTemplates(project, session)
		if err != nil {
			writeMappedError(w, err)
			return
		}
	}
	if compileRequested {
		session, err = s.compileBootstrapSession(project, session)
		if err != nil {
			writeMappedError(w, err)
			return
		}
		if err := s.saveCompiledProjectConfig(r, project); err != nil {
			writeMappedError(w, err)
			return
		}
	}
	if fields := app.BootstrapSessionCredentialFields(req); len(fields) > 0 {
		s.writeCredentialAuditLog(r, project.ID, session.ID, fields)
	}
	if strategies := app.BootstrapSessionSecretStrategyFields(req); len(strategies) > 0 {
		s.writeSecretStrategyAuditLog(r, project.ID, session.ID, strategies)
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) compileProjectBootstrapSession(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	session, err := s.bootstrapSessions.Get(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	session, err = s.compileBootstrapSession(project, session)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if err := s.saveCompiledProjectConfig(r, project); err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) simulateBootstrapSession(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}

	session, err := s.bootstrapSessions.Get(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("failed to read request body: %w", err))
		return
	}
	var req bootstrapSimulatePRRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
			return
		}
	}

	session, validation, err := s.validateBootstrapSessionForCompilation(project, session)
	if err != nil {
		writeMappedError(w, err)
		return
	}

	templates := asBootstrapManifestTemplates(session.Data[bootstrapManifestTemplatesKey])
	if templates == nil {
		templates = []bootstrap.ManifestTemplate{}
	}
	files := make([]string, 0, len(templates))
	for _, item := range templates {
		files = append(files, bootstrapManifestTemplateFilePath(item))
	}
	response := bootstrapSimulatePRResponse{
		Validation:        validation,
		ManifestTemplates: templates,
	}
	if req.DryRunCommit || req.DryRunCommitSnake {
		response.DryRun = &bootstrapSimulatePRDryRun{
			Enabled:     true,
			CommitPath:  fmt.Sprintf("%s/simulations/%s", project.ID, time.Now().UTC().Format("20060102T150405Z")),
			FileCount:   len(templates),
			Files:       files,
			SimulatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		if validation.Valid {
			response.DryRun.Status = "simulated"
			response.DryRun.Message = "Dry-run commit simulation completed."
		} else {
			response.DryRun.Status = "failed"
			response.DryRun.Message = "Dry-run commit simulation failed due to template validation errors."
		}
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) validateBootstrapSessionForCompilation(project domain.Project, session domain.BootstrapSession) (domain.BootstrapSession, bootstrap.ManifestTemplateValidationResult, error) {
	session, err := s.refreshBootstrapManifestTemplates(project, session)
	if err != nil {
		return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, err
	}
	backend, backendErr := bootstrapSessionDeploymentBackend(session.Data)
	if backendErr != nil {
		return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, backendErr
	}
	if backend == domain.DeploymentBackendFluxCD {
		report, ok := asCapabilityReport(session.Data[bootstrapCapabilityReportKey])
		if !ok || !hasFluxCapabilities(report) {
			return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, app.ValidationError{Message: "fluxcd backend requires Flux capabilities in cluster capability report"}
		}
	}
	if policy, ok, parseErr := bootstrapResourcePolicyFromData(session.Data); parseErr != nil {
		return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, app.ValidationError{Message: fmt.Sprintf("invalid resource policy: %v", parseErr)}
	} else if ok {
		if validateErr := bootstrap.ValidateResourcePolicyConfig(policy); validateErr != nil {
			return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, app.ValidationError{Message: fmt.Sprintf("invalid resource policy: %v", validateErr)}
		}
	}
	if networkPolicy, ok, parseErr := bootstrapNetworkPolicyFromData(session.Data, bootstrapDefaultBaseNamespaces(project)); parseErr != nil {
		return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, app.ValidationError{Message: fmt.Sprintf("invalid network policy: %v", parseErr)}
	} else if ok {
		if validateErr := bootstrap.ValidateNetworkPolicyConfig(networkPolicy); validateErr != nil {
			return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, app.ValidationError{Message: fmt.Sprintf("invalid network policy: %v", validateErr)}
		}
	}
	if cleanupSafety, ok, targetNamespaces, parseErr := bootstrapCleanupSafetyFromData(session.Data); parseErr != nil {
		return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, app.ValidationError{Message: fmt.Sprintf("invalid cleanup safety config: %v", parseErr)}
	} else if ok {
		if validateErr := bootstrap.ValidateCleanupSafetyConfig(cleanupSafety, targetNamespaces); validateErr != nil {
			return domain.BootstrapSession{}, bootstrap.ManifestTemplateValidationResult{}, app.ValidationError{Message: fmt.Sprintf("invalid cleanup safety config: %v", validateErr)}
		}
	}
	validation := bootstrap.ValidateManifestTemplates(asBootstrapManifestTemplates(session.Data[bootstrapManifestTemplatesKey]))
	return session, validation, nil
}

func (s *Server) saveCompiledProjectConfig(r *http.Request, project domain.Project) error {
	if s.projectConfigs == nil {
		return fmt.Errorf("project config storage is not configured")
	}
	rawSession, err := s.bootstrapSessions.GetStored(project.ID)
	if err != nil {
		return err
	}
	_, _, user := s.auditActor(r, apiRole("public"))
	if strings.TrimSpace(user) == "" {
		user = rawSession.CreatedBy
	}
	config, err := s.projectConfigs.SaveFromBootstrapSession(project, rawSession, user)
	if err != nil {
		return err
	}
	s.writeProjectConfigAuditLog(r, project.ID, rawSession.ID, config.Version)
	return nil
}

func bootstrapSessionDeploymentBackend(data map[string]any) (domain.DeploymentBackend, error) {
	rawDeployment, ok := asStringAnyMap(data[bootstrapDeploymentConfigKey])
	if !ok {
		rawDeployment = map[string]any{}
	}
	backend := domain.InferDeploymentBackend(rawDeployment["backend"], rawDeployment, data)
	switch backend {
	case domain.DeploymentBackendHelmDirect, domain.DeploymentBackendFluxCD, domain.DeploymentBackendGitOpsManifest:
		return backend, nil
	case "":
		return domain.DeploymentBackendHelmDirect, nil
	default:
		return backend, app.ValidationError{Message: fmt.Sprintf("unsupported deployment.backend: %q", backend)}
	}
}

func hasFluxCapabilities(report domain.ClusterCapabilityReport) bool {
	if len(report.FluxCRDs) > 0 {
		return true
	}
	for _, flag := range report.CapabilityFlags {
		if strings.Contains(strings.ToLower(strings.TrimSpace(flag)), "flux") {
			return true
		}
	}
	return false
}

func (s *Server) getProjectConfig(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.projectConfigs == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("project config storage is not configured"))
		return
	}
	config, err := s.projectConfigs.Latest(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) writeCredentialAuditLog(r *http.Request, projectID string, sessionID string, fields []string) {
	role := apiRole("public")
	if value, ok := r.Context().Value(apiRequestRoleKey).(apiRole); ok && value != "" {
		role = value
	} else {
		role = s.apiRoleForToken(extractTokenFromRequest(r))
		if role == "" {
			role = apiRole("public")
		}
	}
	actorType, actorID, user := s.auditActor(r, role)
	entry := s.auditEventEntry(r, auditEventBootstrapSCMCredentialsSaved, auditEndpointBootstrapSessionUpdate, projectID, user, "success", "")
	entry["session_id"] = sessionID
	entry["fields"] = fields
	entry["role"] = string(role)
	entry["actor_type"] = actorType
	entry["actor"] = actorID
	entry["actor_user"] = user
	s.writeAuditLog(entry)
}

func (s *Server) auditEventEntry(r *http.Request, event string, endpoint string, projectID string, userID string, outcome string, reason string) map[string]any {
	if outcome = strings.TrimSpace(outcome); outcome == "" {
		outcome = "success"
	}
	return map[string]any{
		"ts":            time.Now().UTC().Format(time.RFC3339Nano),
		"event":         strings.TrimSpace(event),
		"endpoint":      strings.TrimSpace(endpoint),
		"project_id":    normalizeSettingsID(projectID),
		"user_id":       strings.TrimSpace(userID),
		"reason":        strings.TrimSpace(reason),
		"outcome":       outcome,
		"remote_addr":   clientIP(r),
		"trace_id":      r.Context().Value(apiRequestTraceIDKey),
		"trace_span_id": r.Context().Value(apiRequestSpanIDKey),
	}
}

func (s *Server) writeSecretStrategyAuditLog(r *http.Request, projectID string, sessionID string, secretIDs []string) {
	role := apiRole("public")
	if value, ok := r.Context().Value(apiRequestRoleKey).(apiRole); ok && value != "" {
		role = value
	} else {
		role = s.apiRoleForToken(extractTokenFromRequest(r))
		if role == "" {
			role = apiRole("public")
		}
	}
	actorType, actorID, user := s.auditActor(r, role)
	entry := s.auditEventEntry(r, auditEventBootstrapSecretStrategiesSaved, auditEndpointBootstrapSessionUpdate, projectID, user, "success", "")
	entry["session_id"] = sessionID
	entry["secret_ids"] = secretIDs
	entry["role"] = string(role)
	entry["actor_type"] = actorType
	entry["actor"] = actorID
	entry["actor_user"] = user
	s.writeAuditLog(entry)
}

func (s *Server) writeProjectConfigAuditLog(r *http.Request, projectID string, sessionID string, version int) {
	role := apiRole("public")
	if value, ok := r.Context().Value(apiRequestRoleKey).(apiRole); ok && value != "" {
		role = value
	} else {
		role = s.apiRoleForToken(extractTokenFromRequest(r))
		if role == "" {
			role = apiRole("public")
		}
	}
	actorType, actorID, user := s.auditActor(r, role)
	entry := s.auditEventEntry(r, auditEventProjectConfigSaved, auditEndpointBootstrapSessionCompile, projectID, user, "success", "")
	entry["session_id"] = sessionID
	entry["config_version"] = version
	entry["role"] = string(role)
	entry["actor_type"] = actorType
	entry["actor"] = actorID
	entry["actor_user"] = user
	s.writeAuditLog(entry)
}

func (s *Server) writeRunnerHeartbeatAuthFailureAuditLog(r *http.Request, projectID string, runnerID string, token string, reason string) {
	s.writeBootstrapAuthAuditLog(r, auditEventRunnerHeartbeatAuthFailed, auditEndpointRunnerHeartbeat, projectID, auditSubjectRunnerID, runnerID, token, reason)
}

func (s *Server) writeBootstrapAuthAuditLog(r *http.Request, event string, endpoint string, projectID string, subjectKey string, subjectID string, token string, reason string) {
	entry := s.bootstrapAuditEntry(r, event, endpoint, projectID, subjectKey, subjectID, token)
	entry["outcome"] = "failure"
	if trimmedReason := strings.TrimSpace(reason); trimmedReason != "" {
		entry["reason"] = trimmedReason
	}
	s.writeAuditLog(entry)
}

func (s *Server) writeBootstrapSuccessAuditLog(r *http.Request, event string, endpoint string, projectID string, subjectKey string, subjectID string, token string) {
	entry := s.bootstrapAuditEntry(r, event, endpoint, projectID, subjectKey, subjectID, token)
	entry["outcome"] = "success"
	s.writeAuditLog(entry)
}

func (s *Server) writeBootstrapRateLimitAuditLog(r *http.Request, endpoint string, projectID string, subjectKey string, subjectID string, token string, reason string) {
	entry := s.bootstrapAuditEntry(r, auditEventBootstrapRateLimitHit, endpoint, projectID, subjectKey, subjectID, token)
	entry["outcome"] = "rate_limited"
	if trimmedReason := strings.TrimSpace(reason); trimmedReason != "" {
		entry["reason"] = trimmedReason
	}
	s.writeAuditLog(entry)
}

func (s *Server) bootstrapAuditEntry(r *http.Request, event string, endpoint string, projectID string, subjectKey string, subjectID string, token string) map[string]any {
	entry := map[string]any{
		"ts":            time.Now().UTC().Format(time.RFC3339Nano),
		"event":         strings.TrimSpace(event),
		"endpoint":      strings.TrimSpace(endpoint),
		"project_id":    normalizeSettingsID(projectID),
		"user_id":       "",
		"reason":        "",
		"outcome":       "",
		"remote_addr":   clientIP(r),
		"trace_id":      r.Context().Value(apiRequestTraceIDKey),
		"trace_span_id": r.Context().Value(apiRequestSpanIDKey),
	}
	if subjectKey = strings.TrimSpace(subjectKey); subjectKey != "" {
		entry[subjectKey] = strings.TrimSpace(subjectID)
	}
	if strings.TrimSpace(token) != "" {
		entry["token_fingerprint"] = tokenFingerprint(token)
	}
	return entry
}

func (s *Server) createBootstrapAgentToken(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req domain.AgentRegistrationTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	clusterID := firstNonEmpty(req.ClusterID, req.ClusterIDSnake, project.ClusterID, "default")
	agentNamespace := strings.TrimSpace(req.AgentNamespace)
	if agentNamespace == "" {
		agentNamespace = "envpilot-system"
	}
	releaseName := strings.TrimSpace(req.ReleaseName)
	if releaseName == "" {
		releaseName = "envpilot-discovery-agent"
	}
	token, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to generate registration token: %w", err))
		return
	}
	now := time.Now().UTC()
	ttl := s.config().AgentRegistrationTokenTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	expiresAt := now.Add(ttl)
	_, err = s.bootstrapSessions.Update(project.ID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapAgentTokenHashKey:      hashToken(token),
			bootstrapAgentTokenProjectKey:   project.ID,
			bootstrapAgentTokenIssuedAtKey:  now.Format(time.RFC3339Nano),
			bootstrapAgentTokenExpiresAtKey: expiresAt.Format(time.RFC3339Nano),
			bootstrapAgentTokenUsedAtKey:    "",
			bootstrapAgentStatusKey:         "waiting",
			bootstrapAgentClusterIDKey:      clusterID,
			bootstrapAgentErrorKey:          "",
		},
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	bootstrapSecretName := "envpilot-agent-bootstrap"
	registrationTokenKey := "registration-token"
	response := domain.AgentRegistrationTokenResponse{
		ProjectID:         project.ID,
		ClusterID:         clusterID,
		AgentNamespace:    agentNamespace,
		ReleaseName:       releaseName,
		RegistrationToken: token,
		ExpiresAt:         expiresAt,
		BootstrapSecretCommand: fmt.Sprintf(
			"kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f - && kubectl create secret generic %s --namespace %s --from-literal=%s=%q --dry-run=client -o yaml | kubectl apply -f -",
			agentNamespace,
			bootstrapSecretName,
			agentNamespace,
			registrationTokenKey,
			token,
		),
		BootstrapSecretCommandSensitive: true,
		HelmCommand: fmt.Sprintf(
			"helm upgrade --install %s envpilot/envpilot-agent --namespace %s --create-namespace --set controlPlane.url=%q --set controlPlane.existingSecret=%q --set controlPlane.registrationTokenKey=%q --set cluster.id=%q --set bootstrap.projectId=%q",
			releaseName,
			agentNamespace,
			controlPlaneURLFromRequest(r),
			bootstrapSecretName,
			registrationTokenKey,
			clusterID,
			project.ID,
		),
		Status: "waiting",
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getBootstrapAgentStatus(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	session, err := s.bootstrapSessions.Get(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	response := domain.BootstrapAgentStatusResponse{
		Status:             firstNonEmpty(asString(session.Data[bootstrapAgentStatusKey]), "waiting"),
		ClusterID:          asString(session.Data[bootstrapAgentClusterIDKey]),
		AgentID:            asString(session.Data[bootstrapAgentIDKey]),
		ResourceScanStatus: firstNonEmpty(asString(session.Data[bootstrapResourceScanStatusKey]), "idle"),
		Error:              asString(session.Data[bootstrapAgentErrorKey]),
		SelectedNamespaces: asStringSlice(session.Data[bootstrapSelectedNamespacesKey]),
	}
	if timestamp, ok := parseRFC3339Pointer(asString(session.Data[bootstrapAgentLastSeenAtKey])); ok {
		response.LastSeenAt = timestamp
	}
	if timestamp, ok := parseRFC3339Pointer(asString(session.Data[bootstrapAgentTokenIssuedAtKey])); ok {
		response.TokenIssuedAt = timestamp
	}
	if timestamp, ok := parseRFC3339Pointer(asString(session.Data[bootstrapAgentTokenExpiresAtKey])); ok {
		response.TokenExpiresAt = timestamp
	}
	if report, ok := asCapabilityReport(session.Data[bootstrapCapabilityReportKey]); ok {
		response.CapabilityReport = &report
	}
	response.ResourceCount = len(asResourceSnapshots(session.Data[bootstrapResourceScanReportKey]))
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) generateBootstrapRunnerDeploymentInstructions(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req domain.RunnerDeploymentInstructionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.DeploymentMode))
	if mode == "" {
		mode = string(domain.RunnerDeploymentModeHelm)
	}
	if mode != string(domain.RunnerDeploymentModeHelm) && mode != string(domain.RunnerDeploymentModeGitOps) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("deploymentMode must be helm or gitops"))
		return
	}
	projectID := normalizeSettingsID(r.PathValue("id"))
	if projectID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("project ID is required"))
		return
	}
	clusterID := firstNonEmpty(req.ClusterID, req.ClusterIDSnake, project.ClusterID, "default")
	runnerNamespace := strings.TrimSpace(req.RunnerNamespace)
	if runnerNamespace == "" {
		runnerNamespace = "envpilot-system"
	}
	releaseName := strings.TrimSpace(req.ReleaseName)
	if releaseName == "" {
		releaseName = fmt.Sprintf("envpilot-runner-%s", project.ID)
	}
	gitOpsPath := strings.TrimSpace(req.GitOpsPath)
	if gitOpsPath == "" {
		gitOpsPath = strings.TrimSpace(req.GitOpsPathSnake)
	}
	runnerID := safeIdentifier(fmt.Sprintf("%s-runner", project.ID))
	if runnerID == "" {
		runnerID = "envpilot-runner"
	}
	runnerNamespace = sanitizePathComponent(runnerNamespace)
	if runnerNamespace == "" {
		runnerNamespace = "envpilot-system"
	}
	releaseName = sanitizePathComponent(releaseName)
	if releaseName == "" {
		releaseName = fmt.Sprintf("envpilot-runner-%s", project.ID)
	}
	lock := s.runnerDeploymentInstructionMutex(project.ID)
	lock.Lock()
	defer lock.Unlock()

	if session, err := s.bootstrapSessions.Get(project.ID); err == nil && s.bootstrapRunnerInstructionsAlreadyDisplayed(session, clusterID, runnerID, mode, runnerNamespace, releaseName) {
		response := s.maskedBootstrapRunnerDeploymentInstructionsResponse(r, project.ID, clusterID, runnerID, mode, runnerNamespace, releaseName, session)
		if err := writeJSONWithError(w, http.StatusOK, response); err != nil && s.logger != nil {
			s.logger.Error("failed to write runner deployment instructions response", "project_id", project.ID, "error", err)
		}
		return
	}

	registrationToken, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to generate registration token: %w", err))
		return
	}
	projectConfigToken, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to generate project config token: %w", err))
		return
	}
	now := time.Now().UTC()
	ttl := s.bootstrapRunnerTokenTTL()
	expiresAt := now.Add(ttl)
	if err := s.createBootstrapRunnerState(project.ID, registrationToken, projectConfigToken, clusterID, runnerID, mode, runnerNamespace, releaseName, now, expiresAt); err != nil {
		writeMappedError(w, err)
		return
	}
	projectConfigURL := fmt.Sprintf("%s/api/v1/projects/%s/runner-config", controlPlaneURLFromRequest(r), project.ID)
	response := domain.RunnerDeploymentInstructionsResponse{
		ProjectID:                       project.ID,
		ClusterID:                       clusterID,
		DeploymentMode:                  domain.RunnerDeploymentMode(mode),
		RunnerNamespace:                 runnerNamespace,
		ReleaseName:                     releaseName,
		RegistrationToken:               "[masked]",
		ProjectConfigToken:              "[masked]",
		ProjectConfigURL:                projectConfigURL,
		ExpiresAt:                       expiresAt,
		Status:                          "waiting",
		BootstrapSecretCommandSensitive: mode == string(domain.RunnerDeploymentModeHelm),
	}
	switch mode {
	case string(domain.RunnerDeploymentModeGitOps):
		if gitOpsPath == "" {
			gitOpsPath = fmt.Sprintf("gitops/runners/%s-runner.yaml", project.ID)
		}
		response.GitOpsPath = gitOpsPath
		response.GitOpsManifest = s.renderRunnerGitOpsManifest(
			controlPlaneURLFromRequest(r),
			project.ID,
			clusterID,
			releaseName,
			runnerNamespace,
			projectConfigURL,
		)
	default:
		bootstrapSecretName := "envpilot-runner-bootstrap"
		response.BootstrapSecretCommand = fmt.Sprintf(
			"kubectl create secret generic %s --namespace %s --from-literal=token=%q --from-literal=project-config-token=%q --dry-run=client -o yaml | kubectl apply -f -",
			bootstrapSecretName,
			runnerNamespace,
			registrationToken,
			projectConfigToken,
		)
		response.HelmCommand = fmt.Sprintf(
			"helm upgrade --install %s envpilot/envpilot-runner --namespace %s --create-namespace --set controlPlane.url=%q --set controlPlane.existingSecret=%q --set project.id=%q --set project.runnerId=%q --set project.namespace=%q --set project.deploymentMode=%q --set project.configUrl=%q",
			releaseName,
			runnerNamespace,
			controlPlaneURLFromRequest(r),
			bootstrapSecretName,
			project.ID,
			runnerID,
			runnerNamespace,
			mode,
			projectConfigURL,
		)
	}
	if err := s.markBootstrapRunnerDeploymentInstructionsDisplayed(project.ID); err != nil {
		writeMappedError(w, err)
		return
	}
	if err := writeJSONWithError(w, http.StatusOK, response); err != nil {
		if s.logger != nil {
			s.logger.Error("failed to write runner deployment instructions response", "project_id", project.ID, "error", err)
		}
		return
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventRunnerBootstrapTokenGenerated, auditEndpointRunnerDeploymentInstructions, project.ID, auditSubjectRunnerID, runnerID, registrationToken)
}

func (s *Server) bootstrapRunnerTokenTTL() time.Duration {
	ttl := s.config().AgentRegistrationTokenTTL
	if ttl <= 0 || ttl > 5*time.Minute {
		return 5 * time.Minute
	}
	return ttl
}

func (s *Server) bootstrapRunnerInstructionsAlreadyDisplayed(session domain.BootstrapSession, clusterID, runnerID, mode, namespace, releaseName string) bool {
	if strings.TrimSpace(asString(session.Data[bootstrapRunnerSecretCommandDisplayedAtKey])) == "" {
		return false
	}
	if strings.TrimSpace(asString(session.Data[bootstrapRunnerTokenHashKey])) == "" || strings.TrimSpace(asString(session.Data[bootstrapRunnerConfigTokenHashKey])) == "" {
		return false
	}
	if strings.TrimSpace(asString(session.Data[bootstrapRunnerClusterIDKey])) != strings.TrimSpace(clusterID) {
		return false
	}
	if strings.TrimSpace(asString(session.Data[bootstrapRunnerIDKey])) != strings.TrimSpace(runnerID) {
		return false
	}
	if strings.ToLower(strings.TrimSpace(asString(session.Data[bootstrapRunnerModeKey]))) != strings.ToLower(strings.TrimSpace(mode)) {
		return false
	}
	if strings.TrimSpace(asString(session.Data[bootstrapRunnerNamespaceKey])) != strings.TrimSpace(namespace) {
		return false
	}
	if strings.TrimSpace(asString(session.Data[bootstrapRunnerReleaseNameKey])) != strings.TrimSpace(releaseName) {
		return false
	}
	return true
}

func (s *Server) maskedBootstrapRunnerDeploymentInstructionsResponse(r *http.Request, projectID, clusterID, runnerID, mode, namespace, releaseName string, session domain.BootstrapSession) domain.RunnerDeploymentInstructionsResponse {
	expiresAt := time.Time{}
	if parsed, ok := parseRFC3339Pointer(asString(session.Data[bootstrapRunnerTokenExpiresAtKey])); ok && parsed != nil {
		expiresAt = *parsed
	}
	projectConfigURL := fmt.Sprintf("%s/api/v1/projects/%s/runner-config", controlPlaneURLFromRequest(r), projectID)
	response := domain.RunnerDeploymentInstructionsResponse{
		ProjectID:                       projectID,
		ClusterID:                       clusterID,
		DeploymentMode:                  domain.RunnerDeploymentMode(mode),
		RunnerNamespace:                 namespace,
		ReleaseName:                     releaseName,
		RegistrationToken:               "[masked]",
		ProjectConfigToken:              "[masked]",
		ProjectConfigURL:                projectConfigURL,
		ExpiresAt:                       expiresAt,
		Status:                          firstNonEmpty(asString(session.Data[bootstrapRunnerStatusKey]), "waiting"),
		BootstrapSecretCommandSensitive: mode == string(domain.RunnerDeploymentModeHelm),
	}
	switch mode {
	case string(domain.RunnerDeploymentModeGitOps):
		response.GitOpsManifest = s.renderRunnerGitOpsManifest(controlPlaneURLFromRequest(r), projectID, clusterID, releaseName, namespace, projectConfigURL)
	default:
		bootstrapSecretName := "envpilot-runner-bootstrap"
		response.BootstrapSecretCommand = fmt.Sprintf(
			"kubectl create secret generic %s --namespace %s --from-literal=token=%q --from-literal=project-config-token=%q --dry-run=client -o yaml | kubectl apply -f -",
			bootstrapSecretName,
			namespace,
			"[masked]",
			"[masked]",
		)
		response.HelmCommand = fmt.Sprintf(
			"helm upgrade --install %s envpilot/envpilot-runner --namespace %s --create-namespace --set controlPlane.url=%q --set controlPlane.existingSecret=%q --set project.id=%q --set project.runnerId=%q --set project.namespace=%q --set project.deploymentMode=%q --set project.configUrl=%q",
			releaseName,
			namespace,
			controlPlaneURLFromRequest(r),
			bootstrapSecretName,
			projectID,
			runnerID,
			namespace,
			mode,
			projectConfigURL,
		)
	}
	return response
}

func (s *Server) getBootstrapRunnerStatus(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	session, err := s.bootstrapSessions.Get(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	response := domain.RunnerStatusResponse{
		Status:          firstNonEmpty(asString(session.Data[bootstrapRunnerStatusKey]), "waiting"),
		ClusterID:       asString(session.Data[bootstrapRunnerClusterIDKey]),
		RunnerID:        asString(session.Data[bootstrapRunnerIDKey]),
		DeploymentMode:  strings.TrimSpace(asString(session.Data[bootstrapRunnerModeKey])),
		RunnerNamespace: asString(session.Data[bootstrapRunnerNamespaceKey]),
		Error:           asString(session.Data[bootstrapRunnerErrorKey]),
	}
	if response.DeploymentMode == "" {
		response.DeploymentMode = string(domain.RunnerDeploymentModeHelm)
	}
	if response.RunnerNamespace == "" {
		response.RunnerNamespace = "envpilot-system"
	}
	if response.Status == "" {
		response.Status = "waiting"
	}
	if timestamp, ok := parseRFC3339Pointer(asString(session.Data[bootstrapRunnerLastSeenAtKey])); ok {
		response.LastSeenAt = timestamp
	}
	if timestamp, ok := parseRFC3339Pointer(asString(session.Data[bootstrapRunnerTokenIssuedAtKey])); ok {
		response.TokenIssuedAt = timestamp
	}
	if timestamp, ok := parseRFC3339Pointer(asString(session.Data[bootstrapRunnerTokenExpiresAtKey])); ok {
		response.TokenExpiresAt = timestamp
	}
	response.ProjectConfigURL = fmt.Sprintf("%s/api/v1/projects/%s/runner-config", controlPlaneURLFromRequest(r), project.ID)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getBootstrapRunnerConfig(w http.ResponseWriter, r *http.Request) {
	projectID := normalizeSettingsID(r.PathValue("id"))
	if projectID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("projectId is required"))
		return
	}
	if s.projectConfigs == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("project config storage is not configured"))
		return
	}
	req, err := decodeRunnerConfigRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token := runnerConfigTokenFromRequest(r, req)
	if token == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventRunnerConfigFetchAuthFailed, auditEndpointRunnerConfigFetch, projectID, auditSubjectRunnerID, "", token, errors.New("project config token is required"))
		return
	}
	clusterID := strings.TrimSpace(firstNonEmpty(req.ClusterID, req.ClusterIDSnake))
	runnerID := strings.TrimSpace(firstNonEmpty(req.RunnerID, req.RunnerIDSnake))
	runnerNamespace := strings.TrimSpace(firstNonEmpty(req.RunnerNamespace, req.RunnerNamespaceSnake))
	deploymentMode := strings.ToLower(strings.TrimSpace(firstNonEmpty(req.DeploymentMode, req.DeploymentModeSnake)))
	if clusterID == "" || runnerID == "" || runnerNamespace == "" || deploymentMode == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventRunnerConfigFetchAuthFailed, auditEndpointRunnerConfigFetch, projectID, auditSubjectRunnerID, runnerID, token, errors.New("runner identity is required"))
		return
	}
	if err := s.validateBootstrapRunnerConfigToken(projectID, token, clusterID, runnerID, runnerNamespace, deploymentMode); err != nil {
		s.rejectBootstrapAuthFailure(w, r, bootstrapAuthFailureStatus(err), auditEventRunnerConfigFetchAuthFailed, auditEndpointRunnerConfigFetch, projectID, auditSubjectRunnerID, runnerID, token, err)
		return
	}
	config, err := s.projectConfigs.Latest(projectID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	response := runnerProjectConfigResponseFromDomain(config)
	if err := s.markBootstrapRunnerConfigTokenUsed(projectID); err != nil {
		writeMappedError(w, err)
		return
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventRunnerConfigFetchSucceeded, auditEndpointRunnerConfigFetch, projectID, auditSubjectRunnerID, runnerID, token)
	writeJSON(w, http.StatusOK, response)
}

type runnerProjectConfigResponse struct {
	ProjectID     string         `json:"project_id"`
	Version       int            `json:"version"`
	Config        map[string]any `json:"config,omitempty"`
	SensitiveRefs map[string]any `json:"sensitive_refs,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

func runnerProjectConfigResponseFromDomain(config domain.ProjectConfig) runnerProjectConfigResponse {
	return runnerProjectConfigResponse{
		ProjectID:     config.ProjectID,
		Version:       config.Version,
		Config:        config.Config,
		SensitiveRefs: config.SensitiveRefs,
		CreatedAt:     config.CreatedAt,
	}
}

type runnerConfigRequest struct {
	ClusterID            string `json:"clusterId,omitempty"`
	ClusterIDSnake       string `json:"cluster_id,omitempty"`
	RunnerID             string `json:"runnerId,omitempty"`
	RunnerIDSnake        string `json:"runner_id,omitempty"`
	RunnerNamespace      string `json:"runnerNamespace,omitempty"`
	RunnerNamespaceSnake string `json:"runner_namespace,omitempty"`
	DeploymentMode       string `json:"deploymentMode,omitempty"`
	DeploymentModeSnake  string `json:"deployment_mode,omitempty"`
	ProjectConfigToken   string `json:"projectConfigToken,omitempty"`
	ProjectConfigToken_  string `json:"project_config_token,omitempty"`
	RegistrationToken    string `json:"registrationToken,omitempty"`
	RegistrationToken_   string `json:"registration_token,omitempty"`
}

func decodeRunnerConfigRequest(r *http.Request) (runnerConfigRequest, error) {
	var req runnerConfigRequest
	if r.Body == nil {
		return req, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return runnerConfigRequest{}, fmt.Errorf("invalid request body: %w", err)
	}
	if strings.TrimSpace(req.ProjectConfigToken) != "" || strings.TrimSpace(req.ProjectConfigToken_) != "" ||
		strings.TrimSpace(req.RegistrationToken) != "" || strings.TrimSpace(req.RegistrationToken_) != "" {
		return runnerConfigRequest{}, fmt.Errorf("project config token must be sent only in the Authorization header")
	}
	return req, nil
}

func runnerConfigTokenFromRequest(r *http.Request, req runnerConfigRequest) string {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if token, ok := strings.CutPrefix(authorization, "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	if token, ok := strings.CutPrefix(authorization, "bearer "); ok {
		return strings.TrimSpace(token)
	}
	return ""
}

func agentRegistrationResponse(cluster domain.ClusterTarget, agentAuthToken string) domain.AgentRegistrationResponse {
	return domain.AgentRegistrationResponse{
		ID:              cluster.ID,
		Name:            cluster.Name,
		Provider:        cluster.Provider,
		AgentID:         cluster.AgentID,
		AgentStatus:     cluster.AgentStatus,
		LastHeartbeatAt: cluster.LastHeartbeatAt,
		Enabled:         cluster.Enabled,
		AgentAuthToken:  strings.TrimSpace(agentAuthToken),
	}
}

func agentHeartbeatAuthToken(req domain.AgentHeartbeatRequest) string {
	return strings.TrimSpace(req.AgentAuthToken)
}

func agentAuthTokenFromRequest(r *http.Request, tokens ...string) string {
	if r != nil {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		if token, ok := strings.CutPrefix(authorization, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
		if token, ok := strings.CutPrefix(authorization, "bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	return strings.TrimSpace(firstNonEmpty(tokens...))
}

func runnerHeartbeatAuthToken(req domain.RunnerHeartbeatRequest) string {
	return strings.TrimSpace(req.RunnerAuthToken)
}

func (s *Server) runnersHealthcheck(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"component": "runner",
		"at":        time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) createBootstrapRunnerState(projectID, registrationToken, projectConfigToken, clusterID, runnerID, mode, namespace, releaseName string, now, expiresAt time.Time) error {
	sessionPatch := app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapRunnerTokenHashKey:                hashToken(registrationToken),
			bootstrapRunnerTokenProjectKey:             projectID,
			bootstrapRunnerTokenIssuedAtKey:            now.Format(time.RFC3339Nano),
			bootstrapRunnerTokenExpiresAtKey:           expiresAt.Format(time.RFC3339Nano),
			bootstrapRunnerTokenUsedAtKey:              "",
			bootstrapRunnerConfigTokenHashKey:          hashToken(projectConfigToken),
			bootstrapRunnerConfigTokenProjectKey:       projectID,
			bootstrapRunnerConfigTokenIssuedAtKey:      now.Format(time.RFC3339Nano),
			bootstrapRunnerConfigTokenExpiresAtKey:     expiresAt.Format(time.RFC3339Nano),
			bootstrapRunnerConfigTokenUsedAtKey:        "",
			bootstrapRunnerStatusKey:                   "waiting",
			bootstrapRunnerClusterIDKey:                clusterID,
			bootstrapRunnerIDKey:                       runnerID,
			bootstrapRunnerModeKey:                     mode,
			bootstrapRunnerNamespaceKey:                namespace,
			bootstrapRunnerReleaseNameKey:              releaseName,
			bootstrapRunnerSecretCommandDisplayedAtKey: "",
			bootstrapRunnerErrorKey:                    "",
		},
	}
	_, err := s.bootstrapSessions.Update(projectID, sessionPatch)
	return err
}

func (s *Server) markBootstrapRunnerDeploymentInstructionsDisplayed(projectID string) error {
	_, err := s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapRunnerSecretCommandDisplayedAtKey: time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
	return err
}

func (s *Server) runnerDeploymentInstructionMutex(projectID string) *sync.Mutex {
	normalized := normalizeSettingsID(projectID)
	if normalized == "" {
		normalized = strings.TrimSpace(projectID)
	}
	actual, _ := s.runnerDeploymentInstructionMu.LoadOrStore(normalized, &sync.Mutex{})
	mutex, _ := actual.(*sync.Mutex)
	return mutex
}

func (s *Server) runnerRegistrationMutex(projectID string) *sync.Mutex {
	normalized := normalizeSettingsID(projectID)
	if normalized == "" {
		normalized = strings.TrimSpace(projectID)
	}
	actual, _ := s.runnerRegistrationMu.LoadOrStore(normalized, &sync.Mutex{})
	mutex, _ := actual.(*sync.Mutex)
	return mutex
}

func (s *Server) agentRegistrationMutex(projectID string) *sync.Mutex {
	normalized := normalizeSettingsID(projectID)
	if normalized == "" {
		normalized = strings.TrimSpace(projectID)
	}
	actual, _ := s.agentRegistrationMu.LoadOrStore(normalized, &sync.Mutex{})
	mutex, _ := actual.(*sync.Mutex)
	return mutex
}

func (s *Server) renderRunnerGitOpsManifest(controlPlaneURL, projectID, clusterID, releaseName, namespace, configURL string) string {
	runnerName := sanitizePathComponent(fmt.Sprintf("%s-runner", projectID))
	if runnerName == "" {
		runnerName = "envpilot-runner"
	}
	releaseName = sanitizePathComponent(strings.TrimSpace(releaseName))
	if releaseName == "" {
		releaseName = runnerName
	}
	namespace = sanitizePathComponent(strings.TrimSpace(namespace))
	if namespace == "" {
		namespace = "envpilot-system"
	}
	secretName := fmt.Sprintf("%s-token", runnerName)

	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
# IMPORTANT: create the runner registration token secret out-of-band.
# Do not commit live tokens to Git.
# Example:
# kubectl -n %s create secret generic %s --from-literal=ENVPILOT_RUNNER_REGISTRATION_TOKEN='<runner-registration-token>' --from-literal=ENVPILOT_PROJECT_CONFIG_TOKEN='<project-config-token>' --dry-run=client -o yaml | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: %s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %s-rbac
rules:
  - apiGroups: [""]
    resources:
      - namespaces
      - pods
      - services
      - configmaps
      - secrets
      - events
    verbs: ["get","list","watch"]
  - apiGroups: ["apps"]
    resources:
      - deployments
      - statefulsets
    verbs: ["get","list","watch"]
  - apiGroups: ["batch"]
    resources:
      - jobs
      - cronjobs
    verbs: ["get","list","watch"]
  - apiGroups: ["networking.k8s.io"]
    resources:
      - ingresses
      - networkpolicies
    verbs: ["get","list","watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %s-rbac
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %s-rbac
subjects:
  - kind: ServiceAccount
    name: %s
    namespace: %s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: %s
    app.kubernetes.io/managed-by: envpilot
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: %s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %s
    spec:
      serviceAccountName: %s
      containers:
        - name: runner
          image: ghcr.io/envpilot/envpilot-runner:latest
          imagePullPolicy: IfNotPresent
          env:
            - name: ENVPILOT_CONTROL_PLANE_URL
              value: %q
            - name: ENVPILOT_CLUSTER_ID
              value: %q
            - name: ENVPILOT_PROJECT_ID
              value: %q
            - name: ENVPILOT_RUNNER_ID
              value: %q
            - name: ENVPILOT_RUNNER_NAMESPACE
              value: %q
            - name: ENVPILOT_RUNNER_DEPLOYMENT_MODE
              value: "gitops"
            - name: ENVPILOT_PROJECT_CONFIG_URL
              value: %q
            - name: ENVPILOT_PROJECT_CONFIG_TOKEN
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: ENVPILOT_PROJECT_CONFIG_TOKEN
            - name: ENVPILOT_RUNNER_REGISTRATION_TOKEN
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: ENVPILOT_RUNNER_REGISTRATION_TOKEN
          livenessProbe:
            httpGet:
              path: /health
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 2
          readinessProbe:
            httpGet:
              path: /health
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 2
	`, namespace, namespace, secretName, runnerName, namespace, runnerName, runnerName, runnerName, runnerName, namespace, releaseName, namespace, runnerName, runnerName, runnerName, runnerName, controlPlaneURL, clusterID, projectID, runnerName, namespace, configURL, secretName, secretName)
}

func (s *Server) startBootstrapResourceScan(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	session, err := s.bootstrapSessions.Get(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if len(asStringSlice(session.Data[bootstrapSelectedNamespacesKey])) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("at least one namespace must be selected"))
		return
	}
	namespaces := asStringSlice(session.Data[bootstrapSelectedNamespacesKey])
	requestedAt := time.Now().UTC()
	updated, err := s.bootstrapSessions.Update(project.ID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapResourceScanStatusKey:    "pending",
			bootstrapResourceScanStartedAtKey: requestedAt.Format(time.RFC3339Nano),
			bootstrapResourceScanTaskKey: map[string]any{
				"projectId":   project.ID,
				"clusterId":   asString(session.Data[bootstrapAgentClusterIDKey]),
				"agentId":     asString(session.Data[bootstrapAgentIDKey]),
				"namespaces":  namespaces,
				"status":      "pending",
				"requestedAt": requestedAt.Format(time.RFC3339Nano),
			},
			bootstrapAgentErrorKey: "",
		},
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, updated)
}

func (s *Server) getProjectHybridConfig(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	writeJSON(w, http.StatusOK, projectHybridConfigResponse(project))
}

func (s *Server) saveProjectHybridConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		BaseNamespace           *string          `json:"baseNamespace"`
		BaseNamespaceSnake      *string          `json:"base_namespace"`
		BaseDomain              *string          `json:"baseDomain"`
		BaseDomainSnake         *string          `json:"base_domain"`
		BaseIngress             *string          `json:"baseIngress"`
		BaseIngressSnake        *string          `json:"base_ingress"`
		SharedDependencies      *[]string        `json:"sharedDependencies"`
		SharedDependenciesSnake *[]string        `json:"shared_dependencies"`
		HybridOverrides         *map[string]bool `json:"hybrid_overrides"`
		HybridOverridesCamel    *map[string]bool `json:"hybridOverrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	project, err := s.projects.GetProject(id)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if req.BaseNamespace != nil {
		project.BaseEnvConfig.Namespace = strings.TrimSpace(*req.BaseNamespace)
	}
	if req.BaseNamespaceSnake != nil {
		project.BaseEnvConfig.Namespace = strings.TrimSpace(*req.BaseNamespaceSnake)
	}
	if req.BaseDomain != nil {
		project.BaseEnvConfig.Domain = strings.TrimSpace(*req.BaseDomain)
	}
	if req.BaseDomainSnake != nil {
		project.BaseEnvConfig.Domain = strings.TrimSpace(*req.BaseDomainSnake)
	}
	if req.BaseIngress != nil {
		project.BaseEnvConfig.ConfigPath = strings.TrimSpace(*req.BaseIngress)
	}
	if req.BaseIngressSnake != nil {
		project.BaseEnvConfig.ConfigPath = strings.TrimSpace(*req.BaseIngressSnake)
	}
	if req.SharedDependencies != nil {
		project.BaseEnvConfig.Services = baseServicesFromNames(*req.SharedDependencies)
	}
	if req.SharedDependenciesSnake != nil {
		project.BaseEnvConfig.Services = baseServicesFromNames(*req.SharedDependenciesSnake)
	}
	overrides := project.BaseEnvConfig.HybridOverrides
	if req.HybridOverrides != nil {
		overrides = *req.HybridOverrides
	}
	if req.HybridOverridesCamel != nil {
		overrides = *req.HybridOverridesCamel
	}
	project.BaseEnvConfig.HybridOverrides = overrides
	saved, err := s.projects.SaveProject(project)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectHybridConfigResponse(saved))
}

func (s *Server) getProjectCostPolicy(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	writeJSON(w, http.StatusOK, projectCostPolicyResponse(project.ID, project.CostPolicy))
}

func (s *Server) saveProjectCostPolicy(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	var req struct {
		DefaultTTLHoursCamel         *int  `json:"defaultTTLHours"`
		DefaultTTLHours              *int  `json:"default_ttl_hours"`
		MaxActiveEnvsCamel           *int  `json:"maxActiveEnvs"`
		MaxActiveEnvs                *int  `json:"max_active_envs"`
		MaxActiveEnvsPerProjectCamel *int  `json:"maxActiveEnvsPerProject"`
		MaxActiveEnvsPerProject      *int  `json:"max_active_envs_per_project"`
		CPUQuotaCamel                *int  `json:"cpuQuota"`
		CPUQuota                     *int  `json:"cpu_quota"`
		MaxCPUPerEnvCamel            *int  `json:"maxCPUPerEnv"`
		MaxCPUPerEnv                 *int  `json:"max_cpu_per_env"`
		MemoryQuotaCamel             *int  `json:"memoryQuota"`
		MemoryQuota                  *int  `json:"memory_quota"`
		MaxMemoryPerEnvCamel         *int  `json:"maxMemoryPerEnv"`
		MaxMemoryPerEnv              *int  `json:"max_memory_per_env"`
		IdleTimeoutHoursCamel        *int  `json:"idleTimeoutHours"`
		IdleTimeoutHours             *int  `json:"idle_timeout_hours"`
		AutoDeleteIdleEnvsCamel      *bool `json:"autoDeleteIdleEnvs"`
		AutoDeleteIdleEnvs           *bool `json:"auto_delete_idle_envs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	policy := project.CostPolicy
	if req.DefaultTTLHours != nil {
		policy.DefaultTTLHours = *req.DefaultTTLHours
	}
	if req.DefaultTTLHoursCamel != nil {
		policy.DefaultTTLHours = *req.DefaultTTLHoursCamel
	}
	if req.MaxActiveEnvs != nil {
		policy.MaxActiveEnvsPerProject = *req.MaxActiveEnvs
	}
	if req.MaxActiveEnvsCamel != nil {
		policy.MaxActiveEnvsPerProject = *req.MaxActiveEnvsCamel
	}
	if req.MaxActiveEnvsPerProject != nil {
		policy.MaxActiveEnvsPerProject = *req.MaxActiveEnvsPerProject
	}
	if req.MaxActiveEnvsPerProjectCamel != nil {
		policy.MaxActiveEnvsPerProject = *req.MaxActiveEnvsPerProjectCamel
	}
	if req.CPUQuota != nil {
		policy.MaxCPUPerEnv = *req.CPUQuota
	}
	if req.CPUQuotaCamel != nil {
		policy.MaxCPUPerEnv = *req.CPUQuotaCamel
	}
	if req.MaxCPUPerEnv != nil {
		policy.MaxCPUPerEnv = *req.MaxCPUPerEnv
	}
	if req.MaxCPUPerEnvCamel != nil {
		policy.MaxCPUPerEnv = *req.MaxCPUPerEnvCamel
	}
	if req.MemoryQuota != nil {
		policy.MaxMemoryPerEnv = *req.MemoryQuota
	}
	if req.MemoryQuotaCamel != nil {
		policy.MaxMemoryPerEnv = *req.MemoryQuotaCamel
	}
	if req.MaxMemoryPerEnv != nil {
		policy.MaxMemoryPerEnv = *req.MaxMemoryPerEnv
	}
	if req.MaxMemoryPerEnvCamel != nil {
		policy.MaxMemoryPerEnv = *req.MaxMemoryPerEnvCamel
	}
	if req.IdleTimeoutHours != nil {
		policy.IdleTimeoutHours = *req.IdleTimeoutHours
	}
	if req.IdleTimeoutHoursCamel != nil {
		policy.IdleTimeoutHours = *req.IdleTimeoutHoursCamel
	}
	if req.AutoDeleteIdleEnvs != nil {
		policy.AutoDeleteIdleEnvs = req.AutoDeleteIdleEnvs
	}
	if req.AutoDeleteIdleEnvsCamel != nil {
		policy.AutoDeleteIdleEnvs = req.AutoDeleteIdleEnvsCamel
	}
	saved, err := s.projects.SaveProjectCostPolicy(project.ID, policy)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectCostPolicyResponse(project.ID, saved.CostPolicy))
}

func projectHybridConfigResponse(project domain.Project) map[string]any {
	base := project.BaseEnvConfig
	sharedDependencies := make([]string, 0, len(base.Services))
	for _, service := range base.Services {
		if service.Name != "" {
			sharedDependencies = append(sharedDependencies, service.Name)
		}
	}
	return map[string]any{
		"projectId":          project.ID,
		"baseNamespace":      base.Namespace,
		"baseDomain":         base.Domain,
		"baseIngress":        base.ConfigPath,
		"sharedDependencies": sharedDependencies,
		"services":           base.Services,
		"hybridOverrides":    base.HybridOverrides,
		"hybrid_overrides":   base.HybridOverrides,
	}
}

func mergeProjectPatch(project domain.Project, patch domain.Project) domain.Project {
	if patch.Name != "" {
		project.Name = patch.Name
	}
	if patch.ProductID != "" {
		project.ProductID = patch.ProductID
	}
	if patch.AppRepositoryID != "" {
		project.AppRepositoryID = patch.AppRepositoryID
	}
	if patch.GitOpsRepositoryID != "" {
		project.GitOpsRepositoryID = patch.GitOpsRepositoryID
	}
	if len(patch.WebhookBranchFilters) > 0 {
		project.WebhookBranchFilters = patch.WebhookBranchFilters
	}
	if len(patch.WebhookLabels) > 0 {
		project.WebhookLabels = patch.WebhookLabels
	}
	if len(patch.GitHubInstallationIDs) > 0 {
		project.GitHubInstallationIDs = patch.GitHubInstallationIDs
	}
	if len(patch.GitLabProjectIDs) > 0 {
		project.GitLabProjectIDs = patch.GitLabProjectIDs
	}
	if patch.ClusterID != "" {
		project.ClusterID = patch.ClusterID
	}
	if len(patch.AccessUsers) > 0 {
		project.AccessUsers = patch.AccessUsers
	}
	if len(patch.AccessOrganizations) > 0 {
		project.AccessOrganizations = patch.AccessOrganizations
	}
	if len(patch.SecretRefs) > 0 {
		project.SecretRefs = patch.SecretRefs
	}
	if patch.GitRepo.Provider != "" {
		project.GitRepo.Provider = patch.GitRepo.Provider
	}
	if patch.GitRepo.URL != "" {
		project.GitRepo.URL = patch.GitRepo.URL
	}
	if patch.GitRepo.DefaultBranch != "" {
		project.GitRepo.DefaultBranch = patch.GitRepo.DefaultBranch
	}
	if patch.GitRepo.Path != "" {
		project.GitRepo.Path = patch.GitRepo.Path
	}
	if patch.GitOpsRepo.Provider != "" {
		project.GitOpsRepo.Provider = patch.GitOpsRepo.Provider
	}
	if patch.GitOpsRepo.URL != "" {
		project.GitOpsRepo.URL = patch.GitOpsRepo.URL
	}
	if patch.GitOpsRepo.DefaultBranch != "" {
		project.GitOpsRepo.DefaultBranch = patch.GitOpsRepo.DefaultBranch
	}
	if patch.GitOpsRepo.Path != "" {
		project.GitOpsRepo.Path = patch.GitOpsRepo.Path
	}
	if patch.BaseEnvConfig.EnvironmentID != "" {
		project.BaseEnvConfig.EnvironmentID = patch.BaseEnvConfig.EnvironmentID
	}
	if patch.BaseEnvConfig.Namespace != "" {
		project.BaseEnvConfig.Namespace = patch.BaseEnvConfig.Namespace
	}
	if patch.BaseEnvConfig.Domain != "" {
		project.BaseEnvConfig.Domain = patch.BaseEnvConfig.Domain
	}
	if patch.BaseEnvConfig.ConfigPath != "" {
		project.BaseEnvConfig.ConfigPath = patch.BaseEnvConfig.ConfigPath
	}
	if len(patch.BaseEnvConfig.Services) > 0 {
		project.BaseEnvConfig.Services = patch.BaseEnvConfig.Services
	}
	if len(patch.BaseEnvConfig.Values) > 0 {
		project.BaseEnvConfig.Values = patch.BaseEnvConfig.Values
	}
	if patch.BaseEnvConfig.HybridOverrides != nil {
		project.BaseEnvConfig.HybridOverrides = patch.BaseEnvConfig.HybridOverrides
	}
	if patch.CostPolicy.DefaultTTLHours != 0 {
		project.CostPolicy.DefaultTTLHours = patch.CostPolicy.DefaultTTLHours
	}
	if patch.CostPolicy.MaxActiveEnvsPerProject != 0 {
		project.CostPolicy.MaxActiveEnvsPerProject = patch.CostPolicy.MaxActiveEnvsPerProject
	}
	if patch.CostPolicy.MaxCPUPerEnv != 0 {
		project.CostPolicy.MaxCPUPerEnv = patch.CostPolicy.MaxCPUPerEnv
	}
	if patch.CostPolicy.MaxMemoryPerEnv != 0 {
		project.CostPolicy.MaxMemoryPerEnv = patch.CostPolicy.MaxMemoryPerEnv
	}
	if patch.CostPolicy.IdleTimeoutHours != 0 {
		project.CostPolicy.IdleTimeoutHours = patch.CostPolicy.IdleTimeoutHours
	}
	if patch.CostPolicy.AutoDeleteIdleEnvs != nil {
		project.CostPolicy.AutoDeleteIdleEnvs = patch.CostPolicy.AutoDeleteIdleEnvs
	}
	return project
}

func mergeProjectPatchAliases(project domain.Project, aliases map[string]json.RawMessage) domain.Project {
	if value := rawString(aliases, "repositoryUrl", "repository_url"); value != "" {
		project.GitRepo.URL = value
	}
	if value := rawString(aliases, "gitopsRepoUrl", "gitops_repo_url"); value != "" {
		project.GitOpsRepo.URL = value
	}
	if value := rawString(aliases, "previewDomain", "preview_domain"); value != "" {
		project.BaseEnvConfig.Domain = value
	}
	if value := rawString(aliases, "helmChartPath", "helm_chart_path"); value != "" {
		project.BaseEnvConfig.ConfigPath = value
	}
	if value := rawString(aliases, "fluxPath", "flux_path"); value != "" {
		project.GitOpsRepo.Path = value
	}
	if value := rawString(aliases, "defaultBranch", "default_branch"); value != "" {
		project.GitRepo.DefaultBranch = value
	}
	if value := rawString(aliases, "gitopsDefaultBranch", "gitops_default_branch"); value != "" {
		project.GitOpsRepo.DefaultBranch = value
	}
	if value := rawString(aliases, "baseNamespace", "base_namespace"); value != "" {
		project.BaseEnvConfig.Namespace = value
	}
	return project
}

func rawString(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func baseServicesFromNames(names []string) []domain.BaseServiceRef {
	services := make([]domain.BaseServiceRef, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			services = append(services, domain.BaseServiceRef{Name: name})
		}
	}
	return services
}

func projectCostPolicyResponse(projectID string, policy domain.ProjectCostPolicy) map[string]any {
	var autoDeleteIdleEnvs any
	if policy.AutoDeleteIdleEnvs != nil {
		autoDeleteIdleEnvs = *policy.AutoDeleteIdleEnvs
	}
	return map[string]any{
		"projectId":                   projectID,
		"defaultTTLHours":             policy.DefaultTTLHours,
		"default_ttl_hours":           policy.DefaultTTLHours,
		"maxActiveEnvs":               policy.MaxActiveEnvsPerProject,
		"max_active_envs":             policy.MaxActiveEnvsPerProject,
		"maxActiveEnvsPerProject":     policy.MaxActiveEnvsPerProject,
		"max_active_envs_per_project": policy.MaxActiveEnvsPerProject,
		"cpuQuota":                    policy.MaxCPUPerEnv,
		"cpu_quota":                   policy.MaxCPUPerEnv,
		"maxCPUPerEnv":                policy.MaxCPUPerEnv,
		"max_cpu_per_env":             policy.MaxCPUPerEnv,
		"memoryQuota":                 policy.MaxMemoryPerEnv,
		"memory_quota":                policy.MaxMemoryPerEnv,
		"maxMemoryPerEnv":             policy.MaxMemoryPerEnv,
		"max_memory_per_env":          policy.MaxMemoryPerEnv,
		"idleTimeoutHours":            policy.IdleTimeoutHours,
		"idle_timeout_hours":          policy.IdleTimeoutHours,
		"autoDeleteIdleEnvs":          autoDeleteIdleEnvs,
		"auto_delete_idle_envs":       autoDeleteIdleEnvs,
	}
}

func parseCostEstimateDay(value string) float64 {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "~")
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "€")
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "/day")
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.settings.GetSettings()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var settings domain.ControlPlaneSettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	saved, err := s.settings.SaveSettings(settings, r.Header.Get("X-EnvPilot-User"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) validateSecretRef(w http.ResponseWriter, r *http.Request) {
	settings, err := s.settings.GetSettings()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	id := normalizeSettingsID(r.PathValue("id"))
	for _, ref := range settings.SecretRefs {
		if normalizeSettingsID(ref.ID) == id {
			result := secrets.NewResolver().Validate(r.Context(), ref)
			status := http.StatusOK
			if !result.Valid {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, result)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("secret reference %q not found", r.PathValue("id")))
}

func (s *Server) getClusterHealth(w http.ResponseWriter, r *http.Request) {
	settings, err := s.settings.GetSettings()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	id := normalizeSettingsID(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cluster id is required"))
		return
	}
	for _, cluster := range settings.Clusters {
		if normalizeSettingsID(cluster.ID) == id {
			writeJSON(w, http.StatusOK, cluster)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("cluster %q not found", r.PathValue("id")))
}

func (s *Server) registerAgent(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req domain.AgentRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	projectID := normalizeSettingsID(req.ProjectID)
	registrationToken := strings.TrimSpace(req.RegistrationToken)
	if projectID == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventAgentRegistrationAuthFailed, auditEndpointAgentRegistration, projectID, auditSubjectAgentID, req.AgentID, registrationToken, errors.New("projectId is required"))
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventAgentRegistrationAuthFailed, auditEndpointAgentRegistration, projectID, auditSubjectAgentID, req.AgentID, registrationToken, errors.New("agentId is required"))
		return
	}
	if strings.TrimSpace(req.ClusterID) == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventAgentRegistrationAuthFailed, auditEndpointAgentRegistration, projectID, auditSubjectAgentID, req.AgentID, registrationToken, errors.New("clusterId is required"))
		return
	}
	if registrationToken == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventAgentRegistrationAuthFailed, auditEndpointAgentRegistration, projectID, auditSubjectAgentID, req.AgentID, registrationToken, errors.New("registrationToken is required"))
		return
	}
	lock := s.agentRegistrationMutex(projectID)
	lock.Lock()
	cluster, agentAuthToken, err := s.registerAgentWithBootstrapClaim(projectID, registrationToken, req)
	lock.Unlock()
	if err != nil {
		s.rejectBootstrapAuthFailure(w, r, bootstrapAuthFailureStatus(err), auditEventAgentRegistrationAuthFailed, auditEndpointAgentRegistration, projectID, auditSubjectAgentID, req.AgentID, registrationToken, err)
		return
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventAgentRegistrationSucceeded, auditEndpointAgentRegistration, projectID, auditSubjectAgentID, req.AgentID, registrationToken)
	writeJSON(w, http.StatusAccepted, agentRegistrationResponse(cluster, agentAuthToken))
}

func (s *Server) registerAgentWithBootstrapClaim(projectID, registrationToken string, req domain.AgentRegistrationRequest) (domain.ClusterTarget, string, error) {
	// This pre-validation is only for fast feedback and audit classification.
	// The authoritative security boundary is the store-level ClaimBootstrapToken
	// CAS, which revalidates token hash, usedAt, expiry, and identity binding
	// atomically under the store transaction/lock.
	if err := s.validateBootstrapRegistrationToken(projectID, registrationToken); err != nil {
		return domain.ClusterTarget{}, "", err
	}
	if err := s.validateAgentRegistrationBinding(projectID, req); err != nil {
		return domain.ClusterTarget{}, "", err
	}
	pendingKey := agentRegistrationIdempotencyKey(projectID, req)
	agentAuthToken, err := s.pendingAgentRegistrationToken(pendingKey)
	if err != nil {
		return domain.ClusterTarget{}, "", err
	}
	// RegisterAgentWithBootstrapClaim persists settings first, then invokes this
	// callback to consume the one-time token and persist the auth-token hash. If
	// settings persistence fails, this callback never runs. If this callback
	// fails, the raw token is not returned to the caller and remains only in the
	// in-memory pending map so an immediate retry for the same identity can
	// complete with the same auth token instead of minting duplicates.
	claimAttempted := false
	cluster, claimedToken, err := s.settings.RegisterAgentWithBootstrapClaim(req, func(now time.Time) (string, error) {
		claimAttempted = true
		if err := s.claimBootstrapAgentRegistration(projectID, registrationToken, req, agentAuthToken, now); err != nil {
			return "", err
		}
		return agentAuthToken, nil
	})
	if err != nil {
		if !claimAttempted {
			// Settings persistence failed before the bootstrap claim boundary, so
			// no caller-visible state was completed. Drop the pending raw token
			// instead of retaining a recoverable secret for a request that never
			// reached token finalization.
			s.deletePendingAgentRegistrationToken(pendingKey)
			var validation app.ValidationError
			if !errors.As(err, &validation) {
				err = fmt.Errorf("persist agent settings registration: %w", err)
			}
		}
		return domain.ClusterTarget{}, "", err
	}
	s.deletePendingAgentRegistrationToken(pendingKey)
	return cluster, claimedToken, nil
}

func (s *Server) pendingAgentRegistrationToken(key string) (string, error) {
	now := time.Now().UTC()
	ttl := s.pendingRegistrationTokenTTL()
	s.pendingAgentRegistrationMu.Lock()
	defer s.pendingAgentRegistrationMu.Unlock()
	s.cleanupExpiredPendingRegistrationTokensLocked(now)
	if entry, ok := s.pendingAgentRegistration[key]; ok {
		if token := entry.validToken(now); token != "" {
			return token, nil
		}
		s.deletePendingAgentRegistrationTokenLocked(key)
	}
	agentAuthToken, err := randomToken(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate agent auth token: %w", err)
	}
	entry := pendingRegistrationToken{
		Token:     agentAuthToken,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	s.storePendingAgentRegistrationTokenLocked(key, entry)
	return agentAuthToken, nil
}

func (s *Server) storePendingAgentRegistrationToken(key string, entry pendingRegistrationToken) (any, bool, error) {
	s.pendingAgentRegistrationMu.Lock()
	defer s.pendingAgentRegistrationMu.Unlock()
	loaded := false
	if existing, ok := s.pendingAgentRegistration[key]; ok {
		loaded = true
		return existing, loaded, nil
	}
	s.storePendingAgentRegistrationTokenLocked(key, entry)
	return entry, loaded, nil
}

func (s *Server) storePendingAgentRegistrationTokenLocked(key string, entry pendingRegistrationToken) {
	if s.pendingAgentRegistration == nil {
		s.pendingAgentRegistration = map[string]pendingRegistrationToken{}
	}
	if max := s.pendingRegistrationTokenMax(); max > 0 {
		s.enforcePendingRegistrationTokenBoundLocked(time.Now().UTC(), max-1)
	}
	s.pendingAgentRegistration[key] = entry
}

func (s *Server) enforcePendingRegistrationTokenBoundLocked(now time.Time, targetMax int) {
	if targetMax < 0 {
		targetMax = 0
	}
	expired := s.cleanupExpiredPendingRegistrationTokensLocked(now)
	if expired > 0 {
		s.recordPendingRegistrationTokenEvictions(expired)
	}
	current := len(s.pendingAgentRegistration)
	if current <= targetMax {
		return
	}
	removed := s.evictOldestPendingRegistrationTokensLocked(current - targetMax)
	if removed > 0 {
		s.recordPendingRegistrationTokenEvictions(removed)
	}
}

func (s *Server) evictOldestPendingRegistrationTokensLocked(limit int) int {
	if limit <= 0 {
		return 0
	}
	type pendingItem struct {
		key       string
		createdAt time.Time
	}
	items := make([]pendingItem, 0, len(s.pendingAgentRegistration))
	for key, entry := range s.pendingAgentRegistration {
		items = append(items, pendingItem{key: key, createdAt: entry.CreatedAt})
	}
	sort.Slice(items, func(i, j int) bool {
		left := items[i].createdAt
		right := items[j].createdAt
		if left.IsZero() && right.IsZero() {
			return fmt.Sprint(items[i].key) < fmt.Sprint(items[j].key)
		}
		if left.IsZero() {
			return true
		}
		if right.IsZero() {
			return false
		}
		return left.Before(right)
	})
	removed := 0
	for _, item := range items {
		if removed >= limit {
			break
		}
		if s.deletePendingAgentRegistrationTokenLocked(item.key) {
			removed++
		}
	}
	return removed
}

func (s *Server) deletePendingAgentRegistrationToken(key string) bool {
	s.pendingAgentRegistrationMu.Lock()
	defer s.pendingAgentRegistrationMu.Unlock()
	return s.deletePendingAgentRegistrationTokenLocked(key)
}

func (s *Server) deletePendingAgentRegistrationTokenLocked(key string) bool {
	if _, loaded := s.pendingAgentRegistration[key]; loaded {
		delete(s.pendingAgentRegistration, key)
		return true
	}
	return false
}

func (s *Server) pendingAgentRegistrationSize() int {
	s.pendingAgentRegistrationMu.Lock()
	defer s.pendingAgentRegistrationMu.Unlock()
	return len(s.pendingAgentRegistration)
}

func (s *Server) recordPendingRegistrationTokenEvictions(count int) {
	if count <= 0 || s.metrics == nil {
		return
	}
	s.metricMu.Lock()
	defer s.metricMu.Unlock()
	s.metrics.pendingTokenEvictions += int64(count)
}

func (entry pendingRegistrationToken) validToken(now time.Time) string {
	if strings.TrimSpace(entry.Token) == "" {
		return ""
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !entry.ExpiresAt.IsZero() && !now.Before(entry.ExpiresAt) {
		return ""
	}
	return entry.Token
}

func (s *Server) pendingRegistrationTokenTTL() time.Duration {
	ttl := s.config().PendingRegistrationTokenTTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	if registrationTTL := s.config().AgentRegistrationTokenTTL; registrationTTL > 0 && ttl > registrationTTL {
		ttl = registrationTTL
	}
	return ttl
}

func (s *Server) pendingRegistrationTokenMax() int {
	max := s.config().PendingRegistrationTokenMax
	if max <= 0 {
		max = 1024
	}
	return max
}

func (s *Server) cleanupExpiredPendingRegistrationTokens(now time.Time) int {
	s.pendingAgentRegistrationMu.Lock()
	defer s.pendingAgentRegistrationMu.Unlock()
	return s.cleanupExpiredPendingRegistrationTokensLocked(now)
}

func (s *Server) cleanupExpiredPendingRegistrationTokensLocked(now time.Time) int {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	removed := 0
	for key, entry := range s.pendingAgentRegistration {
		if entry.validToken(now) == "" {
			if s.deletePendingAgentRegistrationTokenLocked(key) {
				removed++
			}
		}
	}
	return removed
}

func (s *Server) pendingAgentRegistrationTokenForTest(key string) string {
	s.pendingAgentRegistrationMu.Lock()
	defer s.pendingAgentRegistrationMu.Unlock()
	entry, ok := s.pendingAgentRegistration[key]
	if !ok {
		return ""
	}
	token := entry.validToken(time.Now().UTC())
	if token == "" {
		s.deletePendingAgentRegistrationTokenLocked(key)
		return ""
	}
	return token
}

func (s *Server) hasPendingAgentRegistrationTokenForTest(key string) bool {
	s.pendingAgentRegistrationMu.Lock()
	defer s.pendingAgentRegistrationMu.Unlock()
	_, ok := s.pendingAgentRegistration[key]
	return ok
}

func agentRegistrationIdempotencyKey(projectID string, req domain.AgentRegistrationRequest) string {
	return strings.Join([]string{
		normalizeSettingsID(projectID),
		strings.TrimSpace(req.AgentID),
		strings.TrimSpace(req.ClusterID),
	}, "\x00")
}

func (s *Server) claimBootstrapAgentRegistration(projectID, registrationToken string, req domain.AgentRegistrationRequest, agentAuthToken string, now time.Time) error {
	if strings.TrimSpace(agentAuthToken) == "" {
		return fmt.Errorf("agent auth token is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, updateErr := s.bootstrapSessions.ClaimBootstrapToken(store.BootstrapTokenClaimRequest{
		ProjectID:       projectID,
		TokenProjectKey: bootstrapAgentTokenProjectKey,
		TokenHashKey:    bootstrapAgentTokenHashKey,
		TokenHash:       hashToken(registrationToken),
		TokenUsedAtKey:  bootstrapAgentTokenUsedAtKey,
		TokenExpiresKey: bootstrapAgentTokenExpiresAtKey,
		Identity: map[string]string{
			bootstrapAgentClusterIDKey: strings.TrimSpace(req.ClusterID),
		},
		Now: now,
		StepData: map[string]any{
			bootstrapAgentStatusKey:            "connected",
			bootstrapAgentClusterIDKey:         strings.TrimSpace(req.ClusterID),
			bootstrapAgentIDKey:                strings.TrimSpace(req.AgentID),
			bootstrapAgentLastSeenAtKey:        now.Format(time.RFC3339Nano),
			bootstrapCapabilityReportKey:       req.CapabilityReport,
			bootstrapAgentErrorKey:             "",
			bootstrapAgentAuthTokenHashKey:     hashToken(agentAuthToken),
			bootstrapAgentAuthTokenProjectKey:  projectID,
			bootstrapAgentAuthTokenIssuedAtKey: now.Format(time.RFC3339Nano),
			bootstrapAgentTokenUsedAtKey:       now.Format(time.RFC3339Nano),
		},
	})
	if updateErr != nil {
		return fmt.Errorf("persist agent registration state: %w", updateErr)
	}
	return nil
}

func (s *Server) validateAgentRegistrationBinding(projectID string, req domain.AgentRegistrationRequest) error {
	projectID = normalizeSettingsID(projectID)
	if projectID == "" {
		return fmt.Errorf("projectId is required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	expectedClusterID := strings.TrimSpace(asString(session.Data[bootstrapAgentClusterIDKey]))
	actualClusterID := strings.TrimSpace(req.ClusterID)
	if expectedClusterID != "" && actualClusterID != expectedClusterID {
		return fmt.Errorf("%w: ERR_AGENT_CLUSTER_MISMATCH: expected clusterId=%q, got %q", ErrBootstrapIdentityMismatch, expectedClusterID, actualClusterID)
	}
	return nil
}

func (s *Server) registerRunner(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req domain.RunnerRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	projectID := normalizeSettingsID(req.ProjectID)
	registrationToken := strings.TrimSpace(req.RegistrationToken)
	if projectID == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventRunnerRegistrationAuthFailed, auditEndpointRunnerRegistration, projectID, auditSubjectRunnerID, req.RunnerID, registrationToken, errors.New("projectId and registrationToken are required"))
		return
	}
	if registrationToken == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventRunnerRegistrationAuthFailed, auditEndpointRunnerRegistration, projectID, auditSubjectRunnerID, req.RunnerID, registrationToken, errors.New("registrationToken is required"))
		return
	}
	lock := s.runnerRegistrationMutex(projectID)
	lock.Lock()
	runnerAuthToken, err := s.claimBootstrapRunnerRegistration(projectID, registrationToken, req)
	lock.Unlock()
	if err != nil {
		s.rejectBootstrapAuthFailure(w, r, bootstrapAuthFailureStatus(err), auditEventRunnerRegistrationAuthFailed, auditEndpointRunnerRegistration, projectID, auditSubjectRunnerID, req.RunnerID, registrationToken, err)
		return
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventRunnerRegistrationSucceeded, auditEndpointRunnerRegistration, projectID, auditSubjectRunnerID, req.RunnerID, registrationToken)
	writeJSON(w, http.StatusAccepted, domain.RunnerRegistrationResponse{
		Status:          "accepted",
		Registered:      "runner",
		ProjectID:       projectID,
		RunnerID:        strings.TrimSpace(req.RunnerID),
		RunnerAuthToken: runnerAuthToken,
	})
}

func (s *Server) claimBootstrapRunnerRegistration(projectID, registrationToken string, req domain.RunnerRegistrationRequest) (string, error) {
	// This pre-validation is only for fast feedback and audit classification.
	// Correctness does not depend on this process-local check. The authoritative
	// security boundary is the store-level ClaimBootstrapToken CAS below, which
	// revalidates token hash, usedAt, expiry, and identity binding atomically
	// under the store transaction/lock.
	if err := s.validateBootstrapRunnerRegistrationToken(projectID, registrationToken); err != nil {
		return "", err
	}
	if err := s.validateRunnerRegistrationBinding(projectID, req); err != nil {
		return "", err
	}
	runnerAuthToken, err := randomToken(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate runner auth token: %w", err)
	}
	now := time.Now().UTC()
	_, updateErr := s.bootstrapSessions.ClaimBootstrapToken(store.BootstrapTokenClaimRequest{
		ProjectID:       projectID,
		TokenProjectKey: bootstrapRunnerTokenProjectKey,
		TokenHashKey:    bootstrapRunnerTokenHashKey,
		TokenHash:       hashToken(registrationToken),
		TokenUsedAtKey:  bootstrapRunnerTokenUsedAtKey,
		TokenExpiresKey: bootstrapRunnerTokenExpiresAtKey,
		Identity: map[string]string{
			bootstrapRunnerClusterIDKey: strings.TrimSpace(req.ClusterID),
			bootstrapRunnerIDKey:        strings.TrimSpace(req.RunnerID),
			bootstrapRunnerNamespaceKey: sanitizePathComponent(strings.TrimSpace(req.RunnerNamespace)),
			bootstrapRunnerModeKey:      strings.ToLower(strings.TrimSpace(req.DeploymentMode)),
		},
		Now: now,
		StepData: map[string]any{
			bootstrapRunnerStatusKey:            "connected",
			bootstrapRunnerClusterIDKey:         strings.TrimSpace(req.ClusterID),
			bootstrapRunnerIDKey:                strings.TrimSpace(req.RunnerID),
			bootstrapRunnerLastSeenAtKey:        now.Format(time.RFC3339Nano),
			bootstrapRunnerNamespaceKey:         sanitizePathComponent(strings.TrimSpace(req.RunnerNamespace)),
			bootstrapRunnerModeKey:              strings.ToLower(strings.TrimSpace(req.DeploymentMode)),
			bootstrapRunnerErrorKey:             "",
			bootstrapRunnerAuthTokenHashKey:     hashToken(runnerAuthToken),
			bootstrapRunnerAuthTokenProjectKey:  projectID,
			bootstrapRunnerAuthTokenIssuedAtKey: now.Format(time.RFC3339Nano),
			bootstrapRunnerTokenUsedAtKey:       now.Format(time.RFC3339Nano),
		},
	})
	if updateErr != nil {
		return "", fmt.Errorf("persist runner registration state: %w", updateErr)
	}
	return runnerAuthToken, nil
}

func bootstrapAuthFailureStatus(err error) int {
	if err == nil {
		return http.StatusUnauthorized
	}
	// Replayed/used bootstrap tokens are authentication failures by design. We
	// intentionally return 401 instead of 409 to avoid exposing a separate replay
	// detection oracle to unauthenticated callers.
	if errors.Is(err, store.ErrBootstrapTokenAlreadyUsed) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrBootstrapIdentityMismatch) {
		return http.StatusForbidden
	}
	if strings.Contains(err.Error(), "failed to generate") ||
		strings.Contains(err.Error(), "persist runner registration state") ||
		strings.Contains(err.Error(), "persist agent registration state") ||
		strings.Contains(err.Error(), "persist agent settings registration") {
		return http.StatusInternalServerError
	}
	return http.StatusUnauthorized
}

func (s *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req domain.AgentHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	projectID := normalizeSettingsID(req.ProjectID)
	agentAuthToken := agentHeartbeatAuthToken(req)
	if projectID == "" || strings.TrimSpace(req.AgentID) == "" || strings.TrimSpace(req.ClusterID) == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventAgentHeartbeatAuthFailed, auditEndpointAgentHeartbeat, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken, errors.New("projectId and agentAuthToken are required"))
		return
	}
	if agentAuthToken == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventAgentHeartbeatAuthFailed, auditEndpointAgentHeartbeat, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken, errors.New("agentAuthToken is required"))
		return
	} else if err := s.validateBootstrapAgentAuthToken(projectID, agentAuthToken, req.AgentID, req.ClusterID); err != nil {
		s.rejectBootstrapAuthFailure(w, r, bootstrapAuthFailureStatus(err), auditEventAgentHeartbeatAuthFailed, auditEndpointAgentHeartbeat, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken, err)
		return
	}
	cluster, err := s.settings.RecordAgentHeartbeat(req)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if projectID != "" {
		now := time.Now().UTC()
		_, _ = s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
			StepData: map[string]any{
				bootstrapAgentStatusKey:     firstNonEmpty(req.Status, "connected"),
				bootstrapAgentClusterIDKey:  req.ClusterID,
				bootstrapAgentIDKey:         req.AgentID,
				bootstrapAgentLastSeenAtKey: now.Format(time.RFC3339Nano),
				bootstrapAgentErrorKey:      strings.TrimSpace(req.Error),
			},
		})
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventAgentHeartbeatSucceeded, auditEndpointAgentHeartbeat, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken)
	writeJSON(w, http.StatusAccepted, cluster)
}

func (s *Server) runnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req domain.RunnerHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	projectID := normalizeSettingsID(req.ProjectID)
	runnerAuthToken := runnerHeartbeatAuthToken(req)
	runnerID := strings.TrimSpace(req.RunnerID)
	if projectID == "" || runnerID == "" {
		if allowed, retryAfter := s.recordBootstrapAuthFailure(r, auditEndpointRunnerHeartbeat, projectID, runnerAuthToken); !allowed {
			s.writeBootstrapRateLimitAuditLog(r, auditEndpointRunnerHeartbeat, projectID, auditSubjectRunnerID, runnerID, runnerAuthToken, "missing_required_fields")
			writeRateLimitError(w, s.config().BootstrapRateLimitRequests, retryAfter)
			return
		}
		s.writeRunnerHeartbeatAuthFailureAuditLog(r, projectID, runnerID, runnerAuthToken, "missing_required_fields")
		writeError(w, http.StatusBadRequest, fmt.Errorf("projectId, runnerId, and runnerAuthToken are required"))
		return
	}
	if runnerAuthToken == "" {
		if allowed, retryAfter := s.recordBootstrapAuthFailure(r, auditEndpointRunnerHeartbeat, projectID, runnerAuthToken); !allowed {
			s.writeBootstrapRateLimitAuditLog(r, auditEndpointRunnerHeartbeat, projectID, auditSubjectRunnerID, runnerID, runnerAuthToken, "missing_runner_auth_token")
			writeRateLimitError(w, s.config().BootstrapRateLimitRequests, retryAfter)
			return
		}
		s.writeRunnerHeartbeatAuthFailureAuditLog(r, projectID, runnerID, runnerAuthToken, "runnerAuthToken is required")
		writeError(w, http.StatusUnauthorized, fmt.Errorf("runnerAuthToken is required"))
		return
	}
	if err := s.validateBootstrapRunnerHeartbeatAuthentication(projectID, runnerAuthToken, req); err != nil {
		if allowed, retryAfter := s.recordBootstrapAuthFailure(r, auditEndpointRunnerHeartbeat, projectID, runnerAuthToken); !allowed {
			s.writeBootstrapRateLimitAuditLog(r, auditEndpointRunnerHeartbeat, projectID, auditSubjectRunnerID, runnerID, runnerAuthToken, err.Error())
			writeRateLimitError(w, s.config().BootstrapRateLimitRequests, retryAfter)
			return
		}
		s.writeRunnerHeartbeatAuthFailureAuditLog(r, projectID, runnerID, runnerAuthToken, err.Error())
		writeError(w, bootstrapAuthFailureStatus(err), err)
		return
	}
	status, ok := domain.ParseRunnerHeartbeatStatus(req.Status)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("status must be one of: waiting, connected, online, degraded, failed"))
		return
	}
	now := time.Now().UTC()
	if err := s.updateRunnerSessionHeartbeat(projectID, req, string(status), now); err != nil {
		writeMappedError(w, err)
		return
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventRunnerHeartbeatSucceeded, auditEndpointRunnerHeartbeat, projectID, auditSubjectRunnerID, runnerID, runnerAuthToken)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  string(status),
		"project": projectID,
	})
}

func (s *Server) updateRunnerSessionHeartbeat(projectID string, req domain.RunnerHeartbeatRequest, status string, now time.Time) error {
	stepData := map[string]any{
		bootstrapRunnerStatusKey:     firstNonEmpty(status, "connected"),
		bootstrapRunnerIDKey:         strings.TrimSpace(req.RunnerID),
		bootstrapRunnerLastSeenAtKey: now.Format(time.RFC3339Nano),
		bootstrapRunnerErrorKey:      strings.TrimSpace(req.Error),
	}
	if clusterID := strings.TrimSpace(req.ClusterID); clusterID != "" {
		stepData[bootstrapRunnerClusterIDKey] = clusterID
	}
	if runnerNamespace := strings.TrimSpace(req.RunnerNamespace); runnerNamespace != "" {
		stepData[bootstrapRunnerNamespaceKey] = runnerNamespace
	}
	if deploymentMode := strings.TrimSpace(req.DeploymentMode); deploymentMode != "" {
		stepData[bootstrapRunnerModeKey] = deploymentMode
	}
	_, err := s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{StepData: stepData})
	return err
}

func (s *Server) nextAgentResourceScan(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	projectID := normalizeSettingsID(firstNonEmpty(r.URL.Query().Get("projectId"), r.URL.Query().Get("project_id")))
	clusterID := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("clusterId"), r.URL.Query().Get("cluster_id")))
	agentID := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("agentId"), r.URL.Query().Get("agent_id")))
	agentAuthToken := agentAuthTokenFromRequest(r)
	if projectID == "" || clusterID == "" || agentID == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventBootstrapTokenValidationFail, auditEndpointAgentResourceScanNext, projectID, auditSubjectAgentID, agentID, agentAuthToken, errors.New("projectId, clusterId, agentId, and agentAuthToken are required"))
		return
	}
	if agentAuthToken == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventBootstrapTokenValidationFail, auditEndpointAgentResourceScanNext, projectID, auditSubjectAgentID, agentID, agentAuthToken, errors.New("agentAuthToken is required"))
		return
	}
	if err := s.validateBootstrapAgentAuthToken(projectID, agentAuthToken, agentID, clusterID); err != nil {
		s.rejectBootstrapAuthFailure(w, r, bootstrapAuthFailureStatus(err), auditEventBootstrapTokenValidationFail, auditEndpointAgentResourceScanNext, projectID, auditSubjectAgentID, agentID, agentAuthToken, err)
		return
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	task, ok := asStringAnyMap(session.Data[bootstrapResourceScanTaskKey])
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	taskStatus := strings.TrimSpace(asString(task["status"]))
	if taskStatus != "pending" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	taskClusterID := strings.TrimSpace(asString(task["clusterId"]))
	taskAgentID := strings.TrimSpace(asString(task["agentId"]))
	if taskClusterID != "" && clusterID != "" && taskClusterID != clusterID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if taskAgentID != "" && agentID != "" && taskAgentID != agentID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	namespaces := asStringSlice(task["namespaces"])
	if len(namespaces) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	observedAt, ok := parseRFC3339Pointer(asString(task["requestedAt"]))
	if !ok {
		now := time.Now().UTC()
		observedAt = &now
	}
	_, err = s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapResourceScanStatusKey: "running",
			bootstrapResourceScanTaskKey: map[string]any{
				"projectId":   projectID,
				"clusterId":   firstNonEmpty(taskClusterID, clusterID),
				"agentId":     firstNonEmpty(taskAgentID, agentID),
				"namespaces":  namespaces,
				"status":      "dispatched",
				"requestedAt": observedAt.Format(time.RFC3339Nano),
			},
		},
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, domain.AgentResourceScanTaskResponse{
		ProjectID:  projectID,
		ClusterID:  firstNonEmpty(taskClusterID, clusterID),
		AgentID:    firstNonEmpty(taskAgentID, agentID),
		Namespaces: namespaces,
		ObservedAt: *observedAt,
	})
}

func (s *Server) ingestAgentResourceScan(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapSessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("bootstrap sessions are not configured"))
		return
	}
	var req domain.AgentResourceScanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	projectID := normalizeSettingsID(firstNonEmpty(req.ProjectID, req.ProjectIDSnake))
	clusterID := strings.TrimSpace(firstNonEmpty(req.ClusterID, req.ClusterIDSnake))
	agentAuthToken := agentAuthTokenFromRequest(r)
	if projectID == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventBootstrapTokenValidationFail, auditEndpointAgentResourceScanIngest, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken, errors.New("projectId is required"))
		return
	}
	if clusterID == "" || strings.TrimSpace(req.AgentID) == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusBadRequest, auditEventBootstrapTokenValidationFail, auditEndpointAgentResourceScanIngest, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken, errors.New("clusterId, agentId, and agentAuthToken are required"))
		return
	}
	if agentAuthToken == "" {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventBootstrapTokenValidationFail, auditEndpointAgentResourceScanIngest, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken, errors.New("agentAuthToken is required"))
		return
	}
	if err := s.validateBootstrapAgentAuthToken(projectID, agentAuthToken, req.AgentID, clusterID); err != nil {
		s.rejectBootstrapAuthFailure(w, r, bootstrapAuthFailureStatus(err), auditEventBootstrapTokenValidationFail, auditEndpointAgentResourceScanIngest, projectID, auditSubjectAgentID, req.AgentID, agentAuthToken, err)
		return
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	task, ok := asStringAnyMap(session.Data[bootstrapResourceScanTaskKey])
	if !ok {
		writeError(w, http.StatusConflict, fmt.Errorf("resource scan task is not scheduled"))
		return
	}
	taskStatus := strings.TrimSpace(asString(task["status"]))
	if taskStatus != "pending" && taskStatus != "dispatched" && taskStatus != "running" {
		writeError(w, http.StatusConflict, fmt.Errorf("resource scan task is not active"))
		return
	}
	selected, err := s.selectedNamespacesForProject(projectID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if len(selected) == 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("resource scan was not requested"))
		return
	}
	allowed := make(map[string]struct{}, len(selected))
	for _, namespace := range selected {
		allowed[namespace] = struct{}{}
	}
	filtered := make([]domain.ResourceSnapshot, 0, len(req.ResourceSnapshots))
	for _, snapshot := range req.ResourceSnapshots {
		namespace := strings.TrimSpace(snapshot.Namespace)
		if _, ok := allowed[namespace]; !ok {
			continue
		}
		filtered = append(filtered, snapshot)
	}
	observedAt := req.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	session, err = s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapResourceScanStatusKey:      "completed",
			bootstrapResourceScanCompletedAtKey: observedAt.Format(time.RFC3339Nano),
			bootstrapResourceScanReportKey:      filtered,
			bootstrapServiceGraphKey:            req.ServiceGraph,
			bootstrapServiceEnvsKey:             req.ServiceEnvs,
			bootstrapResourceScanTaskKey: map[string]any{
				"projectId":   projectID,
				"clusterId":   firstNonEmpty(strings.TrimSpace(req.ClusterID), asString(task["clusterId"])),
				"agentId":     firstNonEmpty(strings.TrimSpace(req.AgentID), asString(task["agentId"])),
				"namespaces":  selected,
				"status":      "completed",
				"requestedAt": asString(task["requestedAt"]),
			},
			bootstrapAgentErrorKey: "",
		},
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	project, err := s.projects.GetProject(projectID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	session, err = s.refreshBootstrapManifestTemplates(project, session)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":        "accepted",
		"resourceCount": len(filtered),
		"session":       session,
	})
}

func (s *Server) listJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.jobs.List())
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("job %q not found", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobs.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListEnvironments()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	items = s.filterEnvironmentsForRequest(r, items)
	decorated := make([]apiEnvironmentResponse, 0, len(items))
	for _, item := range items {
		decorated = append(decorated, s.decorateEnvironmentForAPI(item))
	}
	writeJSON(w, http.StatusOK, decorated)
}

func (s *Server) listEnvironmentRecords(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListEnvironmentRecords()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	items = s.filterEnvironmentRecordsForRequest(r, items)
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	item, err := s.service.GetEnvironment(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProjectID(r, item.Project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	resp := s.decorateEnvironmentForAPI(item)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) decorateEnvironmentForAPI(env domain.Environment) apiEnvironmentResponse {
	backend := deploymentBackendFromProjectConfigs(s.projectConfigs, env.Project)
	response := apiEnvironmentResponse{
		Environment:       env,
		DeploymentBackend: string(backend),
	}
	response.DeploymentStatus = buildAPIEnvironmentDeploymentStatus(env, backend)
	return response
}

func deploymentBackendFromProjectConfigs(projectConfigs interface {
	Latest(string) (domain.ProjectConfig, error)
}, projectID string) domain.DeploymentBackend {
	if projectConfigs == nil || strings.TrimSpace(projectID) == "" {
		return domain.DeploymentBackendHelmDirect
	}
	projectConfig, err := projectConfigs.Latest(projectID)
	if err != nil {
		return domain.DeploymentBackendHelmDirect
	}
	rawDeployment, ok := projectConfig.Config["deployment"]
	if !ok {
		return domain.InferDeploymentBackend("", map[string]any{}, projectConfig.Config)
	}
	deployment, ok := rawDeployment.(map[string]any)
	if !ok {
		return domain.InferDeploymentBackend("", map[string]any{}, projectConfig.Config)
	}
	return domain.InferDeploymentBackend(deployment["backend"], deployment, projectConfig.Config)
}

func buildAPIEnvironmentDeploymentStatus(env domain.Environment, backend domain.DeploymentBackend) *apiEnvironmentDeploymentStatus {
	switch backend {
	case domain.DeploymentBackendFluxCD:
		return &apiEnvironmentDeploymentStatus{
			Backend: string(backend),
			Flux:    env.FluxStatus,
		}
	default:
		return &apiEnvironmentDeploymentStatus{
			Backend: string(backend),
			Helm: &apiEnvironmentHelmStatus{
				Status: string(env.Status),
				Ready:  env.Status == domain.StatusReady,
			},
		}
	}
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEnvironmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if !s.canAccessProjectID(r, req.Project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	env, err := s.service.CreateEnvironment(r.Context(), req)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, env)
}

func (s *Server) previewRender(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEnvironmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if !s.canAccessProjectID(r, req.Project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	preview, err := s.service.PreviewEnvironment(req)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	existing, err := s.service.GetEnvironment(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProjectID(r, existing.Project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	force := r.URL.Query().Get("force") == "true"
	env, err := s.service.DeleteEnvironment(r.Context(), r.PathValue("id"), force)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) pinEnvironment(w http.ResponseWriter, r *http.Request) {
	s.setPinned(w, r, true)
}

func (s *Server) unpinEnvironment(w http.ResponseWriter, r *http.Request) {
	s.setPinned(w, r, false)
}

func (s *Server) extendEnvironmentTTL(w http.ResponseWriter, r *http.Request) {
	env, err := s.service.ExtendTTL(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) recreateEnvironment(w http.ResponseWriter, r *http.Request) {
	env, err := s.service.GetEnvironment(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProjectID(r, env.Project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}

	req := createEnvironmentRequestFromEnvironment(env)
	event := scm.PullRequestEvent{
		Provider:       scm.Provider(env.Source.Provider),
		Action:         scm.ActionUpdate,
		Repo:           env.Source.Repository,
		Branch:         env.Source.Branch,
		ChangeID:       env.Source.PullRequestID,
		CommitSHA:      env.Source.Commit,
		Author:         env.Source.Author,
		URL:            env.Source.URL,
		EventID:        fmt.Sprintf("ui-recreate-%s-%d", env.ID, time.Now().UnixNano()),
		InstallationID: "",
	}
	job, err := s.jobs.SubmitCreateEnvironment(r.Context(), req, event)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJobResponseWithStatus(w, job, http.StatusAccepted)
}

func createEnvironmentRequestFromEnvironment(env domain.Environment) domain.CreateEnvironmentRequest {
	return domain.CreateEnvironmentRequest{
		ID:             env.ID,
		Project:        env.Project,
		Product:        env.Product,
		ClusterID:      env.ClusterID,
		Namespace:      env.Namespace,
		Mode:           env.Mode,
		Domain:         env.Domain,
		Source:         env.Source,
		Base:           env.Base,
		Charts:         env.Charts,
		Infrastructure: env.Infrastructure,
		Services:       env.Services,
		Overrides:      env.Overrides,
		TTLHours:       env.TTLHours,
		Pinned:         env.Pinned,
	}
}

func (s *Server) setPinned(w http.ResponseWriter, r *http.Request, pinned bool) {
	env, err := s.service.SetPinned(r.PathValue("id"), pinned)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	var req domain.UpdateEnvironmentStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	env, err := s.service.UpdateStatusFromCluster(r.PathValue("id"), req.Status, req.Message, req.ClusterID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) listEnvironmentEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.service.ListEvents(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) ingestEnvironmentEvents(w http.ResponseWriter, r *http.Request) {
	var req domain.IngestEnvironmentEventsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	env, err := s.service.RecordEventsFromCluster(r.PathValue("id"), req.Events, req.ClusterID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"events": env.Events})
}

func (s *Server) getFluxStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.service.GetFluxStatus(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fluxStatus": status})
}

func (s *Server) ingestFluxStatus(w http.ResponseWriter, r *http.Request) {
	var req domain.IngestFluxStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	env, err := s.service.RecordFluxStatusFromCluster(r.PathValue("id"), req.FluxStatus, req.ClusterID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"fluxStatus": env.FluxStatus})
}

func (s *Server) reconcileExpired(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.service.ReconcileExpired(r.Context())
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) githubOAuthLogin(w http.ResponseWriter, r *http.Request) {
	s.oauthLogin(w, r, "github")
}

func (s *Server) githubOAuthCallback(w http.ResponseWriter, r *http.Request) {
	s.oauthCallback(w, r, "github")
}

func (s *Server) gitlabOAuthLogin(w http.ResponseWriter, r *http.Request) {
	s.oauthLogin(w, r, "gitlab")
}

func (s *Server) gitlabOAuthCallback(w http.ResponseWriter, r *http.Request) {
	s.oauthCallback(w, r, "gitlab")
}

func (s *Server) oidcOAuthLogin(w http.ResponseWriter, r *http.Request) {
	s.oauthLogin(w, r, "oidc")
}

func (s *Server) oidcOAuthCallback(w http.ResponseWriter, r *http.Request) {
	s.oauthCallback(w, r, "oidc")
}

func (s *Server) oauthLogin(w http.ResponseWriter, r *http.Request, provider string) {
	cfg := s.config()
	oauthCfg := s.oauthProvider(provider, cfg)
	if oauthCfg.ClientID == "" || oauthCfg.AuthURL == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s oauth is not configured", provider))
		return
	}
	org := strings.TrimSpace(r.URL.Query().Get("org"))
	if org == "" {
		org = "global"
	}
	state := s.generateHexID(16)
	stateValue := state + "." + base64.RawURLEncoding.EncodeToString([]byte(org))
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    provider + ":" + stateValue,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oauthStateCookieMaxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     oauthOrgCookieName,
		Value:    org,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oauthStateCookieMaxAge,
	})
	redirectURL := s.oauthRedirectURL(r, provider)
	values := url.Values{}
	values.Set("client_id", oauthCfg.ClientID)
	values.Set("redirect_uri", redirectURL)
	values.Set("response_type", "code")
	values.Set("state", stateValue)
	if oauthCfg.Scopes != "" {
		values.Set("scope", oauthCfg.Scopes)
	}
	authURL := oauthCfg.AuthURL
	separator := "?"
	if strings.Contains(authURL, "?") {
		separator = "&"
	}
	http.Redirect(w, r, authURL+separator+values.Encode(), http.StatusFound)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request, provider string) {
	cfg := s.config()
	oauthCfg := s.oauthProvider(provider, cfg)
	if oauthCfg.ClientID == "" || oauthCfg.ClientSecret == "" || oauthCfg.TokenURL == "" || oauthCfg.UserURL == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s oauth is not configured", provider))
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if code == "" || state == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("oauth code and state are required"))
		return
	}
	cookie, err := r.Cookie(oauthStateCookieName)
	if err != nil || cookie.Value != provider+":"+state {
		writeError(w, http.StatusUnauthorized, fmt.Errorf("invalid oauth state"))
		return
	}
	org := "global"
	if orgCookie, err := r.Cookie(oauthOrgCookieName); err == nil {
		org = strings.TrimSpace(orgCookie.Value)
		if org == "" {
			org = "global"
		}
	} else if _, encodedOrg, ok := strings.Cut(state, "."); ok {
		if decoded, err := base64.RawURLEncoding.DecodeString(encodedOrg); err == nil {
			org = strings.TrimSpace(string(decoded))
			if org == "" {
				org = "global"
			}
		}
	}
	accessToken, err := s.exchangeOAuthCode(r.Context(), oauthCfg, code, s.oauthRedirectURL(r, provider))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	user, err := s.fetchOAuthUser(r.Context(), oauthCfg, accessToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	session := s.buildOAuthSession(provider, user, apiRoleAdmin, org)
	if session == "" {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("oauth session secret is not configured"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     apiSessionCookieName,
		Value:    session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(oauthSessionCookieTTL.Seconds()),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     oauthOrgCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, s.frontendRedirectURL(), http.StatusFound)
}

func (s *Server) frontendRedirectURL() string {
	value := strings.TrimSpace(s.config().FrontendURL)
	if value == "" {
		return "/"
	}
	return value
}

func (s *Server) oauthRedirectURL(r *http.Request, provider string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}
	host := r.Host
	if forwardedHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
		host = forwardedHost
	}
	return scheme + "://" + host + "/auth/" + provider + "/callback"
}

func (s *Server) exchangeOAuthCode(ctx context.Context, oauthCfg oauthProviderConfig, code string, redirectURL string) (string, error) {
	form := url.Values{}
	form.Set("client_id", oauthCfg.ClientID)
	form.Set("client_secret", oauthCfg.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURL)
	form.Set("grant_type", "authorization_code")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthCfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("oauth token exchange failed with status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", fmt.Errorf("oauth token response did not include access_token")
	}
	return strings.TrimSpace(payload.AccessToken), nil
}

func (s *Server) fetchOAuthUser(ctx context.Context, oauthCfg oauthProviderConfig, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthCfg.UserURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("oauth user lookup failed with status %d", resp.StatusCode)
	}
	var payload struct {
		Login          string `json:"login"`
		Username       string `json:"username"`
		Email          string `json:"email"`
		Name           string `json:"name"`
		PreferredUser  string `json:"preferred_username"`
		Sub            string `json:"sub"`
		PreferredName  string `json:"preferred_name"`
		EmailPreferred string `json:"preferred_email"`
		ExternalRef    string `json:"external_id"`
		ID             any    `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	for _, candidate := range []string{
		payload.Login,
		payload.Username,
		payload.PreferredUser,
		payload.Email,
		payload.EmailPreferred,
		payload.Name,
		payload.PreferredName,
		payload.Sub,
		payload.ExternalRef,
		fmt.Sprint(payload.ID),
	} {
		if value := strings.TrimSpace(candidate); value != "" && value != "<nil>" {
			return value, nil
		}
	}
	return "", fmt.Errorf("oauth user response did not include identity")
}

func (s *Server) gitlabWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.validGitLabToken(r) {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventWebhookAuthFailed, auditEndpointGitLabWebhook, "", "", "", r.Header.Get("X-Gitlab-Token"), errors.New("invalid webhook token"))
		return
	}
	eventType := r.Header.Get("X-Gitlab-Event")
	if eventType != "" && eventType != "Merge Request Hook" && eventType != "Note Hook" {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("failed to read request body: %w", err))
		return
	}
	if eventType == "Note Hook" {
		command, err := scm.ParseGitLabPRCommand(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
			return
		}
		command.EventID = strings.TrimSpace(r.Header.Get("X-Gitlab-Event-UUID"))
		if command.EventID == "" {
			command.EventID = strings.TrimSpace(r.Header.Get("X-Gitlab-Delivery"))
		}
		s.handlePRCommand(w, r, command, http.StatusAccepted)
		return
	}
	event, err := scm.ParseGitLabMergeRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	event.EventID = strings.TrimSpace(r.Header.Get("X-Gitlab-Event-UUID"))
	if event.EventID == "" {
		event.EventID = strings.TrimSpace(r.Header.Get("X-Gitlab-Delivery"))
	}
	if event.Action == scm.ActionIgnore {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventWebhookAuthSucceeded, auditEndpointGitLabWebhook, "", "", "", r.Header.Get("X-Gitlab-Token"))

	job, err := s.jobs.SubmitSCMEvent(r.Context(), event)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJobResponse(w, job)
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	s.handleGitHubWebhook(w, r, http.StatusAccepted)
}

func (s *Server) githubWebhookPoC(w http.ResponseWriter, r *http.Request) {
	s.handleGitHubWebhook(w, r, http.StatusOK)
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request, successStatus int) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("failed to read request body: %w", err))
		return
	}
	eventType := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")

	if eventType != "pull_request" && eventType != "issue_comment" {
		writeJSON(w, successStatus, map[string]string{"status": "ignored"})
		return
	}
	if !s.validGitHubSignature(r, body) {
		s.rejectBootstrapAuthFailure(w, r, http.StatusUnauthorized, auditEventWebhookAuthFailed, auditEndpointGitHubWebhook, "", "", "", r.Header.Get("X-Hub-Signature-256"), errors.New("invalid webhook signature"))
		return
	}
	s.writeBootstrapSuccessAuditLog(r, auditEventWebhookAuthSucceeded, auditEndpointGitHubWebhook, "", "", "", r.Header.Get("X-Hub-Signature-256"))

	if eventType == "issue_comment" {
		command, err := scm.ParseGitHubPRCommand(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
			return
		}
		command.EventID = deliveryID
		s.logGitHubWebhook(body, eventType, deliveryID, string(command.Command), command.Repo, command.ChangeID)
		s.handlePRCommand(w, r, command, successStatus)
		return
	}

	event, err := scm.ParseGitHubPullRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	event.EventID = deliveryID
	s.logGitHubWebhook(body, eventType, deliveryID, string(event.Action), event.Repo, event.ChangeID)

	switch event.Action {
	case scm.ActionOpen, scm.ActionUpdate:
		job, err := s.jobs.SubmitSCMEvent(r.Context(), event)
		if err != nil {
			writeMappedError(w, err)
			return
		}
		writeJobResponseWithStatus(w, job, successStatus)
	case scm.ActionClose:
		job, err := s.jobs.SubmitSCMEvent(r.Context(), event)
		if err != nil {
			writeMappedError(w, err)
			return
		}
		writeJobResponseWithStatus(w, job, successStatus)
	default:
		writeJSON(w, successStatus, map[string]string{"status": "ignored", "action": string(event.Action)})
	}
}

func (s *Server) logGitHubWebhook(body []byte, eventType string, deliveryID string, action string, repository string, prNumber string) {
	number, _ := strconv.Atoi(strings.TrimSpace(prNumber))
	args := []any{
		"event", strings.TrimSpace(eventType),
		"delivery", strings.TrimSpace(deliveryID),
		"action", strings.TrimSpace(action),
		"repo", strings.TrimSpace(repository),
		"pr_number", number,
	}
	if s.config().GitHubWebhookDebugPayloadLog {
		args = append(args, "payload", string(body))
	}
	s.logger.Info("github webhook", args...)
}

func (s *Server) handlePRCommand(w http.ResponseWriter, r *http.Request, command scm.PullRequestCommand, successStatus int) {
	switch command.Command {
	case scm.CommandRecreate:
		job, err := s.jobs.SubmitSCMEvent(r.Context(), command.PullRequestEvent(scm.ActionUpdate))
		if err != nil {
			writeMappedError(w, err)
			return
		}
		writeJobResponseWithStatus(w, job, successStatus)
	case scm.CommandDestroy:
		job, err := s.jobs.SubmitSCMEvent(r.Context(), command.PullRequestEvent(scm.ActionClose))
		if err != nil {
			writeMappedError(w, err)
			return
		}
		writeJobResponseWithStatus(w, job, successStatus)
	case scm.CommandPin:
		env, err := s.service.SetPinnedFor(command.EnvironmentID(), command.PinDuration)
		if err != nil {
			writeMappedError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "pinned",
			"pin":         command.PinRaw,
			"environment": env,
		})
	default:
		writeJSON(w, successStatus, map[string]string{"status": "ignored"})
	}
}

func (s *Server) validGitHubSignature(r *http.Request, body []byte) bool {
	cfg := s.config()
	if cfg.GitHubWebhookSecret == "" {
		return true
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	signature, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cfg.GitHubWebhookSecret))
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (s *Server) validGitLabToken(r *http.Request) bool {
	cfg := s.config()
	if cfg.GitLabWebhookSecret == "" {
		return true
	}
	got := r.Header.Get("X-Gitlab-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(cfg.GitLabWebhookSecret)) == 1
}

func writeMappedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrProductNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrBootstrapSessionNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrProjectConfigNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case varValidation(err):
		writeError(w, http.StatusBadRequest, err)
	case varConflict(err):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func varValidation(err error) bool {
	var target app.ValidationError
	return errors.As(err, &target)
}

func varConflict(err error) bool {
	var target app.ConflictError
	return errors.As(err, &target)
}

func controlPlaneURLFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	scheme := "http"
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || r.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "localhost:8080"
	}
	return scheme + "://" + host
}

func randomToken(length int) (string, error) {
	if length <= 0 {
		length = 32
	}
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func (s *Server) consumeBootstrapRegistrationToken(projectID string, token string) error {
	projectID = normalizeSettingsID(projectID)
	token = strings.TrimSpace(token)
	if projectID == "" || token == "" {
		return fmt.Errorf("project config token is required")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	if asString(session.Data[bootstrapAgentTokenProjectKey]) != projectID {
		return fmt.Errorf("registration token is not issued for project %q", projectID)
	}
	if asString(session.Data[bootstrapAgentTokenHashKey]) == "" {
		return fmt.Errorf("registration token is not configured")
	}
	if subtle.ConstantTimeCompare([]byte(asString(session.Data[bootstrapAgentTokenHashKey])), []byte(hashToken(token))) != 1 {
		return fmt.Errorf("invalid registration token")
	}
	if usedAt := asString(session.Data[bootstrapAgentTokenUsedAtKey]); usedAt != "" {
		return fmt.Errorf("%w: registration token already used", store.ErrBootstrapTokenAlreadyUsed)
	}
	expiresAt := asString(session.Data[bootstrapAgentTokenExpiresAtKey])
	if expiresAt == "" {
		return fmt.Errorf("registration token is expired")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, expiresAt); err != nil || time.Now().UTC().After(parsed) {
		return fmt.Errorf("registration token is expired")
	}
	_, err = s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapAgentTokenUsedAtKey: time.Now().UTC().Format(time.RFC3339Nano),
			bootstrapAgentStatusKey:      "connected",
		},
	})
	return err
}

func (s *Server) validateBootstrapRegistrationToken(projectID string, token string) error {
	projectID = normalizeSettingsID(projectID)
	token = strings.TrimSpace(token)
	if projectID == "" || token == "" {
		return fmt.Errorf("project config token is required")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	if asString(session.Data[bootstrapAgentTokenProjectKey]) != projectID {
		return fmt.Errorf("registration token is not issued for project %q", projectID)
	}
	if subtle.ConstantTimeCompare([]byte(asString(session.Data[bootstrapAgentTokenHashKey])), []byte(hashToken(token))) != 1 {
		return fmt.Errorf("invalid registration token")
	}
	if usedAt := asString(session.Data[bootstrapAgentTokenUsedAtKey]); usedAt != "" {
		return fmt.Errorf("%w: registration token already used", store.ErrBootstrapTokenAlreadyUsed)
	}
	expiresAt := asString(session.Data[bootstrapAgentTokenExpiresAtKey])
	if expiresAt == "" {
		return fmt.Errorf("registration token is expired")
	}
	parsed, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || time.Now().UTC().After(parsed) {
		return fmt.Errorf("registration token is expired")
	}
	return nil
}

func (s *Server) validateBootstrapAgentAuthToken(projectID string, token string, agentID string, clusterID string) error {
	projectID = normalizeSettingsID(projectID)
	token = strings.TrimSpace(token)
	agentID = strings.TrimSpace(agentID)
	clusterID = strings.TrimSpace(clusterID)
	if projectID == "" || token == "" || agentID == "" || clusterID == "" {
		return fmt.Errorf("projectId, clusterId, agentId, and agentAuthToken are required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	if asString(session.Data[bootstrapAgentAuthTokenProjectKey]) != projectID {
		return fmt.Errorf("agent auth token is not issued for project %q", projectID)
	}
	if asString(session.Data[bootstrapAgentAuthTokenHashKey]) == "" {
		return fmt.Errorf("agent auth token is not configured")
	}
	if subtle.ConstantTimeCompare([]byte(asString(session.Data[bootstrapAgentAuthTokenHashKey])), []byte(hashToken(token))) != 1 {
		return fmt.Errorf("invalid agent auth token")
	}
	expectedAgentID := strings.TrimSpace(asString(session.Data[bootstrapAgentIDKey]))
	if expectedAgentID != "" && agentID != expectedAgentID {
		return fmt.Errorf("%w: ERR_AGENT_ID_MISMATCH: expected agentId=%q, got %q", ErrBootstrapIdentityMismatch, expectedAgentID, agentID)
	}
	expectedClusterID := strings.TrimSpace(asString(session.Data[bootstrapAgentClusterIDKey]))
	if expectedClusterID != "" && clusterID != expectedClusterID {
		return fmt.Errorf("%w: ERR_AGENT_CLUSTER_MISMATCH: expected clusterId=%q, got %q", ErrBootstrapIdentityMismatch, expectedClusterID, clusterID)
	}
	return nil
}

func (s *Server) validateBootstrapRunnerConfigToken(projectID, token, clusterID, runnerID, runnerNamespace, deploymentMode string) error {
	projectID = normalizeSettingsID(projectID)
	token = strings.TrimSpace(token)
	if projectID == "" || token == "" {
		return fmt.Errorf("project config token is required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	if asString(session.Data[bootstrapRunnerConfigTokenProjectKey]) != projectID {
		return fmt.Errorf("project config token is not issued for project %q", projectID)
	}
	if asString(session.Data[bootstrapRunnerConfigTokenHashKey]) == "" {
		return fmt.Errorf("project config token is not configured")
	}
	if subtle.ConstantTimeCompare([]byte(asString(session.Data[bootstrapRunnerConfigTokenHashKey])), []byte(hashToken(token))) != 1 {
		return fmt.Errorf("invalid project config token")
	}
	if usedAt := asString(session.Data[bootstrapRunnerConfigTokenUsedAtKey]); usedAt != "" {
		return fmt.Errorf("%w: project config token already used", store.ErrBootstrapTokenAlreadyUsed)
	}
	expiresAt := asString(session.Data[bootstrapRunnerConfigTokenExpiresAtKey])
	if expiresAt == "" {
		return fmt.Errorf("project config token is expired")
	}
	parsed, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || time.Now().UTC().After(parsed) {
		return fmt.Errorf("project config token is expired")
	}
	expectedClusterID := strings.TrimSpace(asString(session.Data[bootstrapRunnerClusterIDKey]))
	if expectedClusterID != "" && strings.TrimSpace(clusterID) != expectedClusterID {
		return fmt.Errorf("%w: ERR_RUNNER_CLUSTER_ID_MISMATCH: expected clusterId=%q, got %q", ErrBootstrapIdentityMismatch, expectedClusterID, strings.TrimSpace(clusterID))
	}
	expectedRunnerID := strings.TrimSpace(asString(session.Data[bootstrapRunnerIDKey]))
	if expectedRunnerID != "" && strings.TrimSpace(runnerID) != expectedRunnerID {
		return fmt.Errorf("%w: ERR_RUNNER_ID_MISMATCH: expected runnerId=%q, got %q", ErrBootstrapIdentityMismatch, expectedRunnerID, strings.TrimSpace(runnerID))
	}
	expectedNamespace := strings.TrimSpace(asString(session.Data[bootstrapRunnerNamespaceKey]))
	actualNamespace := sanitizePathComponent(strings.TrimSpace(runnerNamespace))
	if actualNamespace == "" {
		actualNamespace = "envpilot-system"
	}
	if expectedNamespace != "" && actualNamespace != expectedNamespace {
		return fmt.Errorf("%w: ERR_RUNNER_NAMESPACE_MISMATCH: expected runnerNamespace=%q, got %q", ErrBootstrapIdentityMismatch, expectedNamespace, actualNamespace)
	}
	expectedMode := strings.ToLower(strings.TrimSpace(asString(session.Data[bootstrapRunnerModeKey])))
	if expectedMode != "" && strings.ToLower(strings.TrimSpace(deploymentMode)) != expectedMode {
		return fmt.Errorf("%w: ERR_RUNNER_DEPLOYMENT_MODE_MISMATCH: expected deploymentMode=%q, got %q", ErrBootstrapIdentityMismatch, expectedMode, strings.ToLower(strings.TrimSpace(deploymentMode)))
	}
	return nil
}

func (s *Server) markBootstrapRunnerConfigTokenUsed(projectID string) error {
	projectID = normalizeSettingsID(projectID)
	if projectID == "" {
		return fmt.Errorf("projectId is required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	_, err := s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapRunnerConfigTokenUsedAtKey: time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
	return err
}

func (s *Server) consumeBootstrapRunnerRegistrationToken(projectID string, token string) error {
	projectID = normalizeSettingsID(projectID)
	token = strings.TrimSpace(token)
	if projectID == "" || token == "" {
		return fmt.Errorf("registration token is required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	if asString(session.Data[bootstrapRunnerTokenProjectKey]) != projectID {
		return fmt.Errorf("registration token is not issued for project %q", projectID)
	}
	if asString(session.Data[bootstrapRunnerTokenHashKey]) == "" {
		return fmt.Errorf("registration token is not configured")
	}
	if subtle.ConstantTimeCompare([]byte(asString(session.Data[bootstrapRunnerTokenHashKey])), []byte(hashToken(token))) != 1 {
		return fmt.Errorf("invalid registration token")
	}
	if usedAt := asString(session.Data[bootstrapRunnerTokenUsedAtKey]); usedAt != "" {
		return fmt.Errorf("%w: registration token already used", store.ErrBootstrapTokenAlreadyUsed)
	}
	expiresAt := asString(session.Data[bootstrapRunnerTokenExpiresAtKey])
	if expiresAt == "" {
		return fmt.Errorf("registration token is expired")
	}
	parsed, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || time.Now().UTC().After(parsed) {
		return fmt.Errorf("registration token is expired")
	}
	_, err = s.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapRunnerTokenUsedAtKey: time.Now().UTC().Format(time.RFC3339Nano),
			bootstrapRunnerStatusKey:      "connected",
		},
	})
	return err
}

func (s *Server) validateBootstrapRunnerRegistrationToken(projectID string, token string) error {
	projectID = normalizeSettingsID(projectID)
	token = strings.TrimSpace(token)
	if projectID == "" || token == "" {
		return fmt.Errorf("registration token is required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	if asString(session.Data[bootstrapRunnerTokenProjectKey]) != projectID {
		return fmt.Errorf("registration token is not issued for project %q", projectID)
	}
	if subtle.ConstantTimeCompare([]byte(asString(session.Data[bootstrapRunnerTokenHashKey])), []byte(hashToken(token))) != 1 {
		return fmt.Errorf("invalid registration token")
	}
	if usedAt := asString(session.Data[bootstrapRunnerTokenUsedAtKey]); usedAt != "" {
		return fmt.Errorf("%w: registration token already used", store.ErrBootstrapTokenAlreadyUsed)
	}
	expiresAt := asString(session.Data[bootstrapRunnerTokenExpiresAtKey])
	if expiresAt == "" {
		return fmt.Errorf("registration token is expired")
	}
	parsed, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || time.Now().UTC().After(parsed) {
		return fmt.Errorf("registration token is expired")
	}
	return nil
}

func (s *Server) validateBootstrapRunnerHeartbeatAuthentication(projectID string, token string, req domain.RunnerHeartbeatRequest) error {
	projectID = normalizeSettingsID(projectID)
	token = strings.TrimSpace(token)
	runnerID := strings.TrimSpace(req.RunnerID)
	if projectID == "" || token == "" || runnerID == "" {
		return fmt.Errorf("projectId, runnerId, and runnerAuthToken are required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	if asString(session.Data[bootstrapRunnerAuthTokenProjectKey]) != projectID {
		return fmt.Errorf("runner auth token is not issued for project %q", projectID)
	}
	if asString(session.Data[bootstrapRunnerAuthTokenHashKey]) == "" {
		return fmt.Errorf("runner auth token is not configured")
	}
	if subtle.ConstantTimeCompare([]byte(asString(session.Data[bootstrapRunnerAuthTokenHashKey])), []byte(hashToken(token))) != 1 {
		return fmt.Errorf("invalid runner auth token")
	}
	expectedRunnerID := strings.TrimSpace(asString(session.Data[bootstrapRunnerIDKey]))
	if expectedRunnerID == "" {
		expectedRunnerID = safeIdentifier(fmt.Sprintf("%s-runner", projectID))
		if expectedRunnerID == "" {
			expectedRunnerID = "envpilot-runner"
		}
	}
	if runnerID != expectedRunnerID {
		return fmt.Errorf("%w: ERR_RUNNER_ID_MISMATCH: expected runnerId=%q, got %q", ErrBootstrapIdentityMismatch, expectedRunnerID, runnerID)
	}
	if clusterID := strings.TrimSpace(req.ClusterID); clusterID != "" {
		expectedClusterID := strings.TrimSpace(asString(session.Data[bootstrapRunnerClusterIDKey]))
		if expectedClusterID != "" && clusterID != expectedClusterID {
			return fmt.Errorf("%w: ERR_RUNNER_CLUSTER_ID_MISMATCH: expected clusterId=%q, got %q", ErrBootstrapIdentityMismatch, expectedClusterID, clusterID)
		}
	}
	if runnerNamespace := strings.TrimSpace(req.RunnerNamespace); runnerNamespace != "" {
		expectedNamespace := strings.TrimSpace(asString(session.Data[bootstrapRunnerNamespaceKey]))
		actualNamespace := sanitizePathComponent(runnerNamespace)
		if actualNamespace == "" {
			actualNamespace = "envpilot-system"
		}
		if expectedNamespace != "" && actualNamespace != expectedNamespace {
			return fmt.Errorf("%w: ERR_RUNNER_NAMESPACE_MISMATCH: expected runnerNamespace=%q, got %q", ErrBootstrapIdentityMismatch, expectedNamespace, actualNamespace)
		}
	}
	if deploymentMode := strings.TrimSpace(req.DeploymentMode); deploymentMode != "" {
		expectedMode := strings.ToLower(strings.TrimSpace(asString(session.Data[bootstrapRunnerModeKey])))
		actualMode := strings.ToLower(deploymentMode)
		if expectedMode != "" && actualMode != expectedMode {
			return fmt.Errorf("%w: ERR_RUNNER_DEPLOYMENT_MODE_MISMATCH: expected deploymentMode=%q, got %q", ErrBootstrapIdentityMismatch, expectedMode, actualMode)
		}
	}
	return nil
}

func (s *Server) validateRunnerRegistrationBinding(projectID string, req domain.RunnerRegistrationRequest) error {
	projectID = normalizeSettingsID(projectID)
	if projectID == "" {
		return fmt.Errorf("projectId is required")
	}
	if s.bootstrapSessions == nil {
		return fmt.Errorf("bootstrap sessions are not configured")
	}
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return err
	}
	expectedClusterID := strings.TrimSpace(asString(session.Data[bootstrapRunnerClusterIDKey]))
	actualClusterID := strings.TrimSpace(req.ClusterID)
	if expectedClusterID != "" && actualClusterID != expectedClusterID {
		return fmt.Errorf("%w: ERR_RUNNER_CLUSTER_ID_MISMATCH: expected clusterId=%q, got %q", ErrBootstrapIdentityMismatch, expectedClusterID, actualClusterID)
	}
	expectedRunnerID := strings.TrimSpace(asString(session.Data[bootstrapRunnerIDKey]))
	if expectedRunnerID == "" {
		expectedRunnerID = safeIdentifier(fmt.Sprintf("%s-runner", projectID))
		if expectedRunnerID == "" {
			expectedRunnerID = "envpilot-runner"
		}
	}
	actualRunnerID := strings.TrimSpace(req.RunnerID)
	if actualRunnerID != expectedRunnerID {
		return fmt.Errorf("%w: ERR_RUNNER_ID_MISMATCH: expected runnerId=%q, got %q", ErrBootstrapIdentityMismatch, expectedRunnerID, actualRunnerID)
	}
	expectedNamespace := strings.TrimSpace(asString(session.Data[bootstrapRunnerNamespaceKey]))
	actualNamespace := sanitizePathComponent(strings.TrimSpace(req.RunnerNamespace))
	if actualNamespace == "" {
		actualNamespace = "envpilot-system"
	}
	if expectedNamespace != "" && actualNamespace != expectedNamespace {
		return fmt.Errorf("%w: ERR_RUNNER_NAMESPACE_MISMATCH: expected runnerNamespace=%q, got %q", ErrBootstrapIdentityMismatch, expectedNamespace, actualNamespace)
	}
	expectedMode := strings.ToLower(strings.TrimSpace(asString(session.Data[bootstrapRunnerModeKey])))
	actualMode := strings.ToLower(strings.TrimSpace(req.DeploymentMode))
	if expectedMode != "" && actualMode != expectedMode {
		return fmt.Errorf("%w: ERR_RUNNER_DEPLOYMENT_MODE_MISMATCH: expected deploymentMode=%q, got %q", ErrBootstrapIdentityMismatch, expectedMode, actualMode)
	}
	return nil
}

func (s *Server) selectedNamespacesForProject(projectID string) ([]string, error) {
	session, err := s.bootstrapSessions.Get(projectID)
	if err != nil {
		return nil, err
	}
	return asStringSlice(session.Data[bootstrapSelectedNamespacesKey]), nil
}

func asString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func asStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			if normalized := strings.TrimSpace(item); normalized != "" {
				items = append(items, normalized)
			}
		}
		return items
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				items = append(items, strings.TrimSpace(text))
			}
		}
		return items
	default:
		return nil
	}
}

func safeIdentifier(value string) string {
	return sanitizePathComponent(strings.TrimSpace(value))
}

func asStringAnyMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	default:
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		var result map[string]any
		if err := json.Unmarshal(payload, &result); err != nil {
			return nil, false
		}
		return result, true
	}
}

func parseRFC3339Pointer(value string) (*time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}

func asCapabilityReport(value any) (domain.ClusterCapabilityReport, bool) {
	payload, err := json.Marshal(value)
	if err != nil {
		return domain.ClusterCapabilityReport{}, false
	}
	var report domain.ClusterCapabilityReport
	if err := json.Unmarshal(payload, &report); err != nil {
		return domain.ClusterCapabilityReport{}, false
	}
	return report, true
}

func asResourceSnapshots(value any) []domain.ResourceSnapshot {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var snapshots []domain.ResourceSnapshot
	if err := json.Unmarshal(payload, &snapshots); err != nil {
		return nil
	}
	return snapshots
}

func asBootstrapManifestTemplates(value any) []bootstrap.ManifestTemplate {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var items []bootstrap.ManifestTemplate
	if err := json.Unmarshal(payload, &items); err != nil {
		return nil
	}
	return items
}

func bootstrapCompileRequested(req app.BootstrapSessionUpdate) bool {
	if req.Status == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(*req.Status), string(domain.BootstrapSessionStatusCompiled))
}

func bootstrapUpdateContainsManifestTemplates(req app.BootstrapSessionUpdate) bool {
	stepData := req.StepData
	if stepData == nil {
		stepData = req.StepDataSnake
	}
	if stepData == nil {
		return false
	}
	_, ok := stepData[bootstrapManifestTemplatesKey]
	return ok
}

func bootstrapResourcePolicyFromUpdate(req app.BootstrapSessionUpdate) (bootstrap.ResourcePolicyConfig, bool, error) {
	stepData := req.StepData
	if stepData == nil {
		stepData = req.StepDataSnake
	}
	return bootstrapResourcePolicyFromData(stepData)
}

func bootstrapNetworkPolicyFromUpdate(req app.BootstrapSessionUpdate, fallbackBaseNamespaces []string) (bootstrap.NetworkPolicyConfig, bool, error) {
	stepData := req.StepData
	if stepData == nil {
		stepData = req.StepDataSnake
	}
	return bootstrapNetworkPolicyFromData(stepData, fallbackBaseNamespaces)
}

func bootstrapCleanupSafetyFromUpdate(req app.BootstrapSessionUpdate) (bootstrap.CleanupSafetyConfig, bool, []string, error) {
	stepData := req.StepData
	if stepData == nil {
		stepData = req.StepDataSnake
	}
	return bootstrapCleanupSafetyFromData(stepData)
}

func bootstrapDefaultBaseNamespaces(project domain.Project) []string {
	if strings.TrimSpace(project.BaseEnvConfig.Namespace) != "" {
		return []string{strings.TrimSpace(project.BaseEnvConfig.Namespace)}
	}
	return []string{"dev-base"}
}

func (s *Server) refreshBootstrapManifestTemplates(project domain.Project, session domain.BootstrapSession) (domain.BootstrapSession, error) {
	templates, ok, err := buildBootstrapManifestTemplates(project, session.Data)
	if err != nil {
		return domain.BootstrapSession{}, app.ValidationError{Message: fmt.Sprintf("manifest template generation failed: %v", err)}
	}
	if !ok {
		return session, nil
	}
	return s.bootstrapSessions.Update(project.ID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapManifestTemplatesKey: templates,
		},
	})
}

func (s *Server) compileBootstrapSession(project domain.Project, session domain.BootstrapSession) (domain.BootstrapSession, error) {
	var err error
	var validation bootstrap.ManifestTemplateValidationResult
	session, validation, err = s.validateBootstrapSessionForCompilation(project, session)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	if !validation.Valid {
		return domain.BootstrapSession{}, app.ValidationError{Message: formatManifestTemplateValidationError(validation.Issues)}
	}
	compiled := string(domain.BootstrapSessionStatusCompiled)
	return s.bootstrapSessions.Update(project.ID, app.BootstrapSessionUpdate{Status: &compiled})
}

func formatManifestTemplateValidationError(issues []bootstrap.ManifestTemplateValidationIssue) string {
	if len(issues) == 0 {
		return "template validation failed"
	}
	const maxShown = 8
	parts := make([]string, 0, min(len(issues), maxShown))
	for idx, issue := range issues {
		if idx >= maxShown {
			break
		}
		location := strings.TrimSpace(issue.File)
		if location == "" {
			location = "template"
		}
		if issue.Line > 0 {
			location += ":" + strconv.Itoa(issue.Line)
		}
		if issue.Column > 0 {
			location += ":" + strconv.Itoa(issue.Column)
		}
		message := strings.TrimSpace(issue.Message)
		if message == "" {
			message = issue.Code
		}
		parts = append(parts, location+": "+message)
	}
	text := "template validation failed: " + strings.Join(parts, "; ")
	if len(issues) > maxShown {
		text += fmt.Sprintf("; ... %d more", len(issues)-maxShown)
	}
	return text
}

func buildBootstrapManifestTemplates(project domain.Project, data map[string]any) ([]bootstrap.ManifestTemplate, bool, error) {
	if len(data) == 0 {
		return nil, false, nil
	}
	snapshots := asResourceSnapshots(data[bootstrapResourceScanReportKey])
	options := bootstrap.ManifestTemplateGeneratorOptions{
		FeatureNamespaceTemplate: firstNonEmpty(
			asString(data["featureNamespaceTemplate"]),
			asString(data["feature_namespace_template"]),
			"envpilot-pr-{{ .PRNumber }}",
		),
		CommitSHAPlaceholder: "{{ .CommitSHA }}",
		ImagePattern: firstNonEmpty(
			asString(data["imagePattern"]),
			asString(data["image_pattern"]),
		),
		PreviewDomain: firstNonEmpty(
			asString(data["previewDomain"]),
			asString(data["preview_domain"]),
			strings.TrimSpace(project.BaseEnvConfig.Domain),
		),
		HostPatternTemplate: firstNonEmpty(
			asString(data["hostPatternTemplate"]),
			asString(data["host_pattern_template"]),
		),
		Labels: map[string]string{
			"envpilot.io/project": project.ID,
		},
	}
	if project.ProductID != "" {
		options.Labels["envpilot.io/product"] = project.ProductID
	}
	selections := asResourceSelections(data["resourceReview"])
	templates := make([]bootstrap.ManifestTemplate, 0, len(snapshots)+2)
	if len(snapshots) > 0 {
		generated, err := bootstrap.GenerateManifestTemplates(snapshots, selections, options)
		if err != nil {
			return nil, false, err
		}
		templates = append(templates, generated...)
	}
	resourcePolicy, hasResourcePolicy, err := bootstrapResourcePolicyFromData(data)
	if err != nil {
		return nil, false, err
	}
	if hasResourcePolicy {
		generated, err := bootstrap.GenerateResourcePolicyTemplates(resourcePolicy, options.FeatureNamespaceTemplate, options.Labels, options.Annotations)
		if err != nil {
			return nil, false, err
		}
		templates = append(templates, generated...)
	}
	networkPolicy, hasNetworkPolicy, err := bootstrapNetworkPolicyFromData(data, bootstrapDefaultBaseNamespaces(project))
	if err != nil {
		return nil, false, err
	}
	if hasNetworkPolicy {
		generated, err := bootstrap.GenerateNetworkPolicyTemplates(networkPolicy, options.FeatureNamespaceTemplate, options.Labels, options.Annotations)
		if err != nil {
			return nil, false, err
		}
		templates = append(templates, generated...)
	}
	if len(templates) == 0 {
		return nil, false, nil
	}
	sort.Slice(templates, func(i, j int) bool {
		if templates[i].Kind != templates[j].Kind {
			return templates[i].Kind < templates[j].Kind
		}
		if templates[i].Namespace != templates[j].Namespace {
			return templates[i].Namespace < templates[j].Namespace
		}
		return templates[i].Name < templates[j].Name
	})
	return templates, true, nil
}

func bootstrapResourcePolicyFromData(data map[string]any) (bootstrap.ResourcePolicyConfig, bool, error) {
	if len(data) == 0 {
		return bootstrap.ResourcePolicyConfig{}, false, nil
	}
	source := data
	if nested, ok := asStringAnyMap(data["resourcePolicy"]); ok && len(nested) > 0 {
		source = nested
	} else if nested, ok := asStringAnyMap(data["resource_policy"]); ok && len(nested) > 0 {
		source = nested
	}
	policy := bootstrap.ResourcePolicyConfig{
		DefaultTTLHours:       asIntAny(source["defaultTTLHours"], asIntAny(source["default_ttl_hours"], asIntAny(data["defaultTTLHours"], 0))),
		CPURequest:            firstNonEmpty(asString(source["cpuRequest"]), asString(source["cpu_request"]), asString(data["cpuRequest"]), asString(data["cpu_request"])),
		CPULimit:              firstNonEmpty(asString(source["cpuLimit"]), asString(source["cpu_limit"]), asString(data["cpuLimit"]), asString(data["cpu_limit"])),
		MemoryRequest:         firstNonEmpty(asString(source["memoryRequest"]), asString(source["memory_request"]), asString(data["memoryRequest"]), asString(data["memory_request"])),
		MemoryLimit:           firstNonEmpty(asString(source["memoryLimit"]), asString(source["memory_limit"]), asString(data["memoryLimit"]), asString(data["memory_limit"])),
		StorageQuota:          firstNonEmpty(asString(source["storageQuota"]), asString(source["storage_quota"]), asString(data["storageQuota"]), asString(data["storage_quota"])),
		MaxActiveEnvironments: asIntAny(source["maxActiveEnvironments"], asIntAny(source["max_active_environments"], asIntAny(data["maxActiveEnvironments"], asIntAny(data["max_active_environments"], 0)))),
	}
	anyField := policy.DefaultTTLHours != 0 ||
		policy.CPURequest != "" ||
		policy.CPULimit != "" ||
		policy.MemoryRequest != "" ||
		policy.MemoryLimit != "" ||
		policy.StorageQuota != "" ||
		policy.MaxActiveEnvironments != 0
	if !anyField {
		return bootstrap.ResourcePolicyConfig{}, false, nil
	}
	return policy, true, nil
}

func bootstrapNetworkPolicyFromData(data map[string]any, fallbackBaseNamespaces []string) (bootstrap.NetworkPolicyConfig, bool, error) {
	if len(data) == 0 {
		return bootstrap.NetworkPolicyConfig{}, false, nil
	}
	source := data
	if nested, ok := asStringAnyMap(data["networkPolicy"]); ok && len(nested) > 0 {
		source = nested
	} else if nested, ok := asStringAnyMap(data["network_policy"]); ok && len(nested) > 0 {
		source = nested
	}

	egressMode := firstNonEmpty(
		asString(source["networkEgressMode"]),
		asString(source["egressMode"]),
		asString(source["egress_mode"]),
		asString(data["networkEgressMode"]),
		asString(data["egressMode"]),
		asString(data["egress_mode"]),
	)
	baseNamespaces := asStringSlice(source["baseNamespaces"])
	if len(baseNamespaces) == 0 {
		baseNamespaces = asStringSlice(source["base_namespaces"])
	}
	if len(baseNamespaces) == 0 {
		baseNamespaces = asStringSlice(data[bootstrapSelectedNamespacesKey])
	}
	if len(baseNamespaces) == 0 {
		baseNamespaces = fallbackBaseNamespaces
	}

	featureToBase, hasFeatureToBase := optionalBoolAny(source["featureToBase"])
	if !hasFeatureToBase {
		featureToBase, hasFeatureToBase = optionalBoolAny(source["feature_to_base"])
	}
	if !hasFeatureToBase {
		featureToBase, hasFeatureToBase = optionalBoolAny(data["featureToBase"])
	}
	if !hasFeatureToBase {
		featureToBase, hasFeatureToBase = optionalBoolAny(data["networkFeatureToBase"])
	}

	baseToFeature, hasBaseToFeature := optionalBoolAny(source["baseToFeature"])
	if !hasBaseToFeature {
		baseToFeature, hasBaseToFeature = optionalBoolAny(source["base_to_feature"])
	}
	if !hasBaseToFeature {
		baseToFeature, hasBaseToFeature = optionalBoolAny(data["baseToFeature"])
	}
	if !hasBaseToFeature {
		baseToFeature, hasBaseToFeature = optionalBoolAny(data["networkBaseToFeature"])
	}
	allowBaseNamespacePolicies, hasAllowBaseNamespacePolicies := optionalBoolAny(source["allowBaseNamespacePolicies"])
	if !hasAllowBaseNamespacePolicies {
		allowBaseNamespacePolicies, hasAllowBaseNamespacePolicies = optionalBoolAny(source["allow_base_namespace_policies"])
	}
	if !hasAllowBaseNamespacePolicies {
		allowBaseNamespacePolicies, hasAllowBaseNamespacePolicies = optionalBoolAny(data["allowBaseNamespacePolicies"])
	}
	if !hasAllowBaseNamespacePolicies {
		allowBaseNamespacePolicies, hasAllowBaseNamespacePolicies = optionalBoolAny(data["networkAllowBaseNamespacePolicies"])
	}

	anyField := hasFeatureToBase || hasBaseToFeature || hasAllowBaseNamespacePolicies || egressMode != "" || len(asStringSlice(source["baseNamespaces"])) > 0 || len(asStringSlice(source["base_namespaces"])) > 0
	if !anyField {
		return bootstrap.NetworkPolicyConfig{}, false, nil
	}
	if egressMode == "" {
		egressMode = "restricted"
	}
	return bootstrap.NetworkPolicyConfig{
		FeatureToBase:              featureToBase,
		BaseToFeature:              baseToFeature,
		EgressMode:                 egressMode,
		BaseNamespaces:             baseNamespaces,
		AllowBaseNamespacePolicies: allowBaseNamespacePolicies,
	}, true, nil
}

func bootstrapCleanupSafetyFromData(data map[string]any) (bootstrap.CleanupSafetyConfig, bool, []string, error) {
	if len(data) == 0 {
		return bootstrap.CleanupSafetyConfig{}, false, nil, nil
	}
	source := data
	if nested, ok := asStringAnyMap(data["cleanupSafety"]); ok && len(nested) > 0 {
		source = nested
	} else if nested, ok := asStringAnyMap(data["cleanup_safety"]); ok && len(nested) > 0 {
		source = nested
	}

	protected := asStringSlice(source["cleanupProtectedNamespaces"])
	if len(protected) == 0 {
		protected = asStringSlice(source["protectedNamespaces"])
	}
	if len(protected) == 0 {
		protected = asStringSlice(source["protected_namespaces"])
	}
	if len(protected) == 0 {
		protected = commaSeparatedStrings(asString(source["cleanupProtectedNamespaces"]))
	}
	if len(protected) == 0 {
		protected = commaSeparatedStrings(asString(source["protectedNamespaces"]))
	}
	if len(protected) == 0 {
		protected = commaSeparatedStrings(asString(source["protected_namespaces"]))
	}

	labelsOnly, hasLabelsOnly := optionalBoolAny(source["cleanupDeleteEnvPilotLabelsOnly"])
	if !hasLabelsOnly {
		labelsOnly, hasLabelsOnly = optionalBoolAny(source["deleteEnvPilotLabeledOnly"])
	}
	if !hasLabelsOnly {
		labelsOnly, hasLabelsOnly = optionalBoolAny(source["delete_envpilot_labeled_only"])
	}
	finalizerStrategy := firstNonEmpty(
		asString(source["cleanupFinalizerStrategy"]),
		asString(source["finalizerStrategy"]),
		asString(source["finalizer_strategy"]),
	)
	anyField := len(protected) > 0 || hasLabelsOnly || finalizerStrategy != ""
	if !anyField {
		return bootstrap.CleanupSafetyConfig{}, false, nil, nil
	}
	config := bootstrap.DefaultCleanupSafetyConfig()
	if len(protected) > 0 {
		config.ProtectedNamespaces = protected
	}
	if hasLabelsOnly {
		config.DeleteEnvPilotLabeledOnly = labelsOnly
	}
	if finalizerStrategy != "" {
		config.FinalizerStrategy = finalizerStrategy
	}
	target := []string{firstNonEmpty(
		asString(data["featureNamespaceTemplate"]),
		asString(data["feature_namespace_template"]),
		"envpilot-pr-{{ .PRNumber }}",
	)}
	return config, true, target, nil
}

func commaSeparatedStrings(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

func asResourceSelections(value any) map[string]bootstrap.ResourceSelection {
	data, ok := asStringAnyMap(value)
	if !ok || len(data) == 0 {
		return nil
	}
	selections := make(map[string]bootstrap.ResourceSelection, len(data))
	for key, raw := range data {
		id := strings.TrimSpace(key)
		item, ok := asStringAnyMap(raw)
		if !ok || id == "" {
			continue
		}
		include := asBoolAny(item["include"], true)
		strategy := asString(item["strategy"])
		selections[id] = bootstrap.ResourceSelection{
			Include:  include,
			Strategy: strategy,
		}
	}
	return selections
}

func asBoolAny(value any, defaultValue bool) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		switch normalized {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		default:
			return defaultValue
		}
	default:
		return defaultValue
	}
}

func optionalBoolAny(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		if normalized == "" {
			return false, false
		}
		return normalized == "true" || normalized == "1" || normalized == "yes", true
	case float64:
		return typed != 0, true
	case int:
		return typed != 0, true
	default:
		return false, false
	}
}

func asIntAny(value any, defaultValue int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return defaultValue
		}
		return parsed
	default:
		return defaultValue
	}
}

func normalizeSettingsID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.Trim(value, "-")
	return value
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONWithError(w http.ResponseWriter, status int, payload any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return err
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": sanitizeErrorMessage(err)})
}

func writeJobResponse(w http.ResponseWriter, job jobs.Job) {
	status := http.StatusAccepted
	if job.Status == jobs.StatusFailed {
		status = http.StatusInternalServerError
	} else if job.Status == jobs.StatusSucceeded && job.Type == jobs.TypeCreateEnvironment {
		status = http.StatusCreated
	} else if job.Status == jobs.StatusSucceeded && job.Type == jobs.TypeDeleteEnvironment {
		status = http.StatusOK
	}
	writeJSON(w, status, job)
}

func writeJobResponseWithStatus(w http.ResponseWriter, job jobs.Job, successStatus int) {
	if job.Status == jobs.StatusFailed {
		writeJSON(w, http.StatusInternalServerError, job)
		return
	}
	writeJSON(w, successStatus, job)
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		role, _ := r.Context().Value(apiRequestRoleKey).(apiRole)
		roleValue := "public"
		if role != "" {
			roleValue = string(role)
		}
		traceID, _ := r.Context().Value(apiRequestTraceIDKey).(string)
		traceSpanID, _ := r.Context().Value(apiRequestSpanIDKey).(string)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"role", roleValue,
			"trace_id", traceID,
			"trace_span_id", traceSpanID,
			"duration_ms", time.Since(start).Milliseconds())
	})
}
