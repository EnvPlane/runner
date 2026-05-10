package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RepositoryTarget struct {
	URL               string
	Provider          string
	Branch            string
	BranchStrategy    string
	Path              string
	SecretValue       string
	Workspace         string
	Commit            bool
	Push              bool
	PushRemote        string
	PushBranch        string
	AuthorName        string
	AuthorEmail       string
	CreatePullRequest bool
	PullRequestTitle  string
	PullRequestBody   string
	PullRequestAPI    string
}

type RepositoryWriter struct {
	target RepositoryTarget
	once   sync.Once
	err    error
	writer FileWriter
}

func NewRepositoryWriter(target RepositoryTarget) (*RepositoryWriter, error) {
	target.URL = strings.TrimSpace(target.URL)
	target.Provider = strings.ToLower(strings.TrimSpace(target.Provider))
	target.Branch = strings.TrimSpace(target.Branch)
	target.BranchStrategy = normalizeBranchStrategy(target.BranchStrategy)
	target.Path = strings.Trim(strings.TrimSpace(target.Path), "/")
	target.Workspace = strings.TrimSpace(target.Workspace)
	if target.URL == "" {
		return nil, fmt.Errorf("gitops repository url is required")
	}
	if target.Workspace == "" {
		return nil, fmt.Errorf("gitops repository workspace is required")
	}
	if target.Branch == "" {
		target.Branch = "main"
	}
	if target.PushBranch == "" {
		target.PushBranch = target.Branch
	}
	if target.BranchStrategy == "pull-request" {
		target.CreatePullRequest = true
	}
	return &RepositoryWriter{target: target}, nil
}

func (w *RepositoryWriter) WriteManifest(ctx context.Context, filename string, content []byte, message string) (string, error) {
	if err := w.ensure(ctx); err != nil {
		return "", err
	}
	return w.writer.WriteManifest(ctx, filename, content, message)
}

func (w *RepositoryWriter) RemoveManifest(ctx context.Context, filename string, message string) error {
	if err := w.ensure(ctx); err != nil {
		return err
	}
	return w.writer.RemoveManifest(ctx, filename, message)
}

func (w *RepositoryWriter) RemovePath(ctx context.Context, path string, message string) error {
	if err := w.ensure(ctx); err != nil {
		return err
	}
	baseDir := filepath.Clean(w.writer.dir)
	if baseDir == "." {
		baseDir = filepath.Clean(w.target.Workspace)
	}
	repoRoot := filepath.Clean(w.target.Workspace)
	if repoRoot == "" || repoRoot == "." {
		return fmt.Errorf("gitops repository workspace is required")
	}
	baseRel, err := filepath.Rel(repoRoot, baseDir)
	if err != nil {
		return err
	}
	if baseRel == ".." || strings.HasPrefix(baseRel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("writer base path escapes gitops repository root")
	}
	requestedPath := strings.TrimSpace(path)
	if requestedPath == "" {
		return fmt.Errorf("path is required")
	}
	if filepath.IsAbs(requestedPath) {
		return fmt.Errorf("absolute paths are not allowed")
	}
	for _, part := range strings.Split(filepath.ToSlash(requestedPath), "/") {
		if part == ".." {
			return fmt.Errorf("path traversal is not allowed")
		}
	}
	targetPath := filepath.Clean(filepath.Join(baseDir, requestedPath))
	targetRel, err := filepath.Rel(repoRoot, targetPath)
	if err != nil {
		return err
	}
	if targetRel == "." {
		return fmt.Errorf("refusing to remove gitops repository root")
	}
	if targetRel == ".." || strings.HasPrefix(targetRel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes gitops repository root")
	}
	if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (w *RepositoryWriter) Commit(ctx context.Context, message string) (CommitResult, error) {
	if err := w.ensure(ctx); err != nil {
		return CommitResult{}, err
	}
	result, err := w.writer.Commit(ctx, message)
	if err != nil {
		return result, err
	}
	if !result.Committed || !w.target.CreatePullRequest {
		return result, nil
	}
	if strings.TrimSpace(w.target.SecretValue) == "" {
		return result, fmt.Errorf("gitops pull request token is required")
	}
	proposal, err := NewPullRequestService().Create(ctx, PullRequestRequest{
		Provider: w.target.Provider,
		APIBase:  w.target.PullRequestAPI,
		Token:    w.target.SecretValue,
		RepoURL:  w.target.URL,
		Title:    defaultString(w.target.PullRequestTitle, message),
		Body:     w.target.PullRequestBody,
		Head:     w.target.PushBranch,
		Base:     w.target.Branch,
	})
	if err != nil {
		return result, err
	}
	result.PullRequestURL = proposal.URL
	return result, nil
}

func (w *RepositoryWriter) ensure(ctx context.Context) error {
	w.once.Do(func() {
		w.err = w.prepare(ctx)
	})
	return w.err
}

func (w *RepositoryWriter) prepare(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(w.target.Workspace), 0o755); err != nil {
		return err
	}
	cloneURL := repositoryCloneURL(w.target.URL, w.target.SecretValue)
	if _, err := os.Stat(filepath.Join(w.target.Workspace, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.RemoveAll(w.target.Workspace); err != nil {
			return err
		}
		args := []string{"clone"}
		if w.target.Branch != "" {
			args = append(args, "--branch", w.target.Branch)
		}
		args = append(args, cloneURL, w.target.Workspace)
		if err := runGit(ctx, "", args...); err != nil {
			return err
		}
	} else {
		if err := runGit(ctx, w.target.Workspace, "fetch", "origin"); err != nil {
			return err
		}
	}

	if w.usesWorkBranch() {
		if err := runGit(ctx, w.target.Workspace, "checkout", "-B", w.target.PushBranch, "origin/"+w.target.Branch); err != nil {
			return err
		}
	} else {
		if err := runGit(ctx, w.target.Workspace, "checkout", w.target.Branch); err != nil {
			return err
		}
		if err := runGit(ctx, w.target.Workspace, "pull", "--ff-only", "origin", w.target.Branch); err != nil {
			return err
		}
	}

	writeDir := w.target.Workspace
	if w.target.Path != "" {
		writeDir = filepath.Join(writeDir, w.target.Path)
	}
	w.writer = NewGitSubdirWriter(writeDir, w.target.Workspace, w.target.Commit, w.target.Push, w.target.PushRemote, w.target.PushBranch, w.target.AuthorName, w.target.AuthorEmail)
	return nil
}

func (w *RepositoryWriter) usesWorkBranch() bool {
	return w.target.BranchStrategy == "branch" || w.target.BranchStrategy == "pull-request" || w.target.PushBranch != w.target.Branch
}

func RepositoryWorkspace(root string, repositoryURL string, branch string) string {
	identity := strings.TrimSpace(repositoryURL) + "\x00" + strings.TrimSpace(branch)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(root, hex.EncodeToString(sum[:])[:16])
}

func repositoryCloneURL(rawURL string, secretValue string) string {
	secretValue = strings.TrimSpace(secretValue)
	if secretValue == "" {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return rawURL
	}
	parsed.User = url.UserPassword("oauth2", secretValue)
	return parsed.String()
}

func normalizeBranchStrategy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "direct", "main":
		return "direct"
	case "branch", "environment-branch", "env-branch":
		return "branch"
	case "pull-request", "pr", "merge-request", "mr":
		return "pull-request"
	default:
		return "direct"
	}
}
