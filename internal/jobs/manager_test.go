package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"envpilot/internal/app"
	"envpilot/internal/catalog"
	"envpilot/internal/config"
	"envpilot/internal/domain"
	"envpilot/internal/gitops"
	"envpilot/internal/scm"
	"envpilot/internal/store"
)

func TestSubmitSCMEventOpenedQueuesCreateJob(t *testing.T) {
	manager, envStore := newTestManager(t)

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/kan-2301",
		ChangeID:  "2301",
		CommitSHA: "abc123",
	})
	if err != nil {
		t.Fatalf("submit event: %v", err)
	}

	if job.Type != TypeCreateEnvironment {
		t.Fatalf("job type = %q", job.Type)
	}
	if job.Status != StatusQueued {
		t.Fatalf("job status = %q", job.Status)
	}
	if manager.QueueDepth() != 1 {
		t.Fatalf("queue depth = %d", manager.QueueDepth())
	}
	queuedEnv, err := envStore.Get("pr-2301")
	if err != nil {
		t.Fatalf("expected environment record created before job processing: %v", err)
	}
	if queuedEnv.Status != domain.StatusCreating {
		t.Fatalf("queued environment status = %q", queuedEnv.Status)
	}
	if queuedEnv.ManifestPath != "" || queuedEnv.NamespaceManifestPath != "" {
		t.Fatalf("expected queued environment without rendered manifests, got namespace=%q flux=%q", queuedEnv.NamespaceManifestPath, queuedEnv.ManifestPath)
	}
	record, err := envStore.GetRecord("pr-2301")
	if err != nil {
		t.Fatalf("expected db record: %v", err)
	}
	if record.Status != domain.StatusCreating {
		t.Fatalf("record status = %q", record.Status)
	}
	if record.PRID != "2301" || record.Branch != "feature/kan-2301" || record.CommitSHA != "abc123" {
		t.Fatalf("unexpected record source fields: pr=%q branch=%q commit=%q", record.PRID, record.Branch, record.CommitSHA)
	}

	processed, err := manager.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatal("expected queued job to be processed")
	}
	completed, ok := manager.Get(job.ID)
	if !ok {
		t.Fatalf("job not found")
	}
	if completed.Status != StatusSucceeded {
		t.Fatalf("completed job status = %q", completed.Status)
	}
	if _, err := envStore.Get("pr-2301"); err != nil {
		t.Fatalf("expected environment created: %v", err)
	}
	env, err := envStore.Get("pr-2301")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if env.Mode != domain.ModeFull {
		t.Fatalf("expected full environment mode from PR, got %q", env.Mode)
	}
	if env.Namespace != "envpilot-pr-2301" {
		t.Fatalf("namespace = %q", env.Namespace)
	}
	if env.NamespaceManifestPath == "" || env.ManifestPath == "" {
		t.Fatalf("expected namespace and flux manifests, got namespace=%q flux=%q", env.NamespaceManifestPath, env.ManifestPath)
	}
	if env.Domain != "pr-2301.repo.preview.feature.int" {
		t.Fatalf("domain = %q", env.Domain)
	}
	if env.URL != "https://pr-2301.repo.preview.feature.int" {
		t.Fatalf("url = %q", env.URL)
	}
}

