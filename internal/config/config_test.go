package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFromEnvWithRuntimeFileAppliesRuntimeOverrides(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "runtime-config.json")

	content := []byte(`{
		"api_read_token": "file-read-token",
		"api_token_roles": "file-read:reader",
		"webhook_secret": "runtime-webhook-secret",
		"rate_limit_requests": 7,
		"rate_limit_seconds": 30,
		"audit_log_path": "/tmp/envpilot-audit.log"
	}`)
	if err := writeRuntimeConfig(cfgPath, content); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	t.Setenv("ENVPILOT_API_READ_TOKEN", "env-read-token")
	t.Setenv("ENVPILOT_API_WRITE_TOKEN", "env-write-token")
	t.Setenv("ENVPILOT_API_TOKEN_ROLES", "env-reader:reader")
	t.Setenv("ENVPILOT_WEBHOOK_SECRET", "env-webhook-secret")

	cfg, err := FromEnvWithRuntimeFile(cfgPath)
	if err != nil {
		t.Fatalf("load runtime config: %v", err)
	}

	if cfg.APIReadToken != "file-read-token" {
		t.Fatalf("api_read_token = %q", cfg.APIReadToken)
	}
	if cfg.APIWriteToken != "env-write-token" {
		t.Fatalf("api_write_token = %q", cfg.APIWriteToken)
	}
	if cfg.WebhookSecret != "runtime-webhook-secret" {
		t.Fatalf("webhook_secret = %q", cfg.WebhookSecret)
	}
	if cfg.GitHubWebhookSecret != "runtime-webhook-secret" {
		t.Fatalf("github_webhook_secret = %q", cfg.GitHubWebhookSecret)
	}
	if cfg.GitLabWebhookSecret != "runtime-webhook-secret" {
		t.Fatalf("gitlab_webhook_secret = %q", cfg.GitLabWebhookSecret)
	}
	if cfg.RateLimitRequests != 7 {
		t.Fatalf("rate_limit_requests = %d", cfg.RateLimitRequests)
	}
	if cfg.RateLimitWindow != 30*time.Second {
		t.Fatalf("rate_limit_window = %s", cfg.RateLimitWindow)
	}
	if cfg.AuditLogPath != "/tmp/envpilot-audit.log" {
		t.Fatalf("audit_log_path = %q", cfg.AuditLogPath)
	}
	if cfg.APITokenRoles["file-read"] != "reader" {
		t.Fatalf("expected file role mapping, got %#v", cfg.APITokenRoles)
	}
	if cfg.APITokenRoles["file-read-token"] != "reader" {
		t.Fatalf("expected read token role mapping from file, got %#v", cfg.APITokenRoles)
	}
	if _, ok := cfg.APITokenRoles["env-read-token"]; ok {
		t.Fatalf("expected runtime read token override to remove old read token role, got %#v", cfg.APITokenRoles)
	}
	if cfg.APITokenRoles["env-write-token"] != "admin" {
		t.Fatalf("expected env write token role mapping, got %#v", cfg.APITokenRoles)
	}
}

func TestFromEnvWithRuntimeFileErrorsForInvalidPath(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "missing.json")
	if _, err := FromEnvWithRuntimeFile(cfgPath); err == nil {
		t.Fatalf("expected missing config file to fail")
	}
}

func writeRuntimeConfig(path string, content []byte) error {
	return os.WriteFile(path, content, 0o600)
}
