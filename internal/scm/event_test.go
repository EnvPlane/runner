package scm

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"envpilot/internal/domain"
)

func TestParseGitHubPullRequestNormalizesEvent(t *testing.T) {
	event, err := ParseGitHubPullRequest([]byte(githubPayload("synchronize", "feature/kan-2101", "abc123")))
	if err != nil {
		t.Fatalf("parse github event: %v", err)
	}

	if event.Provider != ProviderGitHub {
		t.Fatalf("provider = %q", event.Provider)
	}
	if event.Action != ActionUpdate {
		t.Fatalf("action = %q", event.Action)
	}
	if event.Repo != "owner/repo" {
		t.Fatalf("repo = %q", event.Repo)
	}
	if event.Branch != "feature/kan-2101" {
		t.Fatalf("branch = %q", event.Branch)
	}
	if event.ChangeID != "2101" {
		t.Fatalf("change id = %q", event.ChangeID)
	}
	if event.CommitSHA != "abc123" {
		t.Fatalf("commit = %q", event.CommitSHA)
	}
	if event.EnvironmentID() != "pr-2101" {
		t.Fatalf("environment id = %q", event.EnvironmentID())
	}
	req := event.CreateEnvironmentRequest("", "")
	if req.Source.Provider != "github" || req.Source.Repository != "owner/repo" || req.Source.Commit != "abc123" {
		t.Fatalf("unexpected create request source: %+v", req.Source)
	}
	if req.Mode != domain.ModeFull {
		t.Fatalf("expected full mode create request, got %q", req.Mode)
	}
}

func TestParseGitHubPullRequestExtractsRequiredFieldsForSupportedActions(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		wantAction EventAction
	}{
		{name: "opened", action: "opened", wantAction: ActionOpen},
		{name: "synchronize", action: "synchronize", wantAction: ActionUpdate},
		{name: "closed", action: "closed", wantAction: ActionClose},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := ParseGitHubPullRequest([]byte(githubPayloadWithNumber(tt.action, "42", "feature/new-checkout", "f00ba47")))
			if err != nil {
				t.Fatalf("parse github event: %v", err)
			}

			if event.Provider != ProviderGitHub {
				t.Fatalf("provider = %q", event.Provider)
			}
			if event.Action != tt.wantAction {
				t.Fatalf("action = %q", event.Action)
			}
			if event.ChangeID != "42" {
				t.Fatalf("change id = %q", event.ChangeID)
			}
			if event.Branch != "feature/new-checkout" {
				t.Fatalf("branch = %q", event.Branch)
			}
			if event.CommitSHA != "f00ba47" {
				t.Fatalf("commit = %q", event.CommitSHA)
			}
		})
	}
}

func TestParseGitHubPullRequestCapturesLabelsDraftAndInstallation(t *testing.T) {
	event, err := ParseGitHubPullRequest([]byte(githubPayloadWithNumberAndDetails("opened", "77", "feature/labels", "abc123", true, []string{"bug", "wip"}, 98765)))
	if err != nil {
		t.Fatalf("parse github event: %v", err)
	}

	if !event.Draft {
		t.Fatalf("expected draft true")
	}
	if event.InstallationID != "98765" {
		t.Fatalf("installation id = %q", event.InstallationID)
	}
	if len(event.Labels) != 2 || event.Labels[0] != "bug" || event.Labels[1] != "wip" {
		t.Fatalf("labels = %#v", event.Labels)
	}
}

func TestParseGitLabMergeRequestNormalizesEvent(t *testing.T) {
	event, err := ParseGitLabMergeRequest([]byte(gitlabPayload("open", "opened", "feature/kan-2201", "def456")))
	if err != nil {
		t.Fatalf("parse gitlab event: %v", err)
	}

	if event.Provider != ProviderGitLab {
		t.Fatalf("provider = %q", event.Provider)
	}
	if event.Action != ActionOpen {
		t.Fatalf("action = %q", event.Action)
	}
	if event.Repo != "group/repo" {
		t.Fatalf("repo = %q", event.Repo)
	}
	if event.Branch != "feature/kan-2201" {
		t.Fatalf("branch = %q", event.Branch)
	}
	if event.ChangeID != "2201" {
		t.Fatalf("change id = %q", event.ChangeID)
	}
	if event.CommitSHA != "def456" {
		t.Fatalf("commit = %q", event.CommitSHA)
	}
	if event.EnvironmentID() != "mr-2201" {
		t.Fatalf("environment id = %q", event.EnvironmentID())
	}
	req := event.CreateEnvironmentRequest("", "")
	if req.Source.Provider != "gitlab" || req.Source.Repository != "group/repo" || req.Source.Commit != "def456" {
		t.Fatalf("unexpected create request source: %+v", req.Source)
	}
}

