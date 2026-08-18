package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/envpilot/contracts/domain"
	"github.com/envpilot/contracts/sdk/go/envplanesdk"
	"github.com/envpilot/runner/internal/orchestrator"
)

const (
	runnerCommandAPIVersion       = "1"
	runnerCommandAPIVersionHeader = "X-EnvPlane-Runner-Command-API-Version"
)

type runnerConfig struct {
	EnvDiagnostics                       []string
	ControlPlaneURL                      string
	ControlPlaneEndpointMode             string
	ControlPlaneCAFile                   string
	ControlPlaneTLSServerName            string
	ControlPlaneConnectivityMaxAttempts  int
	ControlPlaneConnectivityInitialDelay time.Duration
	ControlPlaneConnectivityMaxDelay     time.Duration
	ControlPlaneConnectivityDeadline     time.Duration
	RemoteGeneration                     int64
	ProjectID                            string
	ClusterID                            string
	RunnerID                             string
	RunnerNamespace                      string
	DeploymentMode                       string
	RegistrationToken                    string
	RunnerAuthToken                      string
	RunnerAuthTokenFile                  string
	ProjectConfigURL                     string
	ProjectConfigToken                   string
	HeartbeatInterval                    time.Duration
	ReportTimeout                        time.Duration
	HealthAddr                           string
	RunnerVersion                        string
	FeatureEnvWriterMode                 string
	FeatureEnvWriterNamespaces           []string
}

func runnerConfigFromEnv() runnerConfig {
	authTokenFile := getenv("ENVPILOT_RUNNER_AUTH_TOKEN_FILE", "")
	registrationToken := getenv("ENVPILOT_RUNNER_REGISTRATION_TOKEN", "")
	authToken := getenv("ENVPILOT_RUNNER_AUTH_TOKEN", "")
	if strings.TrimSpace(authToken) == "" {
		authToken = readRuntimeTokenFile(authTokenFile)
	}
	// A bootstrap-token rotation updates the runner Secret, but the runner auth
	// token deliberately survives in the auth PVC. Prefer the new one-time token
	// only when it is demonstrably newer than the token that produced the
	// persisted auth token. This preserves ordinary restarts while allowing an
	// in-place Secret update plus rollout to recover the same release.
	if registrationTokenChanged(authTokenFile, registrationToken) {
		authToken = ""
	}
	cfg := runnerConfig{
		ControlPlaneURL:                      strings.TrimRight(getenv("ENVPILOT_CONTROL_PLANE_URL", ""), "/"),
		ControlPlaneEndpointMode:             strings.TrimSpace(getenv("ENVPILOT_CONTROL_PLANE_ENDPOINT_MODE", "sameCluster")),
		ControlPlaneCAFile:                   getenv("ENVPILOT_CONTROL_PLANE_CA_FILE", ""),
		ControlPlaneTLSServerName:            getenv("ENVPILOT_CONTROL_PLANE_TLS_SERVER_NAME", ""),
		ControlPlaneConnectivityMaxAttempts:  getenvInt("ENVPILOT_CONTROL_PLANE_CONNECTIVITY_MAX_ATTEMPTS", 12),
		ControlPlaneConnectivityInitialDelay: time.Duration(getenvInt("ENVPILOT_CONTROL_PLANE_CONNECTIVITY_INITIAL_BACKOFF_SECONDS", 1)) * time.Second,
		ControlPlaneConnectivityMaxDelay:     time.Duration(getenvInt("ENVPILOT_CONTROL_PLANE_CONNECTIVITY_MAX_BACKOFF_SECONDS", 5)) * time.Second,
		ControlPlaneConnectivityDeadline:     time.Duration(maxInt(5, getenvInt("ENVPILOT_CONTROL_PLANE_CONNECTIVITY_DEADLINE_SECONDS", 120))) * time.Second,
		RemoteGeneration:                     int64(getenvInt("ENVPILOT_REMOTE_GENERATION", 0)),
		ProjectID:                            getenv("ENVPILOT_PROJECT_ID", ""),
		ClusterID:                            getenv("ENVPILOT_CLUSTER_ID", "default"),
		RunnerID:                             getenv("ENVPILOT_RUNNER_ID", hostnameFallback("envpilot-runner")),
		RunnerNamespace:                      getenv("ENVPILOT_RUNNER_NAMESPACE", "envpilot-system"),
		DeploymentMode:                       strings.ToLower(getenv("ENVPILOT_RUNNER_DEPLOYMENT_MODE", "helm")),
		RegistrationToken:                    registrationToken,
		RunnerAuthToken:                      authToken,
		RunnerAuthTokenFile:                  authTokenFile,
		ProjectConfigURL:                     getenv("ENVPILOT_PROJECT_CONFIG_URL", ""),
		ProjectConfigToken:                   getenv("ENVPILOT_PROJECT_CONFIG_TOKEN", ""),
		HeartbeatInterval:                    time.Duration(getenvInt("ENVPILOT_RUNNER_HEARTBEAT_INTERVAL_SECONDS", 30)) * time.Second,
		ReportTimeout:                        time.Duration(getenvInt("ENVPILOT_RUNNER_REPORT_TIMEOUT_SECONDS", 10)) * time.Second,
		HealthAddr:                           getenv("ENVPILOT_RUNNER_HEALTH_ADDR", ":8080"),
		RunnerVersion:                        getenv("ENVPILOT_RUNNER_VERSION", "dev"),
		FeatureEnvWriterMode:                 strings.TrimSpace(getenv("ENVPILOT_FEATURE_ENV_WRITER_MODE", "releaseNamespace")),
		FeatureEnvWriterNamespaces:           normalizeRunnerNamespaceList(getenv("ENVPILOT_FEATURE_ENV_WRITER_NAMESPACES", "")),
	}
	cfg.EnvDiagnostics = legacyDiagnostics()
	return cfg
}

func normalizeRunnerNamespaceList(raw string) []string {
	seen := map[string]struct{}{}
	items := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		namespace := strings.TrimSpace(value)
		if namespace == "" {
			continue
		}
		if _, exists := seen[namespace]; exists {
			continue
		}
		seen[namespace] = struct{}{}
		items = append(items, namespace)
	}
	return items
}

// helmTargetNamespaces is deliberately finite. Kubernetes RBAC cannot safely
// express a wildcard namespace RoleBinding, so dynamic namespaces are never
// claimed as writable until the Runner chart has rendered a Role/RoleBinding
// for the exact namespace.
func (c runnerConfig) helmTargetNamespaces() []string {
	if len(c.FeatureEnvWriterNamespaces) > 0 {
		return append([]string(nil), c.FeatureEnvWriterNamespaces...)
	}
	if strings.EqualFold(strings.TrimSpace(c.FeatureEnvWriterMode), "releaseNamespace") {
		return normalizeRunnerNamespaceList(c.RunnerNamespace)
	}
	return []string{}
}

