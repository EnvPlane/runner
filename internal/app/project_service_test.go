package app

import (
	"path/filepath"
	"testing"

	"envpilot/internal/domain"
	"envpilot/internal/store"
)

func TestNormalizeRepositoryIdentity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "https url",
			input:    "https://github.com/owner/repo.git",
			expected: "owner/repo",
		},
		{
			name:     "http url with www host",
			input:    "http://www.GitHub.com/Owner/Repo/",
			expected: "owner/repo",
		},
		{
			name:     "git+ssh scheme",
			input:    "git+ssh://git@github.com/owner/repo.git",
			expected: "owner/repo",
		},
		{
			name:     "git scheme",
			input:    "git://github.com/owner/repo.git",
			expected: "owner/repo",
		},
		{
			name:     "scp-style git path",
			input:    "git@github.com/owner/repo.git",
			expected: "owner/repo",
		},
		{
			name:     "scp-style with colon and slash",
			input:    "git@bitbucket.org/team/repo.git",
			expected: "team/repo",
		},
		{
			name:     "plain path",
			input:    "owner/repo",
			expected: "owner/repo",
		},
		{
			name:     "uppercase and whitespace",
			input:    "  OWNER/Repo  ",
			expected: "owner/repo",
		},
		{
			name:     "slashes with backslashes",
			input:    "Owner\\Repo",
			expected: "owner/repo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeRepositoryIdentity(tc.input)
			if got != tc.expected {
				t.Fatalf("normalizeRepositoryIdentity(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestResolveProjectByRepositoryConsidersProvider(t *testing.T) {
	t.Parallel()

	projectStore, err := store.NewJSONProjectStore(filepath.Join(t.TempDir(), "projects.json"), []domain.Project{
		{
			ID:                 "gitlab",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			GitRepo:            domain.RepositoryRef{Provider: "gitlab", URL: "https://gitlab.example/owner/repo"},
		},
		{
			ID:                 "github",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			GitRepo:            domain.RepositoryRef{Provider: "github", URL: "https://github.com/owner/repo"},
		},
	})
	if err != nil {
		t.Fatalf("new project store: %v", err)
	}

	service := NewProjectService(projectStore)

	githubProject, ok, err := service.ResolveProjectByRepository("github", "owner/repo")
	if err != nil {
		t.Fatalf("github resolve: %v", err)
	}
	if !ok || githubProject.ID != "github" {
		t.Fatalf("expected github project, got ok=%v project=%q", ok, githubProject.ID)
	}

	gitlabProject, ok, err := service.ResolveProjectByRepository("gitlab", "owner/repo")
	if err != nil {
		t.Fatalf("gitlab resolve: %v", err)
	}
	if !ok || gitlabProject.ID != "gitlab" {
		t.Fatalf("expected gitlab project, got ok=%v project=%q", ok, gitlabProject.ID)
	}
}

func TestNormalizeRepositoryRefInfersProviderFromURL(t *testing.T) {
	t.Parallel()

	ref := normalizeRepositoryRef(domain.RepositoryRef{
		Provider: "  ",
		URL:      "git@github.com:owner/repo.git",
	})
	if ref.Provider != "github" {
		t.Fatalf("expected provider github, got %q", ref.Provider)
	}

	ref = normalizeRepositoryRef(domain.RepositoryRef{
		Provider: "  ",
		URL:      "https://gitlab.com/owner/repo.git",
	})
	if ref.Provider != "gitlab" {
		t.Fatalf("expected provider gitlab, got %q", ref.Provider)
	}
}

func TestResolveProjectByRepositoryInfersProviderFromRepositoryURL(t *testing.T) {
	t.Parallel()

	projectStore, err := store.NewJSONProjectStore(filepath.Join(t.TempDir(), "projects.json"), []domain.Project{
		{
			ID:                 "inferred-github",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			GitRepo:            domain.RepositoryRef{URL: "git+ssh://git@github.com/owner/repo.git"},
		},
		{
			ID:                 "gitlab",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			GitRepo:            domain.RepositoryRef{Provider: "gitlab", URL: "https://gitlab.com/owner/repo"},
		},
	})
	if err != nil {
		t.Fatalf("new project store: %v", err)
	}

	service := NewProjectService(projectStore)

	githubProject, ok, err := service.ResolveProjectByRepository("github", "owner/repo")
	if err != nil {
		t.Fatalf("github resolve: %v", err)
	}
	if !ok || githubProject.ID != "inferred-github" {
		t.Fatalf("expected inferred-github project for github event, got ok=%v project=%q", ok, githubProject.ID)
	}

	gitlabProject, ok, err := service.ResolveProjectByRepository("gitlab", "owner/repo")
	if err != nil {
		t.Fatalf("gitlab resolve: %v", err)
	}
	if !ok || gitlabProject.ID != "gitlab" {
		t.Fatalf("expected explicit gitlab project for gitlab event, got ok=%v project=%q", ok, gitlabProject.ID)
	}
}
