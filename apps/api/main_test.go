package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"envpilot/agent"
	"envpilot/internal/domain"
	"envpilot/internal/orchestrator"
)

type fakeCapabilitySource struct {
	capabilities agent.ClusterCapabilities
	err          error
}

type fakeRunnerCommandBackend struct {
	status domain.EnvironmentStatus
}

func (b fakeRunnerCommandBackend) Render(context.Context, domain.Environment, domain.ProjectConfig) ([]orchestrator.Manifest, error) {
	return nil, nil
}
func (b fakeRunnerCommandBackend) Apply(context.Context, domain.Environment, domain.ProjectConfig) error {
	return nil
}
func (b fakeRunnerCommandBackend) Delete(context.Context, domain.Environment, domain.ProjectConfig) error {
	return nil
}
func (b fakeRunnerCommandBackend) Status(context.Context, domain.Environment, domain.ProjectConfig) (domain.EnvironmentStatus, error) {
	return b.status, nil
}
func (b fakeRunnerCommandBackend) DeploymentTarget(environment domain.Environment, _ domain.ProjectConfig) (string, string, error) {
	return environment.ID, environment.Namespace, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNextRunnerCommandClassifiesEndpointAndAuthenticationFailures(t *testing.T) {
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth"}
	tests := []struct {
		name      string
		status    int
		assertion func(error) bool
	}{
		{name: "missing endpoint", status: http.StatusNotFound, assertion: isRunnerCommandAPIIncompatible},
		{name: "authentication failure", status: http.StatusUnauthorized, assertion: isRunnerCommandAuthenticationError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if got := request.Header.Get(runnerCommandAPIVersionHeader); got != runnerCommandAPIVersion {
					t.Fatalf("command API version header = %q", got)
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("test failure")), Request: request}, nil
			})}
			_, _, err := nextRunnerCommand(context.Background(), cfg, client)
			if err == nil || !tc.assertion(err) {
				t.Fatalf("nextRunnerCommand error = %v", err)
			}
		})
	}
}

func TestRunnerConfigRejectsHostLocalRemoteControlPlaneEndpoint(t *testing.T) {
	cfg := runnerConfig{
		ControlPlaneURL:          "http://host.minikube.internal:18080",
		ControlPlaneEndpointMode: "remote",
		ProjectID:                "checkout",
		ClusterID:                "remote-cluster",
		RunnerID:                 "checkout-runner",
		RunnerNamespace:          "envpilot",
		DeploymentMode:           "helm",
		RunnerAuthToken:          "runner-auth-token",
		HeartbeatInterval:        time.Second,
		ReportTimeout:            time.Second,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "target-pod-reachable") {
		t.Fatalf("remote host-local endpoint error=%v", err)
	}
}

func TestNextRunnerCommandRequiresCompatibilityResponseHeader(t *testing.T) {
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth"}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	_, _, err := nextRunnerCommand(context.Background(), cfg, client)
	if err == nil || !isRunnerCommandAPIIncompatible(err) {
		t.Fatalf("nextRunnerCommand error = %v, want compatibility error", err)
	}
}

func TestNextRunnerCommandClassifiesTransportFailure(t *testing.T) {
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth"}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	_, _, err := nextRunnerCommand(context.Background(), cfg, client)
	if err == nil || !isRunnerCommandTransportError(err) {
		t.Fatalf("nextRunnerCommand error = %v, want transport error", err)
	}
}

func TestRunnerPostJSONClassifiesMissingBootstrapSessionForRecovery(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":"bootstrap session not found","code":"bootstrap_session_not_found",` +
				`"recovery":"rotate_runner_bootstrap_credentials"}`)),
			Request: request,
		}, nil
	})}
	err := reportRunnerHeartbeat(context.Background(), runnerConfig{
		ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerNamespace: "envpilot", DeploymentMode: "helm", RunnerAuthToken: "stale-auth",
	}, client, string(domain.RunnerHeartbeatStatusOnline), "")
	if err == nil || !isRunnerStaleBootstrapIdentityError(err) {
		t.Fatalf("heartbeat error = %v, want stale bootstrap identity", err)
	}
}