func (c runnerConfig) canRunHelmInNamespace(namespace string) bool {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return false
	}
	for _, permitted := range c.helmTargetNamespaces() {
		if namespace == permitted {
			return true
		}
	}
	return false
}

func (c runnerConfig) validate() error {
	if strings.TrimSpace(c.ControlPlaneURL) == "" {
		return fmt.Errorf("ENVPILOT_CONTROL_PLANE_URL is required")
	}
	if err := validateRunnerControlPlaneEndpoint(c.ControlPlaneURL, c.ControlPlaneEndpointMode); err != nil {
		return err
	}
	if _, err := newRunnerControlPlaneHTTPClientWithTLS(c.ReportTimeout, c.ControlPlaneCAFile, c.ControlPlaneTLSServerName); err != nil {
		return fmt.Errorf("invalid control-plane TLS configuration: %w", err)
	}
	if strings.TrimSpace(c.ProjectID) == "" {
		return fmt.Errorf("ENVPILOT_PROJECT_ID is required")
	}
	if strings.TrimSpace(c.ClusterID) == "" {
		return fmt.Errorf("ENVPILOT_CLUSTER_ID is required")
	}
	if strings.TrimSpace(c.RunnerID) == "" {
		return fmt.Errorf("ENVPILOT_RUNNER_ID is required")
	}
	if strings.TrimSpace(c.RunnerNamespace) == "" {
		return fmt.Errorf("ENVPILOT_RUNNER_NAMESPACE is required")
	}
	if strings.TrimSpace(c.DeploymentMode) == "" {
		return fmt.Errorf("ENVPILOT_RUNNER_DEPLOYMENT_MODE is required")
	}
	mode := strings.TrimSpace(c.FeatureEnvWriterMode)
	if mode != "releaseNamespace" && mode != "preconfiguredNamespaces" && mode != "generatedFeatureNamespaces" {
		return fmt.Errorf("ENVPILOT_FEATURE_ENV_WRITER_MODE must be releaseNamespace, preconfiguredNamespaces, or generatedFeatureNamespaces")
	}
	if (mode == "preconfiguredNamespaces" || mode == "generatedFeatureNamespaces") && len(c.FeatureEnvWriterNamespaces) == 0 {
		return fmt.Errorf("ENVPILOT_FEATURE_ENV_WRITER_NAMESPACES is required for %s", mode)
	}
	if strings.TrimSpace(c.RegistrationToken) == "" && strings.TrimSpace(c.RunnerAuthToken) == "" {
		return fmt.Errorf("set ENVPILOT_RUNNER_REGISTRATION_TOKEN or ENVPILOT_RUNNER_AUTH_TOKEN")
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("heartbeat interval must be positive")
	}
	if c.ReportTimeout <= 0 {
		return fmt.Errorf("report timeout must be positive")
	}
	return nil
}

func validateRunnerControlPlaneEndpoint(rawURL, endpointMode string) error {
	mode := strings.ToLower(strings.TrimSpace(endpointMode))
	if mode == "" {
		mode = "samecluster"
	}
	if mode != "samecluster" && mode != "remote" {
		return fmt.Errorf("ENVPILOT_CONTROL_PLANE_ENDPOINT_MODE must be sameCluster or remote")
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Errorf("ENVPILOT_CONTROL_PLANE_URL must be an HTTP(S) URL")
	}
	if mode != "remote" {
		return nil
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("remote ENVPILOT_CONTROL_PLANE_URL must be an explicit stable HTTPS URL without credentials, query parameters, or fragments")
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "envpilot.local" || host == "host.minikube.internal" || strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.") {
		return fmt.Errorf("remote ENVPILOT_CONTROL_PLANE_URL must be target-pod-reachable, not host-local or Kubernetes Service DNS")
	}
	return nil
}

