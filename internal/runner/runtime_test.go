package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
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

	"github.com/envplane/contracts/domain"
	"github.com/envplane/runner/internal/orchestrator"
)

type fakeRunnerCommandBackend struct {
	status domain.EnvironmentStatus
}

func runnerCommandWithReleasePlan(command domain.RunnerCommand) domain.RunnerCommand {
	command.ReleasePlanID = "release-plan-test"
	command.ReleasePlanDigest = "sha256:release-plan-test"
	command.ReleasePlanSignature = "signature-test"
	command.ReleasePlanKeyID = "key-test"
	return command
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

func runLocalHTTPTestServer(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp4 for test server: %v", err)
	}
	server := &http.Server{Handler: handler}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(listener)
		close(done)
	}()
	closeServer := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = listener.Close()
		}
		<-done
	}
	return "http://" + listener.Addr().String(), closeServer
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

func TestNextRunnerCommandClassifiesUnissuedRuntimeTokenForRecovery(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"runner auth token is not issued for project \"envplane\""}`)),
			Request:    request,
		}, nil
	})}
	_, _, err := nextRunnerCommand(context.Background(), runnerConfig{
		ControlPlaneURL: "http://runner.test",
		ProjectID:       "envplane",
		ClusterID:       "local",
		RunnerID:        "runner-1",
		RunnerAuthToken: "stale-token",
	}, client)
	if err == nil || !isRunnerAuthTokenNotIssuedError(err) {
		t.Fatalf("nextRunnerCommand error = %v, want runtime-token recovery error", err)
	}
}

func TestRunnerConfigRejectsHostLocalRemoteControlPlaneEndpoint(t *testing.T) {
	cfg := runnerConfig{
		ControlPlaneURL:          "https://host.minikube.internal:18080",
		ControlPlaneEndpointMode: "remote",
		ProjectID:                "checkout",
		ClusterID:                "remote-cluster",
		RunnerID:                 "checkout-runner",
		RunnerNamespace:          "envplane",
		DeploymentMode:           "helm",
		RunnerAuthToken:          "runner-auth-token",
		FeatureEnvWriterMode:     "releaseNamespace",
		HeartbeatInterval:        time.Second,
		ReportTimeout:            time.Second,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "target-pod-reachable") {
		t.Fatalf("remote host-local endpoint error=%v", err)
	}
}

func TestRunnerConfigRequiresStableHTTPSForRemoteControlPlaneEndpoint(t *testing.T) {
	cfg := runnerConfig{
		ControlPlaneURL:          "http://api.remote.example",
		ControlPlaneEndpointMode: "remote",
		ProjectID:                "checkout",
		ClusterID:                "remote-cluster",
		RunnerID:                 "checkout-runner",
		RunnerNamespace:          "envplane",
		DeploymentMode:           "helm",
		RunnerAuthToken:          "runner-auth-token",
		FeatureEnvWriterMode:     "releaseNamespace",
		HeartbeatInterval:        time.Second,
		ReportTimeout:            time.Second,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "stable HTTPS") {
		t.Fatalf("remote HTTP endpoint error=%v", err)
	}
	cfg.ControlPlaneURL = "https://api.remote.example"
	if err := cfg.validate(); err != nil {
		t.Fatalf("stable remote HTTPS endpoint must be valid: %v", err)
	}
}

func TestRunnerControlPlaneHTTPClientTrustsMountedPrivateCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	certificate := server.Certificate()
	if certificate == nil || len(certificate.DNSNames) == 0 {
		t.Fatal("TLS fixture certificate must contain a server name")
	}
	caPath := filepath.Join(t.TempDir(), "management-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatalf("write private CA fixture: %v", err)
	}
	client, err := newRunnerControlPlaneHTTPClientWithTLS(time.Second, caPath, certificate.DNSNames[0])
	if err != nil {
		t.Fatalf("build private-CA runner client: %v", err)
	}
	response, err := client.Get(server.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("private-CA runner health request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("private-CA runner health status=%d", response.StatusCode)
	}
}

func TestRunnerConnectivityCheckWithRetrySucceedsAfterTransientFailure(t *testing.T) {
	attempts := 0
	serverURL, closeServer := runLocalHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer closeServer()

	cfg := runnerConfig{
		ControlPlaneURL:                      serverURL,
		ControlPlaneCAFile:                   "",
		ControlPlaneTLSServerName:            "",
		ControlPlaneConnectivityMaxAttempts:  3,
		ControlPlaneConnectivityInitialDelay: 100 * time.Millisecond,
		ControlPlaneConnectivityMaxDelay:     100 * time.Millisecond,
		ReportTimeout:                        500 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runnerConnectivityCheckWithRetry(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg); err != nil {
		t.Fatalf("runner connectivity retry should recover after transient failure: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("expected retry before success, got attempts=%d", attempts)
	}
}

func TestRunnerConnectivityCheckWithRetryTimesOutWhenControlPlaneUnavailable(t *testing.T) {
	serverURL, closeServer := runLocalHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer closeServer()

	cfg := runnerConfig{
		ControlPlaneURL:                      serverURL,
		ControlPlaneCAFile:                   "",
		ControlPlaneTLSServerName:            "",
		ControlPlaneConnectivityMaxAttempts:  5,
		ControlPlaneConnectivityInitialDelay: 2 * time.Second,
		ControlPlaneConnectivityMaxDelay:     2 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := runnerConnectivityCheckWithRetry(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg); err == nil || !strings.Contains(strings.ToLower(err.Error()), "context deadline") {
		t.Fatalf("expected context deadline failure, got %v", err)
	}
}

func TestNextRunnerCommandRetriesNoContentWithoutCompatibilityResponseHeader(t *testing.T) {
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth"}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	_, _, err := nextRunnerCommand(context.Background(), cfg, client)
	var probe runnerCommandVersionProbeError
	if err == nil || !errors.As(err, &probe) || isRunnerCommandAPIIncompatible(err) {
		t.Fatalf("nextRunnerCommand error = %v, want retryable version probe error", err)
	}
}

func TestNextRunnerCommandRejectsCommandWithoutCompatibilityResponseHeader(t *testing.T) {
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth"}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"unsafe-command"}`)), Request: request}, nil
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
		ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerNamespace: "envplane", DeploymentMode: "helm", RunnerAuthToken: "stale-auth",
	}, client, string(domain.RunnerHeartbeatStatusOnline), "")
	if err == nil || !isRunnerStaleBootstrapIdentityError(err) {
		t.Fatalf("heartbeat error = %v, want stale bootstrap identity", err)
	}
}

