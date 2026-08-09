package comment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/envpilot/contracts/domain"
)

type Commenter interface {
	CommentEnvironment(ctx context.Context, environment domain.Environment) error
}

type Config struct {
	GitHubToken   string
	GitHubAPI     string
	GitLabToken   string
	GitLabAPI     string
	Timeout       time.Duration
	TokenResolver TokenResolver
}

type TokenResolver func(ctx context.Context, provider string, environment domain.Environment) (string, error)

type SCMCommenter struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *SCMCommenter {
	if cfg.GitHubAPI == "" {
		cfg.GitHubAPI = "https://api.github.com"
	}
	if cfg.GitLabAPI == "" {
		cfg.GitLabAPI = "https://gitlab.com/api/v4"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &SCMCommenter{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}
}

func (c *SCMCommenter) CommentEnvironment(ctx context.Context, environment domain.Environment) error {
	if environment.Source.PullRequestID == "" || environment.Source.Repository == "" {
		return nil
	}
	body := BuildEnvironmentComment(environment)
	switch strings.ToLower(strings.TrimSpace(environment.Source.Provider)) {
	case "github":
		token, err := c.token(ctx, "github", environment, c.cfg.GitHubToken)
		if err != nil {
			return err
		}
		if strings.TrimSpace(token) == "" {
			return nil
		}
		return c.commentGitHub(ctx, environment.Source.Repository, environment.Source.PullRequestID, body, token)
	case "gitlab":
		token, err := c.token(ctx, "gitlab", environment, c.cfg.GitLabToken)
		if err != nil {
			return err
		}
		if strings.TrimSpace(token) == "" {
			return nil
		}
		return c.commentGitLab(ctx, environment.Source.Repository, environment.Source.PullRequestID, body, token)
	default:
		return nil
	}
}

func (c *SCMCommenter) token(ctx context.Context, provider string, environment domain.Environment, fallback string) (string, error) {
	if c.cfg.TokenResolver != nil {
		token, err := c.cfg.TokenResolver(ctx, provider, environment)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(token) != "" {
			return token, nil
		}
	}
	return strings.TrimSpace(fallback), nil
}

func BuildEnvironmentComment(environment domain.Environment) string {
	status := strings.TrimSpace(string(environment.Status))
	if environment.FluxStatus != nil && environment.FluxStatus.Status != "" {
		status = string(environment.FluxStatus.Status)
	}
	if status == "" {
		status = "unknown"
	}
	displayStatus := displayStatus(status)
	headlineStatus := strings.ToLower(displayStatus)
	lines := []string{
		"EnvPlane preview environment",
		"",
		"Environment " + headlineStatus + ": " + strings.TrimSpace(environment.URL),
		"Status: " + displayStatus,
	}
	if environment.LastError != "" {
		lines = append(lines, "Error: "+environment.LastError)
	}
	return strings.Join(lines, "\n")
}

func displayStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "Unknown"
	}
	return strings.ToUpper(status[:1]) + strings.ToLower(status[1:])
}

func (c *SCMCommenter) commentGitHub(ctx context.Context, repo string, changeID string, body string, token string) error {
	endpoint := strings.TrimRight(c.cfg.GitHubAPI, "/") + "/repos/" + strings.Trim(repo, "/") + "/issues/" + url.PathEscape(changeID) + "/comments"
	payload := map[string]string{"body": body}
	return c.postJSON(ctx, endpoint, "Authorization", "Bearer "+token, payload)
}

func (c *SCMCommenter) commentGitLab(ctx context.Context, repo string, changeID string, body string, token string) error {
	endpoint := strings.TrimRight(c.cfg.GitLabAPI, "/") + "/projects/" + url.PathEscape(repo) + "/merge_requests/" + url.PathEscape(changeID) + "/notes"
	payload := map[string]string{"body": body}
	return c.postJSON(ctx, endpoint, "PRIVATE-TOKEN", token, payload)
}

func (c *SCMCommenter) postJSON(ctx context.Context, endpoint string, authHeader string, authValue string, payload any) error {
	content, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" && authValue != "" {
		req.Header.Set(authHeader, authValue)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("comment request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}
