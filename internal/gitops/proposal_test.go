package gitops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPullRequestServiceCreatesGitHubPullRequest(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/acme/gitops/pull/7","number":7}`))
	}))
	defer server.Close()

	result, err := PullRequestService{client: server.Client()}.Create(context.Background(), PullRequestRequest{
		Provider: "github",
		APIBase:  server.URL,
		Token:    "token",
		RepoURL:  "https://github.com/acme/gitops.git",
		Title:    "EnvPlane pr-123",
		Body:     "body",
		Head:     "envplane/pr-123",
		Base:     "main",
	})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if gotPath != "/repos/acme/gitops/pulls" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotPayload["head"] != "envplane/pr-123" || gotPayload["base"] != "main" {
		t.Fatalf("payload = %#v", gotPayload)
	}
	if result.URL != "https://github.com/acme/gitops/pull/7" || result.Number != "7" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRepositoryPathNormalizesHTTPSAndSSHURLs(t *testing.T) {
	for _, input := range []string{
		"https://github.com/acme/gitops.git",
		"git@github.com:acme/gitops.git",
	} {
		path, err := RepositoryPath(input)
		if err != nil {
			t.Fatalf("repository path: %v", err)
		}
		if path != "acme/gitops" {
			t.Fatalf("path for %q = %q", input, path)
		}
	}
}