func TestSubmitSCMEventResolvesProjectBindingByRepository(t *testing.T) {
	manager, envStore, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                 "checkout",
			Name:               "Checkout",
			ProductID:          "bethunder",
			AppRepositoryID:    "example/checkout",
			GitOpsRepositoryID: "platform-gitops",
		},
	})

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "example/checkout",
		Branch:    "feature/payment",
		ChangeID:  "2310",
		CommitSHA: "abc123",
	})
	if err != nil {
		t.Fatalf("submit event: %v", err)
	}
	if job.Request.Project != "checkout" {
		t.Fatalf("job project = %q", job.Request.Project)
	}
	if job.Request.Product != "bethunder" {
		t.Fatalf("job product = %q", job.Request.Product)
	}
	env, err := envStore.Get("pr-2310")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected env binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestSubmitSCMEventResolvesProjectBindingByIntegrationID(t *testing.T) {
	manager, envStore, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                    "checkout",
			Name:                  "Checkout",
			ProductID:             "bethunder",
			AppRepositoryID:       "another/repo",
			GitOpsRepositoryID:    "platform-gitops",
			GitHubInstallationIDs: []string{"123"},
			GitLabProjectIDs:      []string{"321"},
			WebhookAllowDraftPRs:  true,
		},
	})

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:       scm.ProviderGitHub,
		Action:         scm.ActionOpen,
		Repo:           "unmatched/repo",
		Branch:         "feature/payment",
		ChangeID:       "2311",
		CommitSHA:      "abc123",
		InstallationID: "123",
	})
	if err != nil {
		t.Fatalf("submit event: %v", err)
	}
	if job.Request.Project != "checkout" || job.Request.Product != "bethunder" {
		t.Fatalf("unexpected binding: project=%q product=%q", job.Request.Project, job.Request.Product)
	}
	env, err := envStore.Get("pr-2311")
	if err != nil {
		t.Fatalf("expected environment reserved: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected env binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestSubmitSCMEventResolvesProjectBindingByIntegrationIDScopedByProvider(t *testing.T) {
	manager, envStore, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                    "checkout-gh",
			Name:                  "Checkout GitHub",
			ProductID:             "bethunder",
			AppRepositoryID:       "other/repo",
			GitOpsRepositoryID:    "platform-gitops",
			GitHubInstallationIDs: []string{"123"},
			WebhookAllowDraftPRs:  true,
		},
		{
			ID:                   "checkout-gl",
			Name:                 "Checkout GitLab",
			ProductID:            "bethunder",
			AppRepositoryID:      "other/repo",
			GitOpsRepositoryID:   "platform-gitops",
			GitLabProjectIDs:     []string{"123"},
			WebhookAllowDraftPRs: true,
		},
	})

	githubJob, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:       scm.ProviderGitHub,
		Action:         scm.ActionOpen,
		Repo:           "unmatched/repo",
		Branch:         "feature/github",
		ChangeID:       "2600",
		CommitSHA:      "abc123",
		InstallationID: "123",
	})
	if err != nil {
		t.Fatalf("github submit: %v", err)
	}
	if githubJob.Request.Project != "checkout-gh" {
		t.Fatalf("unexpected github binding: project=%q", githubJob.Request.Project)
	}
	githubEnv, err := envStore.Get("pr-2600")
	if err != nil {
		t.Fatalf("github env: %v", err)
	}
	if githubEnv.Project != "checkout-gh" {
		t.Fatalf("github env project=%q", githubEnv.Project)
	}

	gitlabJob, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:       scm.ProviderGitLab,
		Action:         scm.ActionOpen,
		Repo:           "unmatched/repo",
		Branch:         "feature/gitlab",
		ChangeID:       "2601",
		CommitSHA:      "abc456",
		InstallationID: "123",
	})
	if err != nil {
		t.Fatalf("gitlab submit: %v", err)
	}
	if gitlabJob.Request.Project != "checkout-gl" {
		t.Fatalf("unexpected gitlab binding: project=%q", gitlabJob.Request.Project)
	}
	gitlabEnv, err := envStore.Get("mr-2601")
	if err != nil {
		t.Fatalf("gitlab env: %v", err)
	}
	if gitlabEnv.Project != "checkout-gl" {
		t.Fatalf("gitlab env project=%q", gitlabEnv.Project)
	}
}