func newRunnerControlPlaneHTTPClientWithTLS(timeout time.Duration, caFile, serverName string) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(caFile) != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read control-plane CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("control-plane CA file contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: strings.TrimSpace(serverName), MinVersion: tls.VersionTLS12}
	} else if strings.TrimSpace(serverName) != "" {
		transport.TLSClientConfig = &tls.Config{ServerName: strings.TrimSpace(serverName), MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// runRunnerConnectivityCheck runs before installation from the Runner image
// itself. It verifies only /api/v1/health and therefore never consumes a
// one-time registration or project-config credential.
func ConnectivityCheck(logger *slog.Logger) {
	cfg := runnerConfigFromEnv()
	if strings.TrimSpace(cfg.ControlPlaneURL) == "" {
		logger.Error("runner control-plane connectivity check failed", "error", "ENVPILOT_CONTROL_PLANE_URL is required")
		os.Exit(1)
	}
	if err := validateRunnerControlPlaneEndpoint(cfg.ControlPlaneURL, cfg.ControlPlaneEndpointMode); err != nil {
		logger.Error("runner control-plane connectivity check failed", "error", err)
		os.Exit(1)
	}
	deadline := cfg.ControlPlaneConnectivityDeadline
	if deadline < 5*time.Second {
		deadline = 5 * time.Second
	}
	if deadline > 10*time.Minute {
		deadline = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if err := runnerConnectivityCheckWithRetry(ctx, logger, cfg); err != nil {
		logger.Error("runner control-plane connectivity check failed", "error", err, "retryable", true, "maxAttempts", cfg.ControlPlaneConnectivityMaxAttempts)
		os.Exit(1)
	}
	logger.Info("runner control-plane connectivity check completed", "control_plane_url", cfg.ControlPlaneURL)
}

func runnerConnectivityCheckWithRetry(ctx context.Context, logger *slog.Logger, cfg runnerConfig) error {
	maxAttempts := cfg.ControlPlaneConnectivityMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 12
	}
	initial := cfg.ControlPlaneConnectivityInitialDelay
	if initial < 1*time.Second {
		initial = 1 * time.Second
	}
	maxDelay := cfg.ControlPlaneConnectivityMaxDelay
	if maxDelay < initial {
		maxDelay = initial
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client, err := newRunnerControlPlaneHTTPClientWithTLS(cfg.ReportTimeout, cfg.ControlPlaneCAFile, cfg.ControlPlaneTLSServerName)
		if err != nil {
			return err
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.ControlPlaneURL, "/")+"/api/v1/health", nil)
		if reqErr != nil {
			return reqErr
		}
		resp, doErr := client.Do(req)
		if doErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				return nil
			}
			lastErr = fmt.Errorf("endpoint_unhealthy: HTTP %d", resp.StatusCode)
		} else {
			lastErr = fmt.Errorf("endpoint_unreachable: %w", doErr)
		}
		if attempt >= maxAttempts {
			break
		}
		delay := initial << (attempt - 1)
		if delay > maxDelay || delay <= 0 {
			delay = maxDelay
		}
		if logger != nil {
			logger.Info("runner preflight delay before retry", "attempt", attempt, "nextAttemptIn", delay.String(), "error", lastErr)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr == nil {
		return nil
	}
	return fmt.Errorf("control-plane preflight retry limit exceeded: %w", lastErr)
}

func Run(logger *slog.Logger) {
	cfg := runnerConfigFromEnv()
	if err := cfg.validate(); err != nil {
		logger.Error("invalid runner configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	health := &runnerHealth{}
	runtimeState := newRunnerRuntimeState()
	go serveRunnerHealth(ctx, cfg.HealthAddr, health, logger)

	client, err := newRunnerControlPlaneHTTPClientWithTLS(cfg.ReportTimeout, cfg.ControlPlaneCAFile, cfg.ControlPlaneTLSServerName)
	if err != nil {
		logger.Error("invalid control-plane TLS configuration", "error", err)
		return
	}
	var registeredNow bool
	bootstrapRegistrationToken := cfg.RegistrationToken
	cfg, registeredNow, err = ensureRunnerRuntimeAuth(ctx, cfg, client, logger)
	if err != nil {
		if isRunnerStaleBootstrapIdentityError(err) {
			markRunnerStaleBootstrapIdentity(runtimeState, health, logger, err)
			<-ctx.Done()
			return
		}
		health.set(false)
		logger.Error("runner registration failed", "error", err)
		os.Exit(1)
	}
	if registeredNow {
		if err := fetchRunnerProjectConfig(ctx, cfg, client, logger); err != nil {
			if isRunnerStaleBootstrapIdentityError(err) {
				markRunnerStaleBootstrapIdentity(runtimeState, health, logger, err)
				<-ctx.Done()
				return
			}
			logger.Warn("runner project config fetch failed", "error", err)
		}
	}
	preflight := probeRunnerManagementEndpoint(ctx, cfg, client)
	initialStatus, initialError := string(domain.RunnerHeartbeatStatusOnline), ""
	if preflight.Code != "passed" {
		initialStatus, initialError = string(domain.RunnerHeartbeatStatusDegraded), "management endpoint preflight failed: "+preflight.Code
	}
	err = reportRunnerHeartbeatWithEndpointPreflight(ctx, cfg, client, initialStatus, initialError, preflight)
	if err != nil && isRunnerAuthTokenNotIssuedError(err) && strings.TrimSpace(bootstrapRegistrationToken) != "" {
		logger.Warn("persisted runner auth token was rejected; re-registering with bootstrap credentials", "project_id", cfg.ProjectID, "runner_id", cfg.RunnerID)
		if clearErr := clearRuntimeToken(cfg.RunnerAuthTokenFile); clearErr != nil {
			logger.Error("clear rejected runner auth token", "error", clearErr)
			return
		}
		cfg.RunnerAuthToken = ""
		cfg.RegistrationToken = bootstrapRegistrationToken
		cfg, registeredNow, err = ensureRunnerRuntimeAuth(ctx, cfg, client, logger)
		if err == nil && registeredNow {
			if configErr := fetchRunnerProjectConfig(ctx, cfg, client, logger); configErr != nil {
				logger.Warn("runner project config fetch failed after auth recovery", "error", configErr)
			}
		}
		if err == nil {
			preflight = probeRunnerManagementEndpoint(ctx, cfg, client)
			initialStatus, initialError = string(domain.RunnerHeartbeatStatusOnline), ""
			if preflight.Code != "passed" {
				initialStatus, initialError = string(domain.RunnerHeartbeatStatusDegraded), "management endpoint preflight failed: "+preflight.Code
			}
			err = reportRunnerHeartbeatWithEndpointPreflight(ctx, cfg, client, initialStatus, initialError, preflight)
		}
	}
	if err != nil {
		if isRunnerFixtureIdentityReissuedError(err) {
			prepareRunnerFixtureRecovery(cfg, logger)
			return
		}
		if isRunnerStaleBootstrapIdentityError(err) {
			markRunnerStaleBootstrapIdentity(runtimeState, health, logger, err)
			<-ctx.Done()
			return
		}
		health.set(false)
		logger.Error("initial runner heartbeat failed", "error", err)
		os.Exit(1)
	}
	health.set(preflight.Code == "passed")
	if len(cfg.EnvDiagnostics) > 0 {
		logger.Warn("deprecated legacy configuration variables are in use", "variables", cfg.EnvDiagnostics)
	}
	logger.Info("envplane runner started", "project_id", cfg.ProjectID, "cluster_id", cfg.ClusterID, "runner_id", cfg.RunnerID, "control_plane_url", cfg.ControlPlaneURL)
	go runRunnerCommands(ctx, cfg, client, runtimeState, health, logger)
	runRunnerHeartbeat(ctx, cfg, client, runtimeState, health, logger, stop)
}

type runnerHealth struct {
	online   atomic.Bool
	degraded atomic.Bool
	stale    atomic.Bool
}

func (h *runnerHealth) set(online bool) {
	h.online.Store(online)
}

func (h *runnerHealth) setDegraded() {
	h.degraded.Store(true)
	h.online.Store(false)
}

func (h *runnerHealth) setStaleBootstrapIdentity() {
	h.stale.Store(true)
	h.degraded.Store(true)
	h.online.Store(false)
}

func serveRunnerHealth(ctx context.Context, addr string, health *runnerHealth, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		// A stale bootstrap identity needs operator recovery, not a CrashLoop.
		// Keep the process live so its readiness/status is observable until a
		// rotated Secret is applied and the Deployment is restarted.
		writeRunnerJSON(w, http.StatusOK, map[string]any{"status": "alive"})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if health.stale.Load() {
			writeRunnerJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":   "stale_bootstrap_identity",
				"reason":   "The bootstrap session or runner credentials are no longer valid.",
				"recovery": "Rotate runner bootstrap credentials, apply the regenerated Secret, then restart or helm upgrade the existing runner release.",
			})
			return
		}
		if health.degraded.Load() {
			writeRunnerJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "degraded"})
			return
		}
		if !health.online.Load() {
			writeRunnerJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
			return
		}
		writeRunnerJSON(w, http.StatusOK, map[string]any{"status": "online"})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		status := 0
		if health.online.Load() {
			status = 1
		}
		_, _ = fmt.Fprintf(w, "# HELP envpilot_runner_up Whether the runner is online.\n# TYPE envpilot_runner_up gauge\nenvpilot_runner_up %d\n", status)
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("runner health server stopped", "error", err)
	}
}

