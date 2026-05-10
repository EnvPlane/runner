package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"envpilot/internal/domain"
)

type StatusReporter interface {
	ReportNamespaceStatus(ctx context.Context, report NamespaceStatusReport) error
	ReportEvents(ctx context.Context, environmentID string, events []domain.KubernetesEvent) error
	ReportFluxStatus(ctx context.Context, environmentID string, status domain.FluxStatus) error
}

type NamespaceStatusReport struct {
	EnvironmentID string
	Namespace     string
	Status        domain.EnvironmentStatus
	Message       string
	EventType     string
	Phase         string
}

type HTTPStatusReporter struct {
	baseURL   string
	token     string
	clusterID string
	agentID   string
	client    *http.Client
}

func NewHTTPStatusReporter(baseURL, token string, timeout time.Duration) *HTTPStatusReporter {
	return NewHTTPStatusReporterForAgent(baseURL, token, "", "", timeout)
}

func NewHTTPStatusReporterForAgent(baseURL, token, clusterID, agentID string, timeout time.Duration) *HTTPStatusReporter {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPStatusReporter{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     strings.TrimSpace(token),
		clusterID: strings.TrimSpace(clusterID),
		agentID:   strings.TrimSpace(agentID),
		client:    &http.Client{Timeout: timeout},
	}
}

func (r *HTTPStatusReporter) ReportNamespaceStatus(ctx context.Context, report NamespaceStatusReport) error {
	if strings.TrimSpace(report.EnvironmentID) == "" {
		return fmt.Errorf("environment id is required")
	}
	payload := domain.UpdateEnvironmentStatusRequest{
		Status:    report.Status,
		Message:   report.Message,
		ClusterID: r.clusterID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := r.baseURL + "/api/v1/environments/" + url.PathEscape(report.EnvironmentID) + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("report namespace status failed: environment=%s status=%d body=%s", report.EnvironmentID, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (r *HTTPStatusReporter) ReportEvents(ctx context.Context, environmentID string, events []domain.KubernetesEvent) error {
	if strings.TrimSpace(environmentID) == "" {
		return fmt.Errorf("environment id is required")
	}
	payload := domain.IngestEnvironmentEventsRequest{ClusterID: r.clusterID, Events: events}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := r.baseURL + "/api/v1/environments/" + url.PathEscape(environmentID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("report kubernetes events failed: environment=%s status=%d body=%s", environmentID, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (r *HTTPStatusReporter) ReportFluxStatus(ctx context.Context, environmentID string, status domain.FluxStatus) error {
	if strings.TrimSpace(environmentID) == "" {
		return fmt.Errorf("environment id is required")
	}
	payload := domain.IngestFluxStatusRequest{ClusterID: r.clusterID, FluxStatus: status}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := r.baseURL + "/api/v1/environments/" + url.PathEscape(environmentID) + "/flux-status"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("report flux status failed: environment=%s status=%d body=%s", environmentID, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (r *HTTPStatusReporter) RegisterAgent(ctx context.Context, cfg Config, capabilities ClusterCapabilities) (string, error) {
	payload := domain.AgentRegistrationRequest{
		ProjectID:                cfg.BootstrapProjectID,
		ClusterID:                cfg.ClusterID,
		AgentID:                  cfg.AgentID,
		RegistrationToken:        cfg.RegistrationToken,
		AgentVersion:             cfg.AgentVersion,
		AgentNamespace:           cfg.AgentNamespace,
		KubernetesVersion:        capabilities.KubernetesVersion,
		FluxNamespace:            cfg.FluxNamespace,
		NamespaceSelector:        cfg.NamespaceSelector,
		Capabilities:             capabilities.Capabilities,
		CapabilityReport:         &capabilities.Report,
		HeartbeatIntervalSeconds: int(cfg.HeartbeatInterval.Seconds()),
		ObservedAt:               time.Now().UTC(),
	}
	var response domain.AgentRegistrationResponse
	if err := r.postJSONDecode(ctx, "/api/v1/agents/register", payload, "register agent", &response); err != nil {
		return "", err
	}
	return strings.TrimSpace(response.AgentAuthToken), nil
}

func (r *HTTPStatusReporter) ReportHeartbeat(ctx context.Context, cfg Config, capabilities ClusterCapabilities, status string, statusErr error) error {
	errorMessage := ""
	if statusErr != nil {
		errorMessage = statusErr.Error()
	}
	payload := domain.AgentHeartbeatRequest{
		ProjectID:         cfg.BootstrapProjectID,
		ClusterID:         cfg.ClusterID,
		AgentID:           cfg.AgentID,
		AgentAuthToken:    cfg.AgentAuthToken,
		AgentVersion:      cfg.AgentVersion,
		KubernetesVersion: capabilities.KubernetesVersion,
		Capabilities:      capabilities.Capabilities,
		Status:            status,
		Error:             errorMessage,
		ObservedAt:        time.Now().UTC(),
	}
	return r.postJSON(ctx, "/api/v1/agents/heartbeat", payload, "report heartbeat")
}

func (r *HTTPStatusReporter) ReportResourceScan(ctx context.Context, cfg Config, result ResourceScanResult) error {
	payload := domain.AgentResourceScanRequest{
		ProjectID:          cfg.BootstrapProjectID,
		ClusterID:          cfg.ClusterID,
		AgentID:            cfg.AgentID,
		ResourceSnapshots:  result.Snapshots,
		ServiceGraph:       result.ServiceGraph,
		ServiceEnvs:        result.ServiceEnvs,
		PermissionWarnings: result.PermissionWarnings,
		ObservedAt:         time.Now().UTC(),
	}
	return r.postJSONWithBearer(ctx, "/api/v1/agents/resource-scan", payload, cfg.AgentAuthToken, "report resource scan")
}

func (r *HTTPStatusReporter) FetchResourceScanTask(ctx context.Context, cfg Config) (*domain.AgentResourceScanTaskResponse, error) {
	query := url.Values{}
	query.Set("projectId", cfg.BootstrapProjectID)
	query.Set("clusterId", cfg.ClusterID)
	query.Set("agentId", cfg.AgentID)
	endpoint := r.baseURL + "/api/v1/agents/resource-scan/next?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(cfg.AgentAuthToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("fetch resource scan task failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var task domain.AgentResourceScanTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (r *HTTPStatusReporter) postJSON(ctx context.Context, path string, payload any, operation string) error {
	return r.postJSONDecode(ctx, path, payload, operation, nil)
}

func (r *HTTPStatusReporter) postJSONWithBearer(ctx context.Context, path string, payload any, bearerToken string, operation string) error {
	return r.postJSONDecodeWithBearer(ctx, path, payload, operation, nil, bearerToken)
}

func (r *HTTPStatusReporter) postJSONDecode(ctx context.Context, path string, payload any, operation string, output any) error {
	return r.postJSONDecodeWithBearer(ctx, path, payload, operation, output, r.token)
}

func (r *HTTPStatusReporter) postJSONDecodeWithBearer(ctx context.Context, path string, payload any, operation string, output any, bearerToken string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(bearerToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s failed: status=%d body=%s", operation, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("%s response decode failed: %w", operation, err)
		}
	}
	return nil
}
