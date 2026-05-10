package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"envpilot/internal/domain"
)

func TestHTTPStatusReporterPostsEnvironmentStatus(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotPayload domain.UpdateEnvironmentStatusRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	reporter := NewHTTPStatusReporterForAgent(server.URL, "agent-token", "dev-us", "agent-1", time.Second)
	err := reporter.ReportNamespaceStatus(context.Background(), NamespaceStatusReport{
		EnvironmentID: "kan-402",
		Namespace:     "envpilot-pr-kan-402",
		Status:        domain.StatusReady,
		Message:       "namespace ready",
	})
	if err != nil {
		t.Fatalf("report status: %v", err)
	}
	if gotPath != "/api/v1/environments/kan-402/status" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer agent-token" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if gotPayload.Status != domain.StatusReady || gotPayload.Message != "namespace ready" {
		t.Fatalf("payload = %#v", gotPayload)
	}
	if gotPayload.ClusterID != "dev-us" {
		t.Fatalf("cluster id = %q", gotPayload.ClusterID)
	}
}

func TestKubernetesAgentReadsNamespaceStatusAndSendsToAPI(t *testing.T) {
	kubeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces" {
			t.Fatalf("unexpected kubernetes path %s", r.URL.Path)
		}
		if r.URL.Query().Get("labelSelector") != "app.kubernetes.io/managed-by=envpilot" {
			t.Fatalf("label selector = %q", r.URL.Query().Get("labelSelector"))
		}
		_ = json.NewEncoder(w).Encode(namespaceList{
			Items: []Namespace{
				{
					Metadata: NamespaceMetadata{
						Name:   "envpilot-pr-kan-402",
						Labels: map[string]string{environmentIDLabel: "kan-402"},
					},
					Status: NamespaceStatus{Phase: "Active"},
				},
			},
		})
	}))
	defer kubeServer.Close()

	var gotPath string
	var gotAuth string
	var gotPayload domain.UpdateEnvironmentStatusRequest
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer apiServer.Close()

	source := NewKubernetesNamespaceSource(kubeServer.URL, "kube-token", "app.kubernetes.io/managed-by=envpilot", nil, kubeServer.Client())
	reporter := NewHTTPStatusReporterForAgent(apiServer.URL, "agent-token", "dev-us", "agent-1", time.Second)
	watcher := NewNamespaceWatcherWithCollectors(source, reporter, nil, nil, nil, time.Second, nil)
	if err := watcher.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync once: %v", err)
	}

	if gotPath != "/api/v1/environments/kan-402/status" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer agent-token" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if gotPayload.Status != domain.StatusReady || gotPayload.Message == "" {
		t.Fatalf("payload = %#v", gotPayload)
	}
	if gotPayload.ClusterID != "dev-us" {
		t.Fatalf("cluster id = %q", gotPayload.ClusterID)
	}
}

func TestHTTPStatusReporterPostsKubernetesEvents(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotPayload domain.IngestEnvironmentEventsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	reporter := NewHTTPStatusReporterForAgent(server.URL, "agent-token", "dev-us", "agent-1", time.Second)
	err := reporter.ReportEvents(context.Background(), "kan-404", []domain.KubernetesEvent{
		{
			Namespace:    "envpilot-pr-kan-404",
			Type:         "Warning",
			Reason:       "FailedScheduling",
			Message:      "0/3 nodes are available",
			InvolvedKind: "Pod",
			InvolvedName: "cms-api-abc",
		},
	})
	if err != nil {
		t.Fatalf("report events: %v", err)
	}
	if gotPath != "/api/v1/environments/kan-404/events" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer agent-token" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if len(gotPayload.Events) != 1 || gotPayload.Events[0].Reason != "FailedScheduling" {
		t.Fatalf("payload = %#v", gotPayload)
	}
	if gotPayload.ClusterID != "dev-us" {
		t.Fatalf("cluster id = %q", gotPayload.ClusterID)
	}
}

func TestHTTPStatusReporterPostsFluxStatus(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotPayload domain.IngestFluxStatusRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	reporter := NewHTTPStatusReporterForAgent(server.URL, "agent-token", "dev-us", "agent-1", time.Second)
	err := reporter.ReportFluxStatus(context.Background(), "kan-405", domain.FluxStatus{
		Status:  domain.StatusReady,
		Message: "flux ready",
		Kustomizations: []domain.FluxResourceStatus{
			{Kind: "Kustomization", Name: "kan-405.bethunder", Ready: true},
		},
	})
	if err != nil {
		t.Fatalf("report flux status: %v", err)
	}
	if gotPath != "/api/v1/environments/kan-405/flux-status" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer agent-token" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if gotPayload.FluxStatus.Status != domain.StatusReady {
		t.Fatalf("payload = %#v", gotPayload)
	}
	if gotPayload.ClusterID != "dev-us" {
		t.Fatalf("cluster id = %q", gotPayload.ClusterID)
	}
}

