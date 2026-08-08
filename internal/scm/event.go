package scm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/envpilot/contracts/domain"
)

type Provider string

const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
)

type EventAction string

const (
	ActionOpen   EventAction = "open"
	ActionUpdate EventAction = "update"
	ActionClose  EventAction = "close"
	ActionIgnore EventAction = "ignore"
)

type PRCommand string

const (
	CommandRecreate PRCommand = "recreate"
	CommandDestroy  PRCommand = "destroy"
	CommandPin      PRCommand = "pin"
)

type PullRequestEvent struct {
	Provider       Provider    `json:"provider"`
	Action         EventAction `json:"action"`
	Repo           string      `json:"repo"`
	Branch         string      `json:"branch"`
	ChangeID       string      `json:"changeId"`
	CommitSHA      string      `json:"commitSha"`
	Author         string      `json:"author"`
	URL            string      `json:"url"`
	EventID        string      `json:"eventId"`
	InstallationID string      `json:"installationId"`
	Labels         []string    `json:"labels"`
	Draft          bool        `json:"draft"`
}

type PullRequestCommand struct {
	Provider       Provider      `json:"provider"`
	Command        PRCommand     `json:"command"`
	Repo           string        `json:"repo"`
	ChangeID       string        `json:"changeId"`
	Author         string        `json:"author"`
	URL            string        `json:"url"`
	EventID        string        `json:"eventId"`
	InstallationID string        `json:"installationId"`
	PinDuration    time.Duration `json:"pinDuration,omitempty"`
	PinRaw         string        `json:"pinRaw,omitempty"`
}

func (e PullRequestEvent) EnvironmentID() string {
	switch e.Provider {
	case ProviderGitHub:
		return "pr-" + normalizeIdentifier(e.ChangeID)
	case ProviderGitLab:
		return "mr-" + normalizeIdentifier(e.ChangeID)
	default:
		if id := normalizeIdentifier(e.ChangeID); id != "" {
			return id
		}
		return branchToEnvironmentID(e.Branch)
	}
}

func (c PullRequestCommand) EnvironmentID() string {
	return PullRequestEvent{Provider: c.Provider, ChangeID: c.ChangeID}.EnvironmentID()
}

func (c PullRequestCommand) PullRequestEvent(action EventAction) PullRequestEvent {
	return PullRequestEvent{
		Provider:       c.Provider,
		Action:         action,
		Repo:           c.Repo,
		ChangeID:       c.ChangeID,
		Author:         c.Author,
		URL:            c.URL,
		EventID:        c.EventID,
		InstallationID: c.InstallationID,
	}
}

func (e PullRequestEvent) DeduplicationKey() string {
	if value := strings.TrimSpace(e.EventID); value != "" {
		return "event:" + strings.ToLower(value)
	}

	provider := strings.ToLower(strings.TrimSpace(string(e.Provider)))
	repo := strings.ToLower(strings.TrimSpace(e.Repo))
	changeID := strings.TrimSpace(e.ChangeID)
	action := strings.ToLower(strings.TrimSpace(string(e.Action)))
	parts := []string{}
	if provider != "" {
		parts = append(parts, provider)
	}
	if repo != "" {
		parts = append(parts, repo)
	}
	if action != "" {
		parts = append(parts, action)
	}
	if changeID != "" {
		parts = append(parts, changeID)
	} else if branch := strings.TrimSpace(e.Branch); branch != "" {
		parts = append(parts, strings.ToLower(branch))
	} else {
		return ""
	}
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts, "|")
}

func (e PullRequestEvent) CreateEnvironmentRequest(product string, project string) domain.CreateEnvironmentRequest {
	if product == "" {
		product = "generic"
	}
	if project == "" {
		project = projectNameFromRepo(e.Repo)
	}
	return domain.CreateEnvironmentRequest{
		ID:      e.EnvironmentID(),
		Project: project,
		Product: product,
		Mode:    domain.ModeFull,
		Source: domain.SCMSource{
			Provider:      string(e.Provider),
			Repository:    e.Repo,
			PullRequestID: e.ChangeID,
			Branch:        e.Branch,
			Commit:        e.CommitSHA,
			Author:        e.Author,
			URL:           e.URL,
		},
	}
}

func ParseGitHubPRCommand(body []byte) (PullRequestCommand, error) {
	var event githubIssueCommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return PullRequestCommand{}, fmt.Errorf("parse github issue_comment event: %w", err)
	}
	if event.Action != "created" || event.Issue.PullRequest.URL == "" {
		return PullRequestCommand{Provider: ProviderGitHub}, nil
	}
	command, duration, raw, ok := parseEnvPilotCommand(event.Comment.Body)
	if !ok {
		return PullRequestCommand{Provider: ProviderGitHub}, nil
	}
	author := event.Comment.User.Login
	if author == "" {
		author = event.Sender.Login
	}
	return PullRequestCommand{
		Provider:       ProviderGitHub,
		Command:        command,
		Repo:           event.Repository.FullName,
		ChangeID:       fmt.Sprintf("%d", event.Issue.Number),
		Author:         author,
		URL:            event.Issue.HTMLURL,
		InstallationID: normalizeWebhookInstallationID(event.Installation.ID),
		PinDuration:    duration,
		PinRaw:         raw,
	}, nil
}

