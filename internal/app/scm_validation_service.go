package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type SCMValidationServiceConfig struct {
	GitHubAPI string
	GitLabAPI string
	Client    *http.Client
}

type SCMAuthMethod string

const (
	SCMAuthMethodOAuth     SCMAuthMethod = "OAuth"
	SCMAuthMethodAppToken  SCMAuthMethod = "App token"
	SCMAuthMethodDeployKey SCMAuthMethod = "Deploy token"
	SCMAuthMethodSSHKey    SCMAuthMethod = "SSH key"
)

type SCMRepositoryValidationRequest struct {
	Provider          string `json:"provider"`
	AppRepoURL        string `json:"appRepoUrl"`
	GitOpsRepoURL     string `json:"gitopsRepoUrl"`
	DefaultBranch     string `json:"defaultBranch"`
	AuthMethod        string `json:"authMethod"`
	OAuthToken        string `json:"oauthToken"`
	AppToken          string `json:"appToken"`
	DeployToken       string `json:"deployToken"`
	SSHPrivateKey     string `json:"sshPrivateKey"`
}

type SCMValidationError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e SCMValidationError) Error() string {
	return e.Message
}

type SCMRepositoryValidationResult struct {
	Provider                   string                `json:"provider"`
	AppRepoURL                 string                `json:"appRepoUrl"`
	GitOpsRepoURL              string                `json:"gitopsRepoUrl"`
	DefaultBranch              string                `json:"defaultBranch"`
	AppRepositoryReadable      bool                  `json:"appRepositoryReadable"`
	GitopsRepositoryWritable   bool                  `json:"gitopsRepositoryWritable"`
	Branches                  []string              `json:"branches"`
	ValidationErrors          []SCMValidationError  `json:"errors"`
	ValidationWarnings        []SCMValidationError  `json:"warnings"`
	ValidationProvider         string                `json:"validationProvider"`
	HasAuthenticationValidated bool                  `json:"hasAuthenticationValidated"`
	Valid                     bool                  `json:"valid"`
}

type SCMValidationService struct {
	client      *http.Client
	githubAPI   string
	gitlabAPI   string
}

func NewSCMValidationService() *SCMValidationService {
	return NewSCMValidationServiceWithConfig(SCMValidationServiceConfig{})
}

func NewSCMValidationServiceWithConfig(cfg SCMValidationServiceConfig) *SCMValidationService {
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	githubAPI := strings.TrimRight(strings.TrimSpace(cfg.GitHubAPI), "/")
	if githubAPI == "" {
		githubAPI = "https://api.github.com"
	}
	gitlabAPI := strings.TrimRight(strings.TrimSpace(cfg.GitLabAPI), "/")
	if gitlabAPI == "" {
		gitlabAPI = "https://gitlab.com/api/v4"
	}
	return &SCMValidationService{
		client:    client,
		githubAPI: githubAPI,
		gitlabAPI: gitlabAPI,
	}
}