// runnerRuntimeState is shared by the heartbeat and command-poll loops. In
// particular, a runner must not overwrite a detected command API incompatibility
// with a later "online" heartbeat.
type runnerRuntimeState struct {
	status atomic.Value
	error  atomic.Value
	stale  atomic.Bool
}

func newRunnerRuntimeState() *runnerRuntimeState {
	state := &runnerRuntimeState{}
	state.status.Store(string(domain.RunnerHeartbeatStatusOnline))
	state.error.Store("")
	return state
}

func (s *runnerRuntimeState) heartbeat() (string, string) {
	return s.status.Load().(string), s.error.Load().(string)
}

func (s *runnerRuntimeState) setDegraded(reason string) {
	s.status.Store(string(domain.RunnerHeartbeatStatusDegraded))
	s.error.Store(strings.TrimSpace(reason))
}

func (s *runnerRuntimeState) setStaleBootstrapIdentity(reason string) {
	s.setDegraded(reason)
	s.stale.Store(true)
}

func markRunnerStaleBootstrapIdentity(state *runnerRuntimeState, health *runnerHealth, logger *slog.Logger, cause error) {
	reason := "Runner bootstrap identity is stale: rotate bootstrap credentials, apply the regenerated Secret, then restart or helm upgrade this release."
	state.setStaleBootstrapIdentity(reason)
	health.setStaleBootstrapIdentity()
	logger.Error("runner bootstrap identity is stale; waiting for rotated credentials", "error", cause)
}

func ensureRunnerRuntimeAuth(ctx context.Context, cfg runnerConfig, client *http.Client, logger *slog.Logger) (runnerConfig, bool, error) {
	if strings.TrimSpace(cfg.RunnerAuthToken) != "" {
		cfg.RegistrationToken = ""
		logger.Info("runner using persisted auth token", "project_id", cfg.ProjectID, "runner_id", cfg.RunnerID)
		return cfg, false, nil
	}
	payload := domain.RunnerRegistrationRequest{
		ProjectID:         cfg.ProjectID,
		ClusterID:         cfg.ClusterID,
		RunnerID:          cfg.RunnerID,
		DeploymentMode:    cfg.DeploymentMode,
		RunnerNamespace:   cfg.RunnerNamespace,
		RegistrationToken: cfg.RegistrationToken,
		RunnerVersion:     cfg.RunnerVersion,
		ObservedAt:        time.Now().UTC(),
	}
	var response domain.RunnerRegistrationResponse
	if err := runnerPostJSON(ctx, client, cfg.ControlPlaneURL+"/api/v1/runners/register", "", payload, &response); err != nil {
		return cfg, false, err
	}
	token := strings.TrimSpace(response.RunnerAuthToken)
	if token == "" {
		return cfg, false, fmt.Errorf("runner registration response did not include runnerAuthToken")
	}
	if err := persistRuntimeToken(cfg.RunnerAuthTokenFile, token); err != nil {
		return cfg, false, fmt.Errorf("persist runner auth token: %w", err)
	}
	if err := persistRegistrationTokenFingerprint(cfg.RunnerAuthTokenFile, cfg.RegistrationToken); err != nil {
		return cfg, false, fmt.Errorf("persist runner registration token fingerprint: %w", err)
	}
	cfg.RunnerAuthToken = token
	cfg.RegistrationToken = ""
	return cfg, true, nil
}

func fetchRunnerProjectConfig(ctx context.Context, cfg runnerConfig, client *http.Client, logger *slog.Logger) error {
	if strings.TrimSpace(cfg.ProjectConfigURL) == "" || strings.TrimSpace(cfg.ProjectConfigToken) == "" {
		return nil
	}
	payload := map[string]string{
		"clusterId":       cfg.ClusterID,
		"runnerId":        cfg.RunnerID,
		"runnerNamespace": cfg.RunnerNamespace,
		"deploymentMode":  cfg.DeploymentMode,
	}
	var response map[string]any
	if err := runnerPostJSON(ctx, client, cfg.ProjectConfigURL, cfg.ProjectConfigToken, payload, &response); err != nil {
		return err
	}
	logger.Info("runner project config fetched", "project_id", cfg.ProjectID, "runner_id", cfg.RunnerID)
	return nil
}

func runRunnerHeartbeat(ctx context.Context, cfg runnerConfig, client *http.Client, state *runnerRuntimeState, health *runnerHealth, logger *slog.Logger, stop context.CancelFunc) {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if state.stale.Load() {
				<-ctx.Done()
				return
			}
			status, statusError := state.heartbeat()
			preflight := probeRunnerManagementEndpoint(ctx, cfg, client)
			if preflight.Code != "passed" {
				status, statusError = string(domain.RunnerHeartbeatStatusDegraded), "management endpoint preflight failed: "+preflight.Code
			}
			if err := reportRunnerHeartbeatWithEndpointPreflight(ctx, cfg, client, status, statusError, preflight); err != nil {
				if isRunnerFixtureIdentityReissuedError(err) {
					prepareRunnerFixtureRecovery(cfg, logger)
					stop()
					return
				}
				if isRunnerStaleBootstrapIdentityError(err) {
					markRunnerStaleBootstrapIdentity(state, health, logger, err)
					<-ctx.Done()
					return
				}
				health.set(false)
				logger.Error("runner heartbeat failed", "project_id", cfg.ProjectID, "runner_id", cfg.RunnerID, "error", err)
				continue
			}
			if status == string(domain.RunnerHeartbeatStatusDegraded) {
				health.setDegraded()
			} else {
				health.set(true)
			}
		}
	}
}

// prepareRunnerFixtureRecovery is used only after the server explicitly
// reports fixture_identity_reissued. It clears the disposable persisted auth
// credential; the Deployment restart policy then starts the same release and
// registers again from its mounted bootstrap Secret. It never touches or logs
// the raw bootstrap credential.
func prepareRunnerFixtureRecovery(cfg runnerConfig, logger *slog.Logger) {
	if err := clearRuntimeToken(cfg.RunnerAuthTokenFile); err != nil {
		logger.Error("clear stale runner auth token", "error", err)
		return
	}
	logger.Warn("runner fixture identity reissued; restarting automatically for registration", "project_id", cfg.ProjectID, "runner_id", cfg.RunnerID)
}

