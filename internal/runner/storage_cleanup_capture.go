package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/envplane/contracts/domain"
)

const runnerStorageCaptureTimeout = 5 * time.Second
const runnerStorageCaptureResponseLimit = 4096

type runnerStorageCaptureRequest struct {
	ProjectID     string `json:"projectId"`
	ClusterID     string `json:"clusterId"`
	RunnerID      string `json:"runnerId"`
	CommandID     string `json:"commandId"`
	EnvironmentID string `json:"environmentId"`
}

// Called only after the existing deletion authorization/namespace checks, and
// before backend removal. Capture is advisory: it never alters cleanup success,
// authenticates using the runtime Bearer only, and cannot produce storage proof.
func attemptRunnerStorageCleanupCapture(ctx context.Context, cfg runnerConfig, command domain.RunnerCommand) {
	if reason := captureRunnerStorageCleanup(ctx, cfg, command); reason != "" {
		// No URLs, identities, paths, tokens, response bodies or raw errors.
		slog.Warn("runner storage cleanup capture unavailable", "reason", reason)
	}
}

func captureRunnerStorageCleanup(ctx context.Context, cfg runnerConfig, command domain.RunnerCommand) string {
	if strings.TrimSpace(cfg.ControlPlaneURL) == "" || strings.TrimSpace(cfg.RunnerAuthToken) == "" {
		return "capture_missing_runtime_configuration"
	}
	if ctx.Err() != nil {
		return "capture_context_canceled"
	}
	client, err := newRunnerControlPlaneHTTPClientWithTLS(runnerStorageCaptureTimeout, cfg.ControlPlaneCAFile, cfg.ControlPlaneTLSServerName)
	if err != nil {
		return "capture_tls_configuration_unavailable"
	}
	defer client.CloseIdleConnections()
	return captureRunnerStorageCleanupWithClient(ctx, cfg, command, client)
}

func captureRunnerStorageCleanupWithClient(ctx context.Context, cfg runnerConfig, command domain.RunnerCommand, client *http.Client) string {
	if command.Operation != "delete" && command.Operation != "force_cleanup" && command.Operation != "cleanup_flux_namespace" {
		return "capture_operation_not_supported"
	}
	for _, value := range []string{cfg.ProjectID, cfg.ClusterID, cfg.RunnerID, command.ID, command.Environment.ID, cfg.RunnerAuthToken, cfg.ControlPlaneURL} {
		if strings.TrimSpace(value) == "" {
			return "capture_missing_identity"
		}
	}
	if (command.ProjectID != "" && command.ProjectID != cfg.ProjectID) || (command.ClusterID != "" && command.ClusterID != cfg.ClusterID) || (command.RunnerID != "" && command.RunnerID != cfg.RunnerID) || (command.Environment.Project != "" && command.Environment.Project != cfg.ProjectID) {
		return "capture_identity_mismatch"
	}
	if ctx.Err() != nil {
		return "capture_context_canceled"
	}
	if validateRunnerControlPlaneEndpoint(cfg.ControlPlaneURL, cfg.ControlPlaneEndpointMode) != nil {
		return "capture_endpoint_not_allowed"
	}
	base, err := url.Parse(strings.TrimSpace(cfg.ControlPlaneURL))
	if err != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return "capture_endpoint_not_allowed"
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/runner/storage-cleanup/capture"
	base.RawPath = ""
	payload, err := json.Marshal(runnerStorageCaptureRequest{ProjectID: cfg.ProjectID, ClusterID: cfg.ClusterID, RunnerID: cfg.RunnerID, CommandID: command.ID, EnvironmentID: command.Environment.ID})
	if err != nil {
		return "capture_request_invalid"
	}
	captureCtx, cancel := context.WithTimeout(ctx, runnerStorageCaptureTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(captureCtx, http.MethodPost, base.String(), bytes.NewReader(payload))
	if err != nil {
		return "capture_request_invalid"
	}
	req.Header.Set("Authorization", "Bearer "+cfg.RunnerAuthToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(runnerCommandAPIVersionHeader, runnerCommandAPIVersion)
	tenant := command.Environment.TenantID
	if tenant == "" {
		tenant = domain.DefaultTenantID
	}
	req.Header.Set("X-envplane-Tenant", tenant)
	if client == nil {
		return "capture_client_unavailable"
	}
	boundedClient := *client
	boundedClient.Timeout = runnerStorageCaptureTimeout
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := boundedClient.Do(req)
	if err != nil {
		return "capture_request_failed"
	}
	defer func() { _ = resp.Body.Close() }()
	n, readErr := io.CopyN(io.Discard, resp.Body, runnerStorageCaptureResponseLimit+1)
	if n > runnerStorageCaptureResponseLimit {
		return "capture_response_too_large"
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "capture_response_unavailable"
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return "capture_endpoint_unavailable"
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "capture_not_acknowledged"
	}
	// Any bounded 2xx acknowledges metadata capture only. Physical storage proof remains a
	// separate authenticated control-plane/operator verification responsibility.
	return ""
}
