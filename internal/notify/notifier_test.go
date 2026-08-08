package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/envpilot/runner/internal/domain"
	"github.com/envpilot/runner/internal/secrets"
)

type testSettingsProvider struct {
	settings domain.ControlPlaneSettings
}

func (p testSettingsProvider) GetSettings() (domain.ControlPlaneSettings, error) {
	return p.settings, nil
}

func TestSlackNotifierResolvesWebhookFromSecretRef(t *testing.T) {
	var payload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("SLACK_WEBHOOK_URL", server.URL)

	notifier := New(testSettingsProvider{settings: domain.ControlPlaneSettings{
		SecretRefs: []domain.SecretReference{
			{ID: "slack-webhook", Provider: "env", Scope: "slack", Reference: "SLACK_WEBHOOK_URL"},
		},
		Notifications: []domain.NotificationTarget{
			{ID: "preview-slack", Provider: "slack", SecretRef: "slack-webhook", Enabled: true},
		},
	}}, secrets.NewResolver())

	err := notifier.NotifyEnvironment(context.Background(), domain.Environment{
		ID:     "pr-123",
		Status: domain.StatusReady,
		URL:    "https://pr-123.preview.example.com",
	})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if !strings.Contains(payload["text"], "pr-123") || !strings.Contains(payload["text"], "ready") {
		t.Fatalf("payload = %#v", payload)
	}
}
