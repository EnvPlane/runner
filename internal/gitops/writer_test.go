package gitops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWriterRemovePathDeletesEnvironmentDirectory(t *testing.T) {
	root := t.TempDir()
	writer := NewFileWriter(root, false, "", "")

	if _, err := writer.WriteManifest(context.Background(), "environments/kan-2601/namespace.yaml", []byte("namespace"), ""); err != nil {
		t.Fatalf("write namespace: %v", err)
	}
	if _, err := writer.WriteManifest(context.Background(), "environments/kan-2601/extra/config.yaml", []byte("extra"), ""); err != nil {
		t.Fatalf("write extra: %v", err)
	}

	if err := writer.RemovePath(context.Background(), "environments/kan-2601", "delete"); err != nil {
		t.Fatalf("remove path: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "environments/kan-2601")); !os.IsNotExist(err) {
		t.Fatalf("expected environment directory removed, err=%v", err)
	}
}

func newRepositoryWriterForRemovePathTests(t *testing.T) (*RepositoryWriter, string, string) {
	t.Helper()
	repoRoot := t.TempDir()
	writeDir := filepath.Join(repoRoot, "clusters", "dev")
	if err := os.MkdirAll(writeDir, 0o755); err != nil {
		t.Fatalf("mkdir write dir: %v", err)
	}
	writer := &RepositoryWriter{
		target: RepositoryTarget{Workspace: repoRoot},
		writer: NewFileWriter(writeDir, false, "", ""),
	}
	writer.once.Do(func() {})
	return writer, repoRoot, writeDir
}

func TestRepositoryWriterRemovePathRejectsTraversal(t *testing.T) {
	writer, repoRoot, _ := newRepositoryWriterForRemovePathTests(t)
	outsideDir := filepath.Join(repoRoot, "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside dir: %v", err)
	}
	if err := writer.RemovePath(context.Background(), "../outside", "delete"); err == nil {
		t.Fatalf("expected traversal path to be rejected")
	}
	if _, err := os.Stat(outsideDir); err != nil {
		t.Fatalf("outside dir should not be deleted, err=%v", err)
	}
}

func TestRepositoryWriterRemovePathRejectsAbsolutePath(t *testing.T) {
	writer, _, writeDir := newRepositoryWriterForRemovePathTests(t)
	absolute := filepath.Join(writeDir, "environments", "pr-1")
	if err := writer.RemovePath(context.Background(), absolute, "delete"); err == nil {
		t.Fatalf("expected absolute path to be rejected")
	}
}