func runRunnerCommands(ctx context.Context, cfg runnerConfig, client *http.Client, state *runnerRuntimeState, health *runnerHealth, logger *slog.Logger) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if state.stale.Load() {
				return
			}
			keepPolling, found := pollRunnerCommandsOnceWithFound(ctx, cfg, client, state, health, logger)
			if !keepPolling {
				return
			}
			if found {
				backoff = time.Second
			} else if backoff < 15*time.Second {
				backoff *= 2
				if backoff > 15*time.Second {
					backoff = 15 * time.Second
				}
			}
		}
	}
}

// pollRunnerCommandsOnce returns false only for a deterministic API
// incompatibility. Transient transport/auth/response failures are kept visible
// in logs and retried on the next interval.
func pollRunnerCommandsOnce(ctx context.Context, cfg runnerConfig, client *http.Client, state *runnerRuntimeState, health *runnerHealth, logger *slog.Logger) bool {
	keepPolling, _ := pollRunnerCommandsOnceWithFound(ctx, cfg, client, state, health, logger)
	return keepPolling
}

func pollRunnerCommandsOnceWithFound(ctx context.Context, cfg runnerConfig, client *http.Client, state *runnerRuntimeState, health *runnerHealth, logger *slog.Logger) (bool, bool) {
	command, found, err := nextRunnerCommand(ctx, cfg, client)
	if err != nil {
		if isRunnerStaleBootstrapIdentityError(err) {
			markRunnerStaleBootstrapIdentity(state, health, logger, err)
			return false, false
		}
		if isRunnerCommandAPIIncompatible(err) {
			reason := "Runner command API is unavailable or incompatible: " + err.Error()
			state.setDegraded(reason)
			health.setDegraded()
			if heartbeatErr := reportRunnerHeartbeat(ctx, cfg, client, string(domain.RunnerHeartbeatStatusDegraded), reason); heartbeatErr != nil {
				logger.Error("runner command API incompatible and degraded heartbeat failed", "error", heartbeatErr)
			}
			logger.Error("runner command API incompatible; command polling disabled", "error", err)
			return false, false
		}
		if isRunnerCommandAuthenticationError(err) {
			logger.Error("runner command poll authentication failed", "error", err)
		} else if isRunnerCommandTransportError(err) {
			logger.Warn("runner command poll transport failed", "error", err)
		} else {
			logger.Error("runner command poll response handling failed", "error", err)
		}
		return true, false
	}
	if !found {
		return true, false
	}
	commandCtx, stopLease := context.WithCancel(ctx)
	defer stopLease()
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(90 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-commandCtx.Done():
				return
			case <-ticker.C:
				if err := renewRunnerCommandLease(commandCtx, cfg, client, command); err != nil {
					logger.Warn("runner command lease renewal failed", "command_id", command.ID, "error", err)
				}
			}
		}
	}()
	result := executeRunnerCommandForConfig(commandCtx, cfg, command)
	stopLease()
	<-leaseDone
	result.ProjectID = cfg.ProjectID
	result.ClusterID = cfg.ClusterID
	result.RunnerID = cfg.RunnerID
	result.RunnerAuthToken = cfg.RunnerAuthToken
	result.AttemptID = command.AttemptID
	// Echo the safe target and credential-epoch binding. The control plane
	// rejects a completion if reconciliation or Runner identity changed while
	// Helm was executing, instead of applying an old result to a new target.
	result.RemoteClusterGeneration = command.RemoteClusterGeneration
	result.RunnerIdentityIssuedAt = command.RunnerIdentityIssuedAt
	// Result delivery must survive cancellation of the command execution
	// context during process shutdown.
	reportCtx, reportCancel := context.WithTimeout(context.Background(), cfg.ReportTimeout)
	defer reportCancel()
	if err := reportRunnerCommandResult(reportCtx, cfg, client, command.ID, result); err != nil {
		if isRunnerStaleBootstrapIdentityError(err) {
			markRunnerStaleBootstrapIdentity(state, health, logger, err)
			return false, true
		}
		logger.Error("runner command result callback failed", "command_id", command.ID, "error", err)
	}
	return true, true
}

func renewRunnerCommandLease(ctx context.Context, cfg runnerConfig, client *http.Client, command domain.RunnerCommand) error {
	endpoint := cfg.ControlPlaneURL + "/api/v1/runners/commands/" + url.PathEscape(command.ID) + "/lease?projectId=" + url.QueryEscape(cfg.ProjectID) + "&clusterId=" + url.QueryEscape(cfg.ClusterID) + "&runnerId=" + url.QueryEscape(cfg.RunnerID) + "&attemptId=" + url.QueryEscape(command.AttemptID)
	return runnerPostJSONWithHeaders(ctx, client, endpoint, cfg.RunnerAuthToken, map[string]any{}, nil, http.Header{runnerCommandAPIVersionHeader: []string{runnerCommandAPIVersion}})
}

type runnerCommandEndpointMissingError struct{ detail string }

func (e runnerCommandEndpointMissingError) Error() string {
	return "command polling endpoint is missing: " + e.detail
}

type runnerCommandCompatibilityError struct{ detail string }

func (e runnerCommandCompatibilityError) Error() string {
	return "command polling API compatibility failure: " + e.detail
}

// runnerCommandVersionProbeError is deliberately retryable. During a rolling
// control-plane upgrade a new Runner can observe a no-content queue response
// from the previous control-plane revision before the new revision is ready.
// There is no command to execute in that response, so retrying cannot weaken
// the version gate. A response which contains a command still requires the
// exact version header and remains a deterministic incompatibility.
type runnerCommandVersionProbeError struct{ detail string }

func (e runnerCommandVersionProbeError) Error() string {
	return "command polling API version probe failed: " + e.detail
}

type runnerCommandAuthenticationError struct{ detail string }

func (e runnerCommandAuthenticationError) Error() string {
	return "command polling authentication failure: " + e.detail
}

type runnerCommandTransportError struct{ cause error }

func (e runnerCommandTransportError) Error() string {
	return "command polling transport failure: " + e.cause.Error()
}
func (e runnerCommandTransportError) Unwrap() error { return e.cause }

func isRunnerCommandAPIIncompatible(err error) bool {
	var missing runnerCommandEndpointMissingError
	var incompatible runnerCommandCompatibilityError
	return errors.As(err, &missing) || errors.As(err, &incompatible)
}

func isRunnerCommandAuthenticationError(err error) bool {
	var auth runnerCommandAuthenticationError
	return errors.As(err, &auth)
}