func ParseGitLabPRCommand(body []byte) (PullRequestCommand, error) {
	var event gitLabNoteEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return PullRequestCommand{}, fmt.Errorf("parse gitlab note event: %w", err)
	}
	if event.ObjectKind != "note" || event.MergeRequest.IID == 0 {
		return PullRequestCommand{Provider: ProviderGitLab}, nil
	}
	command, duration, raw, ok := parseEnvPilotCommand(event.ObjectAttributes.Note)
	if !ok {
		return PullRequestCommand{Provider: ProviderGitLab}, nil
	}
	author := event.User.Username
	if author == "" {
		author = event.User.Name
	}
	return PullRequestCommand{
		Provider:       ProviderGitLab,
		Command:        command,
		Repo:           event.Project.PathWithNamespace,
		ChangeID:       fmt.Sprintf("%d", event.MergeRequest.IID),
		Author:         author,
		URL:            event.MergeRequest.URL,
		InstallationID: normalizeWebhookProjectID(event.Project.ID),
		PinDuration:    duration,
		PinRaw:         raw,
	}, nil
}

func ParseGitHubPullRequest(body []byte) (PullRequestEvent, error) {
	var event githubPullRequestEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return PullRequestEvent{}, fmt.Errorf("parse github pull_request event: %w", err)
	}
	number := event.PullRequest.Number
	if number == 0 {
		number = event.Number
	}
	author := event.PullRequest.User.Login
	if author == "" {
		author = event.Sender.Login
	}

	return PullRequestEvent{
		Provider:       ProviderGitHub,
		Action:         normalizeGitHubAction(event.Action),
		Repo:           event.Repository.FullName,
		Branch:         event.PullRequest.Head.Ref,
		ChangeID:       fmt.Sprintf("%d", number),
		CommitSHA:      event.PullRequest.Head.SHA,
		Author:         author,
		URL:            event.PullRequest.HTMLURL,
		InstallationID: normalizeWebhookInstallationID(event.Installation.ID),
		Labels:         mapLabelNames([]string(event.PullRequest.Labels)),
		Draft:          event.PullRequest.Draft,
	}, nil
}

func ParseGitLabMergeRequest(body []byte) (PullRequestEvent, error) {
	var event gitLabMergeRequestEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return PullRequestEvent{}, fmt.Errorf("parse gitlab merge_request event: %w", err)
	}
	if event.ObjectKind != "merge_request" {
		return PullRequestEvent{Provider: ProviderGitLab, Action: ActionIgnore}, nil
	}
	author := event.User.Username
	if author == "" {
		author = event.User.Name
	}

	return PullRequestEvent{
		Provider:       ProviderGitLab,
		Action:         normalizeGitLabAction(event.ObjectAttributes.Action, event.ObjectAttributes.State),
		Repo:           event.Project.PathWithNamespace,
		Branch:         event.ObjectAttributes.SourceBranch,
		ChangeID:       fmt.Sprintf("%d", event.ObjectAttributes.IID),
		CommitSHA:      event.ObjectAttributes.LastCommit.ID,
		Author:         author,
		URL:            event.ObjectAttributes.URL,
		InstallationID: normalizeWebhookProjectID(event.Project.ID),
		Labels:         mapLabelNames([]string(event.ObjectAttributes.Labels)),
		Draft:          event.ObjectAttributes.WorkInProgress,
	}, nil
}

func parseEnvPilotCommand(body string) (PRCommand, time.Duration, string, bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || fields[0] != "/envpilot" {
			continue
		}
		switch fields[1] {
		case string(CommandRecreate):
			return CommandRecreate, 0, "", true
		case string(CommandDestroy):
			return CommandDestroy, 0, "", true
		case string(CommandPin):
			if len(fields) < 3 {
				return "", 0, "", false
			}
			duration, ok := parseCommandDuration(fields[2])
			if !ok {
				return "", 0, "", false
			}
			return CommandPin, duration, fields[2], true
		default:
			return "", 0, "", false
		}
	}
	return "", 0, "", false
}

func parseCommandDuration(value string) (time.Duration, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, false
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil || count <= 0 {
			return 0, false
		}
		return time.Duration(count) * 24 * time.Hour, true
	}
	duration, err := time.ParseDuration(value)
	return duration, err == nil && duration > 0
}

func normalizeGitHubAction(action string) EventAction {
	switch action {
	case "opened", "reopened":
		return ActionOpen
	case "synchronize", "edited", "ready_for_review":
		return ActionUpdate
	case "closed":
		return ActionClose
	default:
		return ActionIgnore
	}
}

func normalizeGitLabAction(action string, state string) EventAction {
	if action == "close" || action == "merge" || state == "closed" || state == "merged" {
		return ActionClose
	}
	switch action {
	case "open", "reopen":
		return ActionOpen
	case "update", "approved", "unapproved":
		return ActionUpdate
	default:
		if state == "opened" {
			return ActionUpdate
		}
		return ActionIgnore
	}
}