func TestRunnerPostJSONClassifiesFixtureIdentityReissueForAutomaticRecovery(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"fixture runner identity was reissued; retry registration","code":"fixture_identity_reissued"}`)),
			Request:    request,
		}, nil
	})}
	err := reportRunnerHeartbeat(context.Background(), runnerConfig{
		ControlPlaneURL: "http://runner.test", ProjectID: "fixture", ClusterID: "remote", RunnerID: "fixture-runner", RunnerNamespace: "envpilot", DeploymentMode: "helm", RunnerAuthToken: "stale-auth",
	}, client, string(domain.RunnerHeartbeatStatusOnline), "")
	if err == nil || !isRunnerFixtureIdentityReissuedError(err) {
		t.Fatalf("heartbeat error = %v, want fixture identity reissued", err)
	}
}

func TestPrepareRunnerFixtureRecoveryClearsOnlyPersistedRuntimeAuth(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "runner-auth-token")
	if err := persistRuntimeToken(authPath, "runtime-auth"); err != nil {
		t.Fatalf("persist runtime auth: %v", err)
	}
	prepareRunnerFixtureRecovery(runnerConfig{ProjectID: "fixture", RunnerID: "fixture-runner", RunnerAuthTokenFile: authPath}, slog.Default())
	if got := readRuntimeTokenFile(authPath); got != "" {
		t.Fatalf("persisted runtime auth after recovery = %q, want empty", got)
	}
}

func TestRunnerRegistrationTokenRotationOverridesPersistedAuth(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "runner-auth-token")
	if err := persistRuntimeToken(authPath, "old-runner-auth"); err != nil {
		t.Fatalf("persist auth token: %v", err)
	}
	if err := persistRegistrationTokenFingerprint(authPath, "old-registration-token"); err != nil {
		t.Fatalf("persist old registration token fingerprint: %v", err)
	}
	t.Setenv("ENVPILOT_RUNNER_AUTH_TOKEN_FILE", authPath)
	t.Setenv("ENVPILOT_RUNNER_REGISTRATION_TOKEN", "rotated-registration-token")
	t.Setenv("ENVPILOT_RUNNER_AUTH_TOKEN", "")

	cfg := runnerConfigFromEnv()
	if cfg.RunnerAuthToken != "" {
		t.Fatalf("rotated bootstrap token must override persisted auth, got %q", cfg.RunnerAuthToken)
	}
	if cfg.RegistrationToken != "rotated-registration-token" {
		t.Fatalf("registration token = %q", cfg.RegistrationToken)
	}
}

func TestRunnerRegistrationTokenFingerprintAdoptsLegacyAuthThenDetectsRotation(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "runner-auth-token")
	if err := persistRuntimeToken(authPath, "legacy-runner-auth"); err != nil {
		t.Fatalf("persist auth token: %v", err)
	}
	if registrationTokenChanged(authPath, "initial-registration-token") {
		t.Fatal("legacy auth must be adopted without forcing a consumed-token registration")
	}
	if !registrationTokenChanged(authPath, "rotated-registration-token") {
		t.Fatal("changed bootstrap token must trigger a new registration")
	}
}

func TestPollRunnerCommandsOnceDegradesAndStopsForMissingEndpoint(t *testing.T) {
	polls := 0
	heartbeats := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/runners/commands/next":
			polls++
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("missing")), Request: request}, nil
		case "/api/v1/runners/heartbeat":
			heartbeats++
			var heartbeat domain.RunnerHeartbeatRequest
			if err := json.NewDecoder(request.Body).Decode(&heartbeat); err != nil {
				t.Fatalf("decode degraded heartbeat: %v", err)
			}
			if heartbeat.Status != string(domain.RunnerHeartbeatStatusDegraded) || heartbeat.Error == "" {
				t.Fatalf("degraded heartbeat = %#v", heartbeat)
			}
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
			return nil, nil
		}
	})}

	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth", DeploymentMode: "helm", RunnerNamespace: "envpilot"}
	state := newRunnerRuntimeState()
	health := &runnerHealth{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if keepPolling := pollRunnerCommandsOnce(context.Background(), cfg, client, state, health, logger); keepPolling {
		t.Fatal("missing command endpoint must disable polling")
	}
	status, reason := state.heartbeat()
	if status != string(domain.RunnerHeartbeatStatusDegraded) || reason == "" {
		t.Fatalf("runtime state = (%q, %q)", status, reason)
	}
	if polls != 1 || heartbeats != 1 || !health.degraded.Load() {
		t.Fatalf("polls=%d heartbeats=%d degraded=%v", polls, heartbeats, health.degraded.Load())
	}
}

func TestPollRunnerCommandsOnceStopsForMissingBootstrapSession(t *testing.T) {
	polls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		polls++
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"bootstrap session not found","code":"bootstrap_session_not_found","recovery":"rotate_runner_bootstrap_credentials"}`)),
			Request:    request,
		}, nil
	})}

	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth", DeploymentMode: "helm", RunnerNamespace: "envpilot"}
	state := newRunnerRuntimeState()
	health := &runnerHealth{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if keepPolling := pollRunnerCommandsOnce(context.Background(), cfg, client, state, health, logger); keepPolling {
		t.Fatal("missing bootstrap session must disable command polling")
	}
	status, reason := state.heartbeat()
	if !state.stale.Load() || !health.stale.Load() || health.online.Load() || status != string(domain.RunnerHeartbeatStatusDegraded) || reason == "" {
		t.Fatalf("stale runtime state = status=%q reason=%q stale=%v health=%+v", status, reason, state.stale.Load(), health)
	}
	if polls != 1 {
		t.Fatalf("stale identity must be polled once, got %d polls", polls)
	}
}