type runnerAPIError struct {
	status   int
	code     string
	detail   string
	recovery string
}

func (e runnerAPIError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("runner API request failed: status=%d code=%s detail=%s", e.status, e.code, e.detail)
	}
	return fmt.Sprintf("runner API request failed: status=%d detail=%s", e.status, e.detail)
}

func isRunnerStaleBootstrapIdentityError(err error) bool {
	var apiError runnerAPIError
	if errors.As(err, &apiError) {
		return apiError.code == "bootstrap_session_not_found" || apiError.code == "stale_runner_bootstrap_identity" || apiError.code == "runner_bootstrap_credential_expired"
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "bootstrap session not found") || strings.Contains(message, "runner bootstrap credentials have expired")
}

func isRunnerFixtureIdentityReissuedError(err error) bool {
	var apiError runnerAPIError
	if errors.As(err, &apiError) && apiError.code == "fixture_identity_reissued" {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "fixture_identity_reissued")
}

func isRunnerAuthTokenNotIssuedError(err error) bool {
	var apiError runnerAPIError
	if errors.As(err, &apiError) {
		return apiError.status == http.StatusUnauthorized && strings.Contains(strings.ToLower(apiError.detail), "token is not issued")
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "401") && strings.Contains(message, "token is not issued")
}

func isRunnerCommandTransportError(err error) bool {
	var transport runnerCommandTransportError
	return errors.As(err, &transport)
}