func TestRepositoryWriterRemovePathDeletesNestedValidPath(t *testing.T) {
	writer, _, writeDir := newRepositoryWriterForRemovePathTests(t)
	nestedFile := filepath.Join(writeDir, "environments", "pr-1", "nested", "values.yaml")
	if err := os.MkdirAll(filepath.Dir(nestedFile), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(nestedFile, []byte("kind: ConfigMap\n"), 0o644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	if err := writer.RemovePath(context.Background(), "environments/pr-1/nested", "delete"); err != nil {
		t.Fatalf("remove nested path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(writeDir, "environments", "pr-1", "nested")); !os.IsNotExist(err) {
		t.Fatalf("expected nested directory removed, err=%v", err)
	}
}

func TestRepositoryWriterRemovePathDeletesNormalPath(t *testing.T) {
	writer, _, writeDir := newRepositoryWriterForRemovePathTests(t)
	envFile := filepath.Join(writeDir, "environments", "pr-2", "namespace.yaml")
	if err := os.MkdirAll(filepath.Dir(envFile), 0o755); err != nil {
		t.Fatalf("mkdir env dir: %v", err)
	}
	if err := os.WriteFile(envFile, []byte("kind: Namespace\n"), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	if err := writer.RemovePath(context.Background(), "environments/pr-2", "delete"); err != nil {
		t.Fatalf("remove env path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(writeDir, "environments", "pr-2")); !os.IsNotExist(err) {
		t.Fatalf("expected env directory removed, err=%v", err)
	}
}

func TestRepositoryWriterClonesWritesSubdirAndPushes(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "git", "init", "--bare", remote)
	seed := initRepo(t)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	run(t, seed, "git", "add", ".")
	run(t, seed, "git", "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "seed")
	run(t, seed, "git", "remote", "add", "origin", remote)
	run(t, seed, "git", "push", "origin", "main")

	workspace := filepath.Join(t.TempDir(), "worktree")
	writer, err := NewRepositoryWriter(RepositoryTarget{
		URL:         remote,
		Branch:      "main",
		Path:        "clusters/dev",
		Workspace:   workspace,
		Commit:      true,
		Push:        true,
		AuthorName:  "envpilot",
		AuthorEmail: "envpilot@example.com",
	})
	if err != nil {
		t.Fatalf("repository writer: %v", err)
	}
	if _, err := writer.WriteManifest(context.Background(), "feature-envs/checkout/pr-123/namespace.yaml", []byte("kind: Namespace\n"), "create"); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if result, err := writer.Commit(context.Background(), "envpilot: create pr-123"); err != nil {
		t.Fatalf("commit: %v", err)
	} else if !result.Committed || !result.Pushed {
		t.Fatalf("expected committed and pushed result, got %+v", result)
	}

	verify := filepath.Join(t.TempDir(), "verify")
	run(t, "", "git", "clone", "--branch", "main", remote, verify)
	if subject := strings.TrimSpace(output(t, verify, "git", "log", "-1", "--pretty=%s")); !strings.Contains(subject, "pr-123") {
		t.Fatalf("expected commit message to contain PR id, got %q", subject)
	}
	content, err := os.ReadFile(filepath.Join(verify, "clusters/dev/feature-envs/checkout/pr-123/namespace.yaml"))
	if err != nil {
		t.Fatalf("read pushed manifest: %v", err)
	}
	if string(content) != "kind: Namespace\n" {
		t.Fatalf("manifest content = %q", string(content))
	}
}

func TestRepositoryWriterBranchStrategyPushesEnvironmentBranch(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "git", "init", "--bare", remote)
	seed := initRepo(t)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	run(t, seed, "git", "add", ".")
	run(t, seed, "git", "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "seed")
	run(t, seed, "git", "remote", "add", "origin", remote)
	run(t, seed, "git", "push", "origin", "main")

	writer, err := NewRepositoryWriter(RepositoryTarget{
		URL:            remote,
		Branch:         "main",
		BranchStrategy: "branch",
		PushBranch:     "envpilot/pr-123",
		Workspace:      filepath.Join(t.TempDir(), "worktree"),
		Commit:         true,
		Push:           true,
		AuthorName:     "envpilot",
		AuthorEmail:    "envpilot@example.com",
	})
	if err != nil {
		t.Fatalf("repository writer: %v", err)
	}
	if _, err := writer.WriteManifest(context.Background(), "feature-envs/checkout/pr-123/namespace.yaml", []byte("kind: Namespace\n"), "create"); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	result, err := writer.Commit(context.Background(), "envpilot: create pr-123")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if result.Branch != "envpilot/pr-123" || !result.Pushed {
		t.Fatalf("unexpected commit result: %+v", result)
	}

	verify := filepath.Join(t.TempDir(), "verify")
	run(t, "", "git", "clone", "--branch", "envpilot/pr-123", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "feature-envs/checkout/pr-123/namespace.yaml")); err != nil {
		t.Fatalf("expected manifest on env branch: %v", err)
	}
}

func TestRepositoryWriterPullRequestStrategyCreatesProposal(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "owner", "repo.git")
	run(t, "", "mkdir", "-p", filepath.Dir(remote))
	run(t, "", "git", "init", "--bare", remote)

	seed := initRepo(t)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	run(t, seed, "git", "add", ".")
	run(t, seed, "git", "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "seed")
	run(t, seed, "git", "remote", "add", "origin", remote)
	run(t, seed, "git", "push", "origin", "main")

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
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/acme/gitops/pull/42","number":42}`))
	}))
	defer server.Close()

	writer, err := NewRepositoryWriter(RepositoryTarget{
		URL:              remote,
		Provider:         "github",
		Branch:           "main",
		BranchStrategy:   "pull-request",
		Path:             "clusters/dev",
		SecretValue:      "test-token",
		Workspace:        filepath.Join(t.TempDir(), "worktree"),
		Commit:           true,
		Push:             true,
		PushRemote:       "origin",
		PushBranch:       "envpilot/pr-123",
		AuthorName:       "envpilot",
		AuthorEmail:      "envpilot@example.com",
		PullRequestAPI:   server.URL,
		PullRequestTitle: "EnvPlane pr-123",
		PullRequestBody:  "Generated for pr-123",
	})
	if err != nil {
		t.Fatalf("repository writer: %v", err)
	}
	if _, err := writer.WriteManifest(context.Background(), "feature-envs/checkout/pr-123/namespace.yaml", []byte("kind: Namespace\n"), "create"); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	result, err := writer.Commit(context.Background(), "envpilot: create pr-123")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !result.Committed || !result.Pushed {
		t.Fatalf("expected committed and pushed result, got %+v", result)
	}
	if result.PullRequestURL != "https://github.com/acme/gitops/pull/42" {
		t.Fatalf("unexpected pull request url: %q", result.PullRequestURL)
	}
	if gotPath != "/repos/owner/repo/pulls" {
		t.Fatalf("pull request path = %q", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("pull request auth = %q", gotAuth)
	}
	if gotPayload["head"] != "envpilot/pr-123" || gotPayload["base"] != "main" {
		t.Fatalf("pull request payload = %#v", gotPayload)
	}
}

func TestRepositoryCloneURLInjectsHTTPToken(t *testing.T) {
	cloneURL := repositoryCloneURL("https://github.com/acme/gitops.git", "token value")
	if !strings.Contains(cloneURL, "oauth2:token%20value@github.com") {
		t.Fatalf("clone url did not include encoded token: %q", cloneURL)
	}
}

func TestGitConflictOutputDetection(t *testing.T) {
	if !isGitConflictOutput("Updates were rejected because the tip of your current branch is behind its remote counterpart. Integrate the remote changes before pushing again. non-fast-forward") {
		t.Fatal("expected non-fast-forward output to be classified as conflict")
	}
}