func TestSubmitSCMEventResolvesProjectBindingByGitLabIntegrationIDWithNormalization(t *testing.T) {
	manager, _, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                 "cms",
			Name:               "CMS",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/cms",
			GitOpsRepositoryID: "platform-gitops",
			GitLabProjectIDs:   []string{"  777  "},
		},
	})

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:       scm.ProviderGitLab,
		Action:         scm.ActionOpen,
		Repo:           "other/group",
		Branch:         "feature/cms",
		ChangeID:       "4501",
		CommitSHA:      "abc333",
		InstallationID: " 777 ",
	})
	if err != nil {
		t.Fatalf("submit event: %v", err)
	}
	if job.Request.Project != "cms" || job.Request.Product != "bethunder" {
		t.Fatalf("unexpected binding: project=%q product=%q", job.Request.Project, job.Request.Product)
	}
}

func TestSubmitSCMEventResolvesProjectBindingByGitLabProjectID(t *testing.T) {
	manager, _, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                 "cms",
			Name:               "CMS",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/cms",
			GitOpsRepositoryID: "platform-gitops",
			GitLabProjectIDs:   []string{"555"},
		},
	})

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:       scm.ProviderGitLab,
		Action:         scm.ActionOpen,
		Repo:           "other/group",
		Branch:         "feature/cms",
		ChangeID:       "4500",
		CommitSHA:      "abc333",
		InstallationID: "555",
	})
	if err != nil {
		t.Fatalf("submit event: %v", err)
	}
	if job.Request.Project != "cms" || job.Request.Product != "bethunder" {
		t.Fatalf("unexpected binding: project=%q product=%q", job.Request.Project, job.Request.Product)
	}
}

func TestSubmitSCMEventResolvesProjectBindingByRepositoryProvider(t *testing.T) {
	manager, _, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                 "github",
			Name:               "Github",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			GitRepo:            domain.RepositoryRef{Provider: "github", URL: "https://github.com/owner/repo"},
		},
		{
			ID:                 "gitlab",
			Name:               "GitLab",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			GitRepo:            domain.RepositoryRef{Provider: "gitlab", URL: "https://gitlab.com/owner/repo"},
		},
	})

	gitHubJob, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/github",
		ChangeID:  "2420",
		CommitSHA: "abc123",
	})
	if err != nil {
		t.Fatalf("github submit: %v", err)
	}
	if gitHubJob.Request.Project != "github" {
		t.Fatalf("expected github project, got %q", gitHubJob.Request.Project)
	}

	gitLabJob, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitLab,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/gitlab",
		ChangeID:  "2421",
		CommitSHA: "abc456",
	})
	if err != nil {
		t.Fatalf("gitlab submit: %v", err)
	}
	if gitLabJob.Request.Project != "gitlab" {
		t.Fatalf("expected gitlab project, got %q", gitLabJob.Request.Project)
	}
}

func TestSubmitSCMEventRejectsDuplicateWebhookEvent(t *testing.T) {
	manager, _ := newTestManager(t)
	event := scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/dup",
		ChangeID:  "2501",
		CommitSHA: "abc123",
		EventID:   "evt-dup-2501",
	}

	first, err := manager.SubmitSCMEvent(context.Background(), event)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, err := manager.SubmitSCMEvent(context.Background(), event)
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate event should return same job, got %q and %q", first.ID, second.ID)
	}
}

func TestSubmitSCMEventRejectsDuplicateWebhookEventWithoutEventID(t *testing.T) {
	manager, _ := newTestManager(t)
	event := scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/dup",
		ChangeID:  "2502",
		CommitSHA: "abc123",
	}

	first, err := manager.SubmitSCMEvent(context.Background(), event)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, err := manager.SubmitSCMEvent(context.Background(), event)
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate event without EventID should return same job, got %q and %q", first.ID, second.ID)
	}
}