func nextRunnerCommand(ctx context.Context, cfg runnerConfig, client *http.Client) (domain.RunnerCommand, bool, error) {
	endpoint := cfg.ControlPlaneURL + "/api/v1/runners/commands/next?projectId=" + url.QueryEscape(cfg.ProjectID) + "&clusterId=" + url.QueryEscape(cfg.ClusterID) + "&runnerId=" + url.QueryEscape(cfg.RunnerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return domain.RunnerCommand{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.RunnerAuthToken)
	req.Header.Set(runnerCommandAPIVersionHeader, runnerCommandAPIVersion)
	resp, err := client.Do(req)
	if err != nil {
		return domain.RunnerCommand{}, false, runnerCommandTransportError{cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		if err := validateRunnerCommandAPIResponse(resp); err != nil {
			return domain.RunnerCommand{}, false, runnerCommandVersionProbeError{detail: err.Error()}
		}
		return domain.RunnerCommand{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := fmt.Sprintf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
		if code, recovery, ok := runnerBootstrapIdentityErrorBody(body); ok {
			return domain.RunnerCommand{}, false, runnerAPIError{status: resp.StatusCode, code: code, detail: detail, recovery: recovery}
		}
		if resp.StatusCode == http.StatusNotFound {
			return domain.RunnerCommand{}, false, runnerCommandEndpointMissingError{detail: detail}
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return domain.RunnerCommand{}, false, runnerCommandAuthenticationError{detail: detail}
		}
		if resp.StatusCode == http.StatusUpgradeRequired {
			return domain.RunnerCommand{}, false, runnerCommandCompatibilityError{detail: detail}
		}
		return domain.RunnerCommand{}, false, fmt.Errorf("runner command poll response status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := validateRunnerCommandAPIResponse(resp); err != nil {
		return domain.RunnerCommand{}, false, err
	}
	var command domain.RunnerCommand
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&command); err != nil {
		return domain.RunnerCommand{}, false, err
	}
	return command, true, nil
}

func validateRunnerCommandAPIResponse(resp *http.Response) error {
	if version := strings.TrimSpace(resp.Header.Get(runnerCommandAPIVersionHeader)); version != runnerCommandAPIVersion {
		return runnerCommandCompatibilityError{detail: fmt.Sprintf("expected response header %s=%q, got %q", runnerCommandAPIVersionHeader, runnerCommandAPIVersion, version)}
	}
	return nil
}

func executeRunnerCommandForConfig(ctx context.Context, cfg runnerConfig, command domain.RunnerCommand) domain.RunnerCommandResult {
	return executeRunnerCommandWithNamespaceGuard(ctx, command, orchestrator.NewHelmDirectBackend(nil), cfg.canRunHelmInNamespace)
}

type runnerCommandBackend interface {
	orchestrator.DeploymentBackend
	DeploymentTarget(domain.Environment, domain.ProjectConfig) (string, string, error)
}

func executeRunnerCommandWithBackend(ctx context.Context, command domain.RunnerCommand, backend runnerCommandBackend) domain.RunnerCommandResult {
	return executeRunnerCommandWithNamespaceGuard(ctx, command, backend, nil)
}

// executeRunnerCommandWithNamespaceGuard refuses an operation before invoking
// Helm when the chart-managed ServiceAccount was not granted a Role in the
// exact resolved release namespace. This prevents the late, misleading Helm
// failure "cannot list secrets" and never delegates to a control-plane
// kubeconfig.
func executeRunnerCommandWithNamespaceGuard(ctx context.Context, command domain.RunnerCommand, backend runnerCommandBackend, namespaceAllowed func(string) bool) domain.RunnerCommandResult {
	result := domain.RunnerCommandResult{CommandID: command.ID, Status: "failed", Namespace: command.Environment.Namespace, ReleaseName: command.Environment.ID}
	projectConfig := projectConfigForRunnerCommand(command)
	var err error
	switch command.Operation {
	case "validate_helm_chart":
		result.Namespace = ""
		result.ReleaseName = ""
		if err := validateRunnerHelmChart(ctx, command.ChartRef, command.ChartVersion); err != nil {
			result.ErrorCode, result.Error = classifyHelmChartPreflightError(err)
			return result
		}
		result.Status = "succeeded"
		return result
	case "create", "recreate":
		result.ReleaseName, result.Namespace, err = backend.DeploymentTarget(command.Environment, projectConfig)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if namespaceAllowed != nil && !namespaceAllowed(result.Namespace) {
			result.ErrorCode = "runner_namespace_access_denied"
			result.Error = "target Runner is not authorized for Helm release Secrets in namespace " + result.Namespace
			return result
		}
		err = backend.Apply(ctx, command.Environment, projectConfig)
	case "delete", "force_cleanup":
		result.ReleaseName, result.Namespace, err = backend.DeploymentTarget(command.Environment, projectConfig)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if namespaceAllowed != nil && !namespaceAllowed(result.Namespace) {
			result.ErrorCode = "runner_namespace_access_denied"
			result.Error = "target Runner is not authorized for Helm release Secrets in namespace " + result.Namespace
			return result
		}
		// HelmDirectBackend.Delete is deliberately label-owned: it ignores a
		// shared namespace and deletes a dedicated namespace only after the
		// envpilot.io managed/project/environment labels match the command.
		// The Runner therefore remains the sole Kubernetes actor even for an
		// audited force-clean recovery command.
		err = backend.Delete(ctx, command.Environment, projectConfig)
	case "status":
		result.ReleaseName, result.Namespace, err = backend.DeploymentTarget(command.Environment, projectConfig)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if namespaceAllowed != nil && !namespaceAllowed(result.Namespace) {
			result.ErrorCode = "runner_namespace_access_denied"
			result.Error = "target Runner is not authorized for Helm release Secrets in namespace " + result.Namespace
			return result
		}
		var status domain.EnvironmentStatus
		status, err = backend.Status(ctx, command.Environment, projectConfig)
		if err == nil {
			result.EnvironmentStatus = string(status)
		}
	default:
		result.Error = "unsupported runner command operation"
		return result
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Status = "succeeded"
	return result
}

// projectConfigForRunnerCommand keeps command fields and the compiled project
// configuration coherent while accepting commands from a control plane that
// sends the Helm Direct contract in either representation. It only mutates a
// private copy, never the decoded command object.
func projectConfigForRunnerCommand(command domain.RunnerCommand) domain.ProjectConfig {
	projectConfig := command.ProjectConfig
	if strings.TrimSpace(command.ChartRef) == "" && strings.TrimSpace(command.ChartVersion) == "" {
		return projectConfig
	}
	config := make(map[string]any, len(projectConfig.Config)+1)
	for key, value := range projectConfig.Config {
		config[key] = value
	}
	deployment, _ := config["deployment"].(map[string]any)
	deploymentCopy := make(map[string]any, len(deployment)+2)
	for key, value := range deployment {
		deploymentCopy[key] = value
	}
	deploymentCopy["backend"] = "helm_direct"
	helmDirect, _ := deploymentCopy["helmDirect"].(map[string]any)
	helmDirectCopy := make(map[string]any, len(helmDirect)+2)
	for key, value := range helmDirect {
		helmDirectCopy[key] = value
	}
	if value := strings.TrimSpace(command.ChartRef); value != "" {
		helmDirectCopy["chartRef"] = value
	}
	if value := strings.TrimSpace(command.ChartVersion); value != "" {
		helmDirectCopy["chartVersion"] = value
	}
	deploymentCopy["helmDirect"] = helmDirectCopy
	config["deployment"] = deploymentCopy
	projectConfig.Config = config
	return projectConfig
}

func validateRunnerHelmChart(ctx context.Context, chartRef, chartVersion string) error {
	return validateRunnerHelmChartWithCommand(ctx, chartRef, chartVersion, func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "helm", args...).CombinedOutput()
	})
}

func validateRunnerHelmChartWithCommand(ctx context.Context, chartRef, chartVersion string, run func(context.Context, ...string) ([]byte, error)) error {
	chartRef = strings.TrimSpace(chartRef)
	if !validRunnerHelmChartRef(chartRef) {
		return fmt.Errorf("invalid chart reference")
	}
	args := []string{"show", "chart", chartRef}
	if strings.TrimSpace(chartVersion) != "" && !isDirectRunnerHelmChartArchive(chartRef) {
		args = append(args, "--version", strings.TrimSpace(chartVersion))
	}
	output, err := run(ctx, args...)
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", strings.TrimSpace(string(output)))
}

func isDirectRunnerHelmChartArchive(chartRef string) bool {
	parsed, err := url.Parse(strings.TrimSpace(chartRef))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".tgz")
}

func validRunnerHelmChartRef(chartRef string) bool {
	chartRef = strings.TrimSpace(chartRef)
	if chartRef == "" || strings.HasPrefix(chartRef, "-") || strings.HasPrefix(chartRef, "deploy/helm/") || strings.HasPrefix(chartRef, "./") || strings.HasPrefix(chartRef, "../") || strings.IndexFunc(chartRef, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	if strings.HasPrefix(chartRef, "oci://") || strings.HasPrefix(chartRef, "https://") || strings.HasPrefix(chartRef, "http://") {
		return true
	}
	parts := strings.Split(chartRef, "/")
	return len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != ""
}

func classifyHelmChartPreflightError(err error) (string, string) {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "invalid chart reference"):
		return "helm_chart_reference_invalid", "Helm chart reference must be OCI, HTTP(S), or a configured repo/chart reference."
	case strings.Contains(message, "repo") && strings.Contains(message, "not found"):
		return "helm_repo_missing", "Helm repository is not configured in the target runner."
	case strings.Contains(message, "unauthorized"), strings.Contains(message, "authentication"), strings.Contains(message, "forbidden"), strings.Contains(message, "denied"), strings.Contains(message, "401"):
		return "helm_chart_auth_failed", "Target runner could not authenticate to the Helm chart repository."
	case strings.Contains(message, "version") && strings.Contains(message, "not found"):
		return "helm_chart_version_missing", "Requested Helm chart version was not found in the configured repository."
	case strings.Contains(message, "chart") && strings.Contains(message, "not found"), strings.Contains(message, "not found"):
		return "helm_chart_missing", "Helm chart was not found in the configured repository."
	default:
		return "helm_chart_preflight_failed", "Target runner could not resolve the Helm chart."
	}
}

func reportRunnerHeartbeat(ctx context.Context, cfg runnerConfig, client *http.Client, status string, errorMessage string) error {
	return reportRunnerHeartbeatWithEndpointPreflight(ctx, cfg, client, status, errorMessage, nil)
}

func reportRunnerHeartbeatWithEndpointPreflight(ctx context.Context, cfg runnerConfig, client *http.Client, status string, errorMessage string, preflight *domain.ManagementEndpointPreflight) error {
	payload := domain.RunnerHeartbeatRequest{
		ProjectID:              cfg.ProjectID,
		ClusterID:              cfg.ClusterID,
		RunnerID:               cfg.RunnerID,
		DeploymentMode:         cfg.DeploymentMode,
		RunnerNamespace:        cfg.RunnerNamespace,
		HelmTargetNamespaces:   cfg.helmTargetNamespaces(),
		HelmNamespaceRBACReady: len(cfg.helmTargetNamespaces()) > 0,
		RunnerAuthToken:        cfg.RunnerAuthToken,
		Status:                 status,
		Error:                  errorMessage,
		EndpointPreflight:      preflight,
		ObservedAt:             time.Now().UTC(),
	}
	return runnerPostJSON(ctx, client, cfg.ControlPlaneURL+"/api/v1/runners/heartbeat", "", payload, nil)
}

