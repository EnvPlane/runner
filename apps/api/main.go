package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	"envpilot/agent"
	"envpilot/internal/app"
	"envpilot/internal/catalog"
	"envpilot/internal/config"
	"envpilot/internal/domain"
	"envpilot/internal/gitops"
	"envpilot/internal/jobs"
	"envpilot/internal/notify"
	"envpilot/internal/orchestrator"
	"envpilot/internal/postgres"
	"envpilot/internal/redisqueue"
	scmcomment "envpilot/internal/scm/comment"
	"envpilot/internal/secrets"
	"envpilot/internal/server"
	"envpilot/internal/store"
	"envpilot/internal/ttl"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "agent":
			runAgent(logger)
			return
		case "runner":
			runRunner(logger)
			return
		case "agent-install-check":
			runAgentInstallCheck(logger)
			return
		case "ttl-cleanup":
			runTTLCleanup(logger)
			return
		case "project":
			runProjectCLI(os.Args[2:], logger)
			return
		case "env":
			runEnvCLI(os.Args[2:], logger)
			return
		}
	}
	runServer(logger)
}

func runEnvCLI(args []string, logger *slog.Logger) {
	if len(args) == 0 {
		logger.Error("unknown env command", "usage", "envpilot env list|delete|logs")
		os.Exit(1)
	}
	if err := runEnvCommand(context.Background(), args, os.Stdout); err != nil {
		logger.Error("env command failed", "error", err)
		os.Exit(1)
	}
}

func runEnvCommand(ctx context.Context, args []string, output io.Writer) error {
	command := args[0]
	options := parseEnvCommandOptions(args[1:])
	if options.apiURL == "" {
		return fmt.Errorf("control plane url is required; pass --api or set ENVPILOT_CONTROL_PLANE_URL")
	}
	switch command {
	case "list":
		return envList(ctx, options, output)
	case "delete":
		if options.id == "" {
			return fmt.Errorf("environment id is required")
		}
		return envDelete(ctx, options, output)
	case "logs":
		if options.id == "" {
			return fmt.Errorf("environment id is required")
		}
		return envLogs(ctx, options, output)
	default:
		return fmt.Errorf("unknown env command %q", command)
	}
}

type envCommandOptions struct {
	apiURL string
	token  string
	id     string
}

func parseEnvCommandOptions(args []string) envCommandOptions {
	options := envCommandOptions{
		apiURL: strings.TrimSpace(os.Getenv("ENVPILOT_CONTROL_PLANE_URL")),
		token:  strings.TrimSpace(os.Getenv("ENVPILOT_API_TOKEN")),
	}
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			if i+1 >= len(args) {
				break
			}
			value := strings.TrimSpace(args[i+1])
			switch args[i] {
			case "--api":
				options.apiURL = value
			case "--token":
				options.token = value
			case "--id":
				options.id = value
			}
			i++
			continue
		}
		if options.id == "" {
			options.id = strings.TrimSpace(args[i])
		}
	}
	options.apiURL = strings.TrimRight(options.apiURL, "/")
	return options
}

func envList(ctx context.Context, options envCommandOptions, output io.Writer) error {
	var environments []domain.Environment
	if err := envAPIRequest(ctx, http.MethodGet, options.apiURL+"/api/v1/environments", options.token, nil, &environments); err != nil {
		return err
	}
	for _, env := range environments {
		_, _ = fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%s\n", env.ID, env.Status, env.URL, env.CostEstimateDay, env.UpdatedAt.Format(time.RFC3339))
	}
	return nil
}

func envDelete(ctx context.Context, options envCommandOptions, output io.Writer) error {
	var env domain.Environment
	endpoint := options.apiURL + "/api/v1/environments/" + url.PathEscape(options.id)
	if err := envAPIRequest(ctx, http.MethodDelete, endpoint, options.token, nil, &env); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(output, "%s\t%s\n", env.ID, env.Status)
	return nil
}

func envLogs(ctx context.Context, options envCommandOptions, output io.Writer) error {
	var payload struct {
		Events []domain.KubernetesEvent `json:"events"`
	}
	endpoint := options.apiURL + "/api/v1/environments/" + url.PathEscape(options.id) + "/events"
	if err := envAPIRequest(ctx, http.MethodGet, endpoint, options.token, nil, &payload); err != nil {
		return err
	}
	for _, event := range payload.Events {
		_, _ = fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", event.Type, event.Reason, event.InvolvedName, event.Message)
	}
	return nil
}