func TestPollRunnerCommandsOnceReportsTheClaimedAttemptID(t *testing.T) {
	resultCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/runners/commands/next":
			command := domain.RunnerCommand{
				ID: "delete-42", ProjectID: "checkout", ClusterID: "target", RunnerID: "checkout-runner", Operation: "unsupported", AttemptID: "delete-42-attempt-2", Status: "claimed",
				Environment: domain.Environment{ID: "pr-42", Project: "checkout", Namespace: "feature-42"},
			}
			body, _ := json.Marshal(command)
			headers := make(http.Header)
			headers.Set(runnerCommandAPIVersionHeader, runnerCommandAPIVersion)
			return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
		case "/api/v1/runners/commands/delete-42/result":
			resultCalls++
			var result domain.RunnerCommandResult
			if err := json.NewDecoder(request.Body).Decode(&result); err != nil {
				t.Fatalf("decode command result: %v", err)
			}
			if result.AttemptID != "delete-42-attempt-2" {
				t.Fatalf("result attempt ID=%q", result.AttemptID)
			}
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
			return nil, nil
		}
	})}
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "target", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth", DeploymentMode: "helm", RunnerNamespace: "envpilot"}
	state := newRunnerRuntimeState()
	health := &runnerHealth{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if keepPolling := pollRunnerCommandsOnce(context.Background(), cfg, client, state, health, logger); !keepPolling {
		t.Fatal("successful command callback must keep polling")
	}
	if resultCalls != 1 {
		t.Fatalf("result callbacks=%d, want 1", resultCalls)
	}
}

func TestClassifyHelmChartPreflightError(t *testing.T) {
	tests := []struct {
		name string
		err  string
		code string
	}{
		{name: "missing repo", err: "Error: repo stable not found", code: "helm_repo_missing"},
		{name: "missing chart", err: "Error: chart \"missing\" not found", code: "helm_chart_missing"},
		{name: "missing version", err: "Error: chart version 1.2.3 not found", code: "helm_chart_version_missing"},
		{name: "authentication", err: "Error: unauthorized: authentication required", code: "helm_chart_auth_failed"},
		{name: "invalid reference", err: "invalid chart reference", code: "helm_chart_reference_invalid"},
		{name: "other", err: "network timeout", code: "helm_chart_preflight_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, message := classifyHelmChartPreflightError(errors.New(test.err))
			if code != test.code || strings.TrimSpace(message) == "" {
				t.Fatalf("classify(%q) = (%q, %q), want code %q and safe message", test.err, code, message, test.code)
			}
		})
	}
}

