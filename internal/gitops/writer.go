package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Writer interface {
	WriteManifest(ctx context.Context, filename string, content []byte, message string) (string, error)
	RemoveManifest(ctx context.Context, filename string, message string) error
	RemovePath(ctx context.Context, path string, message string) error
	Commit(ctx context.Context, message string) (CommitResult, error)
}

type FileWriter struct {
	dir       string
	commit    bool
	committer *CommitService
}

func NewFileWriter(dir string, commit bool, authorName string, authorEmail string) FileWriter {
	return FileWriter{dir: dir, commit: commit}
}

func NewGitWriter(dir string, commit bool, push bool, remote string, branch string, authorName string, authorEmail string) FileWriter {
	return NewGitSubdirWriter(dir, dir, commit, push, remote, branch, authorName, authorEmail)
}

func NewGitSubdirWriter(dir string, commitDir string, commit bool, push bool, remote string, branch string, authorName string, authorEmail string) FileWriter {
	return NewGitSubdirWriterWithSecret(dir, commitDir, commit, push, remote, branch, authorName, authorEmail, "")
}

func NewGitSubdirWriterWithSecret(dir string, commitDir string, commit bool, push bool, remote string, branch string, authorName string, authorEmail string, secret string) FileWriter {
	return FileWriter{
		dir:       dir,
		commit:    commit,
		committer: NewCommitServiceWithSecret(commitDir, push, remote, branch, authorName, authorEmail, secret),
	}
}

func (w FileWriter) WriteManifest(_ context.Context, filename string, content []byte, _ string) (string, error) {
	path, err := resolveInsideRoot(w.dir, filename)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (w FileWriter) RemoveManifest(_ context.Context, filename string, _ string) error {
	path, err := resolveInsideRoot(w.dir, filename)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (w FileWriter) RemovePath(_ context.Context, path string, _ string) error {
	fullPath, err := resolveInsideRoot(w.dir, path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(fullPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func resolveInsideRoot(root string, relative string) (string, error) {
	root = filepath.Clean(root)
	relative = strings.TrimSpace(relative)
	if root == "" || root == "." || relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path must be relative to a writer root")
	}
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == ".." {
			return "", fmt.Errorf("path traversal is not allowed")
		}
	}
	path := filepath.Clean(filepath.Join(root, relative))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes writer root")
	}
	return path, nil
}

func (w FileWriter) Commit(ctx context.Context, message string) (CommitResult, error) {
	if !w.commit {
		return CommitResult{}, nil
	}
	return w.committer.Commit(ctx, message)
}