func envAPIRequest(ctx context.Context, method string, endpoint string, token string, body io.Reader, target any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s failed: status=%d body=%s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if target == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func runProjectCLI(args []string, logger *slog.Logger) {
	if len(args) == 0 || args[0] != "init" {
		logger.Error("unknown project command", "usage", "envpilot project init [--path envpilot-project.json] [--id default]")
		os.Exit(1)
	}
	path, project := projectInitConfig(args[1:])
	if err := writeProjectInitConfig(path, project); err != nil {
		logger.Error("project init failed", "error", err)
		os.Exit(1)
	}
	logger.Info("project config created", "path", path, "project_id", project.ID)
}

func projectInitConfig(args []string) (string, domain.Project) {
	path := "envpilot-project.json"
	project := domain.Project{
		ID:                 "default",
		Name:               "Default",
		ProductID:          "generic",
		AppRepositoryID:    "app",
		GitOpsRepositoryID: "gitops",
		GitRepo: domain.RepositoryRef{
			Provider:      "github",
			URL:           "https://github.com/example/app.git",
			DefaultBranch: "main",
		},
		GitOpsRepo: domain.RepositoryRef{
			Provider:      "github",
			URL:           "https://github.com/example/gitops.git",
			DefaultBranch: "main",
			Path:          ".envpilot/gitops",
		},
		BaseEnvConfig: domain.BaseEnvConfig{
			Namespace: "shared",
		},
	}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			break
		}
		value := strings.TrimSpace(args[i+1])
		switch args[i] {
		case "--path":
			if value != "" {
				path = value
			}
			i++
		case "--id":
			if value != "" {
				project.ID = value
			}
			i++
		case "--name":
			if value != "" {
				project.Name = value
			}
			i++
		case "--product":
			if value != "" {
				project.ProductID = value
			}
			i++
		case "--base-namespace":
			if value != "" {
				project.BaseEnvConfig.Namespace = value
			}
			i++
		}
	}
	return path, project
}

func writeProjectInitConfig(path string, project domain.Project) error {
	content, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	return os.WriteFile(path, content, 0644)
}