func TestValidateRunnerHelmChartUsesTargetRunnerHelmForPrivateOCIChart(t *testing.T) {
	var got []string
	err := validateRunnerHelmChartWithCommand(context.Background(), "oci://registry.example.com/charts/orders", "", func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("apiVersion: v2\nname: orders\n"), nil
	})
	if err != nil {
		t.Fatalf("validate chart: %v", err)
	}
	if strings.Join(got, " ") != "show chart oci://registry.example.com/charts/orders" {
		t.Fatalf("helm arguments = %q", got)
	}
}

func TestValidateRunnerHelmChartPassesPinnedVersion(t *testing.T) {
	var got []string
	err := validateRunnerHelmChartWithCommand(context.Background(), "oci://ghcr.io/envpilot/envpilot-e2e-workload", "0.1.0-main.38", func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("apiVersion: v2\nname: envpilot-e2e-workload\n"), nil
	})
	if err != nil {
		t.Fatalf("validate chart: %v", err)
	}
	if strings.Join(got, " ") != "show chart oci://ghcr.io/envpilot/envpilot-e2e-workload --version 0.1.0-main.38" {
		t.Fatalf("helm arguments = %q", got)
	}
}

func TestValidateRunnerHelmChartOmitsVersionForDirectArchive(t *testing.T) {
	var got []string
	err := validateRunnerHelmChartWithCommand(context.Background(), "https://charts.example.test/orders-2.8.1.tgz", "2.8.1", func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("apiVersion: v2\nname: orders\n"), nil
	})
	if err != nil {
		t.Fatalf("validate chart: %v", err)
	}
	if strings.Join(got, " ") != "show chart https://charts.example.test/orders-2.8.1.tgz" {
		t.Fatalf("direct archive must not receive --version, arguments = %q", got)
	}
}

func TestProjectConfigForRunnerCommandCarriesChartVersion(t *testing.T) {
	config := projectConfigForRunnerCommand(domain.RunnerCommand{
		ChartRef:     "oci://registry.example.com/charts/orders",
		ChartVersion: "2.8.1",
		ProjectConfig: domain.ProjectConfig{Config: map[string]any{
			"deployment": map[string]any{"backend": "helm_direct", "helmDirect": map[string]any{"wait": true}},
		}},
	})
	deployment, ok := config.Config["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("deployment config = %#v", config.Config)
	}
	helmDirect, ok := deployment["helmDirect"].(map[string]any)
	if !ok || helmDirect["chartRef"] != "oci://registry.example.com/charts/orders" || helmDirect["chartVersion"] != "2.8.1" {
		t.Fatalf("runner command lost chart contract: %#v", deployment)
	}
}

func TestExecuteRunnerStatusReportsTargetClusterLifecycle(t *testing.T) {
	result := executeRunnerCommandWithBackend(context.Background(), domain.RunnerCommand{
		ID: "status-target-cluster", Operation: "status",
		Environment: domain.Environment{ID: "feature-42", Project: "checkout", Namespace: "envpilot-pr-42"},
	}, fakeRunnerCommandBackend{status: domain.StatusReady})
	if result.Status != "succeeded" || result.EnvironmentStatus != string(domain.StatusReady) {
		t.Fatalf("status result = %#v", result)
	}
}

func TestRunnerRejectsHelmCommandOutsideChartManagedNamespaceRBAC(t *testing.T) {
	cfg := runnerConfig{
		RunnerNamespace:            "envpilot-system",
		FeatureEnvWriterMode:       "generatedFeatureNamespaces",
		FeatureEnvWriterNamespaces: []string{"envpilot-e2e-feature"},
	}
	result := executeRunnerCommandWithNamespaceGuard(context.Background(), domain.RunnerCommand{
		ID: "forbidden-target", Operation: "create",
		Environment: domain.Environment{ID: "feature-201", Project: "checkout", Namespace: "envpilot-pr-201"},
	}, fakeRunnerCommandBackend{}, cfg.canRunHelmInNamespace)
	if result.ErrorCode != "runner_namespace_access_denied" || !strings.Contains(result.Error, "envpilot-pr-201") {
		t.Fatalf("forbidden target result = %#v", result)
	}
}