func (s *SCMValidationService) ValidateSCMConfig(ctx context.Context, req SCMRepositoryValidationRequest) (SCMRepositoryValidationResult, error) {
	requestedProvider := normalizeSCMProvider(req.Provider)
	if requestedProvider == "" {
		return SCMRepositoryValidationResult{}, ValidationError{Message: "provider is required"}
	}
	result := SCMRepositoryValidationResult{
		Provider:                  requestedProvider,
		AppRepoURL:                strings.TrimSpace(req.AppRepoURL),
		GitOpsRepoURL:             strings.TrimSpace(req.GitOpsRepoURL),
		DefaultBranch:             strings.TrimSpace(req.DefaultBranch),
		ValidationProvider:        requestedProvider,
		HasAuthenticationValidated: false,
	}
	if result.AppRepoURL == "" {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "appRepoUrl",
			Code:    "required_field",
			Message: "application repository URL is required",
		})
	}
	if result.GitOpsRepoURL == "" {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "gitopsRepoUrl",
			Code:    "required_field",
			Message: "GitOps repository URL is required",
		})
	}
	authMethod := normalizeSCMAuthMethod(req.AuthMethod)
	if authMethod == "" {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "authMethod",
			Code:    "required_field",
			Message: "authentication method is required",
		})
	}

	var credential string
	if authMethod != "" {
		credential = normalizeSCMCredential(req, authMethod)
		if strings.TrimSpace(credential) == "" && authMethod != SCMAuthMethodSSHKey {
			result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
				Field:   "authCredential",
				Code:    "required_field",
				Message: "authentication secret is required for this auth method",
			})
		}
	}

	if !strings.EqualFold(string(authMethod), string(SCMAuthMethodSSHKey)) && strings.TrimSpace(string(authMethod)) != "" {
		result.HasAuthenticationValidated = true
	}

	appRepo, appRepoProvider, err := parseRepositoryIdentity(result.AppRepoURL, requestedProvider)
	if err != nil {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "appRepoUrl",
			Code:    "invalid_repository_url",
			Message: err.Error(),
		})
	}
	gitopsRepo, gitOpsProvider, err := parseRepositoryIdentity(result.GitOpsRepoURL, requestedProvider)
	if err != nil {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "gitopsRepoUrl",
			Code:    "invalid_repository_url",
			Message: err.Error(),
		})
	}
	if appRepoProvider != "" && !strings.EqualFold(appRepoProvider, requestedProvider) {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "appRepoUrl",
			Code:    "provider_mismatch",
			Message: fmt.Sprintf("provider %q does not match repository provider %q", requestedProvider, appRepoProvider),
		})
	}
	if gitOpsProvider != "" && !strings.EqualFold(gitOpsProvider, requestedProvider) {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "gitopsRepoUrl",
			Code:    "provider_mismatch",
			Message: fmt.Sprintf("provider %q does not match repository provider %q", requestedProvider, gitOpsProvider),
		})
	}

	if len(result.ValidationErrors) > 0 || len(result.ValidationWarnings) > 0 {
		result.Valid = false
		return result, nil
	}

	switch requestedProvider {
	case "github":
		appReadable, appBranchErr := s.validateGitHubRepository(ctx, appRepo, credential)
		result.AppRepositoryReadable = appReadable
		if appBranchErr != nil {
			if vErr, ok := appBranchErr.(SCMValidationError); ok {
				result.ValidationErrors = append(result.ValidationErrors, vErr)
			} else {
				result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
					Field:   "appRepoUrl",
					Code:    "provider_error",
					Message: appBranchErr.Error(),
				})
			}
		}
		gitopsWriteErr := s.validateGitHubRepositoryWrite(ctx, gitopsRepo, credential)
		result.GitopsRepositoryWritable = gitopsWriteErr == nil
		if gitopsWriteErr != nil {
			if vErr, ok := gitopsWriteErr.(SCMValidationError); ok {
				result.ValidationErrors = append(result.ValidationErrors, vErr)
			} else {
				result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
					Field:   "gitopsRepoUrl",
					Code:    "provider_error",
					Message: gitopsWriteErr.Error(),
				})
			}
		}
		branches, branchErr := s.listGitHubBranches(ctx, appRepo, credential)
		if branchErr != nil {
			if vErr, ok := branchErr.(SCMValidationError); ok {
				result.ValidationWarnings = append(result.ValidationWarnings, vErr)
			} else {
				result.ValidationWarnings = append(result.ValidationWarnings, SCMValidationError{
					Field:   "appRepoUrl",
					Code:    "provider_error",
					Message: branchErr.Error(),
				})
			}
		} else {
			result.Branches = branches
		}
	case "gitlab":
		appReadable, appBranchErr := s.validateGitLabRepository(ctx, appRepo, credential)
		result.AppRepositoryReadable = appReadable
		if appBranchErr != nil {
			if vErr, ok := appBranchErr.(SCMValidationError); ok {
				result.ValidationErrors = append(result.ValidationErrors, vErr)
			} else {
				result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
					Field:   "appRepoUrl",
					Code:    "provider_error",
					Message: appBranchErr.Error(),
				})
			}
		}
		gitopsWriteErr := s.validateGitLabRepositoryWrite(ctx, gitopsRepo, credential)
		result.GitopsRepositoryWritable = gitopsWriteErr == nil
		if gitopsWriteErr != nil {
			if vErr, ok := gitopsWriteErr.(SCMValidationError); ok {
				result.ValidationErrors = append(result.ValidationErrors, vErr)
			} else {
				result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
					Field:   "gitopsRepoUrl",
					Code:    "provider_error",
					Message: gitopsWriteErr.Error(),
				})
			}
		}
		branches, branchErr := s.listGitLabBranches(ctx, appRepo, credential)
		if branchErr != nil {
			if vErr, ok := branchErr.(SCMValidationError); ok {
				result.ValidationWarnings = append(result.ValidationWarnings, vErr)
			} else {
				result.ValidationWarnings = append(result.ValidationWarnings, SCMValidationError{
					Field:   "appRepoUrl",
					Code:    "provider_error",
					Message: branchErr.Error(),
				})
			}
		} else {
			result.Branches = branches
		}
	default:
		return result, ValidationError{Message: "unsupported provider"}
	}

	result.Valid = len(result.ValidationErrors) == 0
	if !result.AppRepositoryReadable {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "appRepoUrl",
			Code:    "repository_not_readable",
			Message: "application repository is not readable with provided credentials",
		})
		result.Valid = false
	}
	if !result.GitopsRepositoryWritable {
		result.ValidationErrors = append(result.ValidationErrors, SCMValidationError{
			Field:   "gitopsRepoUrl",
			Code:    "repository_not_writable",
			Message: "GitOps repository does not grant write access",
		})
		result.Valid = false
	}
	return result, nil
}

