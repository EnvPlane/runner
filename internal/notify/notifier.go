package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"envpilot/internal/domain"
	"envpilot/internal/secrets"
)

type SettingsProvider interface {
	GetSettings() (domain.ControlPlaneSettings, error)
}

type Notifier struct {
	settings SettingsProvider
	resolver *secrets.Resolver
	client   *http.Client
}

func New(settings SettingsProvider, resolver *secrets.Resolver) *Notifier {
	if resolver == nil {
		resolver = secrets.NewResolver()
	}
	return &Notifier{
		settings: settings,
		resolver: resolver,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *Notifier) NotifyEnvironment(ctx context.Context, environment domain.Environment) error {
	if n == nil || n.settings == nil {
		return nil
	}
	settings, err := n.settings.GetSettings()
	if err != nil {
		return err
	}
	for _, target := range settings.Notifications {
		if !target.Enabled || strings.TrimSpace(target.SecretRef) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(target.Provider)) {
		case "slack":
			if err := n.notifySlack(ctx, target, settings.SecretRefs, environment); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *Notifier) notifySlack(ctx context.Context, target domain.NotificationTarget, refs []domain.SecretReference, environment domain.Environment) error {
	secret, ok := findSecret(target.SecretRef, refs)
	if !ok {
		return fmt.Errorf("notification secret ref %q not found", target.SecretRef)
	}
	webhookURL, err := n.resolver.Resolve(ctx, secret)
	if err != nil {
		return err
	}
	payload := map[string]string{"text": slackText(environment)}
	content, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(content))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := n.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("slack notification failed: status=%d body=%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func slackText(environment domain.Environment) string {
	status := strings.TrimSpace(string(environment.Status))
	if status == "" {
		status = "unknown"
	}
	parts := []string{"EnvPilot", environment.ID, status}
	if strings.TrimSpace(environment.URL) != "" {
		parts = append(parts, environment.URL)
	}
	return strings.Join(parts, " | ")
}

func findSecret(id string, refs []domain.SecretReference) (domain.SecretReference, bool) {
	id = normalizeID(id)
	for _, ref := range refs {
		if normalizeID(ref.ID) == id {
			return ref, true
		}
	}
	return domain.SecretReference{}, false
}

func normalizeID(value string) string {
	return strings.Trim(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", "-")), "-")
}