func TestParseGitLabMergeRequestCapturesProjectIDAndLabels(t *testing.T) {
	event, err := ParseGitLabMergeRequest([]byte(gitlabPayloadWithProjectID("open", "opened", "feature/kan-2203", "def456", 123, []string{"ci", "release"})))
	if err != nil {
		t.Fatalf("parse gitlab event: %v", err)
	}
	if event.InstallationID != "123" {
		t.Fatalf("installation id = %q", event.InstallationID)
	}
	if len(event.Labels) != 2 || event.Labels[0] != "ci" || event.Labels[1] != "release" {
		t.Fatalf("labels = %#v", event.Labels)
	}
}

func TestParseGitLabMergeRequestCapturesDraftState(t *testing.T) {
	event, err := ParseGitLabMergeRequest([]byte(gitlabPayloadWithDraft("open", "opened", "feature/kan-2204", "def456", true)))
	if err != nil {
		t.Fatalf("parse gitlab event: %v", err)
	}
	if !event.Draft {
		t.Fatalf("expected draft event from work_in_progress")
	}
}

func TestParseGitLabMergeRequestClosedEvent(t *testing.T) {
	event, err := ParseGitLabMergeRequest([]byte(gitlabPayload("merge", "merged", "feature/kan-2202", "def789")))
	if err != nil {
		t.Fatalf("parse gitlab event: %v", err)
	}
	if event.Action != ActionClose {
		t.Fatalf("action = %q", event.Action)
	}
}

func TestParseGitHubPRCommand(t *testing.T) {
	command, err := ParseGitHubPRCommand([]byte(githubIssueCommentPayload("/envpilot pin 7d")))
	if err != nil {
		t.Fatalf("parse github command: %v", err)
	}
	if command.Provider != ProviderGitHub {
		t.Fatalf("provider = %q", command.Provider)
	}
	if command.Command != CommandPin {
		t.Fatalf("command = %q", command.Command)
	}
	if command.ChangeID != "2101" || command.EnvironmentID() != "pr-2101" {
		t.Fatalf("unexpected change/environment id: change=%q env=%q", command.ChangeID, command.EnvironmentID())
	}
	if command.PinDuration != 7*24*time.Hour || command.PinRaw != "7d" {
		t.Fatalf("unexpected pin duration: duration=%s raw=%q", command.PinDuration, command.PinRaw)
	}
}

func TestParseGitLabPRCommand(t *testing.T) {
	command, err := ParseGitLabPRCommand([]byte(gitlabNotePayload("/envpilot destroy")))
	if err != nil {
		t.Fatalf("parse gitlab command: %v", err)
	}
	if command.Provider != ProviderGitLab {
		t.Fatalf("provider = %q", command.Provider)
	}
	if command.Command != CommandDestroy {
		t.Fatalf("command = %q", command.Command)
	}
	if command.ChangeID != "2201" || command.EnvironmentID() != "mr-2201" {
		t.Fatalf("unexpected change/environment id: change=%q env=%q", command.ChangeID, command.EnvironmentID())
	}
}

func TestDeduplicationKeyUsesEventIDAndFallbackTuple(t *testing.T) {
	event := PullRequestEvent{EventID: "evt-1", Provider: ProviderGitHub, Repo: "owner/repo", Action: ActionUpdate, ChangeID: "12"}
	if key := event.DeduplicationKey(); key != "event:evt-1" {
		t.Fatalf("dedup key by event id = %q", key)
	}
	event = PullRequestEvent{Provider: ProviderGitHub, Repo: "owner/repo", Action: ActionUpdate, ChangeID: "12", Branch: "feat/x"}
	if key := event.DeduplicationKey(); key != "github|owner/repo|update|12" {
		t.Fatalf("dedup key by tuple = %q", key)
	}
	event = PullRequestEvent{Provider: ProviderGitHub, Repo: "owner/repo", Action: ActionUpdate, Branch: "feat/x"}
	if key := event.DeduplicationKey(); key != "github|owner/repo|update|feat/x" {
		t.Fatalf("dedup fallback key = %q", key)
	}
}

func githubPayload(action string, branch string, sha string) string {
	return githubPayloadWithNumber(action, "2101", branch, sha)
}

