package gitops

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
)

type PullRequestRequest struct {
	Provider string
	APIBase  string
	Token    string
	RepoURL  string
	Title    string
	Body     string
	Head     string
	Base     string
}

type PullRequestResult struct {
	URL    string `json:"url,omitempty"`
	Number string `json:"number,omitempty"`
}

type PullRequestService struct {
	client *http.Client
}

func NewPullRequestService() PullRequestService {
	return PullRequestService{client: &http.Client{Timeout: 15 * time.Second}}
}

func (s PullRequestService) Create(ctx context.Context, request PullRequestRequest) (PullRequestResult, error) {
	if s.client == nil {
		s.client = &http.Client{Timeout: 15 * time.Second}
	}
	provider := strings.ToLower(strings.TrimSpace(request.Provider))
	switch provider {
	case "github":
		return s.createGitHub(ctx, request)
	case "gitlab":
		return s.createGitLab(ctx, request)
	default:
		return PullRequestResult{}, fmt.Errorf("gitops pull request provider %q is not supported", request.Provider)
	}
}

func (s PullRequestService) createGitHub(ctx context.Context, request PullRequestRequest) (PullRequestResult, error) {
	repo, err := RepositoryPath(request.RepoURL)
	if err != nil {
		return PullRequestResult{}, err
	}
	api := strings.TrimRight(defaultString(request.APIBase, "https://api.github.com"), "/")
	payload := map[string]string{
		"title": request.Title,
		"head":  request.Head,
		"base":  request.Base,
		"body":  request.Body,
	}
	var response struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := s.post(ctx, api+"/repos/"+repo+"/pulls", "Authorization", "Bearer "+request.Token, payload, &response); err != nil {
		return PullRequestResult{}, err
	}
	return PullRequestResult{URL: response.HTMLURL, Number: fmt.Sprintf("%d", response.Number)}, nil
}

func (s PullRequestService) createGitLab(ctx context.Context, request PullRequestRequest) (PullRequestResult, error) {
	repo, err := RepositoryPath(request.RepoURL)
	if err != nil {
		return PullRequestResult{}, err
	}
	api := strings.TrimRight(defaultString(request.APIBase, "https://gitlab.com/api/v4"), "/")
	payload := map[string]string{
		"title":         request.Title,
		"source_branch": request.Head,
		"target_branch": request.Base,
		"description":   request.Body,
	}
	var response struct {
		WebURL string `json:"web_url"`
		IID    int    `json:"iid"`
	}
	if err := s.post(ctx, api+"/projects/"+url.PathEscape(repo)+"/merge_requests", "PRIVATE-TOKEN", request.Token, payload, &response); err != nil {
		return PullRequestResult{}, err
	}
	return PullRequestResult{URL: response.WebURL, Number: fmt.Sprintf("%d", response.IID)}, nil
}

func (s PullRequestService) post(ctx context.Context, endpoint string, authHeader string, authValue string, payload any, target any) error {
	content, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(content))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if authHeader != "" && authValue != "" {
		request.Header.Set(authHeader, authValue)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("gitops pull request failed: status=%d body=%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func RepositoryPath(rawURL string) (string, error) {
	value := strings.TrimSpace(rawURL)
	value = strings.TrimSuffix(value, ".git")
	if strings.HasPrefix(value, "git@") {
		value = strings.TrimPrefix(value, "git@")
		value = strings.Replace(value, ":", "/", 1)
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Host != "" {
		value = strings.Trim(parsed.Path, "/")
	}
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("cannot derive repository path from %q", rawURL)
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1], nil
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