func runAgent(logger *slog.Logger) {
	cfg := agent.ConfigFromEnv()
	if err := cfg.Validate(); err != nil {
		logger.Error("invalid agent configuration", "error", err)
		os.Exit(1)
	}

	source, err := agent.NewKubernetesNamespaceSourceFromConfig(cfg)
	if err != nil {
		logger.Error("failed to initialise kubernetes namespace source", "error", err)
		os.Exit(1)
	}
	reporter := agent.NewHTTPStatusReporterForAgent(cfg.ControlPlaneURL, "", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)
	watcher := agent.NewNamespaceWatcher(source, reporter, cfg.ResyncInterval, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	capabilities := discoverAgentCapabilities(ctx, source, logger)
	cfg, err = ensureAgentRuntimeAuth(ctx, cfg, reporter, capabilities, logger)
	if err != nil {
		logger.Error("agent registration failed", "error", err)
		os.Exit(1)
	}
	go runAgentHeartbeat(ctx, cfg, reporter, source, logger)

	logger.Info("envpilot agent started", "cluster_id", cfg.ClusterID, "agent_id", cfg.AgentID, "control_plane_url", cfg.ControlPlaneURL, "namespace_selector", cfg.NamespaceSelector)
	if err := watcher.Run(ctx); err != nil {
		logger.Error("envpilot agent stopped", "error", err)
		os.Exit(1)
	}
}

func runAgentInstallCheck(logger *slog.Logger) {
	cfg := agent.ConfigFromEnv()
	if err := cfg.Validate(); err != nil {
		logger.Error("invalid agent install check configuration", "error", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source, err := agent.NewKubernetesNamespaceSourceFromConfig(cfg)
	if err != nil {
		logger.Error("failed to initialise kubernetes source", "error", err)
		os.Exit(1)
	}
	reporter := agent.NewHTTPStatusReporterForAgent(cfg.ControlPlaneURL, "", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)
	capabilities, err := runAgentInstallCheckFlow(ctx, cfg, source, reporter)
	if err != nil {
		logger.Error("agent install check failed", "error", err)
		os.Exit(1)
	}
	logger.Info("agent install check completed", "cluster_id", cfg.ClusterID, "agent_id", cfg.AgentID, "capabilities", capabilities.Capabilities)
}

func runAgentInstallCheckFlow(ctx context.Context, cfg agent.Config, source agent.CapabilitySource, reporter *agent.HTTPStatusReporter) (agent.ClusterCapabilities, error) {
	capabilities, err := discoverAgentCapabilitiesRequired(ctx, source)
	if err != nil {
		return agent.ClusterCapabilities{}, err
	}
	cfg, err = ensureAgentRuntimeAuth(ctx, cfg, reporter, capabilities, nil)
	if err != nil {
		return agent.ClusterCapabilities{}, err
	}
	if namespaceSource, ok := source.(*agent.KubernetesNamespaceSource); ok && len(cfg.Namespaces) > 0 {
		scanner := agent.NewResourceDiscoveryScanner(namespaceSource)
		scanResult, scanErr := scanner.Scan(ctx, cfg.Namespaces)
		if scanErr != nil {
			return agent.ClusterCapabilities{}, scanErr
		}
		if err := reporter.ReportResourceScan(ctx, cfg, scanResult); err != nil {
			return agent.ClusterCapabilities{}, err
		}
	}
	if err := reporter.ReportHeartbeat(ctx, cfg, capabilities, "online", nil); err != nil {
		return agent.ClusterCapabilities{}, err
	}
	return capabilities, nil
}

func ensureAgentRuntimeAuth(ctx context.Context, cfg agent.Config, reporter *agent.HTTPStatusReporter, capabilities agent.ClusterCapabilities, logger *slog.Logger) (agent.Config, error) {
	if strings.TrimSpace(cfg.AgentAuthToken) != "" {
		cfg.RegistrationToken = ""
		if logger != nil {
			logger.Info("agent using persisted auth token", "cluster_id", cfg.ClusterID, "agent_id", cfg.AgentID)
		}
		return cfg, nil
	}
	agentAuthToken, err := reporter.RegisterAgent(ctx, cfg, capabilities)
	if err != nil {
		return cfg, err
	}
	if strings.TrimSpace(agentAuthToken) != "" {
		if err := cfg.PersistAgentAuthToken(agentAuthToken); err != nil {
			return cfg, fmt.Errorf("persist agent auth token: %w", err)
		}
		cfg.AgentAuthToken = agentAuthToken
		cfg.RegistrationToken = ""
	}
	return cfg, nil
}

func runAgentHeartbeat(ctx context.Context, cfg agent.Config, reporter *agent.HTTPStatusReporter, source *agent.KubernetesNamespaceSource, logger *slog.Logger) {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()
	capabilitySource := agent.CapabilitySource(source)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			capabilities := discoverAgentCapabilities(ctx, capabilitySource, logger)
			if err := reporter.ReportHeartbeat(ctx, cfg, capabilities, "online", nil); err != nil {
				logger.Error("agent heartbeat failed", "cluster_id", cfg.ClusterID, "agent_id", cfg.AgentID, "error", err)
			}
			if err := runAgentResourceScanTick(ctx, cfg, reporter, source, logger); err != nil {
				logger.Error("agent resource scan dispatch failed", "cluster_id", cfg.ClusterID, "agent_id", cfg.AgentID, "error", err)
			}
		}
	}
}

func runAgentResourceScanTick(ctx context.Context, cfg agent.Config, reporter *agent.HTTPStatusReporter, source *agent.KubernetesNamespaceSource, logger *slog.Logger) error {
	if source == nil {
		return nil
	}
	if strings.TrimSpace(cfg.BootstrapProjectID) == "" {
		logger.Warn("agent resource scan skipped: bootstrap project id is required", "cluster_id", cfg.ClusterID, "agent_id", cfg.AgentID)
		return nil
	}
	if strings.TrimSpace(cfg.AgentAuthToken) == "" {
		logger.Warn("agent resource scan skipped: agent auth token is required", "project_id", cfg.BootstrapProjectID, "cluster_id", cfg.ClusterID, "agent_id", cfg.AgentID)
		return nil
	}
	task, err := reporter.FetchResourceScanTask(ctx, cfg)
	if err != nil {
		return err
	}
	if task == nil || len(task.Namespaces) == 0 {
		return nil
	}
	scanner := agent.NewResourceDiscoveryScanner(source)
	result, err := scanner.Scan(ctx, task.Namespaces)
	if err != nil {
		return err
	}
	if err := reporter.ReportResourceScan(ctx, cfg, result); err != nil {
		return err
	}
	logger.Info("agent resource scan reported", "project_id", task.ProjectID, "resource_count", len(result.Snapshots), "warning_count", len(result.PermissionWarnings))
	return nil
}

type runnerConfig struct {
	ControlPlaneURL     string
	ProjectID           string
	ClusterID           string
	RunnerID            string
	RunnerNamespace     string
	DeploymentMode      string
	RegistrationToken   string
	RunnerAuthToken     string
	RunnerAuthTokenFile string
	ProjectConfigURL    string
	ProjectConfigToken  string
	HeartbeatInterval   time.Duration
	ReportTimeout       time.Duration
	HealthAddr          string
	RunnerVersion       string
}

func runnerConfigFromEnv() runnerConfig {
	authTokenFile := getenv("ENVPILOT_RUNNER_AUTH_TOKEN_FILE", "")
	authToken := getenv("ENVPILOT_RUNNER_AUTH_TOKEN", "")
	if strings.TrimSpace(authToken) == "" {
		authToken = readRuntimeTokenFile(authTokenFile)
	}
	return runnerConfig{
		ControlPlaneURL:     strings.TrimRight(getenv("ENVPILOT_CONTROL_PLANE_URL", ""), "/"),
		ProjectID:           getenv("ENVPILOT_PROJECT_ID", ""),
		ClusterID:           getenv("ENVPILOT_CLUSTER_ID", "default"),
		RunnerID:            getenv("ENVPILOT_RUNNER_ID", hostnameFallback("envpilot-runner")),
		RunnerNamespace:     getenv("ENVPILOT_RUNNER_NAMESPACE", "envpilot-system"),
		DeploymentMode:      strings.ToLower(getenv("ENVPILOT_RUNNER_DEPLOYMENT_MODE", "helm")),
		RegistrationToken:   getenv("ENVPILOT_RUNNER_REGISTRATION_TOKEN", ""),
		RunnerAuthToken:     authToken,
		RunnerAuthTokenFile: authTokenFile,
		ProjectConfigURL:    getenv("ENVPILOT_PROJECT_CONFIG_URL", ""),
		ProjectConfigToken:  getenv("ENVPILOT_PROJECT_CONFIG_TOKEN", ""),
		HeartbeatInterval:   time.Duration(getenvInt("ENVPILOT_RUNNER_HEARTBEAT_INTERVAL_SECONDS", 30)) * time.Second,
		ReportTimeout:       time.Duration(getenvInt("ENVPILOT_RUNNER_REPORT_TIMEOUT_SECONDS", 10)) * time.Second,
		HealthAddr:          getenv("ENVPILOT_RUNNER_HEALTH_ADDR", ":8080"),
		RunnerVersion:       getenv("ENVPILOT_RUNNER_VERSION", "dev"),
	}
}

func (c runnerConfig) validate() error {
	if strings.TrimSpace(c.ControlPlaneURL) == "" {
		return fmt.Errorf("ENVPILOT_CONTROL_PLANE_URL is required")
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

func runRunner(logger *slog.Logger) {
	cfg := runnerConfigFromEnv()
	if err := cfg.validate(); err != nil {
		logger.Error("invalid runner configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	health := &runnerHealth{}
	go serveRunnerHealth(ctx, cfg.HealthAddr, health, logger)

	client := &http.Client{Timeout: cfg.ReportTimeout}
	var err error
	var registeredNow bool
	cfg, registeredNow, err = ensureRunnerRuntimeAuth(ctx, cfg, client, logger)
	if err != nil {
		health.set(false)
		logger.Error("runner registration failed", "error", err)
		os.Exit(1)
	}
	if registeredNow {
		if err := fetchRunnerProjectConfig(ctx, cfg, client, logger); err != nil {
			logger.Warn("runner project config fetch failed", "error", err)
		}
	}
	if err := reportRunnerHeartbeat(ctx, cfg, client, string(domain.RunnerHeartbeatStatusOnline), ""); err != nil {
		health.set(false)
		logger.Error("initial runner heartbeat failed", "error", err)
		os.Exit(1)
	}
	health.set(true)
	logger.Info("envpilot runner started", "project_id", cfg.ProjectID, "cluster_id", cfg.ClusterID, "runner_id", cfg.RunnerID, "control_plane_url", cfg.ControlPlaneURL)
	go runRunnerCommands(ctx, cfg, client, logger)
	runRunnerHeartbeat(ctx, cfg, client, health, logger)
}

type runnerHealth struct {
	online atomic.Bool
}

func (h *runnerHealth) set(online bool) {
	h.online.Store(online)
}

func serveRunnerHealth(ctx context.Context, addr string, health *runnerHealth, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if !health.online.Load() {
			writeRunnerJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
			return
		}
		writeRunnerJSON(w, http.StatusOK, map[string]any{"status": "online"})
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

func runRunnerHeartbeat(ctx context.Context, cfg runnerConfig, client *http.Client, health *runnerHealth, logger *slog.Logger) {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reportRunnerHeartbeat(ctx, cfg, client, string(domain.RunnerHeartbeatStatusOnline), ""); err != nil {
				health.set(false)
				logger.Error("runner heartbeat failed", "project_id", cfg.ProjectID, "runner_id", cfg.RunnerID, "error", err)
				continue
			}
			health.set(true)
		}
	}
}

func runRunnerCommands(ctx context.Context, cfg runnerConfig, client *http.Client, logger *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			command, found, err := nextRunnerCommand(ctx, cfg, client)
			if err != nil {
				logger.Warn("runner command poll failed", "error", err)
				continue
			}
			if !found {
				continue
			}
			result := executeRunnerCommand(ctx, command)
			result.ProjectID = cfg.ProjectID
			result.ClusterID = cfg.ClusterID
			result.RunnerID = cfg.RunnerID
			result.RunnerAuthToken = cfg.RunnerAuthToken
			if err := runnerPostJSON(ctx, client, cfg.ControlPlaneURL+"/api/v1/runners/commands/"+url.PathEscape(command.ID)+"/result", cfg.RunnerAuthToken, result, nil); err != nil {
				logger.Error("runner command result callback failed", "command_id", command.ID, "error", err)
			}
		}
	}
}

func nextRunnerCommand(ctx context.Context, cfg runnerConfig, client *http.Client) (domain.RunnerCommand, bool, error) {
	endpoint := cfg.ControlPlaneURL + "/api/v1/runners/commands/next?projectId=" + url.QueryEscape(cfg.ProjectID) + "&clusterId=" + url.QueryEscape(cfg.ClusterID) + "&runnerId=" + url.QueryEscape(cfg.RunnerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return domain.RunnerCommand{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.RunnerAuthToken)
	resp, err := client.Do(req)
	if err != nil {
		return domain.RunnerCommand{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return domain.RunnerCommand{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return domain.RunnerCommand{}, false, fmt.Errorf("runner command poll status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var command domain.RunnerCommand
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&command); err != nil {
		return domain.RunnerCommand{}, false, err
	}
	return command, true, nil
}

func executeRunnerCommand(ctx context.Context, command domain.RunnerCommand) domain.RunnerCommandResult {
	result := domain.RunnerCommandResult{CommandID: command.ID, Status: "failed", Namespace: command.Environment.Namespace, ReleaseName: command.Environment.ID}
	backend := orchestrator.NewHelmDirectBackend(nil)
	var err error
	switch command.Operation {
	case "validate_helm_chart":
		result.Namespace = ""
		result.ReleaseName = ""
		if err := validateRunnerHelmChart(ctx, command.ChartRef); err != nil {
			result.ErrorCode, result.Error = classifyHelmChartPreflightError(err)
			return result
		}
		result.Status = "succeeded"
		return result
	case "create", "recreate":
		result.ReleaseName, result.Namespace, err = backend.DeploymentTarget(command.Environment, command.ProjectConfig)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		err = backend.Apply(ctx, command.Environment, command.ProjectConfig)
	case "delete":
		result.ReleaseName, result.Namespace, err = backend.DeploymentTarget(command.Environment, command.ProjectConfig)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		err = backend.Delete(ctx, command.Environment, command.ProjectConfig)
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

func validateRunnerHelmChart(ctx context.Context, chartRef string) error {
	return validateRunnerHelmChartWithCommand(ctx, chartRef, func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "helm", args...).CombinedOutput()
	})
}

func validateRunnerHelmChartWithCommand(ctx context.Context, chartRef string, run func(context.Context, ...string) ([]byte, error)) error {
	chartRef = strings.TrimSpace(chartRef)
	if chartRef == "" {
		return fmt.Errorf("chart reference is required")
	}
	output, err := run(ctx, "show", "chart", chartRef)
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", strings.TrimSpace(string(output)))
}

func classifyHelmChartPreflightError(err error) (string, string) {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "repo") && strings.Contains(message, "not found"):
		return "helm_repo_missing", "Helm repository is not configured in the target runner."
	case strings.Contains(message, "unauthorized"), strings.Contains(message, "authentication"), strings.Contains(message, "forbidden"), strings.Contains(message, "denied"), strings.Contains(message, "401"):
		return "helm_chart_auth_failed", "Target runner could not authenticate to the Helm chart repository."
	case strings.Contains(message, "chart") && strings.Contains(message, "not found"), strings.Contains(message, "not found"):
		return "helm_chart_missing", "Helm chart was not found in the configured repository."
	default:
		return "helm_chart_preflight_failed", "Target runner could not resolve the Helm chart."
	}
}

func reportRunnerHeartbeat(ctx context.Context, cfg runnerConfig, client *http.Client, status string, errorMessage string) error {
	payload := domain.RunnerHeartbeatRequest{
		ProjectID:       cfg.ProjectID,
		ClusterID:       cfg.ClusterID,
		RunnerID:        cfg.RunnerID,
		DeploymentMode:  cfg.DeploymentMode,
		RunnerNamespace: cfg.RunnerNamespace,
		RunnerAuthToken: cfg.RunnerAuthToken,
		Status:          status,
		Error:           errorMessage,
		ObservedAt:      time.Now().UTC(),
	}
	return runnerPostJSON(ctx, client, cfg.ControlPlaneURL+"/api/v1/runners/heartbeat", "", payload, nil)
}

func runnerPostJSON(ctx context.Context, client *http.Client, endpoint string, bearerToken string, payload any, target any) error {
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
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s failed: status=%d body=%s", endpoint, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
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

func hostnameFallback(fallback string) string {
	value, err := os.Hostname()
	if err != nil || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func waitForPostgres(ctx context.Context, db *sql.DB, timeout time.Duration, interval time.Duration, logger *slog.Logger) error {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for attempt := 1; ; attempt++ {
		pingCtx, pingCancel := context.WithTimeout(deadline, 5*time.Second)
		lastErr = db.PingContext(pingCtx)
		pingCancel()
		if lastErr == nil {
			if attempt > 1 {
				logger.Info("postgres dependency ready", "attempt", attempt)
			}
			return nil
		}
		if deadline.Err() != nil {
			return lastErr
		}
		logger.Warn("postgres dependency not ready", "attempt", attempt, "error", lastErr)
		select {
		case <-deadline.Done():
			return lastErr
		case <-time.After(interval):
		}
	}
}

func waitForRedisQueue(ctx context.Context, queue *redisqueue.Queue, timeout time.Duration, interval time.Duration, logger *slog.Logger) error {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for attempt := 1; ; attempt++ {
		pingCtx, pingCancel := context.WithTimeout(deadline, 5*time.Second)
		lastErr = queue.Ping(pingCtx)
		pingCancel()
		if lastErr == nil {
			if attempt > 1 {
				logger.Info("redis dependency ready", "attempt", attempt)
			}
			return nil
		}
		if deadline.Err() != nil {
			return lastErr
		}
		logger.Warn("redis dependency not ready", "attempt", attempt, "error", lastErr)
		select {
		case <-deadline.Done():
			return lastErr
		case <-time.After(interval):
		}
	}
}

func discoverAgentCapabilities(ctx context.Context, source agent.CapabilitySource, logger *slog.Logger) agent.ClusterCapabilities {
	capabilities, err := source.DiscoverCapabilities(ctx)
	if err != nil {
		logger.Error("cluster capability discovery failed", "error", err)
		return agent.ClusterCapabilities{}
	}
	return capabilities
}

func discoverAgentCapabilitiesRequired(ctx context.Context, source agent.CapabilitySource) (agent.ClusterCapabilities, error) {
	capabilities, err := source.DiscoverCapabilities(ctx)
	if err != nil {
		return agent.ClusterCapabilities{}, err
	}
	if !hasCapability(capabilities.Capabilities, "core-v1") {
		return agent.ClusterCapabilities{}, fmt.Errorf("missing core-v1 capability")
	}
	return capabilities, nil
}

func hasCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if strings.TrimSpace(capability) == target {
			return true
		}
	}
	return false
}

func runTTLCleanup(logger *slog.Logger) {
	cfg := ttl.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	result, err := ttl.RunCleanup(ctx, cfg)
	if err != nil {
		logger.Error("ttl cleanup failed", "error", err)
		os.Exit(1)
	}
	logger.Info("ttl cleanup completed", "deleted", len(result.Deleted))
}

func runServer(logger *slog.Logger) {
	configFile := strings.TrimSpace(os.Getenv("ENVPILOT_RUNTIME_CONFIG_FILE"))
	cfg, err := loadRuntimeConfig(configFile)
	if err != nil {
		logger.Error("failed to load runtime config", "error", err, "config_file", configFile)
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		logger.Error("failed to create data directory", "error", err)
		os.Exit(1)
	}

	var db *sql.DB
	if cfg.DatabaseURL != "" {
		var err error
		db, err = postgres.Open(cfg.DatabaseURL)
		if err != nil {
			logger.Error("failed to open postgres", "error", err)
			os.Exit(1)
		}
		defer func() {
			_ = db.Close()
		}()
		if err := waitForPostgres(context.Background(), db, cfg.DependencyWaitTimeout, cfg.DependencyWaitInterval, logger); err != nil {
			logger.Error("failed to ping postgres", "error", err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := postgres.NewMigratorWithDir(db, cfg.PostgresMigrationsDir).Apply(ctx); err != nil {
			cancel()
			logger.Error("failed to apply postgres migrations", "error", err)
			os.Exit(1)
		}
		cancel()
		logger.Info("postgres foundation ready", "migrations_dir", cfg.PostgresMigrationsDir)
	}

	var redisJobsQueue *redisqueue.Queue
	if cfg.RedisURL != "" {
		queue, err := redisqueue.New(cfg.RedisURL, "envpilot")
		if err != nil {
			logger.Error("failed to initialise redis queue", "error", err)
			os.Exit(1)
		}
		defer func() {
			_ = queue.Close()
		}()
		if err := waitForRedisQueue(context.Background(), queue, cfg.DependencyWaitTimeout, cfg.DependencyWaitInterval, logger); err != nil {
			logger.Error("failed to ping redis", "error", err)
			os.Exit(1)
		}
		redisJobsQueue = queue
		logger.Info("redis queue foundation ready")
	}

	productCatalog, err := catalog.Load(cfg.CatalogPath)
	if err != nil {
		logger.Error("failed to load product catalog", "error", err)
		os.Exit(1)
	}

	var envStore store.EnvironmentStore
	var projectStore store.ProjectStore
	var productStore store.ProductStore
	var settingsStore store.SettingsStore
	var bootstrapSessionStore store.BootstrapSessionStore
	var projectConfigStore store.ProjectConfigStore
	if db != nil {
		envStore = store.NewSQLStore(db)
		projectStore, err = store.NewSQLProjectStore(db, catalog.DefaultProjects())
		if err != nil {
			logger.Error("failed to initialise project store", "error", err)
			os.Exit(1)
		}
		bootstrapSessionStore, err = store.NewSQLBootstrapSessionStore(db)
		if err != nil {
			logger.Error("failed to initialise bootstrap session store", "error", err)
			os.Exit(1)
		}
		projectConfigStore, err = store.NewSQLProjectConfigStore(db)
		if err != nil {
			logger.Error("failed to initialise project config store", "error", err)
			os.Exit(1)
		}
		settingsStore, err = store.NewSQLSettingsStore(db, app.DefaultControlPlaneSettings(cfg))
		if err != nil {
			logger.Error("failed to initialise settings store", "error", err)
			os.Exit(1)
		}
		productStore, err = store.NewSQLProductStore(db, productCatalog.List())
		if err != nil {
			logger.Error("failed to initialise product store", "error", err)
			os.Exit(1)
		}
	} else {
		envStore, err = store.NewJSONStore(cfg.StorePath())
		if err != nil {
			logger.Error("failed to initialise store", "error", err)
			os.Exit(1)
		}
		projectStore, err = store.NewJSONProjectStore(cfg.ProjectStorePath(), catalog.DefaultProjects())
		if err != nil {
			logger.Error("failed to initialise project store", "error", err)
			os.Exit(1)
		}
		bootstrapSessionStore, err = store.NewJSONBootstrapSessionStore(cfg.BootstrapSessionStorePath())
		if err != nil {
			logger.Error("failed to initialise bootstrap session store", "error", err)
			os.Exit(1)
		}
		projectConfigStore, err = store.NewJSONProjectConfigStore(cfg.ProjectConfigStorePath())
		if err != nil {
			logger.Error("failed to initialise project config store", "error", err)
			os.Exit(1)
		}
		settingsStore, err = store.NewJSONSettingsStore(cfg.SettingsStorePath(), app.DefaultControlPlaneSettings(cfg))
		if err != nil {
			logger.Error("failed to initialise settings store", "error", err)
			os.Exit(1)
		}
		productStore, err = store.NewJSONProductStore(cfg.ProductStorePath(), productCatalog.List())
		if err != nil {
			logger.Error("failed to initialise product store", "error", err)
			os.Exit(1)
		}
	}
	if envStore == nil {
		logger.Error("failed to initialise store")
		os.Exit(1)
	}
	if projectStore == nil {
		logger.Error("failed to initialise project store")
		os.Exit(1)
	}
	if settingsStore == nil {
		logger.Error("failed to initialise settings store")
		os.Exit(1)
	}
	if bootstrapSessionStore == nil {
		logger.Error("failed to initialise bootstrap session store")
		os.Exit(1)
	}
	if projectConfigStore == nil {
		logger.Error("failed to initialise project config store")
		os.Exit(1)
	}
	if productStore == nil {
		logger.Error("failed to initialise product store")
		os.Exit(1)
	}

	renderer := gitops.NewFluxRenderer(cfg.GitOps)
	writer := gitops.NewGitWriter(cfg.GitOpsDir, cfg.EnableGitCommit, cfg.EnableGitPush, cfg.GitPushRemote, cfg.GitPushBranch, cfg.GitAuthorName, cfg.GitAuthorEmail)
	envService := app.NewEnvironmentService(cfg, productCatalog, envStore, renderer, writer)
	productService := app.NewProductService(productStore)
	projectService := app.NewProjectService(projectStore)
	credentialEncryptor, err := app.NewAESGCMCredentialEncryptor(cfg.CredentialEncryptionKey, "local")
	if err != nil {
		logger.Error("failed to initialise credential encryptor", "error", err)
		os.Exit(1)
	}
	bootstrapSessionService := app.NewBootstrapSessionServiceWithEncryptor(bootstrapSessionStore, credentialEncryptor)
	projectConfigService := app.NewProjectConfigService(projectConfigStore)
	scmValidationService := app.NewSCMValidationService()
	settingsService := app.NewSettingsService(settingsStore)
	envService.SetProductProvider(productService)
	envService.SetProjectStore(projectStore)
	envService.SetCommenter(scmcomment.New(scmcomment.Config{
		GitHubToken:   cfg.GitHubToken,
		GitHubAPI:     cfg.GitHubAPI,
		GitLabToken:   cfg.GitLabToken,
		GitLabAPI:     cfg.GitLabAPI,
		TokenResolver: scmTokenResolver(settingsService, projectStore),
	}))
	projectService.SetSettingsProvider(settingsService)
	envService.SetSettingsProvider(settingsService)
	envService.SetNotifier(notify.New(settingsService, secrets.NewResolver()))
	jobOptions := []jobs.Option{
		jobs.WithRetryDelay(cfg.JobRetryDelay),
		jobs.WithMaxAttempts(cfg.JobMaxAttempts),
		jobs.WithProjectResolver(projectService),
	}
	if db != nil {
		jobOptions = append(jobOptions, jobs.WithStore(jobs.NewSQLStore(db)))
	}
	if redisJobsQueue != nil {
		jobOptions = append(jobOptions, jobs.WithQueue(jobs.NewRedisQueue(redisJobsQueue, "jobs")))
	}
	jobManager := jobs.NewManager(envService, jobOptions...)
	application := server.New(server.Dependencies{
		Config:            cfg,
		Service:           envService,
		Products:          productService,
		Projects:          projectService,
		SCMValidation:     scmValidationService,
		BootstrapSessions: bootstrapSessionService,
		ProjectConfigs:    projectConfigService,
		Settings:          settingsService,
		Jobs:              jobManager,
		Logger:            logger,
	})

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           application.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	reloadConfig := make(chan os.Signal, 1)
	signal.Notify(reloadConfig, syscall.SIGHUP)
	defer signal.Stop(reloadConfig)
	reloadPeriod := configReloadInterval()
	var tickerCh <-chan time.Time
	if reloadPeriod > 0 {
		ticker := time.NewTicker(reloadPeriod)
		defer ticker.Stop()
		tickerCh = ticker.C
	}
	reloadConfigNow := func() {
		loaded, loadErr := loadRuntimeConfig(configFile)
		if loadErr != nil {
			logger.Error("failed to reload runtime config", "error", loadErr, "config_file", configFile)
			return
		}
		cfg = loaded
		application.ReloadConfig(cfg)
		logger.Info("config reloaded",
			"api_auth_enabled", strings.TrimSpace(cfg.APIReadToken) != "" || strings.TrimSpace(cfg.APIWriteToken) != "",
			"rate_limit_enabled", cfg.RateLimitRequests > 0 && cfg.RateLimitWindow > 0,
			"github_webhook_secret", cfg.GitHubWebhookSecret != "",
			"gitlab_webhook_secret", cfg.GitLabWebhookSecret != "",
			"audit_log_enabled", strings.TrimSpace(cfg.AuditLogPath) != "")
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-reloadConfig:
				reloadConfigNow()
			case <-tickerCh:
				reloadConfigNow()
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("envpilot started", "addr", cfg.Addr, "gitops_dir", cfg.GitOpsDir)
		errCh <- httpServer.ListenAndServe()
	}()

	controller := app.NewLifecycleController(envService, cfg.TTLCheckInterval, logger, cfg.IdleThreshold)
	go controller.Run(rootCtx)
	go jobManager.Run(rootCtx)

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown requested")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
}

func scmTokenResolver(settingsService *app.SettingsService, projectStore store.ProjectStore) scmcomment.TokenResolver {
	return func(ctx context.Context, provider string, environment domain.Environment) (string, error) {
		settings, err := settingsService.GetSettings()
		if err != nil {
			return "", err
		}
		resolver := secrets.NewResolver()
		provider = strings.ToLower(strings.TrimSpace(provider))

		if projectStore != nil && strings.TrimSpace(environment.Project) != "" {
			project, err := projectStore.Get(environment.Project)
			if err == nil {
				for _, id := range project.SecretRefs {
					if ref, ok := findSecretRef(settings.SecretRefs, id); ok && secretScopeMatchesProvider(ref.Scope, provider) {
						return resolver.Resolve(ctx, ref)
					}
				}
			} else if err != store.ErrProjectNotFound {
				return "", err
			}
		}

		for _, ref := range settings.SecretRefs {
			if secretScopeMatchesProvider(ref.Scope, provider) {
				return resolver.Resolve(ctx, ref)
			}
		}
		return "", nil
	}
}

func findSecretRef(refs []domain.SecretReference, id string) (domain.SecretReference, bool) {
	id = normalizeSecretID(id)
	for _, ref := range refs {
		if normalizeSecretID(ref.ID) == id {
			return ref, true
		}
	}
	return domain.SecretReference{}, false
}

func secretScopeMatchesProvider(scope string, provider string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	switch scope {
	case provider, "scm", "git", "comment", "comments", "repository", "repo":
		return true
	default:
		return false
	}
}

func normalizeSecretID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	return strings.Trim(value, "-")
}

func loadRuntimeConfig(path string) (config.Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return config.FromEnv(), nil
	}
	return config.FromEnvWithRuntimeFile(path)
}

func configReloadInterval() time.Duration {
	value := strings.TrimSpace(os.Getenv("ENVPILOT_CONFIG_RELOAD_SECONDS"))
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0
	}
	return time.Duration(parsed) * time.Second
}
