package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultServiceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultServiceAccountCA    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultNamespaceSelector   = "app.kubernetes.io/managed-by=envpilot"
)

type Config struct {
	ControlPlaneURL    string
	RegistrationToken  string
	AgentAuthToken     string
	AgentAuthTokenFile string
	BootstrapProjectID string
	ClusterID          string
	AgentID            string
	AgentNamespace     string
	AgentVersion       string
	KubernetesAPIURL   string
	KubernetesToken    string
	KubernetesCA       string
	NamespaceSelector  string
	Namespaces         []string
	FluxNamespace      string
	ResyncInterval     time.Duration
	ReportTimeout      time.Duration
	HeartbeatInterval  time.Duration
}

func ConfigFromEnv() Config {
	agentAuthTokenFile := getenv("ENVPILOT_AGENT_AUTH_TOKEN_FILE", "")
	agentAuthToken := getenv("ENVPILOT_AGENT_AUTH_TOKEN", "")
	if strings.TrimSpace(agentAuthToken) == "" {
		agentAuthToken = readTokenFile(agentAuthTokenFile)
	}
	return Config{
		ControlPlaneURL:    getenv("ENVPILOT_CONTROL_PLANE_URL", ""),
		RegistrationToken:  getenv("ENVPILOT_AGENT_REGISTRATION_TOKEN", ""),
		AgentAuthToken:     agentAuthToken,
		AgentAuthTokenFile: agentAuthTokenFile,
		BootstrapProjectID: getenv("ENVPILOT_BOOTSTRAP_PROJECT_ID", ""),
		ClusterID:          getenv("ENVPILOT_CLUSTER_ID", "default"),
		AgentID:            getenv("ENVPILOT_AGENT_ID", hostname()),
		AgentNamespace:     getenv("ENVPILOT_AGENT_NAMESPACE", ""),
		AgentVersion:       getenv("ENVPILOT_AGENT_VERSION", "dev"),
		KubernetesAPIURL:   getenv("ENVPILOT_KUBERNETES_API_URL", inClusterAPIURL()),
		KubernetesToken:    getenv("ENVPILOT_KUBERNETES_TOKEN_PATH", defaultServiceAccountToken),
		KubernetesCA:       getenv("ENVPILOT_KUBERNETES_CA_PATH", defaultServiceAccountCA),
		NamespaceSelector:  getenv("ENVPILOT_WATCH_NAMESPACE_SELECTOR", defaultNamespaceSelector),
		Namespaces:         splitCSV(getenv("ENVPILOT_WATCH_NAMESPACES", "")),
		FluxNamespace:      getenv("ENVPILOT_FLUX_NAMESPACE", "flux-system"),
		ResyncInterval:     time.Duration(getenvInt("ENVPILOT_AGENT_RESYNC_SECONDS", 30)) * time.Second,
		ReportTimeout:      time.Duration(getenvInt("ENVPILOT_AGENT_REPORT_TIMEOUT_SECONDS", 10)) * time.Second,
		HeartbeatInterval:  time.Duration(getenvInt("ENVPILOT_AGENT_HEARTBEAT_SECONDS", 30)) * time.Second,
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ControlPlaneURL) == "" {
		return fmt.Errorf("ENVPILOT_CONTROL_PLANE_URL is required")
	}
	if strings.TrimSpace(c.ClusterID) == "" {
		return fmt.Errorf("ENVPILOT_CLUSTER_ID is required")
	}
	if strings.TrimSpace(c.AgentID) == "" {
		return fmt.Errorf("ENVPILOT_AGENT_ID is required")
	}
	if strings.TrimSpace(c.RegistrationToken) == "" && strings.TrimSpace(c.AgentAuthToken) == "" {
		return fmt.Errorf("set ENVPILOT_AGENT_REGISTRATION_TOKEN or ENVPILOT_AGENT_AUTH_TOKEN")
	}
	if strings.TrimSpace(c.KubernetesAPIURL) == "" {
		return fmt.Errorf("Kubernetes API URL is required; set ENVPILOT_KUBERNETES_API_URL outside the cluster")
	}
	if c.ResyncInterval <= 0 {
		return fmt.Errorf("resync interval must be positive")
	}
	if c.ReportTimeout <= 0 {
		return fmt.Errorf("report timeout must be positive")
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("heartbeat interval must be positive")
	}
	return nil
}

func (c Config) PersistAgentAuthToken(token string) error {
	token = strings.TrimSpace(token)
	path := strings.TrimSpace(c.AgentAuthTokenFile)
	if token == "" || path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

func readTokenFile(path string) string {
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

func inClusterAPIURL() string {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if host == "" {
		return ""
	}
	if port == "" {
		port = "443"
	}
	return "https://" + host + ":" + port
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

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func hostname() string {
	value, err := os.Hostname()
	if err != nil {
		return "envpilot-agent"
	}
	return value
}