func TestRunnerAllowsHelmLifecycleOnlyInConfiguredTargetNamespace(t *testing.T) {
	cfg := runnerConfig{
		FeatureEnvWriterMode:       "preconfiguredNamespaces",
		FeatureEnvWriterNamespaces: []string{"envpilot-pr-201"},
	}
	for _, operation := range []string{"create", "recreate", "status", "delete"} {
		t.Run(operation, func(t *testing.T) {
			result := executeRunnerCommandWithNamespaceGuard(context.Background(), domain.RunnerCommand{
				ID: "allowed-" + operation, Operation: operation,
				Environment: domain.Environment{ID: "feature-201", Project: "checkout", Namespace: "envpilot-pr-201"},
			}, fakeRunnerCommandBackend{status: domain.StatusReady}, cfg.canRunHelmInNamespace)
			if result.Status != "succeeded" || result.ErrorCode != "" {
				t.Fatalf("%s result = %#v", operation, result)
			}
		})
	}
}

func TestValidateRunnerHelmChartRejectsLegacyLocalPathWithoutExecutingHelm(t *testing.T) {
	called := false
	err := validateRunnerHelmChartWithCommand(context.Background(), "deploy/helm/{{ .project.id }}", "", func(_ context.Context, _ ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid chart reference") || called {
		t.Fatalf("legacy local chart ref must be rejected before helm execution, err=%v called=%v", err, called)
	}
}

func (s fakeCapabilitySource) DiscoverCapabilities(context.Context) (agent.ClusterCapabilities, error) {
	if s.err != nil {
		return agent.ClusterCapabilities{}, s.err
	}
	return s.capabilities, nil
}

func TestDiscoverAgentCapabilitiesRequiredRejectsMissingCoreV1(t *testing.T) {
	_, err := discoverAgentCapabilitiesRequired(context.Background(), fakeCapabilitySource{
		capabilities: agent.ClusterCapabilities{Capabilities: []string{"apps-v1", "flux-helm-v2"}},
	})
	if err == nil {
		t.Fatal("expected missing core-v1 capability error")
	}
}

func TestDiscoverAgentCapabilitiesRequiredAllowsCoreV1(t *testing.T) {
	caps, err := discoverAgentCapabilitiesRequired(context.Background(), fakeCapabilitySource{
		capabilities: agent.ClusterCapabilities{Capabilities: []string{"apps-v1", "core-v1", "flux-helm-v2"}},
	})
	if err != nil {
		t.Fatalf("discover capabilities: %v", err)
	}
	if !hasCapability(caps.Capabilities, "core-v1") {
		t.Fatalf("expected core-v1 in discovered capabilities: %#v", caps.Capabilities)
	}
}

func TestProjectInitCreatesBasicConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "envpilot-project.json")
	configPath, project := projectInitConfig([]string{
		"--path", path,
		"--id", "checkout",
		"--name", "Checkout",
		"--product", "bethunder",
		"--base-namespace", "feature",
	})
	if configPath != path {
		t.Fatalf("path = %q", configPath)
	}
	if err := writeProjectInitConfig(configPath, project); err != nil {
		t.Fatalf("write config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var saved domain.Project
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if saved.ID != "checkout" || saved.Name != "Checkout" || saved.ProductID != "bethunder" {
		t.Fatalf("project = %+v", saved)
	}
	if saved.BaseEnvConfig.Namespace != "feature" {
		t.Fatalf("base namespace = %q", saved.BaseEnvConfig.Namespace)
	}
	if saved.GitRepo.DefaultBranch != "main" || saved.GitOpsRepo.Path == "" {
		t.Fatalf("repo defaults = %+v %+v", saved.GitRepo, saved.GitOpsRepo)
	}
}

func TestEnvCLIListDeleteAndLogs(t *testing.T) {
	var deleteCalled bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/environments":
			_ = json.NewEncoder(w).Encode([]domain.Environment{{
				ID:              "pr-123",
				Status:          domain.StatusReady,
				URL:             "https://pr-123.checkout.preview.local",
				CostEstimateDay: "~ €0.60/day",
				UpdatedAt:       time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC),
			}})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/environments/pr-123":
			deleteCalled = true
			_ = json.NewEncoder(w).Encode(domain.Environment{ID: "pr-123", Status: domain.StatusTerminating})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/environments/pr-123/events":
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []domain.KubernetesEvent{{
				Type:         "Warning",
				Reason:       "FailedScheduling",
				InvolvedName: "api-123",
				Message:      "pod pending",
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test server: %v", err)
	}
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler},
	}
	server.Start()
	defer server.Close()

	var list bytes.Buffer
	if err := runEnvCommand(context.Background(), []string{"list", "--api", server.URL}, &list); err != nil {
		t.Fatalf("env list: %v", err)
	}
	if !strings.Contains(list.String(), "pr-123\tready\thttps://pr-123.checkout.preview.local\t~ €0.60/day") {
		t.Fatalf("list output = %q", list.String())
	}

	var deleted bytes.Buffer
	if err := runEnvCommand(context.Background(), []string{"delete", "pr-123", "--api", server.URL}, &deleted); err != nil {
		t.Fatalf("env delete: %v", err)
	}
	if !deleteCalled || !strings.Contains(deleted.String(), "pr-123\tterminating") {
		t.Fatalf("delete called=%v output=%q", deleteCalled, deleted.String())
	}

	var logs bytes.Buffer
	if err := runEnvCommand(context.Background(), []string{"logs", "pr-123", "--api", server.URL}, &logs); err != nil {
		t.Fatalf("env logs: %v", err)
	}
	if !strings.Contains(logs.String(), "Warning\tFailedScheduling\tapi-123\tpod pending") {
		t.Fatalf("logs output = %q", logs.String())
	}
}