func TestRunnerPostJSONClassifiesExpiredBootstrapCredentialForRecovery(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":"runner bootstrap credentials have expired","code":"runner_bootstrap_credential_expired",` +
				`"recovery":"rotate_runner_bootstrap_credentials"}`)),
			Request: request,
		}, nil
	})}
	err := fetchRunnerProjectConfig(context.Background(), runnerConfig{
		ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerNamespace: "envplane", DeploymentMode: "helm", ProjectConfigURL: "http://runner.test/api/v1/projects/checkout/runner-config", ProjectConfigToken: "expired-config-token",
	}, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !isRunnerStaleBootstrapIdentityError(err) {
		t.Fatalf("project config error = %v, want recovery-required expired credential", err)
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
		ControlPlaneURL: "http://runner.test", ProjectID: "fixture", ClusterID: "remote", RunnerID: "fixture-runner", RunnerNamespace: "envplane", DeploymentMode: "helm", RunnerAuthToken: "stale-auth",
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
	t.Setenv("ENVPLANE_RUNNER_AUTH_TOKEN_FILE", authPath)
	t.Setenv("ENVPLANE_RUNNER_REGISTRATION_TOKEN", "rotated-registration-token")
	t.Setenv("ENVPLANE_RUNNER_AUTH_TOKEN", "")

	cfg := runnerConfigFromEnv()
	if cfg.RunnerAuthToken != "" {
		t.Fatalf("rotated bootstrap token must override persisted auth, got %q", cfg.RunnerAuthToken)
	}
	if cfg.RegistrationToken != "rotated-registration-token" {
		t.Fatalf("registration token = %q", cfg.RegistrationToken)
	}
}

func TestRunnerConfigCanonicalAliasesAndLegacyFallback(t *testing.T) {
	for _, name := range []string{"ENVPLANE_PROJECT_ID", "ENVPLANE_PROJECT_ID", "ENVPLANE_CLUSTER_ID", "ENVPLANE_CLUSTER_ID", "ENVPLANE_RUNNER_ID", "ENVPLANE_RUNNER_ID"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
	t.Setenv("ENVPLANE_PROJECT_ID", "legacy-project")
	t.Setenv("ENVPLANE_CLUSTER_ID", "legacy-cluster")
	t.Setenv("ENVPLANE_RUNNER_ID", "legacy-runner")
	legacy := runnerConfigFromEnv()
	if legacy.ProjectID != "legacy-project" || len(legacy.EnvDiagnostics) == 0 {
		t.Fatalf("legacy runner configuration not loaded safely: %#v", legacy)
	}
	t.Setenv("ENVPLANE_PROJECT_ID", "canonical-project")
	if got := runnerConfigFromEnv().ProjectID; got != "canonical-project" {
		t.Fatalf("canonical runner value did not win: %q", got)
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

	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth", DeploymentMode: "helm", RunnerNamespace: "envplane"}
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

func TestRunnerHealthRecoversFromTransientDegradedState(t *testing.T) {
	health := &runnerHealth{}

	health.setDegraded()
	if health.online.Load() || !health.degraded.Load() {
		t.Fatal("expected runner health to be degraded")
	}

	health.set(true)
	if !health.online.Load() || health.degraded.Load() {
		t.Fatal("successful recovery must clear degraded runner health")
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

	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth", DeploymentMode: "helm", RunnerNamespace: "envplane"}
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

func TestPollRunnerCommandsOnceStopsForExpiredBootstrapCredential(t *testing.T) {
	polls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		polls++
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"runner bootstrap credentials have expired","code":"runner_bootstrap_credential_expired","recovery":"rotate_runner_bootstrap_credentials"}`)),
			Request:    request,
		}, nil
	})}
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "dev-us", RunnerID: "checkout-runner", RunnerAuthToken: "expired-auth", DeploymentMode: "helm", RunnerNamespace: "envplane"}
	state := newRunnerRuntimeState()
	health := &runnerHealth{}
	if keepPolling := pollRunnerCommandsOnce(context.Background(), cfg, client, state, health, slog.New(slog.NewTextHandler(io.Discard, nil))); keepPolling {
		t.Fatal("expired bootstrap credential must disable command polling")
	}
	status, reason := state.heartbeat()
	if polls != 1 || !state.stale.Load() || !health.stale.Load() || health.online.Load() || status != string(domain.RunnerHeartbeatStatusDegraded) || reason == "" {
		t.Fatalf("expired credential must leave one stale, non-online runner state: polls=%d status=%q reason=%q stale=%v health=%+v", polls, status, reason, state.stale.Load(), health)
	}
}

func TestPollRunnerCommandsOnceReportsTheClaimedAttemptID(t *testing.T) {
	resultCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/runners/commands/next":
			command := domain.RunnerCommand{
				ID: "delete-42", ProjectID: "checkout", ClusterID: "target", RunnerID: "checkout-runner", Operation: "unsupported", AttemptID: "delete-42-attempt-2", Status: "claimed", RemoteClusterGeneration: 7, RunnerIdentityIssuedAt: "2026-08-03T12:00:00Z",
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
			if result.RemoteClusterGeneration != 7 || result.RunnerIdentityIssuedAt != "2026-08-03T12:00:00Z" {
				t.Fatalf("result lost managed target binding: %#v", result)
			}
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
			return nil, nil
		}
	})}
	cfg := runnerConfig{ControlPlaneURL: "http://runner.test", ProjectID: "checkout", ClusterID: "target", RunnerID: "checkout-runner", RunnerAuthToken: "runner-auth", DeploymentMode: "helm", RunnerNamespace: "envplane"}
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
	err := validateRunnerHelmChartWithCommand(context.Background(), "oci://ghcr.io/envplane/envplane-e2e-workload", "0.1.0-main.38", func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("apiVersion: v2\nname: envplane-e2e-workload\n"), nil
	})
	if err != nil {
		t.Fatalf("validate chart: %v", err)
	}
	if strings.Join(got, " ") != "show chart oci://ghcr.io/envplane/envplane-e2e-workload --version 0.1.0-main.38" {
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
	result := executeRunnerCommandWithBackend(context.Background(), runnerCommandWithReleasePlan(domain.RunnerCommand{
		ID: "status-target-cluster", Operation: "status",
		Environment: domain.Environment{ID: "feature-42", Project: "checkout", Namespace: "envplane-pr-42"},
	}), fakeRunnerCommandBackend{status: domain.StatusReady})
	if result.Status != "succeeded" || result.EnvironmentStatus != string(domain.StatusReady) {
		t.Fatalf("status result = %#v", result)
	}
}

func TestRunnerRejectsHelmCommandOutsideChartManagedNamespaceRBAC(t *testing.T) {
	cfg := runnerConfig{
		RunnerNamespace:            "envplane-system",
		FeatureEnvWriterMode:       "generatedFeatureNamespaces",
		FeatureEnvWriterNamespaces: []string{"envplane-e2e-feature"},
	}
	result := executeRunnerCommandWithNamespaceGuard(context.Background(), runnerCommandWithReleasePlan(domain.RunnerCommand{
		ID: "forbidden-target", Operation: "create",
		Environment: domain.Environment{ID: "feature-201", Project: "checkout", Namespace: "envplane-pr-201"},
	}), fakeRunnerCommandBackend{}, cfg.canRunHelmInNamespace)
	if result.ErrorCode != "runner_namespace_access_denied" || !strings.Contains(result.Error, "envplane-pr-201") {
		t.Fatalf("forbidden target result = %#v", result)
	}
}

func TestRunnerAllowsHelmLifecycleOnlyInConfiguredTargetNamespace(t *testing.T) {
	cfg := runnerConfig{
		FeatureEnvWriterMode:       "preconfiguredNamespaces",
		FeatureEnvWriterNamespaces: []string{"envplane-pr-201"},
	}
	for _, operation := range []string{"create", "recreate", "status", "delete", "force_cleanup"} {
		t.Run(operation, func(t *testing.T) {
			result := executeRunnerCommandWithNamespaceGuard(context.Background(), runnerCommandWithReleasePlan(domain.RunnerCommand{
				ID: "allowed-" + operation, Operation: operation,
				Environment: domain.Environment{ID: "feature-201", Project: "checkout", Namespace: "envplane-pr-201"},
			}), fakeRunnerCommandBackend{status: domain.StatusReady}, cfg.canRunHelmInNamespace)
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