func TestHTTPStatusReporterRegistersAgentAndSendsHeartbeat(t *testing.T) {
	var paths []string
	var register domain.AgentRegistrationRequest
	var heartbeat domain.AgentHeartbeatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/agents/register":
			if err := json.NewDecoder(r.Body).Decode(&register); err != nil {
				t.Fatalf("decode register: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"dev-us","name":"dev-us","provider":"kubernetes","agentAuthToken":"agent-auth-token"}`))
			return
		case "/api/v1/agents/heartbeat":
			if err := json.NewDecoder(r.Body).Decode(&heartbeat); err != nil {
				t.Fatalf("decode heartbeat: %v", err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	cfg := Config{
		ControlPlaneURL:   server.URL,
		ClusterID:         "dev-us",
		AgentID:           "agent-1",
		AgentVersion:      "1.2.3",
		AgentNamespace:    "envpilot",
		FluxNamespace:     "flux-system",
		NamespaceSelector: "app.kubernetes.io/managed-by=envpilot",
		HeartbeatInterval: 30 * time.Second,
	}
	capabilities := ClusterCapabilities{KubernetesVersion: "v1.30.1", Capabilities: []string{"apps-v1", "flux-helm-v2"}}
	reporter := NewHTTPStatusReporterForAgent(server.URL, "agent-token", "dev-us", "agent-1", time.Second)
	agentAuthToken, err := reporter.RegisterAgent(context.Background(), cfg, capabilities)
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	cfg.AgentAuthToken = agentAuthToken
	if err := reporter.ReportHeartbeat(context.Background(), cfg, capabilities, "online", nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	if len(paths) != 2 || paths[0] != "/api/v1/agents/register" || paths[1] != "/api/v1/agents/heartbeat" {
		t.Fatalf("paths = %#v", paths)
	}
	if register.ClusterID != "dev-us" || register.AgentID != "agent-1" || register.KubernetesVersion != "v1.30.1" {
		t.Fatalf("register = %#v", register)
	}
	if heartbeat.ClusterID != "dev-us" || heartbeat.Status != "online" {
		t.Fatalf("heartbeat = %#v", heartbeat)
	}
	if heartbeat.AgentAuthToken != "agent-auth-token" {
		t.Fatalf("heartbeat auth token = %#v", heartbeat)
	}
}

func TestHTTPStatusReporterResourceScanUsesAgentAuthTokenAfterRegistration(t *testing.T) {
	var paths []string
	var nextAuth string
	var nextRawQuery string
	var scanAuth string
	var scanPayload domain.AgentResourceScanRequest
	var rawScanBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/agents/register":
			var register domain.AgentRegistrationRequest
			if err := json.NewDecoder(r.Body).Decode(&register); err != nil {
				t.Fatalf("decode register: %v", err)
			}
			if register.RegistrationToken != "bootstrap-registration-token" {
				t.Fatalf("registration token = %q", register.RegistrationToken)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"dev-us","name":"dev-us","provider":"kubernetes","agentAuthToken":"agent-auth-token"}`))
		case "/api/v1/agents/resource-scan/next":
			nextAuth = r.Header.Get("Authorization")
			nextRawQuery = r.URL.RawQuery
			if r.URL.Query().Get("registrationToken") != "" {
				t.Fatalf("resource scan next sent registrationToken query: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("agentAuthToken") != "" {
				t.Fatalf("resource scan next should use Authorization header, got query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(domain.AgentResourceScanTaskResponse{
				ProjectID:  "bootstrap-project",
				ClusterID:  "dev-us",
				AgentID:    "agent-1",
				Namespaces: []string{"dev-base"},
				ObservedAt: time.Now().UTC(),
			})
		case "/api/v1/agents/resource-scan":
			scanAuth = r.Header.Get("Authorization")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read scan: %v", err)
			}
			rawScanBody = body
			if err := json.Unmarshal(body, &scanPayload); err != nil {
				t.Fatalf("decode scan: %v", err)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := Config{
		ControlPlaneURL:    server.URL,
		BootstrapProjectID: "bootstrap-project",
		ClusterID:          "dev-us",
		AgentID:            "agent-1",
		RegistrationToken:  "bootstrap-registration-token",
		HeartbeatInterval:  30 * time.Second,
	}
	reporter := NewHTTPStatusReporterForAgent(server.URL, "control-plane-api-token", "dev-us", "agent-1", time.Second)
	agentAuthToken, err := reporter.RegisterAgent(context.Background(), cfg, ClusterCapabilities{})
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	cfg.AgentAuthToken = agentAuthToken
	cfg.RegistrationToken = ""

	task, err := reporter.FetchResourceScanTask(context.Background(), cfg)
	if err != nil {
		t.Fatalf("fetch resource scan task: %v", err)
	}
	if task == nil || len(task.Namespaces) != 1 || task.Namespaces[0] != "dev-base" {
		t.Fatalf("task = %#v", task)
	}
	err = reporter.ReportResourceScan(context.Background(), cfg, ResourceScanResult{
		Snapshots: []domain.ResourceSnapshot{{Kind: "Deployment", Namespace: "dev-base", Name: "orders"}},
	})
	if err != nil {
		t.Fatalf("report resource scan: %v", err)
	}

	if len(paths) != 3 ||
		paths[0] != "/api/v1/agents/register" ||
		paths[1] != "/api/v1/agents/resource-scan/next" ||
		paths[2] != "/api/v1/agents/resource-scan" {
		t.Fatalf("paths = %#v", paths)
	}
	if nextAuth != "Bearer agent-auth-token" {
		t.Fatalf("next authorization = %q rawQuery=%q", nextAuth, nextRawQuery)
	}
	if scanAuth != "Bearer agent-auth-token" {
		t.Fatalf("scan authorization = %q", scanAuth)
	}
	if bytes.Contains(rawScanBody, []byte("agentAuthToken")) || bytes.Contains(rawScanBody, []byte("agent_auth_token")) {
		t.Fatalf("scan payload should not include agent auth token when Authorization is used: %s", string(rawScanBody))
	}
	if scanPayload.ProjectID != "bootstrap-project" || scanPayload.ClusterID != "dev-us" || scanPayload.AgentID != "agent-1" {
		t.Fatalf("scan payload identity = %#v", scanPayload)
	}
}