func TestRunAgentInstallCheckFlow(t *testing.T) {
	var register domain.AgentRegistrationRequest
	var heartbeat domain.AgentHeartbeatRequest
	requests := 0

	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/api/v1/agents/register":
			if err := json.NewDecoder(r.Body).Decode(&register); err != nil {
				t.Fatalf("decode register request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"dev-us","name":"dev-us","provider":"kubernetes","agentAuthToken":"agent-install-auth-token"}`))
			return
		case "/api/v1/agents/heartbeat":
			if err := json.NewDecoder(r.Body).Decode(&heartbeat); err != nil {
				t.Fatalf("decode heartbeat request: %v", err)
			}
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer controlPlane.Close()

	cfg := agent.Config{
		ControlPlaneURL:   controlPlane.URL,
		ClusterID:         "dev-us",
		AgentID:           "agent-install-check",
		AgentVersion:      "test",
		HeartbeatInterval: 30 * time.Second,
		ReportTimeout:     5 * time.Second,
	}
	source := fakeCapabilitySource{
		capabilities: agent.ClusterCapabilities{
			KubernetesVersion: "v1.30.1",
			Capabilities:      []string{"core-v1", "apps-v1", "flux-helm-v2"},
		},
	}
	reporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)

	_, err := runAgentInstallCheckFlow(context.Background(), cfg, source, reporter)
	if err != nil {
		t.Fatalf("runAgentInstallCheckFlow: %v", err)
	}
	if requests != 2 {
		t.Fatalf("expected 2 requests, got %d", requests)
	}
	if register.ClusterID != "dev-us" || register.AgentID != "agent-install-check" {
		t.Fatalf("invalid register payload: %#v", register)
	}
	if heartbeat.ClusterID != "dev-us" || heartbeat.Status != "online" {
		t.Fatalf("invalid heartbeat payload: %#v", heartbeat)
	}
	if heartbeat.AgentAuthToken != "agent-install-auth-token" {
		t.Fatalf("invalid heartbeat auth token: %#v", heartbeat)
	}
}

func TestRunAgentInstallCheckFlowPersistsIssuedAgentAuthToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "agent-auth-token")
	var register domain.AgentRegistrationRequest
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/register":
			if err := json.NewDecoder(r.Body).Decode(&register); err != nil {
				t.Fatalf("decode register request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"dev-us","name":"dev-us","provider":"kubernetes","agentAuthToken":"persisted-agent-auth-token"}`))
		case "/api/v1/agents/heartbeat":
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer controlPlane.Close()

	cfg := agent.Config{
		ControlPlaneURL:    controlPlane.URL,
		ClusterID:          "dev-us",
		AgentID:            "agent-install-check",
		AgentVersion:       "test",
		RegistrationToken:  "bootstrap-registration-token",
		AgentAuthTokenFile: tokenPath,
		HeartbeatInterval:  30 * time.Second,
		ReportTimeout:      5 * time.Second,
		KubernetesAPIURL:   "https://kubernetes.example",
		BootstrapProjectID: "bootstrap-project",
		NamespaceSelector:  "",
		AgentNamespace:     "envpilot",
		KubernetesToken:    "kube-token",
		KubernetesCA:       "",
		ResyncInterval:     30 * time.Second,
	}
	source := fakeCapabilitySource{
		capabilities: agent.ClusterCapabilities{Capabilities: []string{"core-v1"}},
	}
	reporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)

	if _, err := runAgentInstallCheckFlow(context.Background(), cfg, source, reporter); err != nil {
		t.Fatalf("runAgentInstallCheckFlow: %v", err)
	}
	if register.RegistrationToken != "bootstrap-registration-token" {
		t.Fatalf("registration token = %q", register.RegistrationToken)
	}
	content, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read persisted auth token: %v", err)
	}
	if strings.TrimSpace(string(content)) != "persisted-agent-auth-token" {
		t.Fatalf("persisted auth token = %q", string(content))
	}
}