func (s *SCMValidationService) validateGitHubRepository(ctx context.Context, repository string, token string) (bool, error) {
	endpoint := s.githubAPI + "/repos/" + repository
	var resp struct {
		Permissions struct {
			Push bool `json:"push"`
			Pull bool `json:"pull"`
		} `json:"permissions"`
	}
	if err := s.fetchRepository(ctx, endpoint, token, "github", &resp); err != nil {
		return false, err
	}
	if resp.Permissions.Pull || resp.Permissions.Push {
		return true, nil
	}
	return true, nil
}

func (s *SCMValidationService) validateGitHubRepositoryWrite(ctx context.Context, repository string, token string) error {
	endpoint := s.githubAPI + "/repos/" + repository
	var resp struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := s.fetchRepository(ctx, endpoint, token, "github", &resp); err != nil {
		return err
	}
	if resp.Permissions.Push {
		return nil
	}
	return SCMValidationError{
		Field:   "gitopsRepoUrl",
		Code:    "write_access_denied",
		Message: "insufficient permissions to write to GitOps repository",
	}
}

func (s *SCMValidationService) validateGitLabRepository(ctx context.Context, repository string, token string) (bool, error) {
	endpoint := s.gitlabAPI + "/projects/" + url.PathEscape(repository)
	var resp struct {
		Permissions struct {
			ProjectAccess struct {
				AccessLevel int `json:"access_level"`
			} `json:"project_access"`
			GroupAccess struct {
				AccessLevel int `json:"access_level"`
			} `json:"group_access"`
		} `json:"permissions"`
	}
	if err := s.fetchRepository(ctx, endpoint, token, "gitlab", &resp); err != nil {
		return false, err
	}
	accessLevel := maxInt(resp.Permissions.ProjectAccess.AccessLevel, resp.Permissions.GroupAccess.AccessLevel)
	if accessLevel > 0 {
		return true, nil
	}
	return true, nil
}

func (s *SCMValidationService) validateGitLabRepositoryWrite(ctx context.Context, repository string, token string) error {
	endpoint := s.gitlabAPI + "/projects/" + url.PathEscape(repository)
	var resp struct {
		Permissions struct {
			ProjectAccess struct {
				AccessLevel int `json:"access_level"`
			} `json:"project_access"`
			GroupAccess struct {
				AccessLevel int `json:"access_level"`
			} `json:"group_access"`
		} `json:"permissions"`
	}
	if err := s.fetchRepository(ctx, endpoint, token, "gitlab", &resp); err != nil {
		return err
	}
	accessLevel := maxInt(resp.Permissions.ProjectAccess.AccessLevel, resp.Permissions.GroupAccess.AccessLevel)
	if accessLevel >= 30 {
		return nil
	}
	return SCMValidationError{
		Field:   "gitopsRepoUrl",
		Code:    "write_access_denied",
		Message: "insufficient permissions to write to GitOps repository",
	}
}