func TestSubmitSCMEventSupportsBranchFiltersAndLabelPolicy(t *testing.T) {
	manager, envStore, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                 "checkout",
			Name:               "Checkout",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			WebhookBranchFilters: []string{
				"release/*",
			},
			WebhookLabels: []string{
				"approved",
			},
			WebhookAllowDraftPRs: true,
		},
	})

	matching, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "release/payment",
		ChangeID:  "2312",
		CommitSHA: "abc123",
		Labels:    []string{"approved", "infra"},
	})
	if err != nil {
		t.Fatalf("matching event: %v", err)
	}
	if matching.Status != StatusQueued {
		t.Fatalf("matching event status = %q", matching.Status)
	}
	if _, err := envStore.Get("pr-2312"); err != nil {
		t.Fatalf("expected reserved env for matching event: %v", err)
	}

	ignoredBranch, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/payment",
		ChangeID:  "2313",
		CommitSHA: "def456",
		Labels:    []string{"approved"},
	})
	if err != nil {
		t.Fatalf("ignored branch submit: %v", err)
	}
	if ignoredBranch.Status != StatusIgnored {
		t.Fatalf("expected ignored status for branch filter miss, got %q", ignoredBranch.Status)
	}

	ignoredLabel, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "release/payment",
		ChangeID:  "2314",
		CommitSHA: "abc999",
		Labels:    []string{"chore"},
	})
	if err != nil {
		t.Fatalf("ignored label submit: %v", err)
	}
	if ignoredLabel.Status != StatusIgnored {
		t.Fatalf("expected ignored status for missing label, got %q", ignoredLabel.Status)
	}
}

func TestSubmitSCMEventSupportsWildcardLabelPolicy(t *testing.T) {
	manager, _, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                 "checkout",
			Name:               "Checkout",
			ProductID:          "bethunder",
			AppRepositoryID:    "owner/repo",
			GitOpsRepositoryID: "platform-gitops",
			WebhookLabels: []string{
				"release-*",
			},
		},
	})

	matched, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/payment",
		ChangeID:  "2402",
		CommitSHA: "abc123",
		Labels:    []string{"release-candidate"},
	})
	if err != nil {
		t.Fatalf("wildcard label submit: %v", err)
	}
	if matched.Status != StatusQueued {
		t.Fatalf("expected queued status for wildcard label match, got %q", matched.Status)
	}

	nonMatched, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/payment",
		ChangeID:  "2403",
		CommitSHA: "def456",
		Labels:    []string{"hotfix"},
	})
	if err != nil {
		t.Fatalf("non wildcard label submit: %v", err)
	}
	if nonMatched.Status != StatusIgnored {
		t.Fatalf("expected ignored status for wildcard mismatch, got %q", nonMatched.Status)
	}
}

func TestSubmitSCMEventRespectsDraftPolicy(t *testing.T) {
	manager, _, _ := newTestManagerWithProjectResolver(t, []domain.Project{
		{
			ID:                   "checkout",
			Name:                 "Checkout",
			ProductID:            "bethunder",
			AppRepositoryID:      "owner/repo",
			GitOpsRepositoryID:   "platform-gitops",
			WebhookAllowDraftPRs: false,
		},
	})

	draft, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/payment",
		ChangeID:  "2315",
		CommitSHA: "abc123",
		Draft:     true,
	})
	if err != nil {
		t.Fatalf("draft submit: %v", err)
	}
	if draft.Status != StatusIgnored {
		t.Fatalf("expected ignored draft event, got %q", draft.Status)
	}

	ready, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider:  scm.ProviderGitHub,
		Action:    scm.ActionOpen,
		Repo:      "owner/repo",
		Branch:    "feature/payment",
		ChangeID:  "2316",
		CommitSHA: "abc124",
		Draft:     false,
	})
	if err != nil {
		t.Fatalf("ready submit: %v", err)
	}
	if ready.Status != StatusQueued {
		t.Fatalf("expected queued non-draft event, got %q", ready.Status)
	}
}

