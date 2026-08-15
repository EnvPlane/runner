package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type CommitResult struct {
	Committed      bool   `json:"committed"`
	Pushed         bool   `json:"pushed"`
	CommitSHA      string `json:"commitSha,omitempty"`
	Branch         string `json:"branch,omitempty"`
	PullRequestURL string `json:"pullRequestUrl,omitempty"`
}

type ConflictError struct {
	Operation string
	Message   string
}

func (e ConflictError) Error() string {
	if e.Operation == "" {
		return e.Message
	}
	return e.Operation + " conflict: " + e.Message
}

type CommitService struct {
	dir         string
	push        bool
	pushRemote  string
	pushBranch  string
	authorName  string
	authorEmail string
	secret      string
}

func NewCommitServiceWithSecret(dir string, push bool, remote string, branch string, authorName string, authorEmail string, secret string) *CommitService {
	service := NewCommitService(dir, push, remote, branch, authorName, authorEmail)
	service.secret = strings.TrimSpace(secret)
	return service
}

func NewCommitService(dir string, push bool, remote string, branch string, authorName string, authorEmail string) *CommitService {
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "main"
	}
	return &CommitService{
		dir:         dir,
		push:        push,
		pushRemote:  remote,
		pushBranch:  branch,
		authorName:  authorName,
		authorEmail: authorEmail,
	}
}

func (s *CommitService) Commit(ctx context.Context, message string) (CommitResult, error) {
	if message == "" {
		message = "envpilot: update gitops manifests"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := runGit(ctx, s.dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return CommitResult{}, err
	}
	if err := runGit(ctx, s.dir, "add", "-A", "."); err != nil {
		return CommitResult{}, err
	}
	changed, err := hasRepoChanges(ctx, s.dir)
	if err != nil {
		return CommitResult{}, err
	}
	if !changed {
		return CommitResult{Branch: s.pushBranch}, nil
	}

	if err := runGit(ctx, s.dir,
		"-c", "user.name="+s.authorName,
		"-c", "user.email="+s.authorEmail,
		"commit", "-m", message,
	); err != nil {
		return CommitResult{}, err
	}
	sha, err := gitOutput(ctx, s.dir, "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, err
	}

	result := CommitResult{
		Committed: true,
		CommitSHA: strings.TrimSpace(sha),
		Branch:    s.pushBranch,
	}
	if s.push {
		if err := runGitWithSecret(ctx, s.dir, s.secret, "push", s.pushRemote, "HEAD:"+s.pushBranch); err != nil {
			return result, err
		}
		result.Pushed = true
	}
	return result, nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	return runGitWithSecret(ctx, dir, "", args...)
}

func runGitWithSecret(ctx context.Context, dir string, secret string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cleanup := installGitAskPass(cmd, secret)
	defer cleanup()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if isGitConflictOutput(message) {
			return ConflictError{Operation: "git operation", Message: message}
		}
		return fmt.Errorf("git operation failed: %w: %s", err, message)
	}
	return nil
}

func installGitAskPass(cmd *exec.Cmd, secret string) func() {
	if strings.TrimSpace(secret) == "" {
		return func() {}
	}
	dir, err := os.MkdirTemp("", "envpilot-git-askpass-")
	if err != nil {
		return func() {}
	}
	script := filepath.Join(dir, "askpass.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' \"$GIT_USERNAME\" ;; *) printf '%s\\n' \"$GIT_PASSWORD\" ;; esac\n"), 0o700)
	cmd.Env = append(os.Environ(), "GIT_ASKPASS="+script, "GIT_USERNAME=oauth2", "GIT_PASSWORD="+secret, "GIT_TERMINAL_PROMPT=0")
	return func() { _ = os.RemoveAll(dir) }
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutputWithSecret(ctx, dir, "", args...)
}

func gitOutputWithSecret(ctx context.Context, dir string, secret string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cleanup := installGitAskPass(cmd, secret)
	defer cleanup()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if isGitConflictOutput(message) {
			return "", ConflictError{Operation: "git operation", Message: message}
		}
		return "", fmt.Errorf("git operation failed: %w: %s", err, message)
	}
	return stdout.String(), nil
}

func hasRepoChanges(ctx context.Context, dir string) (bool, error) {
	output, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

func isGitConflictOutput(output string) bool {
	output = strings.ToLower(output)
	for _, marker := range []string{
		"non-fast-forward",
		"fetch first",
		"not possible to fast-forward",
		"would be overwritten by checkout",
		"would be overwritten by merge",
		"merge conflict",
		"needs merge",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}