func TestRunAgentInstallCheckFlowWithPersistedAuthSkipsRegistration(t *testing.T) {
	var registerCalled bool
	var heartbeat domain.AgentHeartbeatRequest
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/register":
			registerCalled = true
			t.Fatalf("persisted auth flow must not call register")
		case "/api/v1/agents/heartbeat":
			if err := json.NewDecoder(r.Body).Decode(&heartbeat); err != nil {
				t.Fatalf("decode heartbeat request: %v", err)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer controlPlane.Close()

	cfg := agent.Config{
		ControlPlaneURL:    controlPlane.URL,
		ClusterID:          "dev-us",
		AgentID:            "agent-install-check",
		AgentVersion:       "test",
		RegistrationToken:  "consumed-bootstrap-token",
		AgentAuthToken:     "persisted-agent-auth-token",
		HeartbeatInterval:  30 * time.Second,
		ReportTimeout:      5 * time.Second,
		KubernetesAPIURL:   "https://kubernetes.example",
		BootstrapProjectID: "bootstrap-project",
	}
	source := fakeCapabilitySource{
		capabilities: agent.ClusterCapabilities{Capabilities: []string{"core-v1"}},
	}
	reporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)

	if _, err := runAgentInstallCheckFlow(context.Background(), cfg, source, reporter); err != nil {
		t.Fatalf("runAgentInstallCheckFlow: %v", err)
	}
	if registerCalled {
		t.Fatalf("register should not be called when persisted auth token exists")
	}
	if heartbeat.AgentAuthToken != "persisted-agent-auth-token" {
		t.Fatalf("heartbeat agent auth token = %q", heartbeat.AgentAuthToken)
	}
}

func TestRunAgentInstallCheckFlowPropagatesRegistrationFailure(t *testing.T) {
	requests := 0
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/api/v1/agents/register":
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/v1/agents/heartbeat":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer controlPlane.Close()

	cfg := agent.Config{
		ControlPlaneURL:   controlPlane.URL,
		ClusterID:         "dev-us",
		AgentID:           "agent-install-check",
		AgentVersion:      "test",
		HeartbeatInterval: 30 * time.Second,
		ReportTimeout:     5 * time.Second,
	}
	source := fakeCapabilitySource{
		capabilities: agent.ClusterCapabilities{
			KubernetesVersion: "v1.30.1",
			Capabilities:      []string{"core-v1", "apps-v1", "flux-helm-v2"},
		},
	}
	reporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)

	_, err := runAgentInstallCheckFlow(context.Background(), cfg, source, reporter)
	if err == nil {
		t.Fatal("expected registration failure error")
	}
	if requests != 1 {
		t.Fatalf("expected 1 registration request, got %d", requests)
	}
}

