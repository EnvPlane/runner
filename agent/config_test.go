package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFromEnvLoadsPersistedAgentAuthTokenFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "agent-auth-token")
	if err := os.WriteFile(tokenPath, []byte("persisted-agent-auth-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Setenv("ENVPILOT_CONTROL_PLANE_URL", "https://envpilot.example")
	t.Setenv("ENVPILOT_CLUSTER_ID", "dev-us")
	t.Setenv("ENVPILOT_AGENT_ID", "agent-1")
	t.Setenv("ENVPILOT_AGENT_AUTH_TOKEN_FILE", tokenPath)
	t.Setenv("ENVPILOT_AGENT_REGISTRATION_TOKEN", "")
	t.Setenv("ENVPILOT_AGENT_AUTH_TOKEN", "")
	t.Setenv("ENVPILOT_KUBERNETES_API_URL", "https://kubernetes.example")

	cfg := ConfigFromEnv()
	if cfg.AgentAuthToken != "persisted-agent-auth-token" {
		t.Fatalf("agent auth token = %q", cfg.AgentAuthToken)
	}
	if cfg.AgentAuthTokenFile != tokenPath {
		t.Fatalf("agent auth token file = %q", cfg.AgentAuthTokenFile)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate config with persisted auth token: %v", err)
	}
}

func TestConfigFromEnvUsesChartCompatiblePersistedAgentAuthTokenPath(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "var", "lib", "envpilot-agent", "auth")
	tokenPath := filepath.Join(authDir, "agent-auth-token")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create chart auth dir: %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte("chart-persisted-agent-auth-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Setenv("ENVPILOT_CONTROL_PLANE_URL", "https://envpilot.example")
	t.Setenv("ENVPILOT_CLUSTER_ID", "dev-us")
	t.Setenv("ENVPILOT_AGENT_ID", "agent-1")
	t.Setenv("ENVPILOT_AGENT_AUTH_TOKEN_FILE", tokenPath)
	t.Setenv("ENVPILOT_AGENT_REGISTRATION_TOKEN", "")
	t.Setenv("ENVPILOT_AGENT_AUTH_TOKEN", "")
	t.Setenv("ENVPILOT_KUBERNETES_API_URL", "https://kubernetes.example")

	cfg := ConfigFromEnv()
	if cfg.AgentAuthToken != "chart-persisted-agent-auth-token" {
		t.Fatalf("agent auth token = %q", cfg.AgentAuthToken)
	}
	if cfg.RegistrationToken != "" {
		t.Fatalf("bootstrap token should not be required when chart-compatible auth token file exists")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate config with chart-compatible persisted auth token: %v", err)
	}
}

func TestPersistAgentAuthTokenWritesCredentialFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "credentials", "agent-auth-token")
	cfg := Config{AgentAuthTokenFile: tokenPath}
	if err := cfg.PersistAgentAuthToken("issued-agent-auth-token"); err != nil {
		t.Fatalf("persist agent auth token: %v", err)
	}
	content, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(content) != "issued-agent-auth-token\n" {
		t.Fatalf("token file content = %q", string(content))
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %o", info.Mode().Perm())
	}
}