func (s *SCMValidationService) listGitHubBranches(ctx context.Context, repository string, token string) ([]string, error) {
	endpoint := s.githubAPI + "/repos/" + repository + "/branches?per_page=100"
	var branches []struct {
		Name string `json:"name"`
	}
	if err := s.fetchRepository(ctx, endpoint, token, "github", &branches); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(branches))
	for _, item := range branches {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

func (s *SCMValidationService) listGitLabBranches(ctx context.Context, repository string, token string) ([]string, error) {
	endpoint := s.gitlabAPI + "/projects/" + url.PathEscape(repository) + "/repository/branches?per_page=100"
	var branches []struct {
		Name string `json:"name"`
	}
	if err := s.fetchRepository(ctx, endpoint, token, "gitlab", &branches); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(branches))
	for _, item := range branches {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

func (s *SCMValidationService) fetchRepository(ctx context.Context, endpoint string, credential string, provider string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ValidationError{Message: "build request failed: " + err.Error()}
	}
	authHeader, authToken := scmAuthHeader(provider, credential)
	if authHeader != "" && authToken != "" {
		request.Header.Set(authHeader, authToken)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return SCMValidationError{
			Field:   "network",
			Code:    "network_error",
			Message: err.Error(),
		}
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		message := strings.TrimSpace(string(body))
		return mapProviderStatusToValidationError(response.StatusCode, determineFieldByEndpoint(endpoint), message, provider)
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return ValidationError{Message: "decode response failed: " + err.Error()}
	}
	return nil
}

func normalizeSCMCredential(req SCMRepositoryValidationRequest, method SCMAuthMethod) string {
	switch method {
	case SCMAuthMethodOAuth:
		return strings.TrimSpace(req.OAuthToken)
	case SCMAuthMethodAppToken:
		return strings.TrimSpace(req.AppToken)
	case SCMAuthMethodDeployKey:
		return strings.TrimSpace(req.DeployToken)
	case SCMAuthMethodSSHKey:
		return strings.TrimSpace(req.SSHPrivateKey)
	default:
		return ""
	}
}

func normalizeSCMAuthMethod(value string) SCMAuthMethod {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "oauth", "oauth token":
		return SCMAuthMethodOAuth
	case "app token", "apptoken", "app":
		return SCMAuthMethodAppToken
	case "deploy token", "deploytoken", "deploy":
		return SCMAuthMethodDeployKey
	case "ssh key", "ssh", "sshkey":
		return SCMAuthMethodSSHKey
	default:
		return ""
	}
}

func normalizeSCMProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "github":
		return "github"
	case "gitlab":
		return "gitlab"
	default:
		return ""
	}
}

