package ttl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/envpilot/runner/internal/domain"
)

const defaultCleanupTimeout = 30 * time.Second

type CleanupConfig struct {
	ControlPlaneURL string
	Token           string
	Timeout         time.Duration
}

type CleanupResult struct {
	Deleted []domain.Environment `json:"deleted"`
}

func ConfigFromEnv() CleanupConfig {
	timeout := defaultCleanupTimeout
	if raw := strings.TrimSpace(os.Getenv("ENVPILOT_TTL_CLEANUP_TIMEOUT_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}

	token := strings.TrimSpace(os.Getenv("ENVPILOT_TTL_CLEANUP_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("ENVPILOT_AGENT_TOKEN"))
	}

	return CleanupConfig{
		ControlPlaneURL: strings.TrimSpace(os.Getenv("ENVPILOT_CONTROL_PLANE_URL")),
		Token:           token,
		Timeout:         timeout,
	}
}

func RunCleanup(ctx context.Context, cfg CleanupConfig) (CleanupResult, error) {
	if strings.TrimSpace(cfg.ControlPlaneURL) == "" {
		return CleanupResult{}, fmt.Errorf("ENVPILOT_CONTROL_PLANE_URL is required")
	}

	endpoint := strings.TrimRight(cfg.ControlPlaneURL, "/") + "/api/v1/environments/reconcile"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("build cleanup request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	client := &http.Client{Timeout: cfg.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("call cleanup endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return CleanupResult{}, fmt.Errorf("cleanup endpoint returned %s", resp.Status)
	}

	var result CleanupResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CleanupResult{}, fmt.Errorf("decode cleanup response: %w", err)
	}
	return result, nil
}
