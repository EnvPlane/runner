package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/envpilot/contracts/domain"
)

func TestJSONStorePersistsEnvironments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environments.json")
	first, err := NewJSONStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	env := domain.Environment{
		ID:        "kan-1701",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "kan-1701-cms",
		Mode:      domain.ModeHybrid,
		Status:    domain.StatusCreating,
		Source: domain.SCMSource{
			PullRequestID: "1701",
			Branch:        "feature/kan-1701",
			Commit:        "abc123",
		},
		TTLHours:  48,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := first.Save(env); err != nil {
		t.Fatalf("save: %v", err)
	}

	second, err := NewJSONStore(path)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	got, err := second.Get("kan-1701")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Namespace != "kan-1701-cms" {
		t.Fatalf("unexpected namespace: %s", got.Namespace)
	}
	record, err := second.GetRecord("kan-1701")
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if record.ID != "kan-1701" {
		t.Fatalf("record id = %q", record.ID)
	}
	if record.ProjectID != "cms" {
		t.Fatalf("project_id = %q", record.ProjectID)
	}
	if record.PRID != "1701" {
		t.Fatalf("pr_id = %q", record.PRID)
	}
	if record.Branch != "feature/kan-1701" {
		t.Fatalf("branch = %q", record.Branch)
	}
	if record.CommitSHA != "abc123" {
		t.Fatalf("commit_sha = %q", record.CommitSHA)
	}
	if record.Status != domain.StatusCreating {
		t.Fatalf("status = %q", record.Status)
	}
	if record.Type != domain.ModeHybrid {
		t.Fatalf("type = %q", record.Type)
	}
	if record.TTL != 48 {
		t.Fatalf("ttl = %d", record.TTL)
	}
	if record.CreatedAt.IsZero() {
		t.Fatal("created_at is zero")
	}
}

func TestJSONProjectStorePersistsProjectReposAndBaseConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	first, err := NewJSONProjectStore(path, nil)
	if err != nil {
		t.Fatalf("new project store: %v", err)
	}
	project := domain.Project{
		ID:   "cms",
		Name: "CMS",
		GitRepo: domain.RepositoryRef{
			Provider:      "gitlab",
			URL:           "https://gitlab.example/cms.git",
			DefaultBranch: "main",
		},
		GitOpsRepo: domain.RepositoryRef{
			Provider:      "gitlab",
			URL:           "https://gitlab.example/features.git",
			DefaultBranch: "main",
			Path:          "/Users/alex/bh/CMS/features",
		},
		BaseEnvConfig: domain.BaseEnvConfig{
			EnvironmentID: "feature",
			Namespace:     "feature",
			Domain:        "feature.int",
			ConfigPath:    "/Users/alex/bh/CMS/env/ENV/feature",
			Services: []domain.BaseServiceRef{
				{Name: "mysql", Namespace: "feature"},
				{Name: "redis", Namespace: "feature"},
			},
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := first.Save(project); err != nil {
		t.Fatalf("save project: %v", err)
	}

	second, err := NewJSONProjectStore(path, nil)
	if err != nil {
		t.Fatalf("reload project store: %v", err)
	}
	got, err := second.Get("cms")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.GitRepo.URL != project.GitRepo.URL {
		t.Fatalf("git repo url = %q", got.GitRepo.URL)
	}
	if got.GitOpsRepo.URL != project.GitOpsRepo.URL {
		t.Fatalf("gitops repo url = %q", got.GitOpsRepo.URL)
	}
	if got.BaseEnvConfig.ConfigPath != project.BaseEnvConfig.ConfigPath {
		t.Fatalf("base env config path = %q", got.BaseEnvConfig.ConfigPath)
	}
	if len(got.BaseEnvConfig.Services) != 2 || got.BaseEnvConfig.Services[0].Name != "mysql" {
		t.Fatalf("base services = %#v", got.BaseEnvConfig.Services)
	}
}