func TestSubmitSCMEventClosedQueuesDeleteJob(t *testing.T) {
	manager, envStore, gitopsDir := newTestManagerWithDir(t)
	createJob, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider: scm.ProviderGitLab,
		Action:   scm.ActionOpen,
		Repo:     "owner/repo",
		Branch:   "feature/kan-2302",
		ChangeID: "2302",
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	if _, err := manager.ProcessNext(context.Background()); err != nil {
		t.Fatalf("process create job %s: %v", createJob.ID, err)
	}
	envPath := filepath.Join(gitopsDir, "feature-envs", "repo", "mr-2302")
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("expected gitops env path after create: %v", err)
	}

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider: scm.ProviderGitLab,
		Action:   scm.ActionClose,
		Repo:     "owner/repo",
		Branch:   "feature/kan-2302",
		ChangeID: "2302",
	})
	if err != nil {
		t.Fatalf("close event: %v", err)
	}

	if job.Type != TypeDeleteEnvironment {
		t.Fatalf("job type = %q", job.Type)
	}
	if job.Status != StatusQueued {
		t.Fatalf("job status = %q", job.Status)
	}
	if _, err := manager.ProcessNext(context.Background()); err != nil {
		t.Fatalf("process delete job: %v", err)
	}
	completed, ok := manager.Get(job.ID)
	if !ok {
		t.Fatal("delete job not found")
	}
	if completed.Status != StatusSucceeded {
		t.Fatalf("completed job status = %q", completed.Status)
	}
	env, err := envStore.Get("mr-2302")
	if err != nil {
		t.Fatalf("expected environment record: %v", err)
	}
	if env.Status != domain.StatusTerminated {
		t.Fatalf("environment status = %q", env.Status)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatalf("expected gitops env path removed, err=%v", err)
	}
}

func TestJobRetrySupport(t *testing.T) {
	executor := &flakyExecutor{failuresRemaining: 1}
	manager := NewManager(executor, WithRetryDelay(0), WithMaxAttempts(3))

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider: scm.ProviderGitHub,
		Action:   scm.ActionOpen,
		Repo:     "owner/repo",
		Branch:   "feature/kan-2401",
		ChangeID: "2401",
	})
	if err != nil {
		t.Fatalf("submit event: %v", err)
	}

	processed, err := manager.ProcessNext(context.Background())
	if !processed {
		t.Fatal("expected first attempt")
	}
	if err == nil {
		t.Fatal("expected first attempt to fail")
	}
	firstAttempt, _ := manager.Get(job.ID)
	if firstAttempt.Status != StatusQueued {
		t.Fatalf("expected retry to requeue job, got %q", firstAttempt.Status)
	}
	if firstAttempt.Attempts != 1 {
		t.Fatalf("attempts = %d", firstAttempt.Attempts)
	}

	processed, err = manager.ProcessNext(context.Background())
	if err != nil {
		t.Fatalf("expected retry success: %v", err)
	}
	if !processed {
		t.Fatal("expected retry attempt")
	}
	completed, _ := manager.Get(job.ID)
	if completed.Status != StatusSucceeded {
		t.Fatalf("final status = %q", completed.Status)
	}
	if completed.Attempts != 2 {
		t.Fatalf("attempts = %d", completed.Attempts)
	}
}

func TestRecoverRequeuesQueuedAndRunningJobs(t *testing.T) {
	jobStore := NewMemoryStore()
	createdAt := time.Now().UTC().Add(-time.Minute)
	if err := jobStore.Save(Job{
		ID:            "job-000009",
		Type:          TypeCreateEnvironment,
		Status:        StatusQueued,
		EnvironmentID: "pr-900",
		Request:       domain.CreateEnvironmentRequest{ID: "pr-900"},
		MaxAttempts:   3,
		CreatedAt:     createdAt,
	}); err != nil {
		t.Fatalf("save queued job: %v", err)
	}
	startedAt := createdAt.Add(10 * time.Second)
	if err := jobStore.Save(Job{
		ID:            "job-000010",
		Type:          TypeDeleteEnvironment,
		Status:        StatusRunning,
		EnvironmentID: "pr-901",
		Request:       domain.CreateEnvironmentRequest{ID: "pr-901"},
		MaxAttempts:   3,
		CreatedAt:     createdAt,
		StartedAt:     &startedAt,
	}); err != nil {
		t.Fatalf("save running job: %v", err)
	}

	manager := NewManager(&flakyExecutor{}, WithStore(jobStore))
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if manager.QueueDepth() != 2 {
		t.Fatalf("queue depth = %d", manager.QueueDepth())
	}

	recovered, ok := manager.Get("job-000010")
	if !ok {
		t.Fatal("expected recovered running job")
	}
	if recovered.Status != StatusQueued || recovered.StartedAt != nil || recovered.Error == "" {
		t.Fatalf("unexpected recovered job: %+v", recovered)
	}

	job, err := manager.SubmitSCMEvent(context.Background(), scm.PullRequestEvent{
		Provider: scm.ProviderGitHub,
		Action:   scm.ActionOpen,
		Repo:     "owner/repo",
		ChangeID: "902",
	})
	if err != nil {
		t.Fatalf("submit after recovery: %v", err)
	}
	if job.ID != "job-000011" {
		t.Fatalf("job id after recovery = %q", job.ID)
	}
}

