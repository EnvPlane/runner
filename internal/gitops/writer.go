package gitops

import (
	"context"
	"os"
	"path/filepath"
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
	return FileWriter{
		dir:       dir,
		commit:    commit,
		committer: NewCommitService(commitDir, push, remote, branch, authorName, authorEmail),
	}
}

func (w FileWriter) WriteManifest(_ context.Context, filename string, content []byte, _ string) (string, error) {
	path := filepath.Join(w.dir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (w FileWriter) RemoveManifest(_ context.Context, filename string, _ string) error {
	path := filepath.Join(w.dir, filename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (w FileWriter) RemovePath(_ context.Context, path string, _ string) error {
	fullPath := filepath.Join(w.dir, path)
	if err := os.RemoveAll(fullPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (w FileWriter) Commit(ctx context.Context, message string) (CommitResult, error) {
	if !w.commit {
		return CommitResult{}, nil
	}
	return w.committer.Commit(ctx, message)
}