func TestRunAgentResourceScanTickUsesAgentAuthTokenWithoutRegistrationToken(t *testing.T) {
	var nextAuth string
	var scanAuth string
	var scanPayload domain.AgentResourceScanRequest
	var rawScanBody []byte
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/resource-scan/next":
			nextAuth = r.Header.Get("Authorization")
			if r.URL.Query().Get("registrationToken") != "" {
				t.Fatalf("scan next must not send registrationToken: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("agentAuthToken") != "" {
				t.Fatalf("scan next must not send agentAuthToken query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(domain.AgentResourceScanTaskResponse{
				ProjectID:  "bootstrap-project",
				ClusterID:  "dev-us",
				AgentID:    "agent-scan",
				Namespaces: []string{"dev-base"},
				ObservedAt: time.Now().UTC(),
			})
		case "/api/v1/agents/resource-scan":
			scanAuth = r.Header.Get("Authorization")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read scan payload: %v", err)
			}
			rawScanBody = body
			if err := json.Unmarshal(body, &scanPayload); err != nil {
				t.Fatalf("decode scan payload: %v", err)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("unexpected control plane path: %s", r.URL.Path)
		}
	}))
	defer controlPlane.Close()

	kubeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/dev-base" {
			_, _ = w.Write([]byte(`{"metadata":{"name":"dev-base","labels":{"env":"dev"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer kubeServer.Close()

	cfg := agent.Config{
		ControlPlaneURL:    controlPlane.URL,
		BootstrapProjectID: "bootstrap-project",
		ClusterID:          "dev-us",
		AgentID:            "agent-scan",
		AgentAuthToken:     "agent-auth-token",
		RegistrationToken:  "",
		HeartbeatInterval:  30 * time.Second,
		ReportTimeout:      5 * time.Second,
		FluxNamespace:      "flux-system",
		NamespaceSelector:  "",
		AgentNamespace:     "envpilot",
		KubernetesAPIURL:   kubeServer.URL,
	}
	reporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "control-plane-token", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)
	source := agent.NewKubernetesNamespaceSource(kubeServer.URL, "kube-token", "", nil, kubeServer.Client())
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	if err := runAgentResourceScanTick(context.Background(), cfg, reporter, source, logger); err != nil {
		t.Fatalf("resource scan tick: %v", err)
	}
	if nextAuth != "Bearer agent-auth-token" {
		t.Fatalf("next auth = %q", nextAuth)
	}
	if scanAuth != "Bearer agent-auth-token" {
		t.Fatalf("scan auth = %q", scanAuth)
	}
	if bytes.Contains(rawScanBody, []byte("agentAuthToken")) || bytes.Contains(rawScanBody, []byte("agent_auth_token")) {
		t.Fatalf("scan payload must not include agent auth token when bearer auth is used: %s", string(rawScanBody))
	}
	if scanPayload.ProjectID != "bootstrap-project" || scanPayload.ClusterID != "dev-us" || scanPayload.AgentID != "agent-scan" {
		t.Fatalf("scan payload identity = %#v", scanPayload)
	}
}

func TestRunAgentResourceScanTickSkipsWithoutAgentAuthToken(t *testing.T) {
	var requests int
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer controlPlane.Close()
	kubeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer kubeServer.Close()

	cfg := agent.Config{
		ControlPlaneURL:    controlPlane.URL,
		BootstrapProjectID: "bootstrap-project",
		ClusterID:          "dev-us",
		AgentID:            "agent-scan",
		AgentAuthToken:     "",
		RegistrationToken:  "consumed-registration-token",
		ReportTimeout:      5 * time.Second,
	}
	reporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "control-plane-token", cfg.ClusterID, cfg.AgentID, cfg.ReportTimeout)
	source := agent.NewKubernetesNamespaceSource(kubeServer.URL, "kube-token", "", nil, kubeServer.Client())
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	if err := runAgentResourceScanTick(context.Background(), cfg, reporter, source, logger); err != nil {
		t.Fatalf("resource scan tick: %v", err)
	}
	if requests != 0 {
		t.Fatalf("scan without agent auth token should not call control plane or kube API, got %d requests", requests)
	}
	if !strings.Contains(logs.String(), "agent auth token is required") {
		t.Fatalf("expected clear missing token log, got %s", logs.String())
	}
}
