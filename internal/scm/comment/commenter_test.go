package comment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/envpilot/contracts/domain"
)

func TestBuildEnvironmentCommentIncludesURLAndStatus(t *testing.T) {
	body := BuildEnvironmentComment(domain.Environment{
		URL:    "https://pr-123.cms.preview.feature.int",
		Status: domain.StatusReady,
	})
	if !strings.Contains(body, "Environment ready: https://pr-123.cms.preview.feature.int") {
		t.Fatalf("missing url: %s", body)
	}
	if !strings.Contains(body, "Status: Ready") {
		t.Fatalf("missing status: %s", body)
	}
}

func TestBuildEnvironmentCommentMatchesReadyPRContract(t *testing.T) {
	body := BuildEnvironmentComment(domain.Environment{
		URL:    "https://pr-123.cms.preview.feature.int",
		Status: domain.StatusReady,
	})
	for _, expected := range []string{
		"Environment ready: https://pr-123.cms.preview.feature.int",
		"Status: Ready",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("comment does not contain %q: %s", expected, body)
		}
	}
}

func TestGitHubCommentUsesIssueCommentsEndpoint(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	commenter := New(Config{GitHubToken: "github-token", GitHubAPI: server.URL})
	err := commenter.CommentEnvironment(context.Background(), domain.Environment{
		URL:    "https://pr-123.cms.preview.feature.int",
		Status: domain.StatusCreating,
		Source: domain.SCMSource{
			Provider:      "github",
			Repository:    "owner/repo",
			PullRequestID: "123",
		},
	})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if gotPath != "/repos/owner/repo/issues/123/comments" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer github-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotPayload["body"], "Status: Creating") {
		t.Fatalf("payload = %#v", gotPayload)
	}
}

func TestGitHubCommentUsesTokenResolver(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	commenter := New(Config{
		GitHubAPI: server.URL,
		TokenResolver: func(_ context.Context, provider string, _ domain.Environment) (string, error) {
			if provider != "github" {
				t.Fatalf("provider = %q", provider)
			}
			return "resolved-github-token", nil
		},
	})
	err := commenter.CommentEnvironment(context.Background(), domain.Environment{
		URL:    "https://pr-123.cms.preview.feature.int",
		Status: domain.StatusCreating,
		Source: domain.SCMSource{
			Provider:      "github",
			Repository:    "owner/repo",
			PullRequestID: "123",
		},
	})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if gotAuth != "Bearer resolved-github-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestGitLabCommentUsesMergeRequestNotesEndpoint(t *testing.T) {
	var gotPath string
	var gotRequestURI string
	var gotToken string
	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRequestURI = r.RequestURI
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	commenter := New(Config{GitLabToken: "gitlab-token", GitLabAPI: server.URL})
	err := commenter.CommentEnvironment(context.Background(), domain.Environment{
		URL:    "https://pr-123.cms.preview.feature.int",
		Status: domain.StatusReady,
		Source: domain.SCMSource{
			Provider:      "gitlab",
			Repository:    "group/repo",
			PullRequestID: "123",
		},
	})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if gotPath != "/projects/group/repo/merge_requests/123/notes" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotRequestURI, "/projects/group%2Frepo/merge_requests/123/notes") {
		t.Fatalf("request uri = %q", gotRequestURI)
	}
	if gotToken != "gitlab-token" {
		t.Fatalf("token = %q", gotToken)
	}
	if !strings.Contains(gotPayload["body"], "Status: Ready") {
		t.Fatalf("payload = %#v", gotPayload)
	}
}