func githubPayloadWithNumber(action string, number string, branch string, sha string) string {
	return `{
  "action": "` + action + `",
  "number": ` + number + `,
  "repository": {
    "full_name": "owner/repo",
    "html_url": "https://github.com/owner/repo"
  },
  "pull_request": {
    "number": ` + number + `,
    "html_url": "https://github.com/owner/repo/pull/` + number + `",
    "head": {
      "ref": "` + branch + `",
      "sha": "` + sha + `"
    },
    "user": {
      "login": "octocat"
    }
  },
  "sender": {
    "login": "octocat"
  }
}`
}

func githubPayloadWithNumberAndDetails(action string, number string, branch string, sha string, draft bool, labels []string, installation int64) string {
	labelEntries := make([]string, 0, len(labels))
	for _, label := range labels {
		labelEntries = append(labelEntries, `{"name":"`+label+`"}`)
	}
	return `{
  "action": "` + action + `",
  "number": ` + number + `,
  "repository": {
    "full_name": "owner/repo",
    "html_url": "https://github.com/owner/repo"
  },
  "pull_request": {
    "number": ` + number + `,
    "html_url": "https://github.com/owner/repo/pull/` + number + `",
    "head": {
      "ref": "` + branch + `",
      "sha": "` + sha + `"
    },
    "user": {
      "login": "octocat"
    },
    "draft": ` + boolToJSON(draft) + `,
    "labels": [` + strings.Join(labelEntries, ",") + `]
  },
  "sender": {
    "login": "octocat"
  },
  "installation": {
    "id": ` + fmt.Sprint(installation) + `
  }
}`
}

func githubIssueCommentPayload(body string) string {
	return `{
  "action": "created",
  "repository": {
    "full_name": "owner/repo",
    "html_url": "https://github.com/owner/repo"
  },
  "issue": {
    "number": 2101,
    "html_url": "https://github.com/owner/repo/issues/2101",
    "pull_request": {
      "url": "https://api.github.com/repos/owner/repo/pulls/2101"
    }
  },
  "comment": {
    "body": "` + body + `",
    "user": {
      "login": "octocat"
    }
  },
  "sender": {
    "login": "octocat"
  },
  "installation": {
    "id": 98765
  }
}`
}

func gitlabPayload(action string, state string, branch string, sha string) string {
	return gitlabPayloadWithDraft(action, state, branch, sha, false)
}

func gitlabPayloadWithDraft(action string, state string, branch string, sha string, draft bool) string {
	draftValue := "false"
	if draft {
		draftValue = "true"
	}
	return `{
  "object_kind": "merge_request",
  "user": {
    "name": "Alex",
    "username": "alex"
  },
  "project": {
    "path_with_namespace": "group/repo",
    "web_url": "https://gitlab.example/group/repo"
  },
  "object_attributes": {
    "iid": 2201,
    "action": "` + action + `",
    "state": "` + state + `",
    "source_branch": "` + branch + `",
    "work_in_progress": ` + draftValue + `,
    "last_commit": {
      "id": "` + sha + `"
    },
    "url": "https://gitlab.example/group/repo/-/merge_requests/2201"
  }
}`
}

func gitlabPayloadWithProjectID(action string, state string, branch string, sha string, projectID int64, labels []string) string {
	labelEntries := make([]string, 0, len(labels))
	for _, label := range labels {
		labelEntries = append(labelEntries, `{"name":"`+label+`"}`)
	}
	return `{
  "object_kind": "merge_request",
  "user": {
    "name": "Alex",
    "username": "alex"
  },
  "project": {
    "id": ` + fmt.Sprint(projectID) + `,
    "path_with_namespace": "group/repo",
    "web_url": "https://gitlab.example/group/repo"
  },
  "object_attributes": {
    "iid": 2201,
    "action": "` + action + `",
    "state": "` + state + `",
    "source_branch": "` + branch + `",
    "labels": [` + strings.Join(labelEntries, ",") + `],
    "last_commit": {
      "id": "` + sha + `"
    },
    "url": "https://gitlab.example/group/repo/-/merge_requests/2201"
  }
}`
}

func gitlabNotePayload(body string) string {
	return `{
  "object_kind": "note",
  "user": {
    "name": "Alex",
    "username": "alex"
  },
  "project": {
    "path_with_namespace": "group/repo",
    "id": 123,
    "web_url": "https://gitlab.example/group/repo"
  },
  "merge_request": {
    "iid": 2201,
    "url": "https://gitlab.example/group/repo/-/merge_requests/2201"
  },
  "object_attributes": {
    "note": "` + body + `"
  }
}`
}

func boolToJSON(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