func branchToEnvironmentID(branch string) string {
	branch = strings.ToLower(strings.TrimSpace(branch))
	if branch == "" {
		return ""
	}
	return normalizeIdentifier(branch)
}

func projectNameFromRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "default"
	}
	parts := strings.FieldsFunc(repo, func(r rune) bool {
		return r == '/' || r == ':' || r == '\\'
	})
	if len(parts) == 0 {
		return "default"
	}
	name := normalizeIdentifier(parts[len(parts)-1])
	name = strings.TrimSuffix(name, ".git")
	if name == "" {
		return "default"
	}
	return name
}

func normalizeIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("/", "-", "_", "-", " ", "-")
	value = replacer.Replace(value)
	value = strings.Trim(value, "-")
	return value
}

type gitLabMergeRequestEvent struct {
	ObjectKind       string                       `json:"object_kind"`
	User             gitLabUser                   `json:"user"`
	Project          gitLabProject                `json:"project"`
	ObjectAttributes gitLabMergeRequestAttributes `json:"object_attributes"`
}

type gitLabNoteEvent struct {
	ObjectKind       string                 `json:"object_kind"`
	User             gitLabUser             `json:"user"`
	Project          gitLabProject          `json:"project"`
	MergeRequest     gitLabNoteMergeRequest `json:"merge_request"`
	ObjectAttributes gitLabNoteAttributes   `json:"object_attributes"`
}

type gitLabUser struct {
	Name     string `json:"name"`
	Username string `json:"username"`
}

type gitLabProject struct {
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	ID                int64  `json:"id"`
}

type gitLabCommit struct {
	ID string `json:"id"`
}

type gitLabNoteMergeRequest struct {
	IID int    `json:"iid"`
	URL string `json:"url"`
}

type gitLabNoteAttributes struct {
	Note string `json:"note"`
}

type gitLabMergeRequestAttributes struct {
	IID            int           `json:"iid"`
	Action         string        `json:"action"`
	State          string        `json:"state"`
	SourceBranch   string        `json:"source_branch"`
	LastCommit     gitLabCommit  `json:"last_commit"`
	URL            string        `json:"url"`
	WorkInProgress bool          `json:"work_in_progress"`
	Labels         webhookLabels `json:"labels"`
}

type githubRepository struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

type githubPullRequest struct {
	Number  int             `json:"number"`
	HTMLURL string          `json:"html_url"`
	Head    githubBranchRef `json:"head"`
	User    githubUser      `json:"user"`
	Merged  bool            `json:"merged"`
	Labels  webhookLabels   `json:"labels"`
	Draft   bool            `json:"draft"`
}

type githubBranchRef struct {
	Ref  string           `json:"ref"`
	SHA  string           `json:"sha"`
	Repo githubRepository `json:"repo"`
}

type githubUser struct {
	Login string `json:"login"`
}

type githubPullRequestEvent struct {
	Action       string             `json:"action"`
	Number       int                `json:"number"`
	Repository   githubRepository   `json:"repository"`
	PullRequest  githubPullRequest  `json:"pull_request"`
	Sender       githubUser         `json:"sender"`
	Installation githubInstallation `json:"installation"`
}

type githubIssueCommentEvent struct {
	Action       string             `json:"action"`
	Repository   githubRepository   `json:"repository"`
	Issue        githubIssue        `json:"issue"`
	Comment      githubComment      `json:"comment"`
	Sender       githubUser         `json:"sender"`
	Installation githubInstallation `json:"installation"`
}

type githubIssue struct {
	Number      int                    `json:"number"`
	HTMLURL     string                 `json:"html_url"`
	PullRequest githubIssuePullRequest `json:"pull_request"`
}

type githubIssuePullRequest struct {
	URL string `json:"url"`
}

type githubComment struct {
	Body string     `json:"body"`
	User githubUser `json:"user"`
}

type githubInstallation struct {
	ID int64 `json:"id"`
}

type webhookLabels []string

func (labels *webhookLabels) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*labels = nil
		return nil
	}

	var names []string
	if err := json.Unmarshal(data, &names); err == nil {
		*labels = webhookLabels(mapLabelNames(names))
		return nil
	}

	var objects []struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &objects); err != nil {
		return nil
	}
	normalized := make([]string, 0, len(objects))
	for _, label := range objects {
		switch {
		case strings.TrimSpace(label.Name) != "":
			normalized = append(normalized, strings.TrimSpace(label.Name))
		case strings.TrimSpace(label.Title) != "":
			normalized = append(normalized, strings.TrimSpace(label.Title))
		}
	}
	*labels = webhookLabels(mapLabelNames(normalized))
	return nil
}

func normalizeWebhookInstallationID(value int64) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func normalizeWebhookProjectID(value int64) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func mapLabelNames(names []string) []string {
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		normalized = append(normalized, name)
	}
	return normalized
}