func probeRunnerManagementEndpoint(ctx context.Context, cfg runnerConfig, client *http.Client) *domain.ManagementEndpointPreflight {
	checked := time.Now().UTC()
	report := &domain.ManagementEndpointPreflight{Generation: cfg.RemoteGeneration, Code: "dns_failed", CheckedAt: &checked}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.ControlPlaneURL, "/")+"/api/v1/health", nil)
	if err != nil {
		return report
	}
	resp, err := client.Do(req)
	if err != nil {
		report.Code = classifyRunnerEndpointProbeError(err)
		return report
	}
	defer func() { _ = resp.Body.Close() }()
	report.DNSResolved, report.TCPConnected = true, true
	if strings.HasPrefix(strings.ToLower(cfg.ControlPlaneURL), "https://") {
		report.TLSVerified = true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		report.Code = "endpoint_unhealthy"
		return report
	}
	report.HealthReachable = true
	query := url.Values{}
	query.Set("projectId", cfg.ProjectID)
	query.Set("clusterId", cfg.ClusterID)
	query.Set("runnerId", cfg.RunnerID)
	accessReq, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.ControlPlaneURL, "/")+"/api/v1/runners/runtime-access?"+query.Encode(), nil)
	if err != nil {
		return report
	}
	accessReq.Header.Set("Authorization", "Bearer "+cfg.RunnerAuthToken)
	accessResp, err := client.Do(accessReq)
	if err != nil {
		report.Code = "runtime_auth_failed"
		return report
	}
	defer func() { _ = accessResp.Body.Close() }()
	if accessResp.StatusCode != http.StatusNoContent {
		report.Code = "runtime_auth_failed"
		return report
	}
	report.RuntimeAccess = true
	report.Code = "passed"
	return report
}

func classifyRunnerEndpointProbeError(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_failed"
	}
	var unknownCA x509.UnknownAuthorityError
	if errors.As(err, &unknownCA) {
		return "tls_ca_failed"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "certificate") && (strings.Contains(message, "not valid for") || strings.Contains(message, "server name")) {
		return "tls_server_name_mismatch"
	}
	if strings.Contains(message, "certificate") || strings.Contains(message, "tls") {
		return "tls_ca_failed"
	}
	return "tcp_failed"
}

func reportRunnerCommandResult(ctx context.Context, cfg runnerConfig, client *http.Client, commandID string, result domain.RunnerCommandResult) error {
	sdk := envplanesdk.Client{
		BaseURL:       cfg.ControlPlaneURL,
		HTTPClient:    client,
		TokenProvider: func(context.Context) (string, error) { return cfg.RunnerAuthToken, nil },
		Headers:       http.Header{runnerCommandAPIVersionHeader: []string{runnerCommandAPIVersion}},
	}
	return sdk.DoJSON(ctx, http.MethodPost, "/api/v1/runners/commands/"+url.PathEscape(commandID)+"/result", result, nil, "")
}

func runnerPostJSON(ctx context.Context, client *http.Client, endpoint string, bearerToken string, payload any, target any) error {
	return runnerPostJSONWithHeaders(ctx, client, endpoint, bearerToken, payload, target, nil)
}

func runnerPostJSONWithHeaders(ctx context.Context, client *http.Client, endpoint string, bearerToken string, payload any, target any, headers http.Header) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(bearerToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var response struct {
			Code     string `json:"code"`
			Error    string `json:"error"`
			Recovery string `json:"recovery"`
		}
		_ = json.Unmarshal(responseBody, &response)
		detail := strings.TrimSpace(response.Error)
		if detail == "" {
			detail = strings.TrimSpace(string(responseBody))
		}
		if response.Code == "bootstrap_session_not_found" || response.Code == "stale_runner_bootstrap_identity" || response.Code == "runner_bootstrap_credential_expired" || strings.Contains(strings.ToLower(detail), "bootstrap session not found") || strings.Contains(strings.ToLower(detail), "runner bootstrap credentials have expired") {
			code := strings.TrimSpace(response.Code)
			if code == "" {
				code = "bootstrap_session_not_found"
			}
			return runnerAPIError{status: resp.StatusCode, code: code, detail: detail, recovery: response.Recovery}
		}
		return runnerAPIError{status: resp.StatusCode, code: strings.TrimSpace(response.Code), detail: detail, recovery: strings.TrimSpace(response.Recovery)}
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
}

func runnerBootstrapIdentityErrorBody(body []byte) (string, string, bool) {
	var response struct {
		Code     string `json:"code"`
		Recovery string `json:"recovery"`
	}
	if json.Unmarshal(body, &response) == nil {
		code := strings.TrimSpace(response.Code)
		if code == "bootstrap_session_not_found" || code == "stale_runner_bootstrap_identity" || code == "runner_bootstrap_credential_expired" {
			return code, strings.TrimSpace(response.Recovery), true
		}
	}
	if strings.Contains(strings.ToLower(string(body)), "bootstrap session not found") {
		return "bootstrap_session_not_found", "", true
	}
	return "", "", false
}

func writeRunnerJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func readRuntimeTokenFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func persistRuntimeToken(path string, token string) error {
	path = strings.TrimSpace(path)
	token = strings.TrimSpace(token)
	if path == "" || token == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

func clearRuntimeToken(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}

func registrationTokenFingerprintPath(authTokenPath string) string {
	authTokenPath = strings.TrimSpace(authTokenPath)
	if authTokenPath == "" {
		return ""
	}
	return authTokenPath + ".registration-token-sha256"
}

func registrationTokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:])
}

func registrationTokenChanged(authTokenPath, registrationToken string) bool {
	want := registrationTokenFingerprint(registrationToken)
	path := registrationTokenFingerprintPath(authTokenPath)
	if want == "" || path == "" {
		return false
	}
	have := strings.TrimSpace(readRuntimeTokenFile(path))
	// Existing auth PVCs predate the fingerprint. Adopt the current bootstrap
	// token on their first upgraded restart so they keep working; a later token
	// rotation will then be detected deterministically.
	if have == "" {
		_ = persistRegistrationTokenFingerprint(authTokenPath, registrationToken)
		return false
	}
	return have != want
}

func persistRegistrationTokenFingerprint(authTokenPath, registrationToken string) error {
	path := registrationTokenFingerprintPath(authTokenPath)
	fingerprint := registrationTokenFingerprint(registrationToken)
	if path == "" || fingerprint == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fingerprint+"\n"), 0o600)
}

func hostnameFallback(fallback string) string {
	value, err := os.Hostname()
	if err != nil || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

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

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