func parseRepositoryIdentity(raw string, fallback string) (string, string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", fmt.Errorf("repository URL is required")
	}
	repository := strings.TrimSuffix(value, ".git")
	providerHint := strings.ToLower(strings.TrimSpace(fallback))
	if strings.Contains(repository, "://") {
		parsed, err := url.Parse(repository)
		if err != nil {
			return "", "", fmt.Errorf("invalid repository url %q", value)
		}
		host := inferRepositoryHost(parsed.Hostname())
		if parsed.Path == "" || parsed.Path == "/" {
			return "", "", fmt.Errorf("repository path is required in %q", value)
		}
		repo := strings.Trim(parsed.Path, "/")
		segments := []string{}
		for _, segment := range strings.Split(repo, "/") {
			if segment != "" && segment != ".git" {
				segments = append(segments, segment)
			}
		}
		if len(segments) == 0 {
			return "", "", fmt.Errorf("invalid repository path in %q", value)
		}
		if host == "github.com" && len(segments) > 2 {
			segments = segments[:2]
		}
		repository = strings.Join(segments, "/")
		return repository, parseProviderFromHost(host), nil
	}
	if strings.Contains(repository, "@") && strings.Contains(repository, ":") {
		left, right, ok := strings.Cut(strings.TrimSuffix(repository, ".git"), ":")
		if ok {
			userAndHost := strings.TrimPrefix(left, "git@")
			if strings.Contains(userAndHost, "@") {
				parts := strings.SplitN(userAndHost, "@", 2)
				userAndHost = parts[1]
			}
			host := parseProviderFromHost(inferRepositoryHost(userAndHost))
			repo := strings.TrimPrefix(right, "/")
			if repo == "" {
				return "", "", fmt.Errorf("invalid repository path in %q", value)
			}
			repo = strings.TrimSuffix(repo, ".git")
			return repo, host, nil
		}
	}
	repository = strings.TrimPrefix(repository, "github.com/")
	repository = strings.TrimPrefix(repository, "gitlab.com/")
	repository = strings.TrimPrefix(repository, "/")
	if repository == "" || !strings.Contains(repository, "/") {
		return "", "", fmt.Errorf("invalid repository path in %q", value)
	}
	if strings.HasSuffix(repository, ".git") {
		repository = strings.TrimSuffix(repository, ".git")
	}
	return repository, parseProviderFromHost(providerHint), nil
}

func inferRepositoryHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "www.github.com", "github.com":
		return "github.com"
	case "www.gitlab.com", "gitlab.com", "gitlab.example":
		return "gitlab.com"
	default:
		return host
	}
}

func parseProviderFromHost(host string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "github.com":
		return "github"
	case "gitlab.com":
		return "gitlab"
	default:
		return ""
	}
}

func parseRepositoryProvider(raw string) string {
	for _, trimmed := range []string{"https://", "http://"} {
		if strings.HasPrefix(strings.ToLower(raw), trimmed) {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err != nil {
				return ""
			}
			return parseProviderFromHost(u.Hostname())
		}
	}
	return ""
}

func normalizeSCMRepositoryProvider(raw, fallback string) string {
	if raw == "" {
		return ""
	}
	if normalized := parseRepositoryProvider(raw); normalized != "" {
		return normalized
	}
	return fallback
}

func scmAuthHeader(provider, credential string) (string, string) {
	if strings.TrimSpace(credential) == "" {
		return "", ""
	}
	if provider == "github" {
		return "Authorization", "Bearer " + strings.TrimSpace(credential)
	}
	if provider == "gitlab" {
		return "PRIVATE-TOKEN", strings.TrimSpace(credential)
	}
	return "", ""
}

func determineFieldByEndpoint(endpoint string) string {
	if strings.Contains(endpoint, "/repository/branches") {
		return "branches"
	}
	if strings.HasSuffix(endpoint, "/branches") {
		return "branches"
	}
	if strings.Contains(endpoint, "/projects/") || strings.Contains(endpoint, "/repos/") {
		if strings.Contains(endpoint, "/repository/branches") {
			return "branches"
		}
		return "repositories"
	}
	return ""
}

func mapProviderStatusToValidationError(statusCode int, field string, body string, provider string) error {
	switch statusCode {
	case http.StatusUnauthorized:
		return SCMValidationError{
			Field:   field,
			Code:    "auth_failed",
			Message: "authentication failed for " + provider + ": " + body,
		}
	case http.StatusForbidden:
		return SCMValidationError{
			Field:   field,
			Code:    "access_denied",
			Message: "access denied for " + provider + ": " + body,
		}
	case http.StatusNotFound:
		return SCMValidationError{
			Field:   field,
			Code:    "repository_not_found",
			Message: body,
		}
	default:
		return SCMValidationError{
			Field:   field,
			Code:    "provider_error",
			Message: fmt.Sprintf("provider returned status %d: %s", statusCode, body),
		}
	}
}

func maxInt(first, second int) int {
	if first > second {
		return first
	}
	return second
}