func TestManualRetryRequeuesFailedJob(t *testing.T) {
	jobStore := NewMemoryStore()
	if err := jobStore.Save(Job{
		ID:            "job-000001",
		Type:          TypeCreateEnvironment,
		Status:        StatusFailed,
		EnvironmentID: "pr-999",
		Request:       domain.CreateEnvironmentRequest{ID: "pr-999"},
		Error:         "boom",
		Attempts:      3,
		MaxAttempts:   3,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save failed job: %v", err)
	}
	manager := NewManager(&flakyExecutor{}, WithStore(jobStore))

	job, err := manager.Retry(context.Background(), "job-000001")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if job.Status != StatusQueued || job.Error != "" {
		t.Fatalf("unexpected retry job: %+v", job)
	}
	if manager.QueueDepth() != 1 {
		t.Fatalf("queue depth = %d", manager.QueueDepth())
	}
}

func newTestManager(t *testing.T) (*Manager, store.EnvironmentStore) {
	manager, envStore, _ := newTestManagerWithDir(t)
	return manager, envStore
}

func newTestManagerWithDir(t *testing.T) (*Manager, store.EnvironmentStore, string) {
	return newTestManagerWithProjectResolver(t, nil)
}

func newTestManagerWithProjectResolver(t *testing.T, projects []domain.Project) (*Manager, store.EnvironmentStore, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.FromEnv()
	cfg.DataDir = tmp
	cfg.GitOpsDir = tmp
	cfg.DefaultTTL = time.Hour

	envStore, err := store.NewJSONStore(tmp + "/environments.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	service := app.NewEnvironmentService(cfg, catalog.Default(), envStore, gitops.NewFluxRenderer(cfg.GitOps), gitops.NewFileWriter(tmp, false, "", ""))
	options := []Option{WithRetryDelay(0)}
	if projects != nil {
		projectStore, err := store.NewJSONProjectStore(tmp+"/projects.json", projects)
		if err != nil {
			t.Fatalf("project store: %v", err)
		}
		options = append(options, WithProjectResolver(app.NewProjectService(projectStore)))
	}
	return NewManager(service, options...), envStore, tmp
}

type flakyExecutor struct {
	failuresRemaining int
}

func (f *flakyExecutor) CreateEnvironment(_ context.Context, req domain.CreateEnvironmentRequest) (domain.Environment, error) {
	if f.failuresRemaining > 0 {
		f.failuresRemaining--
		return domain.Environment{}, errors.New("temporary failure")
	}
	return domain.Environment{
		ID:        req.ID,
		Project:   req.Project,
		Mode:      req.Mode,
		Status:    domain.StatusCreating,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (f *flakyExecutor) DeleteEnvironment(_ context.Context, id string, _ bool) (domain.Environment, error) {
	return domain.Environment{ID: id, Status: domain.StatusTerminated}, nil
}

func (f *flakyExecutor) GetEnvironment(id string) (domain.Environment, error) {
	return domain.Environment{ID: id, Status: domain.StatusCreating}, nil
}
