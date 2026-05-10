package gitops

import (
	"errors"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitServiceCommitsChangesIdempotently(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "manifest.yaml"), []byte("kind: Namespace\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	service := NewCommitService(repo, false, "", "main", "envpilot", "envpilot@example.com")
	first, err := service.Commit(context.Background(), "envpilot: test commit")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !first.Committed {
		t.Fatal("expected commit")
	}
	if first.CommitSHA == "" {
		t.Fatal("expected commit sha")
	}

	second, err := service.Commit(context.Background(), "envpilot: no-op")
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if second.Committed {
		t.Fatal("expected idempotent no-op commit")
	}
}

func TestCommitServicePushesConfiguredBranch(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "git", "init", "--bare", remote)

	repo := initRepo(t)
	run(t, repo, "git", "remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(repo, "manifest.yaml"), []byte("kind: Namespace\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	service := NewCommitService(repo, true, "origin", "envpilot/test", "envpilot", "envpilot@example.com")
	result, err := service.Commit(context.Background(), "envpilot: push test")
	if err != nil {
		t.Fatalf("commit push: %v", err)
	}
	if !result.Committed || !result.Pushed {
		t.Fatalf("expected committed and pushed, got %+v", result)
	}

	refs := output(t, remote, "git", "show-ref", "--heads")
	if !strings.Contains(refs, "refs/heads/envpilot/test") {
		t.Fatalf("expected pushed branch, refs:\n%s", refs)
	}
}

func TestCommitServicePushConflictReturnsConflictError(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "git", "init", "--bare", remote)

	seed := initRepo(t)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	run(t, seed, "git", "add", "README.md")
	run(t, seed, "git", "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "seed")
	run(t, seed, "git", "remote", "add", "origin", remote)
	run(t, seed, "git", "-c", "user.name=seed", "-c", "user.email=seed@example.com", "push", "origin", "main")

	first := filepath.Join(t.TempDir(), "first")
	run(t, "", "git", "clone", "-b", "main", remote, first)
	second := filepath.Join(t.TempDir(), "second")
	run(t, "", "git", "clone", "-b", "main", remote, second)

	if err := os.WriteFile(filepath.Join(first, "first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	run(t, first, "git", "add", "first.txt")
	run(t, first, "git", "-c", "user.name=first", "-c", "user.email=first@example.com", "commit", "-m", "first commit")
	run(t, first, "git", "push", "origin", "main")

	if err := os.WriteFile(filepath.Join(second, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}
	service := NewCommitService(second, true, "origin", "main", "envpilot", "envpilot@example.com")
	_, err := service.Commit(context.Background(), "envpilot: conflicting push")
	if err == nil {
		t.Fatal("expected conflict on push")
	}
	var conflictErr ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected gitops conflict error, got: %T %v", err, err)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run(t, repo, "git", "init")
	run(t, repo, "git", "checkout", "-b", "main")
	return repo
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}

func output(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
	return string(out)
}
