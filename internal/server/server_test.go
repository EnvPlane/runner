package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/envpilot/contracts/domain"
	"github.com/envpilot/agent/agent"
	"github.com/envpilot/runner/internal/app"
	"github.com/envpilot/runner/internal/catalog"
	"github.com/envpilot/runner/internal/config"
	"github.com/envpilot/runner/internal/gitops"
	"github.com/envpilot/runner/internal/jobs"
	"github.com/envpilot/runner/internal/store"
)

func testRepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ".."
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func TestMain(m *testing.M) {
	_ = os.Setenv("ENVPLANE_DEPLOYMENT_BACKEND", "fluxcd")
	os.Exit(m.Run())
}

type fakeRoundTripper func(*http.Request) (*http.Response, error)

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func requireHTTPServer(t *testing.T) {
	t.Helper()
	if os.Getenv("ENVPLANE_RUN_HTTP_TESTS") != "1" && os.Getenv("ENVPLANE_RUN_NET_TESTS") != "1" {
		t.Skip("HTTP test server binding is disabled. Set ENVPLANE_RUN_HTTP_TESTS=1 (or ENVPLANE_RUN_NET_TESTS=1) to run.")
	}
}

func TestGitHubWebhookRejectsInvalidSignature(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayload("opened", "feature/kan-1901"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventWebhookAuthFailed)
	assertStandardAuditEvent(t, entry, auditEventWebhookAuthFailed, auditEndpointGitHubWebhook, "", "", true)
	requestEntry := findAuditEventEntry(t, entries, auditEventAPIRequest)
	assertStandardRequestAuditEvent(t, requestEntry, http.MethodPost, "/api/v1/webhooks/github", http.StatusUnauthorized, "", "failure", http.StatusText(http.StatusUnauthorized))
	if strings.Contains(mustReadFileString(t, logPath), "sha256=invalid") {
		t.Fatalf("audit log leaked raw webhook signature")
	}
}

func TestGitHubIssueCommentWebhookRejectsInvalidSignature(t *testing.T) {
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubIssueCommentPayload("1990", "/envpilot recreate"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookRejectsMalformedSignedPayload(t *testing.T) {
	application, _, _ := newTestServer(t, "secret")
	body := []byte(`{"not":`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookAcceptsPullRequestValidatesSignatureAndLogsMetadataOnly(t *testing.T) {
	application, envStore, logs := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayload("opened", "feature/kan-1902"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.Type != jobs.TypeCreateEnvironment {
		t.Fatalf("expected create job, got %q", job.Type)
	}
	if job.Status != jobs.StatusQueued {
		t.Fatalf("expected queued job, got %q", job.Status)
	}
	env, err := envStore.Get("pr-1902")
	if err != nil {
		t.Fatalf("expected environment record created from webhook: %v", err)
	}
	if env.Status != domain.StatusCreating {
		t.Fatalf("environment status = %q", env.Status)
	}
	if env.ManifestPath != "" || env.NamespaceManifestPath != "" {
		t.Fatalf("expected queued environment without rendered manifests, got namespace=%q flux=%q", env.NamespaceManifestPath, env.ManifestPath)
	}
	if !strings.Contains(logs.String(), "github webhook") {
		t.Fatalf("expected github webhook log, got %s", logs.String())
	}
	for _, expected := range []string{`"event":"pull_request"`, `"delivery":"delivery-1"`, `"action":"open"`, `"repo":"owner/repo"`, `"pr_number":1902`} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatalf("expected metadata %s in log, got %s", expected, logs.String())
		}
	}
	if strings.Contains(logs.String(), "feature/kan-1902") || strings.Contains(logs.String(), `"payload"`) {
		t.Fatalf("normal github webhook log must not contain raw payload body: %s", logs.String())
	}
}

func TestGitHubWebhookDebugPayloadLoggingCanBeEnabled(t *testing.T) {
	application, _, logs := newTestServer(t, "secret")
	cfg := application.config()
	cfg.GitHubWebhookDebugPayloadLog = true
	application.ReloadConfig(cfg)
	body := []byte(githubPullRequestPayload("opened", "feature/kan-debug-payload"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-debug")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), `"payload"`) || !strings.Contains(logs.String(), "feature/kan-debug-payload") {
		t.Fatalf("expected debug payload log when enabled, got %s", logs.String())
	}
}

func TestGitHubIssueCommentRecreateCreatesJob(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	body := []byte(githubIssueCommentPayload("1991", "/envpilot recreate"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "comment-recreate-1991")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.Type != jobs.TypeCreateEnvironment {
		t.Fatalf("expected create job, got %q", job.Type)
	}
	env, err := envStore.Get("pr-1991")
	if err != nil {
		t.Fatalf("expected environment record from recreate command: %v", err)
	}
	if env.Status != domain.StatusCreating {
		t.Fatalf("environment status = %q", env.Status)
	}
}

func TestGitHubIssueCommentDestroyCreatesDeleteJob(t *testing.T) {
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubIssueCommentPayload("1992", "/envpilot destroy"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "comment-destroy-1992")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.Type != jobs.TypeDeleteEnvironment {
		t.Fatalf("expected delete job, got %q", job.Type)
	}
}

func TestRecreateEnvironmentEndpointQueuesCreateJobWithOriginalIdentity(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	createBody := []byte(`{"id":"qa-dashboard-100","project":"demo-project","product":"generic","source":{"provider":"github","repository":"acme/demo","pullRequestId":"100","branch":"feature/refresh","commit":"abc123","author":"test","url":"https://github.com/acme/demo/pull/100"}}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	_, err := envStore.Get("qa-dashboard-100")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}

	recreateReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/qa-dashboard-100/recreate", nil)
	recreateReq.Header.Set("Content-Type", "application/json")
	recreateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(recreateRec, recreateReq)
	if recreateRec.Code != http.StatusAccepted {
		t.Fatalf("expected recreate 202, got %d: %s", recreateRec.Code, recreateRec.Body.String())
	}
	var recreateJob jobs.Job
	if err := json.Unmarshal(recreateRec.Body.Bytes(), &recreateJob); err != nil {
		t.Fatalf("decode recreate job: %v", err)
	}
	if recreateJob.Type != jobs.TypeCreateEnvironment {
		t.Fatalf("expected create job, got %q", recreateJob.Type)
	}
	if recreateJob.EnvironmentID != "qa-dashboard-100" {
		t.Fatalf("expected environmentId qa-dashboard-100, got %q", recreateJob.EnvironmentID)
	}
	if recreateJob.Request.ID != "qa-dashboard-100" {
		t.Fatalf("expected request id qa-dashboard-100, got %q", recreateJob.Request.ID)
	}
	if recreateJob.Request.Project != "demo-project" {
		t.Fatalf("expected request project demo-project, got %q", recreateJob.Request.Project)
	}
	if _, err := envStore.Get("pr-100"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no derived pr-100 environment, got err=%v", err)
	}
	items, err := envStore.List()
	if err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one environment after recreate, got %d", len(items))
	}

	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/qa-dashboard-100/recreate", nil)
	retryRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusAccepted {
		t.Fatalf("expected retry recreate 202, got %d: %s", retryRec.Code, retryRec.Body.String())
	}
	var retryJob jobs.Job
	if err := json.Unmarshal(retryRec.Body.Bytes(), &retryJob); err != nil {
		t.Fatalf("decode retry recreate job: %v", err)
	}
	if retryJob.ID != recreateJob.ID {
		t.Fatalf("expected idempotent recreate to return job %q, got %q", recreateJob.ID, retryJob.ID)
	}
	items, err = envStore.List()
	if err != nil {
		t.Fatalf("list environments after retry: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one environment after retry recreate, got %d", len(items))
	}
}

func TestGitHubIssueCommentPinPinsEnvironment(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	createBody := []byte(`{"id":"pr-1993","product":"bethunder","ttlHours":24}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	body := []byte(githubIssueCommentPayload("1993", "/envpilot pin 7d"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "comment-pin-1993")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Status      string             `json:"status"`
		Pin         string             `json:"pin"`
		Environment domain.Environment `json:"environment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode pin response: %v", err)
	}
	if payload.Pin != "7d" || payload.Environment.PinnedUntil == nil {
		t.Fatalf("unexpected pin response: %+v", payload)
	}
	env, err := envStore.Get("pr-1993")
	if err != nil {
		t.Fatalf("get pinned environment: %v", err)
	}
	if !env.Pinned || env.PinnedUntil == nil || env.ExpiresAt != nil {
		t.Fatalf("expected timed pinned env without expiration, got %+v", env)
	}
}

func TestAPIReadOnlyTokenAllowsReadOnlyRequestsAndRejectsWrites(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	application, _, _ := newTestServer(t, "")

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	listReq.Header.Set("Authorization", "Bearer readonly-token")
	listRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected read endpoint 200, got %d: %s", listRec.Code, listRec.Body.String())
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo", strings.NewReader(`{"name":"Demo"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer readonly-token")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusForbidden {
		t.Fatalf("expected write endpoint 403, got %d: %s", createRec.Code, createRec.Body.String())
	}
}

func TestAPIWriteTokenPermitsMutatingRequests(t *testing.T) {
	t.Setenv("ENVPLANE_API_WRITE_TOKEN", "write-token")
	application, _, _ := newTestServer(t, "")

	createReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/demo", strings.NewReader(`{"name":"Demo","product_id":"generic","git_repo":{"provider":"github","url":"https://example.com/repo.git","default_branch":"main"},"gitops_repository_id":"platform-gitops"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer write-token")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusOK {
		t.Fatalf("expected write endpoint 200, got %d: %s", createRec.Code, createRec.Body.String())
	}
}

func TestReconcileEndpointDeletesExpiredEnvironment(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	createBody := []byte(`{"id":"kan-ttl","product":"bethunder","ttlHours":1}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	env, err := envStore.Get("kan-ttl")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	expiredAt := time.Now().UTC().Add(-time.Hour)
	env.ExpiresAt = &expiredAt
	if err := envStore.Save(env); err != nil {
		t.Fatalf("save expired environment: %v", err)
	}

	reconcileReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/reconcile", nil)
	reconcileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(reconcileRec, reconcileReq)
	if reconcileRec.Code != http.StatusOK {
		t.Fatalf("expected reconcile 200, got %d: %s", reconcileRec.Code, reconcileRec.Body.String())
	}

	var payload struct {
		Deleted []domain.Environment `json:"deleted"`
	}
	if err := json.Unmarshal(reconcileRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode reconcile response: %v", err)
	}
	if len(payload.Deleted) != 1 || payload.Deleted[0].ID != "kan-ttl" {
		t.Fatalf("deleted = %#v", payload.Deleted)
	}
	stored, err := envStore.Get("kan-ttl")
	if err != nil {
		t.Fatalf("get reconciled environment: %v", err)
	}
	if stored.Status != domain.StatusTerminated {
		t.Fatalf("expected expired environment to terminate after cleanup, got %q", stored.Status)
	}
}

func TestManualDeleteEnvironmentEndpointTransitionsToTerminated(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	createBody := []byte(`{"id":"kan-delete","product":"bethunder"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/environments/kan-delete", nil)
	deleteRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("expected delete 200, got %d: %s", deleteRec.Code, deleteRec.Body.String())
	}

	var deleted domain.Environment
	if err := json.Unmarshal(deleteRec.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if deleted.ID != "kan-delete" || deleted.Status != domain.StatusTerminated {
		t.Fatalf("deleted environment = %+v", deleted)
	}
	stored, err := envStore.Get("kan-delete")
	if err != nil {
		t.Fatalf("get deleted environment: %v", err)
	}
	if stored.Status != domain.StatusTerminated {
		t.Fatalf("expected stored status terminated, got %q", stored.Status)
	}

	deleteAgainReq := httptest.NewRequest(http.MethodDelete, "/environments/kan-delete", nil)
	deleteAgainRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(deleteAgainRec, deleteAgainReq)
	if deleteAgainRec.Code != http.StatusOK {
		t.Fatalf("expected second delete 200, got %d: %s", deleteAgainRec.Code, deleteAgainRec.Body.String())
	}
	var deletedAgain domain.Environment
	if err := json.Unmarshal(deleteAgainRec.Body.Bytes(), &deletedAgain); err != nil {
		t.Fatalf("decode second delete response: %v", err)
	}
	if deletedAgain.Status != domain.StatusTerminated {
		t.Fatalf("expected second delete to remain terminated, got %q", deletedAgain.Status)
	}
}

func TestDashboardSummaryExcludesTerminatingAndTerminatedCost(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	now := time.Now().UTC()
	items := []domain.Environment{
		{ID: "kan-ready", Project: "default", Status: domain.StatusReady, CostEstimateDay: "~ €1.20/day", CreatedAt: now, UpdatedAt: now},
		{ID: "kan-terminating", Project: "default", Status: domain.StatusTerminating, CostEstimateDay: "~ €1.20/day", CreatedAt: now, UpdatedAt: now},
		{ID: "kan-terminated", Project: "default", Status: domain.StatusTerminated, CostEstimateDay: "~ €1.20/day", CreatedAt: now, UpdatedAt: now},
	}
	for _, item := range items {
		if err := envStore.Save(item); err != nil {
			t.Fatalf("save %s: %v", item.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/summary", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected summary 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var summary struct {
		ActiveEnvironments int    `json:"activeEnvironments"`
		EstimatedDailyCost string `json:"estimatedDailyCost"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.ActiveEnvironments != 1 {
		t.Fatalf("active environments = %d", summary.ActiveEnvironments)
	}
	if summary.EstimatedDailyCost != "~ €1.20/day" {
		t.Fatalf("estimated daily cost = %q", summary.EstimatedDailyCost)
	}
}

func TestAPIRBACSupportsTokenRoleMatrix(t *testing.T) {
	t.Setenv("ENVPLANE_API_TOKEN_ROLES", "read-token:reader,admin-token:admin")
	t.Setenv("ENVPLANE_API_READ_TOKEN", "")
	t.Setenv("ENVPLANE_API_WRITE_TOKEN", "")
	application, _, _ := newTestServer(t, "")

	readListReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	readListReq.Header.Set("Authorization", "Bearer read-token")
	readListRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(readListRec, readListReq)
	if readListRec.Code != http.StatusOK {
		t.Fatalf("expected role-mapped read token to read, got %d: %s", readListRec.Code, readListRec.Body.String())
	}

	adminCreateReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/demo-rbac", strings.NewReader(`{"name":"Demo","product_id":"generic","git_repo":{"provider":"github","url":"https://example.com/repo.git","default_branch":"main"},"gitops_repository_id":"platform-gitops"}`))
	adminCreateReq.Header.Set("Content-Type", "application/json")
	adminCreateReq.Header.Set("Authorization", "Bearer admin-token")
	adminCreateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(adminCreateRec, adminCreateReq)
	if adminCreateRec.Code != http.StatusOK {
		t.Fatalf("expected admin token to create project, got %d: %s", adminCreateRec.Code, adminCreateRec.Body.String())
	}

	readerCreateReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/forbidden-rbac", strings.NewReader(`{"name":"Demo","product_id":"generic","git_repo":{"provider":"github","url":"https://example.com/repo.git","default_branch":"main"},"gitops_repository_id":"platform-gitops"}`))
	readerCreateReq.Header.Set("Content-Type", "application/json")
	readerCreateReq.Header.Set("Authorization", "Bearer read-token")
	readerCreateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(readerCreateRec, readerCreateReq)
	if readerCreateRec.Code != http.StatusForbidden {
		t.Fatalf("expected read role to be forbidden for write, got %d: %s", readerCreateRec.Code, readerCreateRec.Body.String())
	}

	headReq := httptest.NewRequest(http.MethodHead, "/api/v1/products", nil)
	headReq.Header.Set("Authorization", "Bearer read-token")
	headRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK {
		t.Fatalf("expected read role to allow HEAD on read endpoint, got %d: %s", headRec.Code, headRec.Body.String())
	}
}

func TestAPIRoleBindingsIgnoreInvalidRoles(t *testing.T) {
	t.Setenv("ENVPLANE_API_TOKEN_ROLES", "invalid:denied,bad-token:bad-role")
	t.Setenv("ENVPLANE_API_READ_TOKEN", "valid-token")
	t.Setenv("ENVPLANE_API_WRITE_TOKEN", "")
	t.Setenv("ENVPLANE_AGENT_TOKEN", "")
	application, _, _ := newTestServer(t, "")
	if got := len(application.config().APITokenRoles); got == 0 {
		t.Fatalf("expected auth roles to be configured")
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	application.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected valid token role to be authorized, got %d: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.Header.Set("Authorization", "Bearer bad-token")
	response = httptest.NewRecorder()
	application.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid token role to be unauthorized, got %d: %s", response.Code, response.Body.String())
	}

	requestReadToken := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	requestReadToken.Header.Set("Authorization", "Bearer invalid")
	responseReadToken := httptest.NewRecorder()
	application.Routes().ServeHTTP(responseReadToken, requestReadToken)
	if responseReadToken.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid token role to be unauthorized, got %d: %s", responseReadToken.Code, responseReadToken.Body.String())
	}
}

func TestServerReloadConfigAffectsAPITokensAtRuntime(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "runtime-old-token")
	application, _, _ := newTestServer(t, "")

	baselineReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	baselineRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(baselineRec, baselineReq)
	if baselineRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized 401 with old token only, got %d: %s", baselineRec.Code, baselineRec.Body.String())
	}

	cfg := application.config()
	cfg.APIReadToken = "runtime-new-token"
	cfg.APITokenRoles = map[string]string{
		"runtime-new-token": "reader",
	}
	application.ReloadConfig(cfg)

	authorizedReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	authorizedReq.Header.Set("Authorization", "Bearer runtime-new-token")
	authorizedRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(authorizedRec, authorizedReq)
	if authorizedRec.Code != http.StatusOK {
		t.Fatalf("expected read endpoint 200 after runtime config reload, got %d: %s", authorizedRec.Code, authorizedRec.Body.String())
	}

	staleReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	staleReq.Header.Set("Authorization", "Bearer runtime-old-token")
	staleRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(staleRec, staleReq)
	if staleRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected old token to be unauthorized after reload, got %d: %s", staleRec.Code, staleRec.Body.String())
	}
}

func TestServerReloadConfigAffectsGitHubWebhookSecretAtRuntime(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "current-secret", "")
	body := []byte(githubPullRequestPayloadWithNumber("opened", "1999", "feature/hot-reload"))

	validReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	validReq.Header.Set("X-GitHub-Event", "pull_request")
	validReq.Header.Set("X-Hub-Signature-256", githubSignature("current-secret", body))
	validRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(validRec, validReq)
	if validRec.Code != http.StatusAccepted {
		t.Fatalf("expected initial signature 202, got %d: %s", validRec.Code, validRec.Body.String())
	}

	cfg := application.config()
	cfg.GitHubWebhookSecret = "rotated-secret"
	application.ReloadConfig(cfg)

	oldSignatureReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	oldSignatureReq.Header.Set("X-GitHub-Event", "pull_request")
	oldSignatureReq.Header.Set("X-Hub-Signature-256", githubSignature("current-secret", body))
	oldSignatureRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(oldSignatureRec, oldSignatureReq)
	if oldSignatureRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected rotated secret to reject old signature with 401, got %d: %s", oldSignatureRec.Code, oldSignatureRec.Body.String())
	}

	newSignatureReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	newSignatureReq.Header.Set("X-GitHub-Event", "pull_request")
	newSignatureReq.Header.Set("X-Hub-Signature-256", githubSignature("rotated-secret", body))
	newSignatureRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(newSignatureRec, newSignatureReq)
	if newSignatureRec.Code != http.StatusAccepted {
		t.Fatalf("expected reloaded secret to accept signature with 202, got %d: %s", newSignatureRec.Code, newSignatureRec.Body.String())
	}
}

func TestAPIWriteTokenRejectsMissingTokenForProtectedEndpoints(t *testing.T) {
	t.Setenv("ENVPLANE_API_WRITE_TOKEN", "write-token")
	application, _, _ := newTestServer(t, "")

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	listRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized 401, got %d: %s", listRec.Code, listRec.Body.String())
	}
}

func TestAPIAuthorizationDoesNotAffectWebhookWithSignatureOnly(t *testing.T) {
	t.Setenv("ENVPLANE_API_WRITE_TOKEN", "agent-token")
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayloadWithNumber("opened", "1905", "feature/secure"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected webhook path to be auth-exempt 202, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBackendDoesNotServeFrontendAssets(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	application, _, _ := newTestServer(t, "")

	for _, path := range []string{"/", "/login", "/static/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected %s to be frontend-owned 404 from backend, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestSessionCookieAuthorizesAPICallsWithoutAuthorizationHeader(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	application, _, _ := newTestServer(t, "")

	apiReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	apiReq.AddCookie(&http.Cookie{Name: apiSessionCookieName, Value: "readonly-token"})
	apiRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("expected API request with session cookie to be authorized, got %d: %s", apiRec.Code, apiRec.Body.String())
	}

	apiReqNoToken := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	apiRecNoToken := httptest.NewRecorder()
	application.Routes().ServeHTTP(apiRecNoToken, apiReqNoToken)
	if apiRecNoToken.Code != http.StatusUnauthorized {
		t.Fatalf("expected API request with missing token to be unauthorized, got %d: %s", apiRecNoToken.Code, apiRecNoToken.Body.String())
	}
}

func TestGitHubOAuthLoginRedirectsToProvider(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.OAuthSessionSecret = "oauth-session-secret"
	cfg.GitHubOAuthClientID = "github-client"
	cfg.GitHubOAuthSecret = "github-secret"
	cfg.GitHubOAuthAuthURL = "https://github.example/oauth/authorize"
	application.ReloadConfig(cfg)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	req.Host = "envpilot.example"
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected oauth redirect 302, got %d: %s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "https://github.example/oauth/authorize?") {
		t.Fatalf("unexpected redirect location: %q", location)
	}
	if !strings.Contains(location, "client_id=github-client") || !strings.Contains(location, "redirect_uri=http%3A%2F%2Fenvpilot.example%2Fauth%2Fgithub%2Fcallback") {
		t.Fatalf("redirect missing oauth query: %q", location)
	}
	if cookie := oauthStateCookieFrom(rec); cookie == nil {
		t.Fatalf("expected oauth state cookie")
	}
}

func TestGitLabOAuthCallbackCreatesSessionAcceptedByAPI(t *testing.T) {
	requireHTTPServer(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse token form: %v", err)
			}
			if r.Form.Get("client_id") != "gitlab-client" || r.Form.Get("client_secret") != "gitlab-secret" || r.Form.Get("code") != "oauth-code" {
				t.Fatalf("unexpected token form: %#v", r.Form)
			}
			writeJSON(w, http.StatusOK, map[string]string{"access_token": "provider-token"})
		case "/api/v4/user":
			if got := r.Header.Get("Authorization"); got != "Bearer provider-token" {
				t.Fatalf("authorization = %q", got)
			}
			writeJSON(w, http.StatusOK, map[string]any{"username": "alex"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.OAuthSessionSecret = "oauth-session-secret"
	cfg.GitLabOAuthClientID = "gitlab-client"
	cfg.GitLabOAuthSecret = "gitlab-secret"
	cfg.GitLabOAuthAuthURL = provider.URL + "/oauth/authorize"
	cfg.GitLabOAuthTokenURL = provider.URL + "/oauth/token"
	cfg.GitLabOAuthUserURL = provider.URL + "/api/v4/user"
	application.ReloadConfig(cfg)

	loginReq := httptest.NewRequest(http.MethodGet, "/auth/gitlab/login", nil)
	loginReq.Host = "envpilot.example"
	loginRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(loginRec, loginReq)
	stateCookie := oauthStateCookieFrom(loginRec)
	if stateCookie == nil {
		t.Fatalf("expected oauth state cookie")
	}
	state := strings.TrimPrefix(stateCookie.Value, "gitlab:")

	callbackReq := httptest.NewRequest(http.MethodGet, "/auth/gitlab/callback?code=oauth-code&state="+state, nil)
	callbackReq.Host = "envpilot.example"
	callbackReq.AddCookie(stateCookie)
	callbackRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusFound {
		t.Fatalf("expected oauth callback redirect 302, got %d: %s", callbackRec.Code, callbackRec.Body.String())
	}
	sessionCookie := sessionCookieFrom(callbackRec)
	if sessionCookie == nil {
		t.Fatalf("expected oauth session cookie")
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	apiReq.AddCookie(sessionCookie)
	apiRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("expected oauth session to authorize API request, got %d: %s", apiRec.Code, apiRec.Body.String())
	}
}

func TestOIDCOAuthLoginRedirectsToProvider(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.OAuthSessionSecret = "oauth-session-secret"
	cfg.OIDCOAuthClientID = "oidc-client"
	cfg.OIDCOAuthSecret = "oidc-secret"
	cfg.OIDCOAuthAuthURL = "https://id.example.com/oauth/authorize"
	cfg.OIDCOAuthScopes = "openid profile email"
	application.ReloadConfig(cfg)

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
	req.Host = "envpilot.example"
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected oauth redirect 302, got %d: %s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "https://id.example.com/oauth/authorize?") {
		t.Fatalf("unexpected redirect location: %q", location)
	}
	if !strings.Contains(location, "client_id=oidc-client") {
		t.Fatalf("redirect missing client_id: %q", location)
	}
	if !strings.Contains(location, "scope=openid+profile+email") {
		t.Fatalf("redirect missing scope: %q", location)
	}
	if !strings.Contains(location, "redirect_uri=http%3A%2F%2Fenvpilot.example%2Fauth%2Foidc%2Fcallback") {
		t.Fatalf("redirect missing oidc callback uri: %q", location)
	}
	if cookie := oauthStateCookieFrom(rec); cookie == nil {
		t.Fatalf("expected oauth state cookie")
	}
}

func TestOIDCOAuthCallbackCreatesSessionAcceptedByAPI(t *testing.T) {
	requireHTTPServer(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse token form: %v", err)
			}
			if r.Form.Get("client_id") != "oidc-client" || r.Form.Get("client_secret") != "oidc-secret" || r.Form.Get("code") != "oauth-code" {
				t.Fatalf("unexpected token form: %#v", r.Form)
			}
			writeJSON(w, http.StatusOK, map[string]string{"access_token": "provider-token"})
		case "/userinfo":
			if got := r.Header.Get("Authorization"); got != "Bearer provider-token" {
				t.Fatalf("authorization = %q", got)
			}
			writeJSON(w, http.StatusOK, map[string]any{"sub": "oidc-user-123", "preferred_username": "oidc-user"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.OAuthSessionSecret = "oauth-session-secret"
	cfg.OIDCOAuthClientID = "oidc-client"
	cfg.OIDCOAuthSecret = "oidc-secret"
	cfg.OIDCOAuthAuthURL = provider.URL + "/oauth/authorize"
	cfg.OIDCOAuthTokenURL = provider.URL + "/oauth/token"
	cfg.OIDCOAuthUserURL = provider.URL + "/userinfo"
	application.ReloadConfig(cfg)

	loginReq := httptest.NewRequest(http.MethodGet, "/auth/oidc/login?org=finance", nil)
	loginReq.Host = "envpilot.example"
	loginRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(loginRec, loginReq)
	stateCookie := oauthStateCookieFrom(loginRec)
	if stateCookie == nil {
		t.Fatalf("expected oauth state cookie")
	}
	state := strings.TrimPrefix(stateCookie.Value, "oidc:")

	callbackReq := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=oauth-code&state="+state, nil)
	callbackReq.Host = "envpilot.example"
	callbackReq.AddCookie(stateCookie)
	callbackRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusFound {
		t.Fatalf("expected oauth callback redirect 302, got %d: %s", callbackRec.Code, callbackRec.Body.String())
	}
	sessionCookie := sessionCookieFrom(callbackRec)
	if sessionCookie == nil {
		t.Fatalf("expected oauth session cookie")
	}
	session, ok := application.parseOAuthSession(sessionCookie.Value)
	if !ok {
		t.Fatalf("expected valid oauth session payload")
	}
	if session.Org != "finance" {
		t.Fatalf("expected session org finance, got %q", session.Org)
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	apiReq.AddCookie(sessionCookie)
	apiRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("expected oauth session to authorize API request, got %d: %s", apiRec.Code, apiRec.Body.String())
	}
}

func TestOAuthLoginPreservesOrganizationFromQuery(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.OAuthSessionSecret = "oauth-session-secret"
	cfg.GitHubOAuthClientID = "github-client"
	cfg.GitHubOAuthSecret = "github-secret"
	application.ReloadConfig(cfg)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/login?org=platform", nil)
	req.Host = "envpilot.example"
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected oauth redirect 302, got %d: %s", rec.Code, rec.Body.String())
	}
	orgCookie := cookieFrom(rec, oauthOrgCookieName)
	if orgCookie == nil {
		t.Fatalf("expected oauth org cookie")
	}
	if got := strings.TrimSpace(orgCookie.Value); got != "platform" {
		t.Fatalf("expected org cookie platform, got %q", got)
	}
}

func TestProjectRBACFiltersProjectsAndEnvironmentsByOAuthUser(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.OAuthSessionSecret = "oauth-session-secret"
	cfg.GitHubOAuthClientID = "github-client"
	application.ReloadConfig(cfg)

	_, err := application.projects.SaveProject(domain.Project{
		ID:                 "alpha",
		Name:               "Alpha",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/owner/alpha",
		GitOpsRepositoryID: "github.com/owner/gitops",
		AccessUsers:        []string{"alice"},
	})
	if err != nil {
		t.Fatalf("save alpha project: %v", err)
	}
	_, err = application.projects.SaveProject(domain.Project{
		ID:                 "beta",
		Name:               "Beta",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/owner/beta",
		GitOpsRepositoryID: "github.com/owner/gitops",
		AccessUsers:        []string{"bob"},
	})
	if err != nil {
		t.Fatalf("save beta project: %v", err)
	}
	if _, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{ID: "env-alpha", Project: "alpha", Product: "bethunder"}); err != nil {
		t.Fatalf("create alpha env: %v", err)
	}
	if _, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{ID: "env-beta", Project: "beta", Product: "bethunder"}); err != nil {
		t.Fatalf("create beta env: %v", err)
	}
	session := application.buildOAuthSession("github", "alice", apiRoleAdmin, "global")
	if session == "" {
		t.Fatalf("expected oauth session")
	}
	sessionCookie := &http.Cookie{Name: apiSessionCookieName, Value: session}

	projectsReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	projectsReq.AddCookie(sessionCookie)
	projectsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectsRec, projectsReq)
	if projectsRec.Code != http.StatusOK {
		t.Fatalf("expected projects 200, got %d: %s", projectsRec.Code, projectsRec.Body.String())
	}
	var projects []domain.Project
	if err := json.Unmarshal(projectsRec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "alpha" {
		t.Fatalf("expected only alpha project, got %+v", projects)
	}

	betaReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/beta", nil)
	betaReq.AddCookie(sessionCookie)
	betaRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(betaRec, betaReq)
	if betaRec.Code != http.StatusForbidden {
		t.Fatalf("expected beta project 403, got %d: %s", betaRec.Code, betaRec.Body.String())
	}

	envReq := httptest.NewRequest(http.MethodGet, "/api/v1/environments", nil)
	envReq.AddCookie(sessionCookie)
	envRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(envRec, envReq)
	if envRec.Code != http.StatusOK {
		t.Fatalf("expected environments 200, got %d: %s", envRec.Code, envRec.Body.String())
	}
	var environments []domain.Environment
	if err := json.Unmarshal(envRec.Body.Bytes(), &environments); err != nil {
		t.Fatalf("decode environments: %v", err)
	}
	if len(environments) != 1 || environments[0].ID != "env-alpha" {
		t.Fatalf("expected only alpha environment, got %+v", environments)
	}
}

func TestProjectRBACFiltersProjectsByOAuthOrganization(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.OAuthSessionSecret = "oauth-session-secret"
	cfg.GitHubOAuthClientID = "github-client"
	application.ReloadConfig(cfg)

	_, err := application.projects.SaveProject(domain.Project{
		ID:                  "alpha",
		Name:                "Alpha",
		ProductID:           "bethunder",
		AppRepositoryID:     "github.com/owner/alpha",
		GitOpsRepositoryID:  "github.com/owner/gitops",
		AccessOrganizations: []string{"platform"},
	})
	if err != nil {
		t.Fatalf("save alpha project: %v", err)
	}
	_, err = application.projects.SaveProject(domain.Project{
		ID:                  "beta",
		Name:                "Beta",
		ProductID:           "bethunder",
		AppRepositoryID:     "github.com/owner/beta",
		GitOpsRepositoryID:  "github.com/owner/gitops",
		AccessOrganizations: []string{"finance"},
	})
	if err != nil {
		t.Fatalf("save beta project: %v", err)
	}
	if _, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{ID: "env-alpha", Project: "alpha", Product: "bethunder"}); err != nil {
		t.Fatalf("create alpha env: %v", err)
	}
	if _, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{ID: "env-beta", Project: "beta", Product: "bethunder"}); err != nil {
		t.Fatalf("create beta env: %v", err)
	}

	session := application.buildOAuthSession("github", "alice", apiRoleAdmin, "platform")
	if session == "" {
		t.Fatalf("expected oauth session")
	}
	sessionCookie := &http.Cookie{Name: apiSessionCookieName, Value: session}

	projectsReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	projectsReq.AddCookie(sessionCookie)
	projectsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectsRec, projectsReq)
	if projectsRec.Code != http.StatusOK {
		t.Fatalf("expected projects 200, got %d: %s", projectsRec.Code, projectsRec.Body.String())
	}
	var projects []domain.Project
	if err := json.Unmarshal(projectsRec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "alpha" {
		t.Fatalf("expected only alpha project, got %+v", projects)
	}

	betaReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/beta", nil)
	betaReq.AddCookie(sessionCookie)
	betaRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(betaRec, betaReq)
	if betaRec.Code != http.StatusForbidden {
		t.Fatalf("expected beta project 403, got %d: %s", betaRec.Code, betaRec.Body.String())
	}

	envReq := httptest.NewRequest(http.MethodGet, "/api/v1/environments", nil)
	envReq.AddCookie(sessionCookie)
	envRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(envRec, envReq)
	if envRec.Code != http.StatusOK {
		t.Fatalf("expected environments 200, got %d: %s", envRec.Code, envRec.Body.String())
	}
	var environments []domain.Environment
	if err := json.Unmarshal(envRec.Body.Bytes(), &environments); err != nil {
		t.Fatalf("decode environments: %v", err)
	}
	if len(environments) != 1 || environments[0].ID != "env-alpha" {
		t.Fatalf("expected only alpha environment, got %+v", environments)
	}
}

func TestAuditLogWritesJSONLineToConfiguredFile(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	req.Header.Set("Authorization", "Bearer readonly-token")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"path":"/api/v1/products"`)) {
		t.Fatalf("missing path in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"status":200`)) {
		t.Fatalf("missing status in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"role":"reader"`)) {
		t.Fatalf("missing role in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"method":"GET"`)) {
		t.Fatalf("missing method in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"event":"api_request"`)) {
		t.Fatalf("missing api_request event in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"endpoint":"/api/v1/products"`)) {
		t.Fatalf("missing endpoint in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"status_code":200`)) {
		t.Fatalf("missing status_code in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"outcome":"success"`)) {
		t.Fatalf("missing success outcome in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"actor_type":"api-token"`)) {
		t.Fatalf("missing actor_type in audit log: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"actor":"`)) {
		t.Fatalf("missing actor in audit log: %s", string(raw))
	}
	if bytes.Contains(raw, []byte("readonly-token")) {
		t.Fatalf("audit log must not contain raw token: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"remote_addr"`)) {
		t.Fatalf("missing remote_addr in audit log: %s", string(raw))
	}
	line, _, found := strings.Cut(string(raw), "\n")
	if !found {
		t.Fatalf("expected audit log to contain newline-separated entry: %s", string(raw))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("audit log entry is not valid json: %v: %s", err, line)
	}
	if _, ok := entry["ts"].(string); !ok {
		t.Fatalf("expected ts field in audit log entry: %s", line)
	}
	assertStandardRequestAuditEvent(t, entry, http.MethodGet, "/api/v1/products", http.StatusOK, "", "success", "")
}

func TestRequestAuditExtractsProjectIDFromProjectEndpoint(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/checkout", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)

	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventAPIRequest)
	assertStandardRequestAuditEvent(t, entry, http.MethodGet, "/api/v1/projects/checkout", rec.Code, "checkout", requestAuditOutcome(rec.Code), http.StatusText(rec.Code))
}

func TestRateLimitRejectsRequestsAboveConfiguredRate(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	t.Setenv("ENVPLANE_RATE_LIMIT_REQUESTS", "2")
	t.Setenv("ENVPLANE_RATE_LIMIT_SECONDS", "60")
	application, _, _ := newTestServer(t, "")

	makeRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
		req.Header.Set("Authorization", "Bearer readonly-token")
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		return rec
	}

	rec1 := makeRequest()
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 for first request, got %d: %s", rec1.Code, rec1.Body.String())
	}
	rec2 := makeRequest()
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for second request, got %d: %s", rec2.Code, rec2.Body.String())
	}
	rec3 := makeRequest()
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on rate-limit breach, got %d: %s", rec3.Code, rec3.Body.String())
	}
	if got := rec3.Header().Get("Retry-After"); got == "" {
		t.Fatalf("expected retry-after header on 429")
	}
}

func TestAuditLogRecordsActorUserHeaderWithoutTokenFingerprint(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer readonly-token")
	req.Header.Set("X-EnvPlane-User", "audit-user")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	line, _, found := strings.Cut(string(raw), "\n")
	if !found {
		t.Fatalf("expected audit log to contain newline-separated entry: %s", string(raw))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("audit log entry is not valid json: %v: %s", err, line)
	}
	if got := entry["actor_type"]; got != "user" {
		t.Fatalf("expected actor_type=user, got %#v", got)
	}
	if got := entry["actor"]; got != "audit-user" {
		t.Fatalf("expected actor=audit-user, got %#v", got)
	}
	if got := entry["actor_user"]; got != "audit-user" {
		t.Fatalf("expected actor_user=audit-user, got %#v", got)
	}
	if _, ok := entry["actor_id"]; ok {
		t.Fatalf("expected no token actor_id for user requests, got %#v", entry["actor_id"])
	}
}

func TestAuditSchemaContractCoversSecurityRequestCredentialsConfigAndSecretEvents(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	projectID := "audit-contract"
	const oauthSecret = "audit-contract-oauth-secret"
	const manualSecret = "audit-contract-manual-secret"
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               "Audit Contract",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(fmt.Sprintf(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "oauthToken": %q,
	    "secretStrategies": {
	      "dev/db-password": {
	        "strategy": "manual input",
	        "required": true,
	        "serviceId": "Service/dev/orders",
	        "container": "orders-api",
	        "variable": "DB_PASSWORD",
	        "manualValue": %q
	      }
	    },
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`, oauthSecret, manualSecret))
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusOK {
		t.Fatalf("compile session status=%d body=%s", compileRec.Code, compileRec.Body.String())
	}

	deployReq := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/bootstrap-session/runner-deployment-instructions", bytes.NewReader([]byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner"
	}`)))
	deployReq.Header.Set("Content-Type", "application/json")
	deployRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(deployRec, deployReq)
	if deployRec.Code != http.StatusOK {
		t.Fatalf("runner deployment instructions status=%d body=%s", deployRec.Code, deployRec.Body.String())
	}
	var deployResp domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(deployRec.Body.Bytes(), &deployResp); err != nil {
		t.Fatalf("decode runner deployment response: %v", err)
	}
	hydrateRunnerBootstrapTokensForTest(t, &deployResp)

	configReq := newRunnerConfigRequest(t, deployResp, "")
	configReq.Header.Del("Authorization")
	configRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(configRec, configReq)
	if configRec.Code != http.StatusUnauthorized {
		t.Fatalf("runner config without token status=%d body=%s", configRec.Code, configRec.Body.String())
	}

	entries := parseAuditLogEntries(t, logPath)
	for _, event := range []string{
		auditEventAPIRequest,
		auditEventBootstrapSCMCredentialsSaved,
		auditEventBootstrapSecretStrategiesSaved,
		auditEventProjectConfigSaved,
		auditEventRunnerBootstrapTokenGenerated,
		auditEventRunnerConfigFetchAuthFailed,
	} {
		_ = findAuditEventEntry(t, entries, event)
	}
	assertAuditSchemaContract(t, entries, oauthSecret, manualSecret, deployResp.RegistrationToken, deployResp.ProjectConfigToken)
}

func TestBootstrapSCMCredentialsAreMaskedAndAudited(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "secure-bootstrap",
		Name:               "Secure Bootstrap",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/secure-bootstrap/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("expected create 200, got %d: %s", createRec.Code, createRec.Body.String())
	}

	const secret = "super-secret-oauth-token"
	body := []byte(`{
	  "current_step": 1,
	  "status": "scanning",
	  "step_data": {
	    "repositoryUrl": "https://github.com/acme/app",
	    "gitopsRepoUrl": "https://github.com/acme/gitops",
	    "oauthToken": "` + secret + `"
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/secure-bootstrap/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected update 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
	if strings.Contains(updateRec.Body.String(), secret) {
		t.Fatalf("update response leaked plaintext secret: %s", updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), `"masked":true`) {
		t.Fatalf("expected masked marker in update response: %s", updateRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/projects/secure-bootstrap/bootstrap-session", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	if strings.Contains(getRec.Body.String(), secret) {
		t.Fatalf("get response leaked plaintext secret: %s", getRec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("audit log leaked plaintext secret: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"event":"bootstrap_scm_credentials_saved"`)) {
		t.Fatalf("expected credential audit event: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"oauthToken"`)) {
		t.Fatalf("expected credential field name in audit event: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventBootstrapSCMCredentialsSaved)
	assertStandardAuditEvent(t, entry, auditEventBootstrapSCMCredentialsSaved, auditEndpointBootstrapSessionUpdate, "secure-bootstrap", "", false)
}

func TestBootstrapSessionPersistsGitOpsConfiguration(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-gitops",
		Name:               "Bootstrap GitOps",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-gitops/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("expected create 200, got %d: %s", createRec.Code, createRec.Body.String())
	}

	const outputPath = "environments/{{ .PRNumber }}/{{ .Service }}"
	const fluxNamespace = "flux-system"
	const repoRef = "ns/envpilot-gitops"
	const kustomizationRef = "ns/envpilot-prs"
	const commitMode = "pull request"
	body := []byte(`{
	  "current_step": 1,
	  "status": "scanning",
	  "step_data": {
	    "gitOpsOutputPath": "` + outputPath + `",
	    "fluxNamespace": "` + fluxNamespace + `",
	    "fluxGitRepositoryRef": "` + repoRef + `",
	    "fluxKustomizationRef": "` + kustomizationRef + `",
	    "gitOpsCommitMode": "` + commitMode + `"
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-gitops/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected update 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	var updated map[string]any
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	data, ok := updated["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing data in response: %#v", updated)
	}
	if data["gitOpsOutputPath"] != outputPath {
		t.Fatalf("unexpected gitOpsOutputPath: %#v", data["gitOpsOutputPath"])
	}
	if data["fluxNamespace"] != fluxNamespace {
		t.Fatalf("unexpected fluxNamespace: %#v", data["fluxNamespace"])
	}
	if data["fluxGitRepositoryRef"] != repoRef {
		t.Fatalf("unexpected fluxGitRepositoryRef: %#v", data["fluxGitRepositoryRef"])
	}
	if data["fluxKustomizationRef"] != kustomizationRef {
		t.Fatalf("unexpected fluxKustomizationRef: %#v", data["fluxKustomizationRef"])
	}
	if data["gitOpsCommitMode"] != commitMode {
		t.Fatalf("unexpected gitOpsCommitMode: %#v", data["gitOpsCommitMode"])
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/projects/bootstrap-gitops/bootstrap-session", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var retrieved map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &retrieved); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	getData, ok := retrieved["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing data in get response: %#v", retrieved)
	}
	if getData["gitOpsOutputPath"] != outputPath {
		t.Fatalf("unexpected get response gitOpsOutputPath: %#v", getData["gitOpsOutputPath"])
	}
	if getData["fluxNamespace"] != fluxNamespace {
		t.Fatalf("unexpected get response fluxNamespace: %#v", getData["fluxNamespace"])
	}
	if getData["fluxGitRepositoryRef"] != repoRef {
		t.Fatalf("unexpected get response fluxGitRepositoryRef: %#v", getData["fluxGitRepositoryRef"])
	}
	if getData["fluxKustomizationRef"] != kustomizationRef {
		t.Fatalf("unexpected get response fluxKustomizationRef: %#v", getData["fluxKustomizationRef"])
	}
	if getData["gitOpsCommitMode"] != commitMode {
		t.Fatalf("unexpected get response gitOpsCommitMode: %#v", getData["gitOpsCommitMode"])
	}
}

func TestBootstrapSessionGeneratesManifestTemplatesFromResourceScan(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-templates",
		Name:               "Bootstrap Templates",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
		BaseEnvConfig: domain.BaseEnvConfig{
			Namespace: "dev-base",
			Domain:    "preview.example.com",
		},
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-templates/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 5,
	  "status": "reviewed",
	  "step_data": {
	    "previewDomain": "preview.example.com",
	    "resourceReview": {
	      "Deployment/dev-base/orders": {"include": true, "strategy": "override per PR"},
	      "Service/dev-base/orders": {"include": true, "strategy": "clone"},
	      "Ingress/dev-base/orders": {"include": true, "strategy": "clone"}
	    },
	    "resourceScanReport": [
	      {
	        "kind": "Deployment",
	        "namespace": "dev-base",
	        "name": "orders",
	        "manifest": {
	          "apiVersion": "apps/v1",
	          "kind": "Deployment",
	          "metadata": {"name": "orders", "namespace": "dev-base"},
	          "spec": {
	            "template": {
	              "spec": {
	                "containers": [
	                  {"name": "orders", "image": "ghcr.io/acme/orders:abc123"}
	                ]
	              }
	            }
	          }
	        }
	      },
	      {
	        "kind": "Service",
	        "namespace": "dev-base",
	        "name": "orders",
	        "manifest": {
	          "apiVersion": "v1",
	          "kind": "Service",
	          "metadata": {"name": "orders", "namespace": "dev-base"},
	          "spec": {
	            "ports": [{"name":"http","port":80}]
	          }
	        }
	      },
	      {
	        "kind": "Ingress",
	        "namespace": "dev-base",
	        "name": "orders",
	        "manifest": {
	          "apiVersion": "networking.k8s.io/v1",
	          "kind": "Ingress",
	          "metadata": {"name": "orders", "namespace": "dev-base"},
	          "spec": {
	            "rules": [{
	              "host": "orders.dev-base.local",
	              "http": {
	                "paths": [{
	                  "path": "/",
	                  "backend": {"service":{"name":"orders","port":{"number":80}}}
	                }]
	              }
	            }]
	          }
	        }
	      }
	    ]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-templates/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(updateRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing data in response: %#v", payload)
	}
	rawTemplates, ok := data["manifestTemplates"].([]any)
	if !ok || len(rawTemplates) == 0 {
		t.Fatalf("expected generated manifestTemplates, got %#v", data["manifestTemplates"])
	}

	foundDeployment := false
	foundIngress := false
	for _, raw := range rawTemplates {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind := asString(item["kind"])
		yamlBody := asString(item["yaml"])
		switch kind {
		case "Deployment":
			foundDeployment = true
			if !strings.Contains(yamlBody, `namespace: "envpilot-pr-{{ .PRNumber }}"`) {
				t.Fatalf("deployment template namespace rewrite missing: %s", yamlBody)
			}
			if !strings.Contains(yamlBody, `image: "ghcr.io/acme/orders:{{ .CommitSHA }}"`) {
				t.Fatalf("deployment template image rewrite missing: %s", yamlBody)
			}
		case "Ingress":
			foundIngress = true
			if !strings.Contains(yamlBody, "host: orders.preview.example.com") {
				t.Fatalf("ingress template host rewrite missing: %s", yamlBody)
			}
		}
	}
	if !foundDeployment {
		t.Fatalf("expected deployment template in generated manifestTemplates")
	}
	if !foundIngress {
		t.Fatalf("expected ingress template in generated manifestTemplates")
	}
}

func TestGetBootstrapManifestTemplatesEndpointReturnsTemplates(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-templates-endpoint",
		Name:               "Bootstrap Templates Endpoint",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
		BaseEnvConfig: domain.BaseEnvConfig{
			Namespace: "dev-base",
			Domain:    "preview.example.com",
		},
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-templates-endpoint/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 5,
	  "status": "reviewed",
	  "step_data": {
	    "previewDomain": "preview.example.com",
	    "resourceReview": {
	      "Deployment/dev-base/orders": {"include": true, "strategy": "override per PR"}
	    },
	    "resourceScanReport": [
	      {
	        "kind": "Deployment",
	        "namespace": "dev-base",
	        "name": "orders",
	        "manifest": {
	          "apiVersion": "apps/v1",
	          "kind": "Deployment",
	          "metadata": {"name": "orders", "namespace": "dev-base"},
	          "spec": {
	            "template": {
	              "spec": {
	                "containers": [
	                  {"name": "orders", "image": "ghcr.io/acme/orders:abc123"}
	                ]
	              }
	            }
	          }
	        }
	      }
	    ]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-templates-endpoint/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/projects/bootstrap-templates-endpoint/bootstrap-session/manifest-templates", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get templates status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var payload struct {
		ProjectID          string `json:"projectId"`
		BootstrapSessionID string `json:"bootstrapSessionId"`
		ManifestTemplates  []struct {
			Kind string `json:"kind"`
			YAML string `json:"yaml"`
		} `json:"manifestTemplates"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode templates response: %v", err)
	}
	if payload.ProjectID != "bootstrap-templates-endpoint" {
		t.Fatalf("projectId=%q", payload.ProjectID)
	}
	if strings.TrimSpace(payload.BootstrapSessionID) == "" {
		t.Fatalf("bootstrapSessionId is empty")
	}
	if len(payload.ManifestTemplates) == 0 {
		t.Fatalf("expected at least one template, got 0")
	}
	if payload.ManifestTemplates[0].Kind != "Deployment" {
		t.Fatalf("expected deployment template, got %q", payload.ManifestTemplates[0].Kind)
	}
	if !strings.Contains(payload.ManifestTemplates[0].YAML, `image: "ghcr.io/acme/orders:{{ .CommitSHA }}"`) {
		t.Fatalf("deployment template image rewrite missing: %s", payload.ManifestTemplates[0].YAML)
	}
}

func TestBootstrapSessionCompileRejectsInvalidTemplateYAML(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-invalid-yaml",
		Name:               "Bootstrap Compile Invalid YAML",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-invalid-yaml/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "status": "compiled",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-invalid-yaml/bootstrap-session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "template validation failed:") {
		t.Fatalf("expected template validation error, got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bootstrap-compile-invalid-yaml") && !strings.Contains(rec.Body.String(), "deployment-orders.yaml") {
		t.Fatalf("expected file hint in error, got %s", rec.Body.String())
	}
}

func TestBootstrapSessionCompileRejectsMissingVariablesAndSchema(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-invalid-template",
		Name:               "Bootstrap Compile Invalid Template",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-invalid-template/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "status": "compiled",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Service",
	      "namespace": "dev-base",
	      "name": "orders",
	      "yaml": "apiVersion: v1\nkind: Service\nmetadata:\n  name: orders\n  namespace: dev-base\nspec:\n  selector:\n    app: orders\n"
	    }]
	  }
	}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-invalid-template/bootstrap-session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "{{ .PRNumber }}") {
		t.Fatalf("expected missing PRNumber variable error, got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "spec.ports") {
		t.Fatalf("expected schema validation error, got %s", rec.Body.String())
	}
}

func TestBootstrapSessionCompileAcceptsValidEditedTemplates(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-valid-template",
		Name:               "Bootstrap Compile Valid Template",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-valid-template/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 9,
	  "status": "compiled",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-valid-template/bootstrap-session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ManifestTemplates []map[string]any `json:"manifestTemplates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Status != "compiled" {
		t.Fatalf("expected status compiled, got %q", response.Status)
	}
	if len(response.Data.ManifestTemplates) != 1 {
		t.Fatalf("expected edited templates persisted, got %d", len(response.Data.ManifestTemplates))
	}
}

func TestBootstrapSessionCompileEndpointCompilesSavedSession(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-endpoint",
		Name:               "Bootstrap Compile Endpoint",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-endpoint/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-endpoint/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-endpoint/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusOK {
		t.Fatalf("compile session status=%d body=%s", compileRec.Code, compileRec.Body.String())
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(compileRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode compile response: %v", err)
	}
	if response.Status != "compiled" {
		t.Fatalf("expected compiled status, got %q", response.Status)
	}
}

func TestBootstrapSessionDeploymentBackendSelectionPersistsInSessionAndProjectConfig(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-deployment-backend-persist",
		Name:               "Bootstrap Compile Deployment Backend Persist",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-deployment-backend-persist/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "deployment": {
	      "backend": "helm_direct",
	      "helmDirect": {
	        "chartRef": "deploy/helm/checkout",
	        "releaseNamePattern": "checkout-pr-{{ .PRNumber }}",
	        "namespacePattern": "envpilot-pr-{{ .PRNumber }}",
	        "timeout": 420,
	        "wait": false,
	        "createNamespace": false,
	        "valuesOverrideStrategy": "set",
	        "imageTagValuePath": "image.tag"
	      }
	    },
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-deployment-backend-persist/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/projects/bootstrap-compile-deployment-backend-persist/bootstrap-session", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get session status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var session map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode get session: %v", err)
	}
	sessionData, ok := session["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing session data: %#v", session)
	}
	deployment, ok := sessionData["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("missing deployment block in session data: %#v", sessionData)
	}
	backendRaw, ok := deployment["backend"].(string)
	if !ok || strings.TrimSpace(backendRaw) != domain.DeploymentBackendHelmDirect {
		t.Fatalf("expected session deployment backend %q, got %#v", domain.DeploymentBackendHelmDirect, deployment["backend"])
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-deployment-backend-persist/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusOK {
		t.Fatalf("compile session status=%d body=%s", compileRec.Code, compileRec.Body.String())
	}
	var compileResponse struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(compileRec.Body.Bytes(), &compileResponse); err != nil {
		t.Fatalf("decode compile response: %v", err)
	}
	if compileResponse.Status != "compiled" {
		t.Fatalf("expected compiled status, got %q", compileResponse.Status)
	}

	rawConfig, err := application.projectConfigs.Latest("bootstrap-compile-deployment-backend-persist")
	if err != nil {
		t.Fatalf("read project config: %v", err)
	}
	configDeployment, ok := rawConfig.Config["deployment"].(map[string]any)
	if !ok {
		t.Fatalf("expected deployment in project config: %#v", rawConfig.Config)
	}
	configBackend, ok := configDeployment["backend"].(string)
	if !ok || strings.TrimSpace(configBackend) != domain.DeploymentBackendHelmDirect {
		t.Fatalf("expected project config deployment backend %q, got %#v", domain.DeploymentBackendHelmDirect, configDeployment["backend"])
	}
	helmDirect, ok := configDeployment["helmDirect"].(map[string]any)
	if !ok {
		t.Fatalf("expected helmDirect config in project config: %#v", configDeployment)
	}
	namespaceMode, ok := helmDirect["namespaceMode"].(string)
	if !ok || strings.TrimSpace(namespaceMode) == "" {
		t.Fatalf("expected namespaceMode in helmDirect config: %#v", helmDirect)
	}
	if strings.TrimSpace(asString(helmDirect["chartRef"])) != "deploy/helm/checkout" {
		t.Fatalf("expected chartRef=deploy/helm/checkout, got %#v", helmDirect["chartRef"])
	}
	if strings.TrimSpace(asString(helmDirect["releaseNamePattern"])) != "checkout-pr-{{ .PRNumber }}" {
		t.Fatalf("expected releaseNamePattern, got %#v", helmDirect["releaseNamePattern"])
	}
	if strings.TrimSpace(asString(helmDirect["namespacePattern"])) != "envpilot-pr-{{ .PRNumber }}" {
		t.Fatalf("expected namespacePattern, got %#v", helmDirect["namespacePattern"])
	}
	if strings.TrimSpace(asString(helmDirect["valuesOverrideStrategy"])) != "set" {
		t.Fatalf("expected valuesOverrideStrategy set, got %#v", helmDirect["valuesOverrideStrategy"])
	}
	if strings.TrimSpace(asString(helmDirect["imageTagValuePath"])) != "image.tag" {
		t.Fatalf("expected imageTagValuePath image.tag, got %#v", helmDirect["imageTagValuePath"])
	}
	if wait, ok := helmDirect["wait"].(bool); !ok || wait != false {
		t.Fatalf("expected wait=false, got %#v", helmDirect["wait"])
	}
	if createNamespace, ok := helmDirect["createNamespace"].(bool); !ok || createNamespace != false {
		t.Fatalf("expected createNamespace=false, got %#v", helmDirect["createNamespace"])
	}
	switch timeout := helmDirect["timeout"].(type) {
	case int:
		if timeout != 420 {
			t.Fatalf("expected timeout=420, got %#v", helmDirect["timeout"])
		}
	case float64:
		if timeout != 420 {
			t.Fatalf("expected timeout=420, got %#v", helmDirect["timeout"])
		}
	default:
		t.Fatalf("expected timeout=420 number, got %#v", helmDirect["timeout"])
	}
}

func TestBootstrapSessionCompileAllowsHelmDirectWithoutFluxCapabilities(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-helm-direct-no-flux",
		Name:               "Bootstrap Compile Helm Direct Without Flux",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-helm-direct-no-flux/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "deployment": {
	      "backend": "helm_direct"
	    },
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-helm-direct-no-flux/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-helm-direct-no-flux/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusOK {
		t.Fatalf("compile session status=%d body=%s", compileRec.Code, compileRec.Body.String())
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(compileRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode compile response: %v", err)
	}
	if response.Status != "compiled" {
		t.Fatalf("expected compiled status, got %q", response.Status)
	}
}

func TestBootstrapSessionCompileRejectsFluxWithoutFluxCapabilities(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-flux-no-flux",
		Name:               "Bootstrap Compile Flux Without Flux",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-flux-no-flux/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "deployment": {
	      "backend": "fluxcd"
	    },
	    "clusterCapabilityReport": {},
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-flux-no-flux/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-flux-no-flux/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", compileRec.Code, compileRec.Body.String())
	}
	if !strings.Contains(compileRec.Body.String(), "fluxcd backend requires Flux capabilities") {
		t.Fatalf("expected flux capabilities validation error, got %s", compileRec.Body.String())
	}
}

func TestBootstrapSessionCompileEndpointRejectsInvalidTemplates(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-compile-endpoint-invalid",
		Name:               "Bootstrap Compile Endpoint Invalid",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-endpoint-invalid/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Service",
	      "namespace": "dev-base",
	      "name": "orders",
	      "yaml": "apiVersion: v1\nkind: Service\nmetadata:\n  name: orders\n  namespace: dev-base\nspec:\n  selector:\n    app: orders\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-compile-endpoint-invalid/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-compile-endpoint-invalid/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", compileRec.Code, compileRec.Body.String())
	}
	if !strings.Contains(compileRec.Body.String(), "{{ .PRNumber }}") {
		t.Fatalf("expected missing variable validation error, got %s", compileRec.Body.String())
	}
}

func TestBootstrapSessionSimulateEndpointReturnsValidationAndTemplates(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-simulate-valid",
		Name:               "Bootstrap Simulate Valid",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-simulate-valid/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-simulate-valid/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	simReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-simulate-valid/bootstrap-session/simulate-pr", bytes.NewReader([]byte(`{"dryRunCommit":true}`)))
	simReq.Header.Set("Content-Type", "application/json")
	simRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(simRec, simReq)
	if simRec.Code != http.StatusOK {
		t.Fatalf("simulate status=%d body=%s", simRec.Code, simRec.Body.String())
	}

	type bootstrapManifestResponseItem struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		YAML      string `json:"yaml"`
	}

	var response struct {
		Validation struct {
			Valid  bool `json:"valid"`
			Issues []struct {
				File    string `json:"file"`
				Line    int    `json:"line"`
				Column  int    `json:"column"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"issues"`
		} `json:"validation"`
		ManifestTemplates []bootstrapManifestResponseItem `json:"manifestTemplates"`
		DryRun            *struct {
			Enabled     bool     `json:"enabled"`
			Status      string   `json:"status"`
			Message     string   `json:"message"`
			CommitPath  string   `json:"commitPath"`
			FileCount   int      `json:"fileCount"`
			Files       []string `json:"files"`
			SimulatedAt string   `json:"simulatedAt"`
		} `json:"dryRun"`
	}

	if err := json.Unmarshal(simRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode simulate response: %v", err)
	}
	if response.Validation.Valid != true {
		t.Fatalf("expected validation valid=true, got false body=%s", simRec.Body.String())
	}
	if len(response.Validation.Issues) != 0 {
		t.Fatalf("expected no validation issues, got %d", len(response.Validation.Issues))
	}
	if response.DryRun == nil {
		t.Fatalf("expected dry run summary, got nil")
	}
	if response.DryRun.Status != "simulated" {
		t.Fatalf("expected dryRun status simulated, got %q", response.DryRun.Status)
	}
	if response.DryRun.FileCount != len(response.ManifestTemplates) {
		t.Fatalf("expected dryRun fileCount=%d, got %d", len(response.ManifestTemplates), response.DryRun.FileCount)
	}
	if response.DryRun.CommitPath == "" {
		t.Fatalf("expected non-empty dry-run commit path")
	}
	if len(response.ManifestTemplates) != 1 {
		t.Fatalf("expected one generated template, got %d", len(response.ManifestTemplates))
	}
}

func TestBootstrapSessionSimulateEndpointReturnsInvalidValidationResult(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-simulate-invalid",
		Name:               "Bootstrap Simulate Invalid",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-simulate-invalid/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Service",
	      "namespace": "dev-base",
	      "name": "orders",
	      "yaml": "apiVersion: v1\nkind: Service\nmetadata:\n  name: orders\n  namespace: dev-base\nspec:\n  selector:\n    app: orders\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-simulate-invalid/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	simReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-simulate-invalid/bootstrap-session/simulate-pr", nil)
	simRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(simRec, simReq)
	if simRec.Code != http.StatusOK {
		t.Fatalf("simulate status=%d body=%s", simRec.Code, simRec.Body.String())
	}

	var response struct {
		Validation struct {
			Valid  bool `json:"valid"`
			Issues []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"issues"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(simRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode simulate response: %v", err)
	}
	if response.Validation.Valid {
		t.Fatalf("expected validation valid=false, got true: %s", simRec.Body.String())
	}
	if len(response.Validation.Issues) == 0 {
		t.Fatalf("expected validation issues, got none: %s", simRec.Body.String())
	}
	if response.Validation.Issues[0].Code == "" {
		t.Fatalf("expected issue code, got none")
	}
}

func TestBootstrapSessionSimulateEndpointCanRunWithoutDryRun(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-simulate-nodryrun",
		Name:               "Bootstrap Simulate No Dry-Run",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-simulate-nodryrun/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-simulate-nodryrun/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	simReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-simulate-nodryrun/bootstrap-session/simulate-pr", bytes.NewReader([]byte(`{"dry_run_commit":false}`)))
	simReq.Header.Set("Content-Type", "application/json")
	simRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(simRec, simReq)
	if simRec.Code != http.StatusOK {
		t.Fatalf("simulate status=%d body=%s", simRec.Code, simRec.Body.String())
	}

	var response struct {
		DryRun *struct {
			Enabled bool `json:"enabled"`
		} `json:"dryRun"`
	}
	if err := json.Unmarshal(simRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode simulate response: %v", err)
	}
	if response.DryRun != nil {
		t.Fatalf("expected dryRun to be omitted when dry-run disabled")
	}
}

func TestBootstrapSessionCompileSavesProjectConfigSecurelyAndAudits(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-secure-config",
		Name:               "Bootstrap Secure Config",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-secure-config/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	const appToken = "super-secret-app-token"
	const manualSecret = "super-secret-db-password"
	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "appToken": "` + appToken + `",
	    "secretStrategies": {
	      "dev/db-password": {
	        "strategy": "manual input",
	        "required": true,
	        "serviceId": "Service/dev/orders",
	        "container": "orders-api",
	        "variable": "DB_PASSWORD",
	        "manualValue": "` + manualSecret + `"
	      }
	    },
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-secure-config/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-secure-config/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusOK {
		t.Fatalf("compile session status=%d body=%s", compileRec.Code, compileRec.Body.String())
	}

	getConfigReq := httptest.NewRequest(http.MethodGet, "/api/projects/bootstrap-secure-config/config", nil)
	getConfigRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getConfigRec, getConfigReq)
	if getConfigRec.Code != http.StatusOK {
		t.Fatalf("get config status=%d body=%s", getConfigRec.Code, getConfigRec.Body.String())
	}
	if strings.Contains(getConfigRec.Body.String(), appToken) || strings.Contains(getConfigRec.Body.String(), manualSecret) {
		t.Fatalf("config response leaked plaintext secret: %s", getConfigRec.Body.String())
	}
	if strings.Contains(getConfigRec.Body.String(), "ciphertext") {
		t.Fatalf("config response leaked encrypted envelope: %s", getConfigRec.Body.String())
	}
	if !strings.Contains(getConfigRec.Body.String(), `"version":1`) {
		t.Fatalf("expected config version 1, got %s", getConfigRec.Body.String())
	}
	if !strings.Contains(getConfigRec.Body.String(), `"sensitive_refs"`) {
		t.Fatalf("expected sensitive refs in config response, got %s", getConfigRec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if bytes.Contains(raw, []byte(appToken)) || bytes.Contains(raw, []byte(manualSecret)) {
		t.Fatalf("audit log leaked plaintext secret: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"event":"project_config_saved"`)) {
		t.Fatalf("expected project config audit event: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"config_version":1`)) {
		t.Fatalf("expected config version in audit event: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventProjectConfigSaved)
	assertStandardAuditEvent(t, entry, auditEventProjectConfigSaved, auditEndpointBootstrapSessionCompile, "bootstrap-secure-config", "", false)
}

func TestProjectRuntimeBundleDoesNotLeakSensitiveValues(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	projectID := "runtime-bundle-leak-project"
	appToken := "bundle-super-secret-app-token"
	manualSecret := "bundle-super-secret-db-password"
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               "Runtime Bundle Leak Project",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(fmt.Sprintf(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "appToken": %q,
	    "secretStrategies": {
	      "orders/db-password": {
	        "name": "db-password",
	        "namespace": "orders",
	        "strategy": "manual input",
	        "manualValue": %q
	      }
	    },
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\n  labels:\n    app.kubernetes.io/managed-by: envpilot\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`, appToken, manualSecret))
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusOK {
		t.Fatalf("compile session status=%d body=%s", compileRec.Code, compileRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/runtime-bundle", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(appToken)) || bytes.Contains(rec.Body.Bytes(), []byte(manualSecret)) {
		t.Fatalf("runtime bundle archive leaked plaintext secret")
	}

	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	foundConfig := false
	foundEncryptedPayload := false
	for _, file := range reader.File {
		handle, err := file.Open()
		if err != nil {
			t.Fatalf("open bundle entry %s: %v", file.Name, err)
		}
		content, err := io.ReadAll(handle)
		_ = handle.Close()
		if err != nil {
			t.Fatalf("read bundle entry %s: %v", file.Name, err)
		}
		if bytes.Contains(content, []byte(appToken)) || bytes.Contains(content, []byte(manualSecret)) {
			t.Fatalf("bundle entry %s leaked plaintext secret: %s", file.Name, string(content))
		}
		if strings.HasPrefix(file.Name, "templates/") &&
			(bytes.Contains(content, []byte("appToken")) || bytes.Contains(content, []byte("manualValue"))) {
			t.Fatalf("generated manifest %s contains sensitive config fields: %s", file.Name, string(content))
		}
		if file.Name == "project-config.enc.yaml" {
			foundConfig = true
			if bytes.Contains(content, []byte("\nsensitive:")) || bytes.HasPrefix(content, []byte("sensitive:")) {
				t.Fatalf("runtime bundle serialized raw Sensitive map: %s", string(content))
			}
			if bytes.Contains(content, []byte("encryptedSensitive:")) && bytes.Contains(content, []byte("ciphertext:")) {
				foundEncryptedPayload = true
			}
		}
	}
	if !foundConfig {
		t.Fatalf("runtime bundle missing project-config.enc.yaml")
	}
	if !foundEncryptedPayload {
		t.Fatalf("runtime bundle missing encrypted sensitive payload")
	}

	rawLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if bytes.Contains(rawLog, []byte(appToken)) || bytes.Contains(rawLog, []byte(manualSecret)) {
		t.Fatalf("audit log leaked plaintext secret: %s", string(rawLog))
	}
}

func TestProjectRuntimeBundleDownloadIsDeterministic(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "runtime-bundle-project",
		Name:               "Runtime Bundle Project",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/runtime-bundle-project/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	updateBody := []byte(`{
	  "current_step": 9,
	  "status": "reviewed",
	  "step_data": {
	    "manifestTemplates": [{
	      "kind": "Deployment",
	      "namespace": "envpilot-pr-{{ .PRNumber }}",
	      "name": "orders",
	      "yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders\n  namespace: envpilot-pr-{{ .PRNumber }}\nspec:\n  template:\n    spec:\n      containers:\n      - name: orders\n        image: ghcr.io/acme/orders:{{ .CommitSHA }}\n"
	    }]
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/runtime-bundle-project/bootstrap-session", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	compileReq := httptest.NewRequest(http.MethodPost, "/api/projects/runtime-bundle-project/bootstrap-session/compile", nil)
	compileRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(compileRec, compileReq)
	if compileRec.Code != http.StatusOK {
		t.Fatalf("compile session status=%d body=%s", compileRec.Code, compileRec.Body.String())
	}

	download := func() []byte {
		req := httptest.NewRequest(http.MethodGet, "/api/projects/runtime-bundle-project/runtime-bundle", nil)
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("download bundle status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "application/zip") {
			t.Fatalf("unexpected content-type: %s", rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Header().Get("Content-Disposition"), "runtime-bundle.zip") {
			t.Fatalf("unexpected content-disposition: %s", rec.Header().Get("Content-Disposition"))
		}
		return rec.Body.Bytes()
	}

	first := download()
	second := download()
	if !bytes.Equal(first, second) {
		t.Fatalf("bundle download is not deterministic")
	}

	reader, err := zip.NewReader(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	entries := make(map[string]bool)
	for _, file := range reader.File {
		entries[file.Name] = true
	}
	for _, required := range []string{
		"project-config.enc.yaml",
		"templates/envpilot-pr-{{-.PRNumber-}}/deployment-orders.yaml",
		"runner/runner-helm-values.yaml",
		"runner/deployment.yaml",
		"runner/rbac.yaml",
	} {
		if !entries[required] {
			t.Fatalf("missing bundle entry: %q", required)
		}
	}
}

func TestBootstrapSessionGeneratesResourcePolicyTemplates(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-policy-templates",
		Name:               "Bootstrap Policy Templates",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-policy-templates/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "defaultTTLHours": 48,
	    "cpuRequest": "250m",
	    "cpuLimit": "1000m",
	    "memoryRequest": "256Mi",
	    "memoryLimit": "1Gi",
	    "storageQuota": "10Gi",
	    "maxActiveEnvironments": 12
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-policy-templates/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	var payload struct {
		Data struct {
			ManifestTemplates []struct {
				Kind string `json:"kind"`
				YAML string `json:"yaml"`
			} `json:"manifestTemplates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(updateRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if len(payload.Data.ManifestTemplates) < 2 {
		t.Fatalf("expected policy templates generated, got %d", len(payload.Data.ManifestTemplates))
	}
	kinds := map[string]string{}
	for _, item := range payload.Data.ManifestTemplates {
		kinds[item.Kind] = item.YAML
	}
	if _, ok := kinds["ResourceQuota"]; !ok {
		t.Fatalf("missing ResourceQuota template")
	}
	if _, ok := kinds["LimitRange"]; !ok {
		t.Fatalf("missing LimitRange template")
	}
	if !strings.Contains(kinds["ResourceQuota"], "requests.storage: 10Gi") {
		t.Fatalf("resource quota template does not include storage quota: %s", kinds["ResourceQuota"])
	}
	if !strings.Contains(kinds["LimitRange"], "cpu: 250m") || !strings.Contains(kinds["LimitRange"], "memory: 256Mi") {
		t.Fatalf("limit range template does not include requests/limits: %s", kinds["LimitRange"])
	}
}

func TestBootstrapSessionRejectsInvalidResourcePolicyLimits(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-policy-invalid",
		Name:               "Bootstrap Policy Invalid",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-policy-invalid/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "defaultTTLHours": 24,
	    "cpuRequest": "foo",
	    "cpuLimit": "1000m",
	    "memoryRequest": "256Mi",
	    "memoryLimit": "1Gi",
	    "storageQuota": "10Gi",
	    "maxActiveEnvironments": 5
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-policy-invalid/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", updateRec.Code, updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), "invalid resource policy") {
		t.Fatalf("expected resource policy error, got %s", updateRec.Body.String())
	}
}

func TestBootstrapSessionGeneratesNetworkPolicyTemplates(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-network-policy",
		Name:               "Bootstrap Network Policy",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
		BaseEnvConfig: domain.BaseEnvConfig{
			Namespace: "dev-base",
		},
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-network-policy/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "selectedBaseNamespaces": ["dev-base"],
	    "networkFeatureToBase": true,
	    "networkBaseToFeature": true,
	    "networkEgressMode": "restricted"
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-network-policy/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	var payload struct {
		Data struct {
			ManifestTemplates []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
				YAML string `json:"yaml"`
			} `json:"manifestTemplates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(updateRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	count := 0
	joined := ""
	for _, item := range payload.Data.ManifestTemplates {
		if item.Kind == "NetworkPolicy" {
			count++
			joined += item.YAML
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 NetworkPolicy templates by default, got %d", count)
	}
	if !strings.Contains(joined, "envpilot-allow-base-to-feature") {
		t.Fatalf("missing base to feature policy: %s", joined)
	}
	if strings.Contains(joined, "envpilot-allow-feature-to-base") {
		t.Fatalf("unexpected base namespace policy without explicit opt-in: %s", joined)
	}
	if !strings.Contains(joined, "k8s-app: kube-dns") {
		t.Fatalf("missing restricted egress DNS rule: %s", joined)
	}
}

func TestBootstrapSessionGeneratesBaseNamespaceNetworkPolicyTemplatesWithExplicitOptIn(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-network-policy-optin",
		Name:               "Bootstrap Network Policy OptIn",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
		BaseEnvConfig: domain.BaseEnvConfig{
			Namespace: "dev-base",
		},
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-network-policy-optin/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	body := []byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "selectedBaseNamespaces": ["dev-base"],
	    "networkFeatureToBase": true,
	    "networkBaseToFeature": true,
	    "networkEgressMode": "restricted",
	    "networkAllowBaseNamespacePolicies": true
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-network-policy-optin/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}
	var payload struct {
		Data struct {
			ManifestTemplates []struct {
				Kind string `json:"kind"`
				YAML string `json:"yaml"`
			} `json:"manifestTemplates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(updateRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	count := 0
	joined := ""
	for _, item := range payload.Data.ManifestTemplates {
		if item.Kind == "NetworkPolicy" {
			count++
			joined += item.YAML
		}
	}
	if count != 3 {
		t.Fatalf("expected 3 NetworkPolicy templates with explicit opt-in, got %d", count)
	}
	if !strings.Contains(joined, "envpilot-allow-feature-to-base") {
		t.Fatalf("missing feature to base policy with explicit opt-in: %s", joined)
	}
}

func TestBootstrapSessionRejectsInvalidNetworkPolicyMode(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-network-policy-invalid",
		Name:               "Bootstrap Network Policy Invalid",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
		BaseEnvConfig: domain.BaseEnvConfig{
			Namespace: "dev-base",
		},
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-network-policy-invalid/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "networkFeatureToBase": true,
	    "networkBaseToFeature": false,
	    "networkEgressMode": "invalid"
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-network-policy-invalid/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", updateRec.Code, updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), "invalid network policy") {
		t.Fatalf("expected network policy error, got %s", updateRec.Body.String())
	}
}

func TestBootstrapSessionPersistsCleanupSafetyConfig(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-cleanup-safety",
		Name:               "Bootstrap Cleanup Safety",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-cleanup-safety/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "cleanupProtectedNamespaces": "default,kube-system,flux-system",
	    "cleanupDeleteEnvPlaneLabelsOnly": true,
	    "cleanupFinalizerStrategy": "foreground"
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-cleanup-safety/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), `"cleanupProtectedNamespaces":"default,kube-system,flux-system"`) {
		t.Fatalf("cleanup protected namespaces not persisted: %s", updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), `"cleanupDeleteEnvPlaneLabelsOnly":true`) {
		t.Fatalf("labels-only cleanup flag not persisted: %s", updateRec.Body.String())
	}
}

func TestBootstrapSessionRejectsProtectedFeatureNamespaceCleanupTarget(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-cleanup-protected",
		Name:               "Bootstrap Cleanup Protected",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-cleanup-protected/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	body := []byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "featureNamespaceTemplate": "envpilot-pr-{{ .PRNumber }}",
	    "cleanupProtectedNamespaces": "default,envpilot-pr-{{ .PRNumber }}",
	    "cleanupDeleteEnvPlaneLabelsOnly": true,
	    "cleanupFinalizerStrategy": "foreground"
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-cleanup-protected/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", updateRec.Code, updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), "protected namespace") {
		t.Fatalf("expected protected namespace error, got %s", updateRec.Body.String())
	}
}

func TestBootstrapSecretStrategiesAreMaskedAndAudited(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "secure-secrets-bootstrap",
		Name:               "Secure Secrets Bootstrap",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/secure-secrets-bootstrap/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("expected create 200, got %d: %s", createRec.Code, createRec.Body.String())
	}

	const secret = "super-secret-db-password"
	body := []byte(`{
	  "current_step": 8,
	  "status": "reviewed",
	  "step_data": {
	    "secretStrategies": {
	      "dev/db-password": {
	        "strategy": "manual input",
	        "required": true,
	        "serviceId": "Service/dev/orders",
	        "container": "orders-api",
	        "variable": "DB_PASSWORD",
	        "manualValue": "` + secret + `"
	      }
	    }
	  }
	}`)
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/projects/secure-secrets-bootstrap/bootstrap-session", bytes.NewReader(body))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected update 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}
	if strings.Contains(updateRec.Body.String(), secret) {
		t.Fatalf("update response leaked plaintext secret: %s", updateRec.Body.String())
	}
	if !strings.Contains(updateRec.Body.String(), `"manualValueMasked":true`) {
		t.Fatalf("expected manual value masked marker in update response: %s", updateRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/projects/secure-secrets-bootstrap/bootstrap-session", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	if strings.Contains(getRec.Body.String(), secret) {
		t.Fatalf("get response leaked plaintext secret: %s", getRec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("audit log leaked plaintext secret: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"event":"bootstrap_secret_strategies_saved"`)) {
		t.Fatalf("expected secret strategy audit event: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"dev/db-password"`)) {
		t.Fatalf("expected secret id in audit event: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventBootstrapSecretStrategiesSaved)
	assertStandardAuditEvent(t, entry, auditEventBootstrapSecretStrategiesSaved, auditEndpointBootstrapSessionUpdate, "secure-secrets-bootstrap", "", false)
}

func TestRateLimitAppliesAcrossAPISessionRequests(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	t.Setenv("ENVPLANE_RATE_LIMIT_REQUESTS", "1")
	t.Setenv("ENVPLANE_RATE_LIMIT_SECONDS", "60")
	application, _, _ := newTestServer(t, "")

	sessionCookie := &http.Cookie{Name: apiSessionCookieName, Value: "readonly-token"}
	firstReq := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	firstReq.AddCookie(sessionCookie)
	firstRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("expected first API request 200, got %d", firstRec.Code)
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	apiReq.AddCookie(sessionCookie)
	apiRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected API request to exceed shared token limit with 429, got %d: %s", apiRec.Code, apiRec.Body.String())
	}
	if got := apiRec.Header().Get("Retry-After"); got == "" {
		t.Fatalf("expected retry-after header on 429")
	}
	if got := apiRec.Header().Get("RateLimit-Limit"); got != "1" {
		t.Fatalf("expected RateLimit-Limit header 1, got %q", got)
	}
	if got := apiRec.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Fatalf("expected RateLimit-Remaining header 0, got %q", got)
	}
}

func TestRateLimitReloadCanDisableLimitAtRuntime(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	t.Setenv("ENVPLANE_RATE_LIMIT_REQUESTS", "1")
	t.Setenv("ENVPLANE_RATE_LIMIT_SECONDS", "60")
	application, _, _ := newTestServer(t, "")

	makeRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
		req.Header.Set("Authorization", "Bearer readonly-token")
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		return rec
	}

	if rec := makeRequest(); rec.Code != http.StatusOK {
		t.Fatalf("expected first request 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := makeRequest(); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second request 429 before runtime reload, got %d: %s", rec.Code, rec.Body.String())
	}

	cfg := application.config()
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	if rec := makeRequest(); rec.Code != http.StatusOK {
		t.Fatalf("expected request to pass after disabling rate limit via reload, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOpenAPISpecEndpointReturnsOpenAPIDocument(t *testing.T) {
	application, _, _ := newTestServer(t, "")

	for _, path := range []string{"/api/v1/openapi.json", "/openapi.json"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected openapi endpoint to be accessible, got %d: %s", rec.Code, rec.Body.String())
			}

			var doc map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode openapi json: %v", err)
			}
			if doc["openapi"] != "3.0.3" {
				t.Fatalf("unexpected openapi version: %#v", doc["openapi"])
			}
			paths, ok := doc["paths"].(map[string]any)
			if !ok {
				t.Fatalf("openapi spec missing paths section")
			}
			if _, ok := paths["/api/v1/products"]; !ok {
				t.Fatalf("openapi spec is expected to include products path")
			}
		})
	}
}

func TestOpenAPIAgentResourceScanUsesAgentAuthToken(t *testing.T) {
	application, _, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected openapi endpoint 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode openapi json: %v", err)
	}
	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("openapi spec missing components")
	}
	securitySchemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("openapi spec missing security schemes")
	}
	if _, ok := securitySchemes["agentAuthToken"]; !ok {
		t.Fatalf("openapi spec missing agentAuthToken security scheme")
	}
	if _, ok := securitySchemes["runnerAuthToken"]; !ok {
		t.Fatalf("openapi spec missing runnerAuthToken security scheme")
	}
	if _, ok := securitySchemes["projectConfigToken"]; !ok {
		t.Fatalf("openapi spec missing projectConfigToken security scheme")
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("openapi spec missing paths section")
	}
	for _, path := range []string{"/api/v1/agents/resource-scan", "/api/v1/agents/resource-scan/next"} {
		rawPath, ok := paths[path]
		if !ok {
			t.Fatalf("openapi spec missing %s", path)
		}
		encoded, err := json.Marshal(rawPath)
		if err != nil {
			t.Fatalf("marshal %s path: %v", path, err)
		}
		if bytes.Contains(encoded, []byte("registrationToken")) {
			t.Fatalf("%s must not document registrationToken: %s", path, string(encoded))
		}
		if !bytes.Contains(encoded, []byte("agentAuthToken")) {
			t.Fatalf("%s must document agentAuthToken auth: %s", path, string(encoded))
		}
	}
	for path, expectedAuth := range map[string]string{
		"/api/v1/agents/heartbeat":            "agentAuthToken",
		"/api/v1/runners/heartbeat":           "runnerAuthToken",
		"/api/v1/projects/{id}/runner-config": "projectConfigToken",
	} {
		rawPath, ok := paths[path]
		if !ok {
			t.Fatalf("openapi spec missing %s", path)
		}
		encoded, err := json.Marshal(rawPath)
		if err != nil {
			t.Fatalf("marshal %s path: %v", path, err)
		}
		if bytes.Contains(encoded, []byte("registrationToken")) {
			t.Fatalf("%s must not document registrationToken: %s", path, string(encoded))
		}
		if bytes.Contains(encoded, []byte("projectConfigToken")) && path != "/api/v1/projects/{id}/runner-config" {
			t.Fatalf("%s must not document body projectConfigToken: %s", path, string(encoded))
		}
		if !bytes.Contains(encoded, []byte(expectedAuth)) {
			t.Fatalf("%s must document %s auth: %s", path, expectedAuth, string(encoded))
		}
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("openapi spec missing schemas")
	}
	scanRequest, ok := schemas["AgentResourceScanRequest"]
	if !ok {
		t.Fatalf("openapi spec missing AgentResourceScanRequest")
	}
	encodedScanRequest, err := json.Marshal(scanRequest)
	if err != nil {
		t.Fatalf("marshal AgentResourceScanRequest: %v", err)
	}
	if bytes.Contains(encodedScanRequest, []byte("registrationToken")) {
		t.Fatalf("AgentResourceScanRequest must not include registrationToken: %s", string(encodedScanRequest))
	}
	if bytes.Contains(encodedScanRequest, []byte("agentAuthToken")) || bytes.Contains(encodedScanRequest, []byte("agent_auth_token")) {
		t.Fatalf("AgentResourceScanRequest must not include body auth token fields: %s", string(encodedScanRequest))
	}
}

func TestMetricsEndpointReturnsPrometheusTextAndIncrementsCounters(t *testing.T) {
	t.Setenv("ENVPLANE_API_READ_TOKEN", "readonly-token")
	application, _, _ := newTestServer(t, "")

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	listReq.Header.Set("Authorization", "Bearer readonly-token")
	listRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected products list 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	notFoundReq := httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil)
	notFoundReq.Header.Set("Authorization", "Bearer readonly-token")
	notFoundRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(notFoundRec, notFoundReq)
	if notFoundRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown path, got %d: %s", notFoundRec.Code, notFoundRec.Body.String())
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer readonly-token")
	metricsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("expected metrics 200, got %d: %s", metricsRec.Code, metricsRec.Body.String())
	}

	text := metricsRec.Body.String()
	if !strings.Contains(text, "envpilot_http_requests_total") {
		t.Fatalf("missing total requests metric: %s", text)
	}
	if !strings.Contains(text, `envpilot_http_requests_by_method_total{method="GET"}`) {
		t.Fatalf("missing method metric: %s", text)
	}
	if !strings.Contains(text, `envpilot_http_requests_by_path_total{path="/api/v1/products"`) {
		t.Fatalf("missing product path metric: %s", text)
	}
	if !strings.Contains(text, "envpilot_http_requests_in_flight ") {
		t.Fatalf("missing in-flight metric: %s", text)
	}
	if !strings.Contains(text, "envpilot_http_request_duration_seconds_bucket{le=\"+Inf\"}") {
		t.Fatalf("missing request duration histogram: %s", text)
	}
	if !strings.Contains(text, "envpilot_http_requests_4xx_total ") {
		t.Fatalf("missing 4xx metric: %s", text)
	}
	if !strings.Contains(text, "envpilot_http_requests_5xx_total ") {
		t.Fatalf("missing 5xx metric: %s", text)
	}
	if !strings.Contains(text, "envpilot_http_requests_4xx_total 1") {
		t.Fatalf("expected at least one 4xx request to be counted: %s", text)
	}
	if contentType := metricsRec.Header().Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("expected text/plain metrics response, got %q", contentType)
	}
}

func TestRequestTracingGeneratesOrPreservesTraceHeader(t *testing.T) {
	application, _, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("X-Trace-Id", "0123456789abcdef0123456789abcdef")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	traceID := rec.Header().Get("X-Trace-Id")
	if traceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("expected trace id to be propagated, got %q", traceID)
	}
	traceParent := rec.Header().Get("traceparent")
	if !isValidTraceParentHeader(t, traceParent, "0123456789abcdef0123456789abcdef") {
		t.Fatalf("invalid traceparent header %q", traceParent)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("traceparent", "00-11111111111111111111111111111111-aaaaaaaaaaaaaaaa-01")
	rec = httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	traceID = rec.Header().Get("X-Trace-Id")
	if traceID != "11111111111111111111111111111111" {
		t.Fatalf("expected trace id derived from traceparent, got %q", traceID)
	}
	traceParent = rec.Header().Get("traceparent")
	if !isValidTraceParentHeader(t, traceParent, "11111111111111111111111111111111") {
		t.Fatalf("invalid traceparent header %q", traceParent)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec = httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	traceID = rec.Header().Get("X-Trace-Id")
	if len(traceID) != 32 {
		t.Fatalf("expected generated trace id to have 32 chars, got %q", traceID)
	}
	if _, err := hex.DecodeString(traceID); err != nil {
		t.Fatalf("expected generated trace id to be hex, got %q: %v", traceID, err)
	}
	traceParent = rec.Header().Get("traceparent")
	if !isValidTraceParentHeader(t, traceParent, traceID) {
		t.Fatalf("invalid generated traceparent header %q", traceParent)
	}
}

func isValidTraceParentHeader(t *testing.T, headerValue, expectedTraceID string) bool {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(headerValue), "-")
	if len(parts) != 4 {
		t.Fatalf("traceparent should have 4 parts, got %q", headerValue)
	}
	if parts[0] != "00" {
		t.Fatalf("expected traceparent version 00, got %q", parts[0])
	}
	if parts[1] != expectedTraceID {
		t.Fatalf("expected trace id %q, got %q", expectedTraceID, parts[1])
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		t.Fatalf("invalid trace-id in traceparent %q: %v", parts[1], err)
	}
	if len(parts[2]) != 16 {
		t.Fatalf("expected span-id 16 hex chars, got %q", parts[2])
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		t.Fatalf("invalid span-id in traceparent %q: %v", parts[2], err)
	}
	if strings.EqualFold(parts[2], "0000000000000000") {
		t.Fatalf("span-id must not be zero")
	}
	if parts[3] != "01" {
		t.Fatalf("expected trace-flags 01, got %q", parts[3])
	}
	return true
}

func TestGitHubPoCWebhookEndpointReturnsOKValidatesSignatureAndLogsMetadataOnly(t *testing.T) {
	application, envStore, logs := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayloadWithNumber("opened", "1904", "feature/kan-1904"))

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-poc")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.Type != jobs.TypeCreateEnvironment {
		t.Fatalf("expected create job, got %q", job.Type)
	}
	env, err := envStore.Get("pr-1904")
	if err != nil {
		t.Fatalf("expected environment record created from webhook: %v", err)
	}
	if env.Status != domain.StatusCreating {
		t.Fatalf("environment status = %q", env.Status)
	}
	if env.ManifestPath != "" || env.NamespaceManifestPath != "" {
		t.Fatalf("expected queued environment without rendered manifests, got namespace=%q flux=%q", env.NamespaceManifestPath, env.ManifestPath)
	}
	if !strings.Contains(logs.String(), "github webhook") || !strings.Contains(logs.String(), `"pr_number":1904`) {
		t.Fatalf("expected metadata log, got %s", logs.String())
	}
	if strings.Contains(logs.String(), "feature/kan-1904") || strings.Contains(logs.String(), `"payload"`) {
		t.Fatalf("normal github webhook log must not contain raw payload body: %s", logs.String())
	}
}

func TestGitHubPoCWebhookRejectsInvalidSignature(t *testing.T) {
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayloadWithNumber("opened", "1905", "feature/kan-1905"))

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookClosedCreatesDeleteJob(t *testing.T) {
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayloadWithNumber("closed", "1903", "feature/kan-1903"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.Type != jobs.TypeDeleteEnvironment {
		t.Fatalf("expected delete job, got %q", job.Type)
	}
	if job.EnvironmentID != "pr-1903" {
		t.Fatalf("expected pr-1903, got %q", job.EnvironmentID)
	}
}

func TestGitHubWebhookResolvesProjectBindingFromRepository(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "checkout-app",
  "gitops_repository_id": "platform-gitops",
  "git_repo": {
    "provider": "github",
    "url": "https://github.com/owner/repo.git",
    "default_branch": "main"
  }
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(githubPullRequestPayloadWithNumber("opened", "1910", "feature/payment"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("pr-1910")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected resolved binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestGitHubWebhookResolvesProjectBindingFromSettingsRepositoryID(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	settingsBody := []byte(`{
  "repositories": [
    {
      "id": "checkout-app",
      "kind": "application",
      "provider": "github",
      "url": "https://github.com/owner/repo.git",
      "default_branch": "main"
    }
  ],
  "runtime": {
    "default_product": "generic",
    "default_project": "default",
    "default_mode": "full",
    "domain_root": "feature.int",
    "namespace_prefix": "envpilot-pr",
    "default_ttl_hours": 48
  }
}`)
	settingsReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings", bytes.NewReader(settingsBody))
	settingsReq.Header.Set("Content-Type", "application/json")
	settingsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(settingsRec, settingsReq)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("expected settings save 200, got %d: %s", settingsRec.Code, settingsRec.Body.String())
	}

	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "checkout-app",
  "gitops_repository_id": "platform-gitops"
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(githubPullRequestPayloadWithNumber("opened", "1911", "feature/payment"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("pr-1911")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected resolved binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestWebhookResolvesProjectClusterIDFromProjectConfig(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "owner/repo",
  "gitops_repository_id": "platform-gitops",
  "cluster_id": "dev-us"
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(githubPullRequestPayloadWithNumber("opened", "1937", "feature/payment"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "delivery-cluster-1937")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected webhook accepted 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("pr-1937")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" {
		t.Fatalf("expected project checkout, got %q", env.Project)
	}
	if env.ClusterID != "dev-us" {
		t.Fatalf("expected cluster_id dev-us, got %q", env.ClusterID)
	}
}

func TestGitHubWebhookResolvesProjectBindingFromInstallationID(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "other/repo",
  "gitops_repository_id": "platform-gitops",
  "github_installation_ids": ["123"]
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action:         "opened",
		Number:         "1920",
		Branch:         "feature/payment",
		InstallationID: "123",
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("pr-1920")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected resolved binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestGitHubWebhookResolvesNormalizedRepositoryFromProjectGitRepo(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "other/repo",
  "gitops_repository_id": "platform-gitops",
  "git_repo": {
    "provider": "github",
    "url": "git+ssh://git@github.com/owner/repo.git",
    "default_branch": "main"
  }
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(githubPullRequestPayloadWithNumber("opened", "1921", "feature/payment"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("pr-1921")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected resolved binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestGitLabWebhookRejectsInvalidToken(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(gitlabMergeRequestPayload("open", "opened", "feature/kan-2001"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "wrong")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitLabNoteWebhookRejectsInvalidToken(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(gitlabNotePayload("2001", "/envpilot destroy"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	req.Header.Set("X-Gitlab-Token", "wrong")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitLabWebhookRejectsMalformedValidTokenPayload(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(`{"not":`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGitLabWebhookAcceptsMergeRequestWithValidToken(t *testing.T) {
	application, envStore, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(gitlabMergeRequestPayload("open", "opened", "feature/kan-2002"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.Type != jobs.TypeCreateEnvironment {
		t.Fatalf("expected create job, got %q", job.Type)
	}
	if job.Status != jobs.StatusQueued {
		t.Fatalf("expected queued job, got %q", job.Status)
	}
	env, err := envStore.Get("mr-2002")
	if err != nil {
		t.Fatalf("expected environment record created from webhook: %v", err)
	}
	if env.Status != domain.StatusCreating {
		t.Fatalf("environment status = %q", env.Status)
	}
	if env.ManifestPath != "" || env.NamespaceManifestPath != "" {
		t.Fatalf("expected queued environment without rendered manifests, got namespace=%q flux=%q", env.NamespaceManifestPath, env.ManifestPath)
	}
}

func TestGitLabWebhookResolvesProjectBindingFromProjectID(t *testing.T) {
	application, envStore, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	projectBody := []byte(`{
  "name": "CMS",
  "product_id": "bethunder",
  "app_repository_id": "other/repo",
  "gitops_repository_id": "platform-gitops",
  "gitlab_project_ids": ["777"]
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/cms", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(gitlabMergeRequestPayloadWithIIDAndProjectID("open", "opened", "1922", "feature/payment", "777"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("mr-1922")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "cms" || env.Product != "bethunder" {
		t.Fatalf("unexpected resolved binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestWebhookIntegrationRoutingIsScopedByProvider(t *testing.T) {
	application, envStore, _ := newTestServerWithSecrets(t, "secret", "gitlab-secret")
	ghProject := []byte(`{
  "name": "Checkout GH",
  "product_id": "bethunder",
  "app_repository_id": "gh-owner/repo",
  "gitops_repository_id": "platform-gitops",
  "github_installation_ids": ["123"]
}`)
	ghReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout-gh", bytes.NewReader(ghProject))
	ghReq.Header.Set("Content-Type", "application/json")
	ghRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(ghRec, ghReq)
	if ghRec.Code != http.StatusOK {
		t.Fatalf("expected github project save 200, got %d: %s", ghRec.Code, ghRec.Body.String())
	}

	glProject := []byte(`{
  "name": "Checkout GL",
  "product_id": "bethunder",
  "app_repository_id": "gl-owner/repo",
  "gitops_repository_id": "platform-gitops",
  "gitlab_project_ids": ["123"]
}`)
	glReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout-gl", bytes.NewReader(glProject))
	glReq.Header.Set("Content-Type", "application/json")
	glRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(glRec, glReq)
	if glRec.Code != http.StatusOK {
		t.Fatalf("expected gitlab project save 200, got %d: %s", glRec.Code, glRec.Body.String())
	}

	ghBody := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action:         "opened",
		Number:         "1941",
		Branch:         "feature/payment",
		InstallationID: "123",
	}))
	ghReqEvent := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(ghBody))
	ghReqEvent.Header.Set("X-GitHub-Event", "pull_request")
	ghReqEvent.Header.Set("X-Hub-Signature-256", githubSignature("secret", ghBody))
	ghEventRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(ghEventRec, ghReqEvent)
	if ghEventRec.Code != http.StatusAccepted {
		t.Fatalf("expected github webhook accepted 202, got %d: %s", ghEventRec.Code, ghEventRec.Body.String())
	}
	ghEnv, err := envStore.Get("pr-1941")
	if err != nil {
		t.Fatalf("expected github environment: %v", err)
	}
	if ghEnv.Project != "checkout-gh" || ghEnv.Product != "bethunder" {
		t.Fatalf("unexpected github routing: project=%q product=%q", ghEnv.Project, ghEnv.Product)
	}

	glBody := []byte(gitlabMergeRequestPayloadWithIIDAndProjectID("open", "opened", "1942", "feature/payment", "123"))
	glReqEvent := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(glBody))
	glReqEvent.Header.Set("X-GitLab-Event", "Merge Request Hook")
	glReqEvent.Header.Set("X-GitLab-Token", "gitlab-secret")
	glEventRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(glEventRec, glReqEvent)
	if glEventRec.Code != http.StatusAccepted {
		t.Fatalf("expected gitlab webhook accepted 202, got %d: %s", glEventRec.Code, glEventRec.Body.String())
	}
	glEnv, err := envStore.Get("mr-1942")
	if err != nil {
		t.Fatalf("expected gitlab environment: %v", err)
	}
	if glEnv.Project != "checkout-gl" || glEnv.Product != "bethunder" {
		t.Fatalf("unexpected gitlab routing: project=%q product=%q", glEnv.Project, glEnv.Product)
	}
}

func TestGitLabWebhookResolvesProjectBindingFromNormalizedRepository(t *testing.T) {
	application, envStore, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "https://gitlab.example/Owner/Repo.git",
  "gitops_repository_id": "platform-gitops",
  "gitlab_project_ids": ["777"]
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(gitlabMergeRequestPayloadWithIIDAndProjectID("open", "opened", "1932", "feature/payment", "999"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	req.Header.Set("X-Gitlab-Delivery", "gitlab-normalized-repo-1932")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("mr-1932")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected resolved binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestGitLabWebhookResolvesProjectBindingFromNormalizedGitRepo(t *testing.T) {
	application, envStore, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "wrong/repo",
  "gitops_repository_id": "platform-gitops",
  "git_repo": {
    "provider": "GITLAB",
    "url": "git+ssh://git@gitlab.example/Owner/Repo.git",
    "default_branch": "main"
  }
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(gitlabMergeRequestPayloadWithIIDAndProjectID("open", "opened", "1940", "feature/payment", "888"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	env, err := envStore.Get("mr-1940")
	if err != nil {
		t.Fatalf("expected reserved environment: %v", err)
	}
	if env.Project != "checkout" || env.Product != "bethunder" {
		t.Fatalf("unexpected resolved binding: project=%q product=%q", env.Project, env.Product)
	}
}

func TestGitLabWebhookDuplicateDeliveryReturnsSameJobWithoutUUID(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(gitlabMergeRequestPayloadWithIIDAndProjectID("open", "opened", "1933", "feature/payment", "888"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	req.Header.Set("X-Gitlab-Delivery", "gitlab-delivery-dup")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected first accepted 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var first jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first job response: %v", err)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	dupReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	dupReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	dupReq.Header.Set("X-Gitlab-Delivery", "gitlab-delivery-dup")
	dupRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusAccepted {
		t.Fatalf("expected duplicate accepted 202, got %d: %s", dupRec.Code, dupRec.Body.String())
	}
	var duplicate jobs.Job
	if err := json.Unmarshal(dupRec.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode duplicate job response: %v", err)
	}
	if first.ID != duplicate.ID {
		t.Fatalf("expected same job id for duplicate delivery, got %q and %q", first.ID, duplicate.ID)
	}
}

func TestGitLabWebhookDuplicateWithoutDeliveryReturnsSameJob(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(gitlabMergeRequestPayloadWithIIDAndProjectID("open", "opened", "1936", "feature/payment", "888"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected first accepted 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var first jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first job response: %v", err)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	dupReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	dupReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	dupRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusAccepted {
		t.Fatalf("expected duplicate accepted 202, got %d: %s", dupRec.Code, dupRec.Body.String())
	}
	var duplicate jobs.Job
	if err := json.Unmarshal(dupRec.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode duplicate job response: %v", err)
	}
	if first.ID != duplicate.ID {
		t.Fatalf("expected same job id for duplicate without delivery id, got %q and %q", first.ID, duplicate.ID)
	}
}

func TestGitLabWebhookAppliesDraftPolicy(t *testing.T) {
	application, envStore, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "owner/repo",
  "gitops_repository_id": "platform-gitops",
  "allow_draft_prs": false
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	draft := []byte(gitlabMergeRequestPayloadWithOptions(gitlabMergeRequestPayloadOptions{
		Action:         "open",
		State:          "opened",
		IID:            "1934",
		Branch:         "feature/payment",
		ProjectID:      "777",
		WorkInProgress: true,
	}))
	draftReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(draft))
	draftReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	draftReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	draftReq.Header.Set("X-Gitlab-Delivery", "gitlab-draft-1934")
	draftRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(draftRec, draftReq)
	if draftRec.Code != http.StatusAccepted {
		t.Fatalf("expected draft response 202, got %d: %s", draftRec.Code, draftRec.Body.String())
	}
	var draftJob jobs.Job
	if err := json.Unmarshal(draftRec.Body.Bytes(), &draftJob); err != nil {
		t.Fatalf("decode draft response: %v", err)
	}
	if draftJob.Status != jobs.StatusIgnored {
		t.Fatalf("expected ignored status for draft MR, got %q", draftJob.Status)
	}
	if _, err := envStore.Get("mr-1934"); err == nil {
		t.Fatalf("expected no environment for draft MR")
	}

	ready := []byte(gitlabMergeRequestPayloadWithOptions(gitlabMergeRequestPayloadOptions{
		Action:         "open",
		State:          "opened",
		IID:            "1935",
		Branch:         "feature/payment",
		ProjectID:      "777",
		WorkInProgress: false,
	}))
	readyReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(ready))
	readyReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	readyReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	readyReq.Header.Set("X-Gitlab-Delivery", "gitlab-ready-1935")
	readyRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusAccepted {
		t.Fatalf("expected ready response 202, got %d: %s", readyRec.Code, readyRec.Body.String())
	}
	var readyJob jobs.Job
	if err := json.Unmarshal(readyRec.Body.Bytes(), &readyJob); err != nil {
		t.Fatalf("decode ready response: %v", err)
	}
	if readyJob.Status != jobs.StatusQueued {
		t.Fatalf("expected queued status for non-draft MR, got %q", readyJob.Status)
	}
	if _, err := envStore.Get("mr-1935"); err != nil {
		t.Fatalf("expected environment created for non-draft MR: %v", err)
	}
}

func TestGitHubWebhookDuplicateDeliveryReturnsSameJob(t *testing.T) {
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action: "opened",
		Number: "1923",
		Branch: "feature/payment",
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "event-dup-1")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected first accepted 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var first jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first job response: %v", err)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	dupReq.Header.Set("X-GitHub-Event", "pull_request")
	dupReq.Header.Set("X-GitHub-Delivery", "event-dup-1")
	dupReq.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	dupRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusAccepted {
		t.Fatalf("expected duplicate accepted 202, got %d: %s", dupRec.Code, dupRec.Body.String())
	}
	var duplicate jobs.Job
	if err := json.Unmarshal(dupRec.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode duplicate job response: %v", err)
	}

	if first.ID != duplicate.ID {
		t.Fatalf("expected same job id for duplicate delivery, got %q and %q", first.ID, duplicate.ID)
	}
}

func TestGitHubWebhookDuplicateWithoutDeliveryReturnsSameJob(t *testing.T) {
	application, _, _ := newTestServer(t, "secret")
	body := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action: "opened",
		Number: "1924",
		Branch: "feature/payment",
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected first accepted 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var first jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first job response: %v", err)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	dupReq.Header.Set("X-GitHub-Event", "pull_request")
	dupReq.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	dupRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusAccepted {
		t.Fatalf("expected duplicate accepted 202, got %d: %s", dupRec.Code, dupRec.Body.String())
	}
	var duplicate jobs.Job
	if err := json.Unmarshal(dupRec.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode duplicate job response: %v", err)
	}
	if first.ID != duplicate.ID {
		t.Fatalf("expected same job id for duplicate without delivery id, got %q and %q", first.ID, duplicate.ID)
	}
}

func TestGitLabWebhookDuplicateDeliveryUUIDReturnsSameJob(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(gitlabMergeRequestPayloadWithIIDAndProjectID("open", "opened", "1924", "feature/payment", "888"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	req.Header.Set("X-Gitlab-Event-UUID", "gitlab-dup-1924")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected first accepted 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var first jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first job response: %v", err)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	dupReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	dupReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	dupReq.Header.Set("X-Gitlab-Event-UUID", "gitlab-dup-1924")
	dupRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusAccepted {
		t.Fatalf("expected duplicate accepted 202, got %d: %s", dupRec.Code, dupRec.Body.String())
	}
	var duplicate jobs.Job
	if err := json.Unmarshal(dupRec.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode duplicate job response: %v", err)
	}
	if first.ID != duplicate.ID {
		t.Fatalf("expected same job id for duplicate UUID, got %q and %q", first.ID, duplicate.ID)
	}
}

func TestGitLabWebhookAppliesBranchFiltersAndLabelPolicy(t *testing.T) {
	application, envStore, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "other/repo",
  "gitops_repository_id": "platform-gitops",
  "gitlab_project_ids": ["777"],
  "branch_filters": ["release/*"],
  "labels": ["team/*"],
  "allow_draft_prs": false
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	branchMissed := []byte(gitlabMergeRequestPayloadWithOptions(gitlabMergeRequestPayloadOptions{
		Action:    "open",
		State:     "opened",
		IID:       "1929",
		Branch:    "feature/payment",
		ProjectID: "777",
		Labels:    []string{"team/backend"},
	}))
	branchReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(branchMissed))
	branchReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	branchReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	branchReq.Header.Set("X-Gitlab-Delivery", "gitlab-branch-1929")
	branchRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(branchRec, branchReq)
	if branchRec.Code != http.StatusAccepted {
		t.Fatalf("expected branch filtered response 202, got %d: %s", branchRec.Code, branchRec.Body.String())
	}
	var branchJob jobs.Job
	if err := json.Unmarshal(branchRec.Body.Bytes(), &branchJob); err != nil {
		t.Fatalf("decode branch response: %v", err)
	}
	if branchJob.Status != jobs.StatusIgnored {
		t.Fatalf("expected ignored status for branch filter miss, got %q", branchJob.Status)
	}
	if _, err := envStore.Get("mr-1929"); err == nil {
		t.Fatalf("expected no environment for ignored branch")
	}

	labelMissed := []byte(gitlabMergeRequestPayloadWithOptions(gitlabMergeRequestPayloadOptions{
		Action:    "open",
		State:     "opened",
		IID:       "1930",
		Branch:    "release/payment",
		ProjectID: "777",
		Labels:    []string{"chore"},
	}))
	labelReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(labelMissed))
	labelReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	labelReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	labelReq.Header.Set("X-Gitlab-Delivery", "gitlab-label-1930")
	labelRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(labelRec, labelReq)
	if labelRec.Code != http.StatusAccepted {
		t.Fatalf("expected label filtered response 202, got %d: %s", labelRec.Code, labelRec.Body.String())
	}
	var labelJob jobs.Job
	if err := json.Unmarshal(labelRec.Body.Bytes(), &labelJob); err != nil {
		t.Fatalf("decode label response: %v", err)
	}
	if labelJob.Status != jobs.StatusIgnored {
		t.Fatalf("expected ignored status for label miss, got %q", labelJob.Status)
	}

	matching := []byte(gitlabMergeRequestPayloadWithOptions(gitlabMergeRequestPayloadOptions{
		Action:    "open",
		State:     "opened",
		IID:       "1931",
		Branch:    "release/payment",
		ProjectID: "777",
		Labels:    []string{"team/backend"},
	}))
	matchingReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(matching))
	matchingReq.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	matchingReq.Header.Set("X-Gitlab-Token", "gitlab-secret")
	matchingReq.Header.Set("X-Gitlab-Delivery", "gitlab-match-1931")
	matchingRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(matchingRec, matchingReq)
	if matchingRec.Code != http.StatusAccepted {
		t.Fatalf("expected matching response 202, got %d: %s", matchingRec.Code, matchingRec.Body.String())
	}
	var matchingJob jobs.Job
	if err := json.Unmarshal(matchingRec.Body.Bytes(), &matchingJob); err != nil {
		t.Fatalf("decode matching response: %v", err)
	}
	if matchingJob.Status != jobs.StatusQueued {
		t.Fatalf("expected queued status for matching webhook, got %q", matchingJob.Status)
	}
	if _, err := envStore.Get("mr-1931"); err != nil {
		t.Fatalf("expected environment created for matching webhook: %v", err)
	}
}

func TestGitHubWebhookAppliesBranchFiltersLabelsAndDraftPolicy(t *testing.T) {
	application, envStore, _ := newTestServer(t, "secret")
	projectBody := []byte(`{
  "name": "Checkout",
  "product_id": "bethunder",
  "app_repository_id": "owner/repo",
  "gitops_repository_id": "platform-gitops",
  "branch_filters": ["release/*"],
  "labels": ["team/*"],
  "allow_draft_prs": false
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	branchMissed := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action: "opened",
		Number: "1925",
		Branch: "feature/payment",
		Labels: []string{"team/backend"},
	}))
	branchReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(branchMissed))
	branchReq.Header.Set("X-GitHub-Event", "pull_request")
	branchReq.Header.Set("X-Hub-Signature-256", githubSignature("secret", branchMissed))
	branchReq.Header.Set("X-GitHub-Delivery", "delivery-branch-1925")
	branchRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(branchRec, branchReq)
	if branchRec.Code != http.StatusAccepted {
		t.Fatalf("expected branch filtered response 202, got %d: %s", branchRec.Code, branchRec.Body.String())
	}
	var branchJob jobs.Job
	if err := json.Unmarshal(branchRec.Body.Bytes(), &branchJob); err != nil {
		t.Fatalf("decode branch response: %v", err)
	}
	if branchJob.Status != jobs.StatusIgnored {
		t.Fatalf("expected ignored status for branch miss, got %q", branchJob.Status)
	}
	if _, err := envStore.Get("pr-1925"); err == nil {
		t.Fatalf("expected no environment for ignored branch")
	}

	labelMissed := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action: "opened",
		Number: "1926",
		Branch: "release/payment",
		Labels: []string{"chore"},
	}))
	labelReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(labelMissed))
	labelReq.Header.Set("X-GitHub-Event", "pull_request")
	labelReq.Header.Set("X-Hub-Signature-256", githubSignature("secret", labelMissed))
	labelReq.Header.Set("X-GitHub-Delivery", "delivery-label-1926")
	labelRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(labelRec, labelReq)
	if labelRec.Code != http.StatusAccepted {
		t.Fatalf("expected label filtered response 202, got %d: %s", labelRec.Code, labelRec.Body.String())
	}
	var labelJob jobs.Job
	if err := json.Unmarshal(labelRec.Body.Bytes(), &labelJob); err != nil {
		t.Fatalf("decode label response: %v", err)
	}
	if labelJob.Status != jobs.StatusIgnored {
		t.Fatalf("expected ignored status for label miss, got %q", labelJob.Status)
	}

	draft := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action: "opened",
		Number: "1927",
		Branch: "release/payment",
		Labels: []string{"team/backend"},
		Draft:  true,
	}))
	draftReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(draft))
	draftReq.Header.Set("X-GitHub-Event", "pull_request")
	draftReq.Header.Set("X-Hub-Signature-256", githubSignature("secret", draft))
	draftReq.Header.Set("X-GitHub-Delivery", "delivery-draft-1927")
	draftRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(draftRec, draftReq)
	if draftRec.Code != http.StatusAccepted {
		t.Fatalf("expected draft response 202, got %d: %s", draftRec.Code, draftRec.Body.String())
	}
	var draftJob jobs.Job
	if err := json.Unmarshal(draftRec.Body.Bytes(), &draftJob); err != nil {
		t.Fatalf("decode draft response: %v", err)
	}
	if draftJob.Status != jobs.StatusIgnored {
		t.Fatalf("expected ignored status for draft PR, got %q", draftJob.Status)
	}

	ready := []byte(githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action: "opened",
		Number: "1928",
		Branch: "release/payment",
		Labels: []string{"team/backend"},
		Draft:  false,
	}))
	readyReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(ready))
	readyReq.Header.Set("X-GitHub-Event", "pull_request")
	readyReq.Header.Set("X-Hub-Signature-256", githubSignature("secret", ready))
	readyReq.Header.Set("X-GitHub-Delivery", "delivery-ready-1928")
	readyRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusAccepted {
		t.Fatalf("expected ready response 202, got %d: %s", readyRec.Code, readyRec.Body.String())
	}
	var readyJob jobs.Job
	if err := json.Unmarshal(readyRec.Body.Bytes(), &readyJob); err != nil {
		t.Fatalf("decode ready response: %v", err)
	}
	if readyJob.Status != jobs.StatusQueued {
		t.Fatalf("expected queued status for matching webhook, got %q", readyJob.Status)
	}
	if _, err := envStore.Get("pr-1928"); err != nil {
		t.Fatalf("expected environment created for matching webhook: %v", err)
	}
}

func TestGitLabWebhookClosedCreatesDeleteJob(t *testing.T) {
	application, _, _ := newTestServerWithSecrets(t, "", "gitlab-secret")
	body := []byte(gitlabMergeRequestPayloadWithIID("close", "closed", "2003", "feature/kan-2003"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/gitlab", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", "gitlab-secret")
	rec := httptest.NewRecorder()

	application.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var job jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.Type != jobs.TypeDeleteEnvironment {
		t.Fatalf("expected delete job, got %q", job.Type)
	}
	if job.EnvironmentID != "mr-2003" {
		t.Fatalf("expected mr-2003, got %q", job.EnvironmentID)
	}
}

func TestProjectEndpointsStoreGitReposAndBaseConfig(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	body := []byte(`{
  "name": "Checkout",
  "product_id": "generic",
  "app_repository_id": "checkout-app",
  "gitops_repository_id": "checkout-gitops",
  "cluster_id": "dev-us",
  "secret_refs": ["github-token", "github-token", " "],
  "git_repo": {
    "provider": "github",
    "url": "https://github.com/example/checkout.git",
    "default_branch": "main"
  },
  "gitops_repo": {
    "provider": "github",
    "url": "https://github.com/example/gitops.git",
    "default_branch": "main",
    "path": "/workspace/gitops"
  },
  "base_env_config": {
    "environment_id": "feature",
    "namespace": "feature",
    "domain": "feature.int",
    "config_path": "/Users/alex/bh/CMS/env/ENV/feature",
    "services": [
      {"name": "mysql"},
      {"name": "redis", "namespace": "feature-shared"}
    ]
  }
}`)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/checkout", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var project domain.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.ProductID != "generic" || project.AppRepositoryID != "checkout-app" || project.GitOpsRepositoryID != "checkout-gitops" || project.ClusterID != "dev-us" {
		t.Fatalf("unexpected project bindings: %+v", project)
	}
	if len(project.SecretRefs) != 1 || project.SecretRefs[0] != "github-token" {
		t.Fatalf("secret refs = %#v", project.SecretRefs)
	}
	if project.GitRepo.URL != "https://github.com/example/checkout.git" {
		t.Fatalf("git repo url = %q", project.GitRepo.URL)
	}
	if project.GitOpsRepo.URL != "https://github.com/example/gitops.git" {
		t.Fatalf("gitops repo url = %q", project.GitOpsRepo.URL)
	}
	if project.BaseEnvConfig.ConfigPath != "/Users/alex/bh/CMS/env/ENV/feature" {
		t.Fatalf("base config path = %q", project.BaseEnvConfig.ConfigPath)
	}
	if project.BaseEnvConfig.Namespace != "feature" {
		t.Fatalf("base namespace = %q", project.BaseEnvConfig.Namespace)
	}
	if len(project.BaseEnvConfig.Services) != 2 || project.BaseEnvConfig.Services[0].Namespace != "feature" || project.BaseEnvConfig.Services[1].Namespace != "feature-shared" {
		t.Fatalf("base services = %#v", project.BaseEnvConfig.Services)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/checkout", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200 get, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var stored domain.Project
	if err := json.Unmarshal(getRec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode stored project: %v", err)
	}
	if stored.BaseEnvConfig.Namespace != "feature" {
		t.Fatalf("stored base namespace = %q", stored.BaseEnvConfig.Namespace)
	}
}

func TestProjectEndpointAllowsFullEnvironmentProjectWithoutBaseConfig(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	body := []byte(`{
  "name": "API",
  "product_id": "generic",
  "app_repository_id": "api-app",
  "gitops_repository_id": "platform-gitops"
}`)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/api", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var project domain.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.ProductID != "generic" || project.BaseEnvConfig.Namespace != "" {
		t.Fatalf("unexpected project: %+v", project)
	}
}

func TestProductEndpointsStoreTemplateAndCreateFlowUsesIt(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	body := []byte(`{
  "project": "payments",
  "basePath": "apps/payments",
  "healthCheck": "api",
  "defaultMode": "full",
  "services": [
    {"name": "api", "tagKey": "apiImageTag", "defaultTag": "stable", "required": true},
    {"name": "worker", "tagKey": "workerImageTag", "defaultTag": "stable"}
  ],
  "substitutions": {
    "replicas": "2"
  }
}`)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/products/payments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var product domain.ProductTemplate
	if err := json.Unmarshal(rec.Body.Bytes(), &product); err != nil {
		t.Fatalf("decode product: %v", err)
	}
	if product.Name != "payments" || len(product.Services) != 2 {
		t.Fatalf("unexpected product: %+v", product)
	}

	createBody := []byte(`{
  "id": "pr-77",
  "product": "payments",
  "source": {
    "provider": "github",
    "repository": "example/payments",
    "pullRequestId": "77",
    "commit": "abc123"
  }
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	env, err := envStore.Get("pr-77")
	if err != nil {
		t.Fatalf("expected environment: %v", err)
	}
	if env.Product != "payments" || env.Project != "payments" {
		t.Fatalf("unexpected product/project: product=%q project=%q", env.Product, env.Project)
	}
	if env.GitOps.Path != "apps/payments" || env.GitOps.HealthCheckName != "api" {
		t.Fatalf("unexpected gitops target: %+v", env.GitOps)
	}
	if env.Overrides["apiImageTag"] != "abc123" || env.Overrides["workerImageTag"] != "abc123" {
		t.Fatalf("expected commit sha substitutions, got %+v", env.Overrides)
	}
	if env.Overrides["replicas"] != "2" {
		t.Fatalf("expected product substitution, got %+v", env.Overrides)
	}
}

func TestProductTemplateUsesManifestSourceFromSettings(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	settingsBody := []byte(`{
  "manifest_sources": [
    {
      "id": "payments-template",
      "kind": "helm",
      "path": "deploy/helm/payments",
      "values_path": "values-preview.yaml",
      "version": "1.2.3",
      "enabled": true
    }
  ],
  "runtime": {
    "default_product": "generic",
    "default_project": "default",
    "default_mode": "full",
    "domain_root": "feature.int",
    "namespace_prefix": "envpilot-pr",
    "default_ttl_hours": 48
  }
}`)
	settingsReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings", bytes.NewReader(settingsBody))
	settingsReq.Header.Set("Content-Type", "application/json")
	settingsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(settingsRec, settingsReq)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("expected settings save 200, got %d: %s", settingsRec.Code, settingsRec.Body.String())
	}

	productBody := []byte(`{
  "project": "payments",
  "manifestSourceId": "payments-template",
  "basePath": "legacy/path",
  "healthCheck": "api",
  "defaultMode": "full",
  "services": [
    {"name": "api", "tagKey": "apiImageTag", "defaultTag": "stable", "required": true}
  ]
}`)
	productReq := httptest.NewRequest(http.MethodPut, "/api/v1/products/payments", bytes.NewReader(productBody))
	productReq.Header.Set("Content-Type", "application/json")
	productRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(productRec, productReq)
	if productRec.Code != http.StatusOK {
		t.Fatalf("expected product save 200, got %d: %s", productRec.Code, productRec.Body.String())
	}

	createBody := []byte(`{
  "id": "pr-78",
  "product": "payments",
  "source": {
    "provider": "github",
    "repository": "example/payments",
    "pullRequestId": "78",
    "commit": "def456"
  }
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	env, err := envStore.Get("pr-78")
	if err != nil {
		t.Fatalf("expected environment: %v", err)
	}
	if env.GitOps.Path != "deploy/helm/payments" {
		t.Fatalf("gitops path = %q", env.GitOps.Path)
	}
	if env.GitOps.Renderer != "helm" || env.GitOps.ValuesPath != "values-preview.yaml" {
		t.Fatalf("unexpected gitops renderer target: %+v", env.GitOps)
	}
	if env.Overrides["valuesPath"] != "values-preview.yaml" ||
		env.Overrides["manifestSourceId"] != "payments-template" ||
		env.Overrides["manifestSourceKind"] != "helm" ||
		env.Overrides["manifestSourceVersion"] != "1.2.3" {
		t.Fatalf("unexpected manifest source overrides: %+v", env.Overrides)
	}
	if env.Overrides["apiImageTag"] != "def456" {
		t.Fatalf("expected service tag substitution, got %+v", env.Overrides)
	}
}

func TestRenderPreviewEndpointReturnsValuesAndManifests(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	body := []byte(`{
  "id": "pr-88",
  "product": "generic",
  "source": {
    "provider": "github",
    "repository": "example/payments",
    "pullRequestId": "88",
    "commit": "abc123"
  },
  "services": [
    {"name": "api", "tag": "abc123"}
  ],
  "overrides": {
    "featureToggle": "'true'"
  }
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/render/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected preview 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var preview domain.RenderPreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Environment.ID != "pr-88" {
		t.Fatalf("environment id = %q", preview.Environment.ID)
	}
	if preview.Values["featureToggle"] != "'true'" {
		t.Fatalf("values = %+v", preview.Values)
	}
	if !strings.Contains(preview.ValuesYAML, "featureToggle: 'true'") {
		t.Fatalf("values yaml = %q", preview.ValuesYAML)
	}
	if len(preview.Manifests) != 3 {
		t.Fatalf("manifests = %+v", preview.Manifests)
	}
}

func TestRenderPreviewHybridEnvironmentUsesRoutingRulesAndFallbackServices(t *testing.T) {
	application, _, _ := newTestServer(t, "")

	projectBody := []byte(`{
  "id": "cms",
  "product_id": "bethunder",
  "app_repository_id": "checkout-app",
  "gitops_repository_id": "platform-gitops",
  "base_env_config": {
    "environment_id": "feature",
    "namespace": "feature",
    "services": [
      {"name": "backend"},
      {"name": "api"},
      {"name": "mysql"}
    ]
  }
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/cms", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(`{
  "id": "pr-1992",
  "project": "cms",
  "product": "bethunder",
  "mode": "hybrid",
  "source": {
    "provider": "github",
    "repository": "owner/repo",
    "pullRequestId": "1992",
    "commit": "abc123"
  },
  "services": [
    {"name": "backend", "tag": "backend-feature", "replace": true}
  ]
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/render/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected preview 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var preview domain.RenderPreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Environment.Mode != domain.ModeHybrid {
		t.Fatalf("environment mode = %q", preview.Environment.Mode)
	}
	if preview.Environment.Base.Namespace != "feature" {
		t.Fatalf("base namespace = %q", preview.Environment.Base.Namespace)
	}
	if preview.Values["routingStrategy"] != "hybrid-ingress" {
		t.Fatalf("routing strategy = %q", preview.Values["routingStrategy"])
	}
	if preview.Values["overrideRoutes"] != "backend="+preview.Environment.Namespace {
		t.Fatalf("override routes = %q", preview.Values["overrideRoutes"])
	}
	if preview.Values["backendRouteTarget"] != "override" || preview.Values["backendRouteNamespace"] != preview.Environment.Namespace {
		t.Fatalf("backend route values = %q %q", preview.Values["backendRouteTarget"], preview.Values["backendRouteNamespace"])
	}
	if preview.Values["apiRouteTarget"] != "base" {
		t.Fatalf("api route target = %q", preview.Values["apiRouteTarget"])
	}
	if !strings.Contains(preview.Values["fallbackRoutes"], "api=feature") {
		t.Fatalf("fallback routes = %q", preview.Values["fallbackRoutes"])
	}
}

func TestRenderPreviewRejectsUnknownHybridReplacementService(t *testing.T) {
	application, _, _ := newTestServer(t, "")

	projectBody := []byte(`{
  "id": "cms",
  "product_id": "bethunder",
  "app_repository_id": "checkout-app",
  "gitops_repository_id": "platform-gitops",
  "base_env_config": {
    "environment_id": "feature",
    "namespace": "feature",
    "services": [
      {"name": "mysql"}
    ]
  }
}`)
	projectReq := httptest.NewRequest(http.MethodPut, "/api/v1/projects/cms", bytes.NewReader(projectBody))
	projectReq.Header.Set("Content-Type", "application/json")
	projectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusOK {
		t.Fatalf("expected project save 200, got %d: %s", projectRec.Code, projectRec.Body.String())
	}

	body := []byte(`{
  "id": "pr-1993",
  "project": "cms",
  "product": "bethunder",
  "mode": "hybrid",
  "source": {
    "provider": "github",
    "repository": "owner/repo",
    "pullRequestId": "1993",
    "commit": "abc123"
  },
  "services": [
    {"name": "backend", "tag": "backend-feature", "replace": true}
  ]
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/render/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected preview 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRenderPreviewEndpointRejectsMissingIdentity(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	body := []byte(`{
  "product": "generic",
  "source": {}
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/render/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected preview 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode preview error response: %v", err)
	}
	if response.Error != "id, source.pullRequestId, or source.branch is required" {
		t.Fatalf("unexpected error = %q", response.Error)
	}
}

func TestProductValidationEndpointRejectsInvalidTemplate(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	body := []byte(`{
  "name": "payments",
  "defaultMode": "sidecar",
  "services": []
}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/products/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected validation 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentRegistrationAndHeartbeatUpdateClusterSettings(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	projectID := "bootstrap-agent-settings"
	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)
	registerBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": "agent-1",
  "registrationToken": %q,
  "agentVersion": "1.2.3",
  "agentNamespace": "envpilot",
  "kubernetesVersion": "v1.30.1",
  "fluxNamespace": "flux-system",
  "namespaceSelector": "app.kubernetes.io/managed-by=envpilot",
  "capabilities": ["apps-v1", "flux-helm-v2"],
  "heartbeatIntervalSeconds": 30
}`, projectID, tokenResp.RegistrationToken))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected register 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var registerResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registerResp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if strings.TrimSpace(registerResp.AgentAuthToken) == "" {
		t.Fatalf("expected agentAuthToken in register response: %s", rec.Body.String())
	}

	heartbeatBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": "agent-1",
  "agentAuthToken": %q,
  "status": "online",
  "capabilities": ["flux-helm-v2", "apps-v1"]
}`, projectID, registerResp.AgentAuthToken))
	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code != http.StatusAccepted {
		t.Fatalf("expected heartbeat 202, got %d: %s", heartbeatRec.Code, heartbeatRec.Body.String())
	}

	settings, err := application.settings.GetSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	var cluster domain.ClusterTarget
	for _, item := range settings.Clusters {
		if item.ID == "dev-us" {
			cluster = item
			break
		}
	}
	if cluster.ID != "dev-us" || cluster.AgentID != "agent-1" || cluster.AgentStatus != "online" {
		t.Fatalf("cluster = %#v", cluster)
	}
	if cluster.LastHeartbeatAt == nil || cluster.KubernetesVersion != "v1.30.1" {
		t.Fatalf("cluster heartbeat/version = %#v", cluster)
	}
	gotCapabilities := append([]string(nil), cluster.Capabilities...)
	sort.Strings(gotCapabilities)
	expectedCapabilities := []string{"apps-v1", "flux-helm-v2"}
	sort.Strings(expectedCapabilities)
	if len(gotCapabilities) != len(expectedCapabilities) {
		t.Fatalf("capabilities = %#v", cluster.Capabilities)
	}
	for index := range expectedCapabilities {
		if gotCapabilities[index] != expectedCapabilities[index] {
			t.Fatalf("capabilities = %#v", cluster.Capabilities)
		}
	}
}

func TestBootstrapAgentRegisterAndHeartbeatRequireAuthByDefault(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	registerBody := []byte(`{
  "clusterId": "dev-us",
  "agentId": "agent-1"
}`)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusBadRequest {
		t.Fatalf("expected unauthenticated agent register 400, got %d body=%s", registerRec.Code, registerRec.Body.String())
	}

	heartbeatBody := []byte(`{
  "clusterId": "dev-us",
  "agentId": "agent-1",
  "status": "online"
}`)
	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code != http.StatusBadRequest {
		t.Fatalf("expected unauthenticated agent heartbeat 400, got %d body=%s", heartbeatRec.Code, heartbeatRec.Body.String())
	}
}

func TestBootstrapAgentRegisterAndHeartbeatRequireAuthEvenWhenLegacyUnauthAgentsEnabled(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.AllowUnauthenticatedAgents = true
	application.ReloadConfig(cfg)

	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader([]byte(`{
  "clusterId": "dev-us",
  "agentId": "agent-legacy"
}`)))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusBadRequest {
		t.Fatalf("bootstrap agent register must ignore legacy unauth flag, got %d body=%s", registerRec.Code, registerRec.Body.String())
	}

	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader([]byte(`{
  "clusterId": "dev-us",
  "agentId": "agent-legacy",
  "status": "online"
}`)))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code != http.StatusBadRequest {
		t.Fatalf("bootstrap agent heartbeat must ignore legacy unauth flag, got %d body=%s", heartbeatRec.Code, heartbeatRec.Body.String())
	}
}

func TestBootstrapAgentRegistrationTokenFlow(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-agent",
		Name:               "Bootstrap Agent",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-agent/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	tokenReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-agent/bootstrap-session/agent-token", bytes.NewReader([]byte(`{}`)))
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("token status=%d body=%s", tokenRec.Code, tokenRec.Body.String())
	}
	var tokenResp domain.AgentRegistrationTokenResponse
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if strings.TrimSpace(tokenResp.RegistrationToken) == "" {
		t.Fatalf("expected non-empty registration token")
	}
	if strings.Contains(tokenResp.HelmCommand, tokenResp.RegistrationToken) {
		t.Fatalf("agent helm command leaked registration token: %q", tokenResp.HelmCommand)
	}
	if strings.Contains(tokenResp.HelmCommand, "ENVPLANE_AGENT_REGISTRATION_TOKEN") {
		t.Fatalf("agent helm command must not set live registration token env var: %q", tokenResp.HelmCommand)
	}
	if !strings.Contains(tokenResp.HelmCommand, "controlPlane.existingSecret") ||
		!strings.Contains(tokenResp.HelmCommand, "envpilot-agent-bootstrap") {
		t.Fatalf("agent helm command must reference existingSecret, got %q", tokenResp.HelmCommand)
	}
	if strings.TrimSpace(tokenResp.BootstrapSecretCommand) == "" {
		t.Fatalf("expected bootstrap secret command")
	}
	if !tokenResp.BootstrapSecretCommandSensitive {
		t.Fatalf("bootstrap secret command should be marked sensitive")
	}
	if !strings.Contains(tokenResp.BootstrapSecretCommand, "kubectl create secret generic envpilot-agent-bootstrap") ||
		!strings.Contains(tokenResp.BootstrapSecretCommand, tokenResp.RegistrationToken) {
		t.Fatalf("bootstrap secret command should contain one-time registration token: %q", tokenResp.BootstrapSecretCommand)
	}

	registerBody := []byte(fmt.Sprintf(`{
  "projectId": "bootstrap-agent",
  "clusterId": %q,
  "agentId": "agent-1",
  "registrationToken": %q,
  "capabilityReport": {"namespaces": ["dev-base", "shared"]}
}`, tokenResp.ClusterID, tokenResp.RegistrationToken))
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registerResp); err != nil {
		t.Fatalf("decode agent registration response: %v", err)
	}
	if strings.TrimSpace(registerResp.AgentAuthToken) == "" {
		t.Fatalf("expected agentAuthToken in registration response: %s", registerRec.Body.String())
	}
	if registerResp.AgentAuthToken == legacyDerivedAgentAuthToken("bootstrap-agent", "agent-1", tokenResp.ClusterID, tokenResp.RegistrationToken) {
		t.Fatalf("agent auth token must be random server-generated, got legacy derived token")
	}

	oldTokenHeartbeat := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader([]byte(fmt.Sprintf(`{
  "projectId": "bootstrap-agent",
  "clusterId": %q,
  "agentId": "agent-1",
  "registrationToken": %q,
  "status": "online"
}`, tokenResp.ClusterID, tokenResp.RegistrationToken))))
	oldTokenHeartbeat.Header.Set("Content-Type", "application/json")
	oldTokenHeartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(oldTokenHeartbeatRec, oldTokenHeartbeat)
	if oldTokenHeartbeatRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected heartbeat with registration token to be ignored as missing auth token, got %d body=%s", oldTokenHeartbeatRec.Code, oldTokenHeartbeatRec.Body.String())
	}

	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader([]byte(fmt.Sprintf(`{
  "projectId": "bootstrap-agent",
  "clusterId": %q,
  "agentId": "agent-1",
  "agentAuthToken": %q,
  "status": "online"
}`, tokenResp.ClusterID, registerResp.AgentAuthToken))))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code != http.StatusAccepted {
		t.Fatalf("heartbeat with agent auth token status=%d body=%s", heartbeatRec.Code, heartbeatRec.Body.String())
	}

	reuseRec := httptest.NewRecorder()
	reuseReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	reuseReq.Header.Set("Content-Type", "application/json")
	application.Routes().ServeHTTP(reuseRec, reuseReq)
	if reuseRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected token reuse 401, got %d body=%s", reuseRec.Code, reuseRec.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/projects/bootstrap-agent/bootstrap-session/agent-status", nil)
	statusRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status endpoint code=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	if !strings.Contains(statusRec.Body.String(), `"status":"online"`) {
		t.Fatalf("expected online status body=%s", statusRec.Body.String())
	}
	entries := parseAuditLogEntries(t, logPath)
	registerEntry := findAuditEventEntry(t, entries, auditEventAgentRegistrationSucceeded)
	assertStandardAuditEvent(t, registerEntry, auditEventAgentRegistrationSucceeded, auditEndpointAgentRegistration, "bootstrap-agent", auditSubjectAgentID, true)
	heartbeatEntry := findAuditEventEntry(t, entries, auditEventAgentHeartbeatSucceeded)
	assertStandardAuditEvent(t, heartbeatEntry, auditEventAgentHeartbeatSucceeded, auditEndpointAgentHeartbeat, "bootstrap-agent", auditSubjectAgentID, true)
	raw := mustReadFileString(t, logPath)
	if strings.Contains(raw, tokenResp.RegistrationToken) || strings.Contains(raw, registerResp.AgentAuthToken) {
		t.Fatalf("audit log leaked raw agent token: %s", raw)
	}
}

func TestAgentRegistrationPersistenceFailureDoesNotConsumeBootstrapToken(t *testing.T) {
	projectID := "bootstrap-agent-register-retry"
	agentID := "agent-retry"
	application, _, _ := newTestServer(t, "")
	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)
	stored, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	failStore := &failOnceBootstrapSessionStore{session: stored, failOnSave: 1}
	application.bootstrapSessions = app.NewBootstrapSessionServiceWithEncryptor(failStore, app.MustNewAESGCMCredentialEncryptor("test-bootstrap-key", "test"))

	body := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "registrationToken": %q
}`, projectID, agentID, tokenResp.RegistrationToken))
	firstReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	firstReq.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code == http.StatusAccepted {
		t.Fatalf("expected transient finalization failure, got %d body=%s", firstRec.Code, firstRec.Body.String())
	}
	if usedAt := asString(failStore.session.Data[bootstrapAgentTokenUsedAtKey]); usedAt != "" {
		t.Fatalf("partial failure must not consume agent bootstrap token, usedAt=%q", usedAt)
	}
	if hash := asString(failStore.session.Data[bootstrapAgentAuthTokenHashKey]); hash != "" {
		t.Fatalf("partial failure must not finalize agent auth token hash")
	}
	pendingKey := agentRegistrationIdempotencyKey(projectID, domain.AgentRegistrationRequest{
		AgentID:   agentID,
		ClusterID: "dev-us",
	})
	pendingToken := application.pendingAgentRegistrationTokenForTest(pendingKey)
	if strings.TrimSpace(pendingToken) == "" {
		t.Fatalf("bootstrap finalization failure should retain one in-memory pending token for idempotent retry")
	}
	settingsAfterFailure, err := application.settings.GetSettings()
	if err != nil {
		t.Fatalf("settings after claim failure: %v", err)
	}
	failedCluster := clusterByIDForTest(settingsAfterFailure, "dev-us")
	if failedCluster.AgentStatus == "online" {
		t.Fatalf("claim failure must not mark agent online: %#v", failedCluster)
	}
	if failedCluster.AgentStatus != "registration_pending" {
		t.Fatalf("claim failure should leave registration pending, got %#v", failedCluster)
	}
	heartbeatBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "agentAuthToken": %q,
  "status": "online"
}`, projectID, agentID, pendingToken))
	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code == http.StatusAccepted {
		t.Fatalf("heartbeat must fail while auth token hash is not persisted")
	}

	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	retryReq.Header.Set("Content-Type", "application/json")
	retryRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusAccepted {
		t.Fatalf("retry registration status=%d body=%s", retryRec.Code, retryRec.Body.String())
	}
	var retryResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(retryRec.Body.Bytes(), &retryResp); err != nil {
		t.Fatalf("decode retry registration response: %v", err)
	}
	if strings.TrimSpace(retryResp.AgentAuthToken) == "" {
		t.Fatalf("retry must return agent auth token")
	}
	if retryResp.AgentAuthToken != pendingToken {
		t.Fatalf("retry after finalization failure must reuse pending auth token, got %q want %q", retryResp.AgentAuthToken, pendingToken)
	}
	if retryResp.AgentAuthToken == legacyDerivedAgentAuthToken(projectID, agentID, "dev-us", tokenResp.RegistrationToken) {
		t.Fatalf("agent auth token must be random server-generated, got legacy derived token")
	}
	if application.hasPendingAgentRegistrationTokenForTest(pendingKey) {
		t.Fatalf("successful retry must clear pending auth token")
	}
	settingsAfterRetry, err := application.settings.GetSettings()
	if err != nil {
		t.Fatalf("settings after retry: %v", err)
	}
	retryCluster := clusterByIDForTest(settingsAfterRetry, "dev-us")
	if retryCluster.AgentStatus == "online" {
		t.Fatalf("registration response must not mark agent online before authenticated heartbeat: %#v", retryCluster)
	}
	if usedAt := asString(failStore.session.Data[bootstrapAgentTokenUsedAtKey]); usedAt == "" {
		t.Fatalf("successful registration must consume agent bootstrap token")
	}
	if hash := asString(failStore.session.Data[bootstrapAgentAuthTokenHashKey]); hash == "" {
		t.Fatalf("successful registration must persist agent auth token hash")
	}

	reuseReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	reuseReq.Header.Set("Content-Type", "application/json")
	reuseRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(reuseRec, reuseReq)
	if reuseRec.Code != http.StatusUnauthorized {
		t.Fatalf("reuse after successful registration must fail, got %d body=%s", reuseRec.Code, reuseRec.Body.String())
	}
	if strings.Contains(reuseRec.Body.String(), "already used") {
		t.Fatalf("reuse response must not reveal bootstrap replay state: %s", reuseRec.Body.String())
	}
	if !strings.Contains(reuseRec.Body.String(), "invalid bootstrap token") {
		t.Fatalf("reuse response should use generic invalid bootstrap token error: %s", reuseRec.Body.String())
	}
}

func TestBootstrapAlreadyUsedTokensReturnUnauthorizedAndAuditReplay(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, runnerDeployResp := prepareRunnerConfigFixture(t, "bootstrap-replay-runner")

	agentProjectID := "bootstrap-replay-agent"
	agentTokenResp := createBootstrapAgentTokenForTest(t, application, agentProjectID)
	agentBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": %q,
  "agentId": "agent-replay",
  "registrationToken": %q
}`, agentProjectID, agentTokenResp.ClusterID, agentTokenResp.RegistrationToken))
	agentReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(agentBody))
	agentReq.Header.Set("Content-Type", "application/json")
	agentRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(agentRec, agentReq)
	if agentRec.Code != http.StatusAccepted {
		t.Fatalf("agent first registration status=%d body=%s", agentRec.Code, agentRec.Body.String())
	}
	agentReplayReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(agentBody))
	agentReplayReq.Header.Set("Content-Type", "application/json")
	agentReplayRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(agentReplayRec, agentReplayReq)
	if agentReplayRec.Code != http.StatusUnauthorized {
		t.Fatalf("agent replay status=%d want=401 body=%s", agentReplayRec.Code, agentReplayRec.Body.String())
	}
	if strings.Contains(agentReplayRec.Body.String(), "already used") {
		t.Fatalf("agent replay response must not reveal replay state: %s", agentReplayRec.Body.String())
	}
	if !strings.Contains(agentReplayRec.Body.String(), "invalid bootstrap token") {
		t.Fatalf("agent replay response should use generic invalid bootstrap token error: %s", agentReplayRec.Body.String())
	}
	if strings.Contains(agentReplayRec.Body.String(), agentTokenResp.RegistrationToken) {
		t.Fatalf("agent replay response leaked raw token: %s", agentReplayRec.Body.String())
	}

	runnerRegisterBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": %q,
  "runnerId": %q,
  "registrationToken": %q,
  "deploymentMode": %q,
  "runnerNamespace": %q
}`, runnerDeployResp.ProjectID, runnerDeployResp.ClusterID, runnerDeployResp.ProjectID+"-runner", runnerDeployResp.RegistrationToken, runnerDeployResp.DeploymentMode, runnerDeployResp.RunnerNamespace))
	runnerReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader(runnerRegisterBody))
	runnerReq.Header.Set("Content-Type", "application/json")
	runnerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(runnerRec, runnerReq)
	if runnerRec.Code != http.StatusAccepted {
		t.Fatalf("runner first registration status=%d body=%s", runnerRec.Code, runnerRec.Body.String())
	}
	runnerReplayReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader(runnerRegisterBody))
	runnerReplayReq.Header.Set("Content-Type", "application/json")
	runnerReplayRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(runnerReplayRec, runnerReplayReq)
	if runnerReplayRec.Code != http.StatusUnauthorized {
		t.Fatalf("runner replay status=%d want=401 body=%s", runnerReplayRec.Code, runnerReplayRec.Body.String())
	}
	if strings.Contains(runnerReplayRec.Body.String(), "already used") {
		t.Fatalf("runner replay response must not reveal replay state: %s", runnerReplayRec.Body.String())
	}
	if !strings.Contains(runnerReplayRec.Body.String(), "invalid bootstrap token") {
		t.Fatalf("runner replay response should use generic invalid bootstrap token error: %s", runnerReplayRec.Body.String())
	}
	if strings.Contains(runnerReplayRec.Body.String(), runnerDeployResp.RegistrationToken) {
		t.Fatalf("runner replay response leaked raw token: %s", runnerReplayRec.Body.String())
	}

	firstConfigReq := newRunnerConfigRequest(t, runnerDeployResp, runnerDeployResp.ProjectConfigToken)
	firstConfigRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstConfigRec, firstConfigReq)
	if firstConfigRec.Code != http.StatusOK {
		t.Fatalf("runner config first fetch status=%d body=%s", firstConfigRec.Code, firstConfigRec.Body.String())
	}
	secondConfigReq := newRunnerConfigRequest(t, runnerDeployResp, runnerDeployResp.ProjectConfigToken)
	secondConfigRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(secondConfigRec, secondConfigReq)
	if secondConfigRec.Code != http.StatusUnauthorized {
		t.Fatalf("runner config replay status=%d want=401 body=%s", secondConfigRec.Code, secondConfigRec.Body.String())
	}
	if strings.Contains(secondConfigRec.Body.String(), "already used") {
		t.Fatalf("runner config replay response must not reveal replay state: %s", secondConfigRec.Body.String())
	}
	if !strings.Contains(secondConfigRec.Body.String(), "invalid config token") {
		t.Fatalf("runner config replay response should use generic invalid config token error: %s", secondConfigRec.Body.String())
	}
	if strings.Contains(secondConfigRec.Body.String(), runnerDeployResp.ProjectConfigToken) {
		t.Fatalf("runner config replay response leaked raw token: %s", secondConfigRec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	assertAuditLogContainsNoRawTokens(t, raw, logPath, agentTokenResp.RegistrationToken, runnerDeployResp.RegistrationToken, runnerDeployResp.ProjectConfigToken)
	if !bytes.Contains(raw, []byte(`"reason":"already_used"`)) {
		t.Fatalf("audit log should record replay/already-used reason: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	for _, event := range []string{
		auditEventAgentRegistrationAuthFailed,
		auditEventRunnerRegistrationAuthFailed,
		auditEventRunnerConfigFetchAuthFailed,
	} {
		entry := findAuditEventEntry(t, entries, event)
		if reason := fmt.Sprint(entry["reason"]); reason != "already_used" {
			t.Fatalf("audit event %s reason=%q want already_used: %#v", event, reason, entry)
		}
		if reason := fmt.Sprint(entry["reason"]); reason == "invalid_bootstrap_token" || reason == "invalid_config_token" {
			t.Fatalf("audit event %s collapsed replay reason into generic client reason: %#v", event, entry)
		}
		if fingerprint := strings.TrimSpace(fmt.Sprint(entry["token_fingerprint"])); fingerprint == "" {
			t.Fatalf("audit event %s missing token_fingerprint: %#v", event, entry)
		}
	}
}

func TestAgentRegistrationSettingsFailureDoesNotConsumeBootstrapToken(t *testing.T) {
	projectID := "bootstrap-agent-settings-retry"
	agentID := "agent-settings-retry"
	application, _, _ := newTestServer(t, "")
	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)
	settings, err := application.settings.GetSettings()
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	settingsStore := &failOnceSettingsStore{settings: settings, failOnSave: 1}
	application.settings = app.NewSettingsService(settingsStore)

	body := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "registrationToken": %q
}`, projectID, agentID, tokenResp.RegistrationToken))
	firstReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	firstReq.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code == http.StatusAccepted {
		t.Fatalf("expected settings persistence failure, got %d body=%s", firstRec.Code, firstRec.Body.String())
	}
	stored, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	if usedAt := asString(stored.Data[bootstrapAgentTokenUsedAtKey]); usedAt != "" {
		t.Fatalf("settings failure must not consume agent bootstrap token, usedAt=%q", usedAt)
	}
	if hash := asString(stored.Data[bootstrapAgentAuthTokenHashKey]); hash != "" {
		t.Fatalf("settings failure must not persist agent auth token hash")
	}
	pendingKey := agentRegistrationIdempotencyKey(projectID, domain.AgentRegistrationRequest{
		AgentID:   agentID,
		ClusterID: "dev-us",
	})
	if application.hasPendingAgentRegistrationTokenForTest(pendingKey) {
		t.Fatalf("settings failure must not retain recoverable pending auth token")
	}

	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	retryReq.Header.Set("Content-Type", "application/json")
	retryRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusAccepted {
		t.Fatalf("retry registration status=%d body=%s", retryRec.Code, retryRec.Body.String())
	}
	var retryResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(retryRec.Body.Bytes(), &retryResp); err != nil {
		t.Fatalf("decode retry registration response: %v", err)
	}
	if strings.TrimSpace(retryResp.AgentAuthToken) == "" {
		t.Fatalf("retry must return agent auth token")
	}
	stored, err = application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session after retry: %v", err)
	}
	if usedAt := asString(stored.Data[bootstrapAgentTokenUsedAtKey]); usedAt == "" {
		t.Fatalf("successful retry must consume agent bootstrap token")
	}
	if hash := asString(stored.Data[bootstrapAgentAuthTokenHashKey]); hash == "" {
		t.Fatalf("successful retry must persist agent auth token hash")
	}
}

func TestPendingAgentRegistrationTokenExpiresAndCleanup(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.PendingRegistrationTokenTTL = time.Minute
	application.ReloadConfig(cfg)

	key := agentRegistrationIdempotencyKey("pending-expiry", domain.AgentRegistrationRequest{
		AgentID:   "agent-expiry",
		ClusterID: "dev-us",
	})
	first, err := application.pendingAgentRegistrationToken(key)
	if err != nil {
		t.Fatalf("first pending token: %v", err)
	}
	second, err := application.pendingAgentRegistrationToken(key)
	if err != nil {
		t.Fatalf("second pending token: %v", err)
	}
	if second != first {
		t.Fatalf("retry before expiry should reuse pending token, got %q want %q", second, first)
	}

	expiredAt := time.Now().UTC().Add(-time.Second)
	application.deletePendingAgentRegistrationToken(key)
	if _, _, err := application.storePendingAgentRegistrationToken(key, pendingRegistrationToken{
		Token:     first,
		CreatedAt: expiredAt.Add(-time.Minute),
		ExpiresAt: expiredAt,
	}); err != nil {
		t.Fatalf("store expired pending token: %v", err)
	}
	application.cleanupExpiredPendingRegistrationTokens(time.Now().UTC())
	if application.hasPendingAgentRegistrationTokenForTest(key) {
		t.Fatalf("cleanup should remove expired pending token")
	}

	third, err := application.pendingAgentRegistrationToken(key)
	if err != nil {
		t.Fatalf("third pending token: %v", err)
	}
	if third == "" {
		t.Fatalf("retry after expiry must return a token")
	}
	if third == first {
		t.Fatalf("retry after expiry must generate a new token")
	}
}

func TestPendingAgentRegistrationTokenMaxSizeGuard(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.PendingRegistrationTokenMax = 1
	cfg.PendingRegistrationTokenTTL = time.Minute
	application.ReloadConfig(cfg)

	firstKey := agentRegistrationIdempotencyKey("pending-max", domain.AgentRegistrationRequest{
		AgentID:   "agent-1",
		ClusterID: "dev-us",
	})
	if _, err := application.pendingAgentRegistrationToken(firstKey); err != nil {
		t.Fatalf("first pending token: %v", err)
	}
	secondKey := agentRegistrationIdempotencyKey("pending-max", domain.AgentRegistrationRequest{
		AgentID:   "agent-2",
		ClusterID: "dev-us",
	})
	second, err := application.pendingAgentRegistrationToken(secondKey)
	if err != nil {
		t.Fatalf("second pending token should evict oldest instead of failing: %v", err)
	}
	if strings.TrimSpace(second) == "" {
		t.Fatalf("second pending token is empty")
	}
	if application.hasPendingAgentRegistrationTokenForTest(firstKey) {
		t.Fatalf("oldest pending token should be evicted")
	}
	if !application.hasPendingAgentRegistrationTokenForTest(secondKey) {
		t.Fatalf("new pending token should be retained")
	}
	if got := application.pendingAgentRegistrationSize(); got != 1 {
		t.Fatalf("pending token count=%d want=1", got)
	}
	if application.metrics.pendingTokenEvictions == 0 {
		t.Fatalf("expected pending token eviction metric to increment")
	}
}

func TestPendingAgentRegistrationTokenMapDoesNotGrowUnbounded(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.PendingRegistrationTokenMax = 3
	cfg.PendingRegistrationTokenTTL = time.Hour
	application.ReloadConfig(cfg)

	issued := map[string]string{}
	for idx := 0; idx < 20; idx++ {
		key := agentRegistrationIdempotencyKey("pending-bounded", domain.AgentRegistrationRequest{
			AgentID:   fmt.Sprintf("agent-%02d", idx),
			ClusterID: "dev-us",
		})
		token, err := application.pendingAgentRegistrationToken(key)
		if err != nil {
			t.Fatalf("pending token %d: %v", idx, err)
		}
		issued[key] = token
		if got := application.pendingAgentRegistrationSize(); got > cfg.PendingRegistrationTokenMax {
			t.Fatalf("pending token count grew beyond max: got=%d max=%d", got, cfg.PendingRegistrationTokenMax)
		}
	}
	if got := application.pendingAgentRegistrationSize(); got != cfg.PendingRegistrationTokenMax {
		t.Fatalf("pending token count=%d want=%d", got, cfg.PendingRegistrationTokenMax)
	}
	if application.metrics.pendingTokenEvictions == 0 {
		t.Fatalf("expected eviction metric after high-volume distinct pending tokens")
	}

	lastKey := agentRegistrationIdempotencyKey("pending-bounded", domain.AgentRegistrationRequest{
		AgentID:   "agent-19",
		ClusterID: "dev-us",
	})
	retryToken, err := application.pendingAgentRegistrationToken(lastKey)
	if err != nil {
		t.Fatalf("retry retained pending token: %v", err)
	}
	if retryToken != issued[lastKey] {
		t.Fatalf("retry before expiry should reuse non-evicted pending token")
	}

	firstKey := agentRegistrationIdempotencyKey("pending-bounded", domain.AgentRegistrationRequest{
		AgentID:   "agent-00",
		ClusterID: "dev-us",
	})
	newToken, err := application.pendingAgentRegistrationToken(firstKey)
	if err != nil {
		t.Fatalf("retry evicted pending token: %v", err)
	}
	if newToken == issued[firstKey] {
		t.Fatalf("retry after eviction must generate a new token safely")
	}
}

func TestPendingAgentRegistrationTokenConcurrentDistinctKeysCannotExceedMax(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.PendingRegistrationTokenMax = 5
	cfg.PendingRegistrationTokenTTL = time.Hour
	application.ReloadConfig(cfg)

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for idx := 0; idx < 100; idx++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := agentRegistrationIdempotencyKey("pending-concurrent", domain.AgentRegistrationRequest{
				AgentID:   fmt.Sprintf("agent-%03d", i),
				ClusterID: "dev-us",
			})
			if _, err := application.pendingAgentRegistrationToken(key); err != nil {
				errs <- err
			}
		}(idx)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("pending token under concurrent pressure: %v", err)
	}
	if got := application.pendingAgentRegistrationSize(); got > cfg.PendingRegistrationTokenMax {
		t.Fatalf("pending token count exceeded max under concurrency: got=%d max=%d", got, cfg.PendingRegistrationTokenMax)
	}
	if application.metrics.pendingTokenEvictions == 0 {
		t.Fatalf("expected eviction metric under concurrent pressure")
	}
}

func TestAgentRegistrationConcurrentRequestsWithSameTokenIssueSingleAuthToken(t *testing.T) {
	projectID := "bootstrap-agent-register-concurrent-token"
	agentID := "agent-concurrent"
	application, _, _ := newTestServer(t, "")
	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)
	body := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "registrationToken": %q
}`, projectID, agentID, tokenResp.RegistrationToken))

	var wg sync.WaitGroup
	statuses := make([]int, 8)
	authTokens := make([]string, 8)
	for idx := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)
			statuses[i] = rec.Code
			if rec.Code == http.StatusAccepted {
				var resp domain.AgentRegistrationResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Errorf("decode response %d: %v", i, err)
					return
				}
				authTokens[i] = strings.TrimSpace(resp.AgentAuthToken)
			}
		}(idx)
	}
	wg.Wait()

	accepted := 0
	issued := map[string]struct{}{}
	for idx, status := range statuses {
		switch status {
		case http.StatusAccepted:
			accepted++
			if authTokens[idx] == "" {
				t.Fatalf("accepted response %d did not include agentAuthToken", idx)
			}
			issued[authTokens[idx]] = struct{}{}
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unexpected status at %d: %d", idx, status)
		}
	}
	if accepted != 1 {
		t.Fatalf("expected exactly one accepted registration, got %d statuses=%v", accepted, statuses)
	}
	if len(issued) != 1 {
		t.Fatalf("expected exactly one unique agent auth token, got %d", len(issued))
	}

	replayReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	replayReq.Header.Set("Content-Type", "application/json")
	replayRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed registration token must fail, got %d body=%s", replayRec.Code, replayRec.Body.String())
	}
}

func legacyDerivedAgentAuthToken(projectID, agentID, clusterID, registrationToken string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"envpilot-agent-auth-v1",
		normalizeSettingsID(projectID),
		strings.TrimSpace(agentID),
		strings.TrimSpace(clusterID),
		hashToken(registrationToken),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func TestBootstrapNamespaceSelectionAndResourceScanFlow(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-scan",
		Name:               "Bootstrap Scan",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-scan/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	tokenReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-scan/bootstrap-session/agent-token", bytes.NewReader([]byte(`{}`)))
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("token status=%d body=%s", tokenRec.Code, tokenRec.Body.String())
	}
	var tokenResp domain.AgentRegistrationTokenResponse
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}

	registerBody := []byte(fmt.Sprintf(`{
  "projectId": "bootstrap-scan",
  "clusterId": %q,
  "agentId": "agent-2",
  "registrationToken": %q,
  "capabilityReport": {"namespaces": ["dev-base", "shared"]}
}`, tokenResp.ClusterID, tokenResp.RegistrationToken))
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registerResp); err != nil {
		t.Fatalf("decode agent registration response: %v", err)
	}
	if strings.TrimSpace(registerResp.AgentAuthToken) == "" {
		t.Fatalf("expected agent auth token: %s", registerRec.Body.String())
	}

	selectNSBody := []byte(`{
  "current_step": 3,
  "status": "reviewed",
  "step_data": {"selectedBaseNamespaces": ["dev-base"]}
}`)
	selectReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-scan/bootstrap-session", bytes.NewReader(selectNSBody))
	selectReq.Header.Set("Content-Type", "application/json")
	selectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(selectRec, selectReq)
	if selectRec.Code != http.StatusOK {
		t.Fatalf("select namespaces status=%d body=%s", selectRec.Code, selectRec.Body.String())
	}

	startReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-scan/bootstrap-session/resource-scan/start", nil)
	startRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("start scan status=%d body=%s", startRec.Code, startRec.Body.String())
	}

	scanBody := []byte(fmt.Sprintf(`{
  "projectId": "bootstrap-scan",
  "clusterId": %q,
  "agentId": "agent-2",
  "agentAuthToken": %q,
  "resourceSnapshots": [
    {"kind":"Deployment","namespace":"dev-base","name":"orders"},
    {"kind":"Secret","namespace":"dev-base","name":"payments-token"}
  ]
}`, tokenResp.ClusterID, registerResp.AgentAuthToken))
	bodyOnlyScanReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/resource-scan", bytes.NewReader(scanBody))
	bodyOnlyScanReq.Header.Set("Content-Type", "application/json")
	bodyOnlyScanRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(bodyOnlyScanRec, bodyOnlyScanReq)
	if bodyOnlyScanRec.Code != http.StatusUnauthorized {
		t.Fatalf("body-only agentAuthToken must not authenticate ingest, got %d body=%s", bodyOnlyScanRec.Code, bodyOnlyScanRec.Body.String())
	}

	scanReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/resource-scan", bytes.NewReader(scanBody))
	scanReq.Header.Set("Content-Type", "application/json")
	scanReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	scanRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(scanRec, scanReq)
	if scanRec.Code != http.StatusAccepted {
		t.Fatalf("ingest scan status=%d body=%s", scanRec.Code, scanRec.Body.String())
	}
	if strings.Contains(scanRec.Body.String(), "payments-token") == false {
		t.Fatalf("expected resource names in persisted scan response body=%s", scanRec.Body.String())
	}
}

func TestBootstrapRunnerDeploymentInstructionsSupportHelmAndGitOpsModes(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-runner",
		Name:               "Bootstrap Runner",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	helmReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner/bootstrap-session/runner-deployment-instructions", bytes.NewReader([]byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner",
	  "releaseName":"runner-bootstrap"
	}`)))
	helmReq.Header.Set("Content-Type", "application/json")
	helmRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(helmRec, helmReq)
	if helmRec.Code != http.StatusOK {
		t.Fatalf("helm instructions status=%d body=%s", helmRec.Code, helmRec.Body.String())
	}
	var helmResp domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(helmRec.Body.Bytes(), &helmResp); err != nil {
		t.Fatalf("decode helm response: %v", err)
	}
	if helmResp.DeploymentMode != domain.RunnerDeploymentModeHelm {
		t.Fatalf("expected helm deployment mode, got %q", helmResp.DeploymentMode)
	}
	registrationToken, projectConfigToken := runnerBootstrapTokensFromCommand(t, helmResp.BootstrapSecretCommand)
	if helmResp.RegistrationToken != "[masked]" {
		t.Fatalf("standalone registration token field must be masked, got %q", helmResp.RegistrationToken)
	}
	if !strings.Contains(helmResp.HelmCommand, "helm upgrade --install") {
		t.Fatalf("expected helm command, got %q", helmResp.HelmCommand)
	}
	if helmResp.ProjectConfigToken != "[masked]" {
		t.Fatalf("standalone project config token field must be masked, got %q", helmResp.ProjectConfigToken)
	}
	if strings.TrimSpace(helmResp.BootstrapSecretCommand) == "" {
		t.Fatalf("expected separate bootstrap secret creation instruction")
	}
	if !helmResp.BootstrapSecretCommandSensitive {
		t.Fatalf("bootstrap secret command must be marked sensitive")
	}
	if !strings.Contains(helmResp.BootstrapSecretCommand, "kubectl create secret generic envpilot-runner-bootstrap") {
		t.Fatalf("expected kubectl secret creation instruction, got %q", helmResp.BootstrapSecretCommand)
	}
	if !strings.Contains(helmResp.HelmCommand, "controlPlane.existingSecret") ||
		!strings.Contains(helmResp.HelmCommand, "envpilot-runner-bootstrap") {
		t.Fatalf("helm command must reference bootstrap existingSecret, got %q", helmResp.HelmCommand)
	}
	if strings.Contains(helmResp.HelmCommand, registrationToken) ||
		strings.Contains(helmResp.HelmCommand, projectConfigToken) ||
		strings.Contains(helmResp.HelmCommand, "controlPlane.token") ||
		strings.Contains(helmResp.HelmCommand, "controlPlane.configToken") {
		t.Fatalf("helm command must not contain raw tokens or token set flags: %q", helmResp.HelmCommand)
	}
	if helmResp.ProjectConfigURL == "" ||
		!strings.Contains(helmResp.ProjectConfigURL, "/api/v1/projects/bootstrap-runner/runner-config") ||
		strings.Contains(helmResp.ProjectConfigURL, "?") ||
		strings.Contains(helmResp.ProjectConfigURL, registrationToken) ||
		strings.Contains(helmResp.ProjectConfigURL, projectConfigToken) {
		t.Fatalf("unexpected runner config url: %q", helmResp.ProjectConfigURL)
	}
	if helmResp.GitOpsManifest != "" {
		t.Fatalf("expected empty gitops manifest for helm mode")
	}

	legacyReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner/bootstrap-session/runner-deploy", bytes.NewReader([]byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner"
	}`)))
	legacyReq.Header.Set("Content-Type", "application/json")
	legacyRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(legacyRec, legacyReq)
	if legacyRec.Code != http.StatusOK {
		t.Fatalf("legacy runner-deploy alias status=%d body=%s", legacyRec.Code, legacyRec.Body.String())
	}

	gitOpsReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner/bootstrap-session/runner-deployment-instructions", bytes.NewReader([]byte(`{
	  "deploymentMode":"gitops",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner",
	  "releaseName":"runner-bootstrap-gitops",
	  "gitOpsPath":"gitops/runners/bootstrap-runner-gitops.yaml"
	}`)))
	gitOpsReq.Header.Set("Content-Type", "application/json")
	gitOpsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(gitOpsRec, gitOpsReq)
	if gitOpsRec.Code != http.StatusOK {
		t.Fatalf("gitops instructions status=%d body=%s", gitOpsRec.Code, gitOpsRec.Body.String())
	}
	var gitOpsResp domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(gitOpsRec.Body.Bytes(), &gitOpsResp); err != nil {
		t.Fatalf("decode gitops response: %v", err)
	}
	if gitOpsResp.DeploymentMode != domain.RunnerDeploymentModeGitOps {
		t.Fatalf("expected gitops deployment mode, got %q", gitOpsResp.DeploymentMode)
	}
	if gitOpsResp.RegistrationToken != "[masked]" {
		t.Fatalf("standalone gitops registration token field must be masked, got %q", gitOpsResp.RegistrationToken)
	}
	if gitOpsResp.ProjectConfigToken != "[masked]" {
		t.Fatalf("standalone gitops project config token field must be masked, got %q", gitOpsResp.ProjectConfigToken)
	}
	if !strings.Contains(gitOpsResp.GitOpsManifest, "apiVersion: apps/v1") {
		t.Fatalf("expected gitops manifest, got %q", gitOpsResp.GitOpsManifest)
	}
	if strings.Contains(gitOpsResp.ProjectConfigURL, "?") ||
		strings.Contains(gitOpsResp.GitOpsManifest, "runner-config?") {
		t.Fatalf("gitops config url must not include token query params: url=%q manifest=%q", gitOpsResp.ProjectConfigURL, gitOpsResp.GitOpsManifest)
	}
	if strings.Contains(gitOpsResp.GitOpsManifest, "ENVPLANE_RUNNER_REGISTRATION_TOKEN: \"") {
		t.Fatalf("gitops manifest must not render token value into stringData: %q", gitOpsResp.GitOpsManifest)
	}
	if !strings.Contains(gitOpsResp.GitOpsManifest, "create secret generic") {
		t.Fatalf("expected out-of-band secret creation instruction in manifest")
	}
	if gitOpsResp.GitOpsPath == "" {
		t.Fatalf("expected gitops path")
	}
	if !strings.HasSuffix(gitOpsResp.GitOpsPath, ".yaml") {
		t.Fatalf("expected yaml gitops path, got %q", gitOpsResp.GitOpsPath)
	}
}

func TestBootstrapWizardDoesNotRenderStandaloneRunnerTokens(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(testRepoRoot(), "frontend", "components", "bootstrap", "BootstrapWizardClient.tsx"))
	if err != nil {
		t.Fatalf("read bootstrap wizard: %v", err)
	}
	source := string(content)
	for _, forbidden := range []string{
		"Registration token:",
		"Project config token:",
		"runnerDeploymentInstructions.registrationToken}",
		"runnerDeploymentInstructions.projectConfigToken}",
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("bootstrap wizard renders standalone runner token field %q", forbidden)
		}
	}
	if !strings.Contains(source, "Bootstrap secret command") {
		t.Fatalf("bootstrap wizard should still render the sensitive one-time bootstrap command")
	}
}

func TestBootstrapRunnerDeploymentInstructionsOneTimeSecretCommandDisplay(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	projectID := "bootstrap-runner-one-time"
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               "Bootstrap Runner One Time",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	body := []byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner",
	  "releaseName":"runner-bootstrap"
	}`)
	firstReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader(body))
	firstReq.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first instructions status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	var first domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(firstRec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first instructions: %v", err)
	}
	firstRegistrationToken, firstProjectConfigToken := runnerBootstrapTokensFromCommand(t, first.BootstrapSecretCommand)
	if first.RegistrationToken != "[masked]" || first.ProjectConfigToken != "[masked]" {
		t.Fatalf("standalone token fields must be masked in initial response: %+v", first)
	}
	firstSession, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	if strings.TrimSpace(asString(firstSession.Data[bootstrapRunnerSecretCommandDisplayedAtKey])) == "" {
		t.Fatalf("expected displayed timestamp to be set after successful response")
	}
	if !first.BootstrapSecretCommandSensitive {
		t.Fatalf("bootstrap secret command must be marked sensitive")
	}
	if !strings.Contains(first.BootstrapSecretCommand, firstRegistrationToken) ||
		!strings.Contains(first.BootstrapSecretCommand, firstProjectConfigToken) {
		t.Fatalf("initial bootstrap secret command must contain live token literals")
	}
	if first.ExpiresAt.After(time.Now().UTC().Add(5*time.Minute + 5*time.Second)) {
		t.Fatalf("runner bootstrap token TTL must be short, expiresAt=%s", first.ExpiresAt)
	}

	secondReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader(body))
	secondReq.Header.Set("Content-Type", "application/json")
	secondRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second instructions status=%d body=%s", secondRec.Code, secondRec.Body.String())
	}
	var second domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(secondRec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second instructions: %v", err)
	}
	if second.RegistrationToken != "[masked]" || second.ProjectConfigToken != "[masked]" {
		t.Fatalf("re-fetched tokens must be masked: %+v", second)
	}
	if strings.Contains(second.BootstrapSecretCommand, firstRegistrationToken) ||
		strings.Contains(second.BootstrapSecretCommand, firstProjectConfigToken) {
		t.Fatalf("masked re-display leaked live tokens: %q", second.BootstrapSecretCommand)
	}
	if !strings.Contains(second.BootstrapSecretCommand, "[masked]") {
		t.Fatalf("masked re-display should include masked placeholders: %q", second.BootstrapSecretCommand)
	}

	_, err = application.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapRunnerTokenExpiresAtKey: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatalf("expire runner registration token: %v", err)
	}
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(fmt.Sprintf(`{
	  "projectId": %q,
	  "clusterId": "dev-us",
	  "runnerId": %q,
	  "registrationToken": %q,
	  "deploymentMode": "helm",
	  "runnerNamespace": "envpilot-runner"
	}`, projectID, projectID+"-runner", firstRegistrationToken))))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusUnauthorized {
		t.Fatalf("expired runner bootstrap token must fail, got %d body=%s", registerRec.Code, registerRec.Body.String())
	}

	raw := mustReadFileString(t, logPath)
	if strings.Contains(raw, firstRegistrationToken) || strings.Contains(raw, firstProjectConfigToken) {
		t.Fatalf("audit log leaked raw bootstrap token: %s", raw)
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventRunnerBootstrapTokenGenerated)
	assertStandardAuditEvent(t, entry, auditEventRunnerBootstrapTokenGenerated, auditEndpointRunnerDeploymentInstructions, projectID, auditSubjectRunnerID, true)
}

func TestBootstrapRunnerDeploymentInstructionsResponseWriteFailureDoesNotRediscloseSecretCommand(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	projectID := "bootstrap-runner-write-failure"
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               "Bootstrap Runner Write Failure",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	body := []byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner",
	  "releaseName":"runner-bootstrap"
	}`)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	underlying := httptest.NewRecorder()
	failing := &failingWriteResponseWriter{inner: underlying}
	application.Routes().ServeHTTP(failing, req)

	sessionAfterFailure, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	if strings.TrimSpace(asString(sessionAfterFailure.Data[bootstrapRunnerSecretCommandDisplayedAtKey])) == "" {
		t.Fatalf("displayed timestamp should be persisted before disclosing raw command")
	}

	retryReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader(body))
	retryReq.Header.Set("Content-Type", "application/json")
	retryRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", retryRec.Code, retryRec.Body.String())
	}
	var retryResponse domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(retryRec.Body.Bytes(), &retryResponse); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryResponse.RegistrationToken != "[masked]" {
		t.Fatalf("retry must not re-disclose registration token after display marker is persisted: %+v", retryResponse)
	}
	if retryResponse.ProjectConfigToken != "[masked]" {
		t.Fatalf("retry must not re-disclose project config token after display marker is persisted: %+v", retryResponse)
	}
}

func TestBootstrapRunnerDeploymentInstructionsPersistFailureDoesNotDiscloseSecretCommand(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	projectID := "bootstrap-runner-display-persist-failure"
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               "Bootstrap Runner Persist Failure",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	stored, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	failStore := &failOnceBootstrapSessionStore{session: stored, failOnSave: 2}
	application.bootstrapSessions = app.NewBootstrapSessionServiceWithEncryptor(failStore, app.MustNewAESGCMCredentialEncryptor("test-bootstrap-key", "test"))

	body := []byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner",
	  "releaseName":"runner-bootstrap"
	}`)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected display marker persistence failure, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "kubectl create secret") ||
		strings.Contains(rec.Body.String(), "registrationToken") ||
		strings.Contains(rec.Body.String(), "projectConfigToken") {
		t.Fatalf("persist failure must not disclose secret command or token fields: %s", rec.Body.String())
	}
	if strings.TrimSpace(asString(failStore.session.Data[bootstrapRunnerSecretCommandDisplayedAtKey])) != "" {
		t.Fatalf("failed display marker persistence must not mark displayed")
	}
}

func TestBootstrapRunnerDeploymentInstructionsConcurrentRequestsOneTimeDisplay(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	projectID := "bootstrap-runner-concurrent"
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               "Bootstrap Runner Concurrent",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	body := []byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner",
	  "releaseName":"runner-bootstrap"
	}`)

	var wg sync.WaitGroup
	responses := make([]domain.RunnerDeploymentInstructionsResponse, 2)
	errCh := make(chan error, 2)

	for idx := 0; idx < 2; idx++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("request %d status=%d body=%s", i, rec.Code, rec.Body.String())
				return
			}
			var resp domain.RunnerDeploymentInstructionsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				errCh <- fmt.Errorf("request %d decode: %v", i, err)
				return
			}
			responses[i] = resp
			errCh <- nil
		}(idx)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	rawCount := 0
	maskedCount := 0
	for _, response := range responses {
		if response.RegistrationToken != "[masked]" || response.ProjectConfigToken != "[masked]" {
			t.Fatalf("standalone token fields must always be masked: %+v", response)
		}
		if strings.Contains(response.BootstrapSecretCommand, "[masked]") {
			maskedCount++
		} else {
			runnerBootstrapTokensFromCommand(t, response.BootstrapSecretCommand)
			rawCount++
		}
	}
	if rawCount != 1 {
		t.Fatalf("expected exactly one raw response, got %d", rawCount)
	}
	if maskedCount != 1 {
		t.Fatalf("expected exactly one masked response, got %d", maskedCount)
	}
	firstRawToken, _ := runnerBootstrapTokensFromCommand(t, responses[rawResponseIndex(responses)].BootstrapSecretCommand)
	if strings.Contains(responses[1-rawResponseIndex(responses)].BootstrapSecretCommand, firstRawToken) {
		t.Fatalf("concurrent requests should not both contain the same registration token")
	}
}

func TestBootstrapRunnerRegistrationHeartbeatAndStatusVisibility(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-runner-visibility",
		Name:               "Runner Visibility",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner-visibility/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	deployReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner-visibility/bootstrap-session/runner-deployment-instructions", bytes.NewReader([]byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner"
	}`)))
	deployReq.Header.Set("Content-Type", "application/json")
	deployRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(deployRec, deployReq)
	if deployRec.Code != http.StatusOK {
		t.Fatalf("runner instructions status=%d body=%s", deployRec.Code, deployRec.Body.String())
	}
	var deployResp domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(deployRec.Body.Bytes(), &deployResp); err != nil {
		t.Fatalf("decode runner instructions response: %v", err)
	}
	hydrateRunnerBootstrapTokensForTest(t, &deployResp)

	statusReq := httptest.NewRequest(http.MethodGet, "/api/projects/bootstrap-runner-visibility/bootstrap-session/runner-status", nil)
	statusRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var before domain.RunnerStatusResponse
	if err := json.Unmarshal(statusRec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode runner status: %v", err)
	}
	if before.Status != "waiting" {
		t.Fatalf("expected waiting status, got %q", before.Status)
	}
	if before.DeploymentMode != "helm" {
		t.Fatalf("expected helm deploymentMode, got %q", before.DeploymentMode)
	}

	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(fmt.Sprintf(`{
		  "projectId": "bootstrap-runner-visibility",
		  "clusterId": "dev-us",
		  "runnerId": "bootstrap-runner-visibility-runner",
		  "registrationToken": %q,
		  "deploymentMode": "helm",
		  "runnerNamespace": "envpilot-runner"
		}`, deployResp.RegistrationToken))))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerResp domain.RunnerRegistrationResponse
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registerResp); err != nil {
		t.Fatalf("decode runner registration response: %v", err)
	}
	if strings.TrimSpace(registerResp.RunnerAuthToken) == "" {
		t.Fatalf("expected runnerAuthToken in registration response: %s", registerRec.Body.String())
	}

	statusReq = httptest.NewRequest(http.MethodGet, "/api/projects/bootstrap-runner-visibility/bootstrap-session/runner-status", nil)
	statusRec = httptest.NewRecorder()
	application.Routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var after domain.RunnerStatusResponse
	if err := json.Unmarshal(statusRec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode runner status: %v", err)
	}
	if after.Status != "connected" {
		t.Fatalf("expected connected status after registration, got %q", after.Status)
	}
	if after.RunnerID != "bootstrap-runner-visibility-runner" {
		t.Fatalf("expected runner id, got %q", after.RunnerID)
	}
	if after.DeploymentMode != "helm" {
		t.Fatalf("expected helm deploymentMode after registration, got %q", after.DeploymentMode)
	}

	repeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(fmt.Sprintf(`{
		  "projectId": "bootstrap-runner-visibility",
		  "clusterId": "dev-us",
		  "runnerId": "bootstrap-runner-visibility-runner",
		  "registrationToken": %q,
		  "deploymentMode": "helm",
		  "runnerNamespace": "envpilot-runner"
		}`, deployResp.RegistrationToken))))
	repeatReq.Header.Set("Content-Type", "application/json")
	repeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(repeatRec, repeatReq)
	if repeatRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected repeated registration to fail, got %d: %s", repeatRec.Code, repeatRec.Body.String())
	}
}

func TestRunnerRegistrationRejectsBindingMismatchAndAcceptsValidBinding(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-runner-binding",
		Name:               "Runner Binding",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner-binding/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	deployReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-runner-binding/bootstrap-session/runner-deployment-instructions", bytes.NewReader([]byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner"
	}`)))
	deployReq.Header.Set("Content-Type", "application/json")
	deployRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(deployRec, deployReq)
	if deployRec.Code != http.StatusOK {
		t.Fatalf("runner instructions status=%d body=%s", deployRec.Code, deployRec.Body.String())
	}
	var deployResp domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(deployRec.Body.Bytes(), &deployResp); err != nil {
		t.Fatalf("decode runner instructions response: %v", err)
	}
	hydrateRunnerBootstrapTokensForTest(t, &deployResp)

	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{
			name: "wrong clusterId",
			body: fmt.Sprintf(`{
			  "projectId": "bootstrap-runner-binding",
			  "clusterId": "dev-eu",
			  "runnerId": %q,
			  "registrationToken": %q,
			  "deploymentMode": "helm",
			  "runnerNamespace": "envpilot-runner"
			}`, "bootstrap-runner-binding-runner", deployResp.RegistrationToken),
			wantCode: "ERR_RUNNER_CLUSTER_ID_MISMATCH",
		},
		{
			name: "wrong namespace",
			body: fmt.Sprintf(`{
			  "projectId": "bootstrap-runner-binding",
			  "clusterId": "dev-us",
			  "runnerId": %q,
			  "registrationToken": %q,
			  "deploymentMode": "helm",
			  "runnerNamespace": "other-namespace"
			}`, "bootstrap-runner-binding-runner", deployResp.RegistrationToken),
			wantCode: "ERR_RUNNER_NAMESPACE_MISMATCH",
		},
		{
			name: "wrong deploymentMode",
			body: fmt.Sprintf(`{
			  "projectId": "bootstrap-runner-binding",
			  "clusterId": "dev-us",
			  "runnerId": %q,
			  "registrationToken": %q,
			  "deploymentMode": "gitops",
			  "runnerNamespace": "envpilot-runner"
			}`, "bootstrap-runner-binding-runner", deployResp.RegistrationToken),
			wantCode: "ERR_RUNNER_DEPLOYMENT_MODE_MISMATCH",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("expected error code %q in %s", tc.wantCode, rec.Body.String())
			}
		})
	}

	validReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(fmt.Sprintf(`{
	  "projectId": "bootstrap-runner-binding",
	  "clusterId": "dev-us",
	  "runnerId": %q,
	  "registrationToken": %q,
	  "deploymentMode": "helm",
	  "runnerNamespace": "envpilot-runner"
	}`, "bootstrap-runner-binding-runner", deployResp.RegistrationToken))))
	validReq.Header.Set("Content-Type", "application/json")
	validRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(validRec, validReq)
	if validRec.Code != http.StatusAccepted {
		t.Fatalf("valid registration status=%d body=%s", validRec.Code, validRec.Body.String())
	}
	var validResp domain.RunnerRegistrationResponse
	if err := json.Unmarshal(validRec.Body.Bytes(), &validResp); err != nil {
		t.Fatalf("decode valid registration response: %v", err)
	}
	if strings.TrimSpace(validResp.RunnerAuthToken) == "" {
		t.Fatalf("expected runnerAuthToken, got %s", validRec.Body.String())
	}
}

func TestRunnerRegistrationRequiresRegistrationTokenAndHeartbeatRequiresRunnerAuthToken(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	tests := []struct {
		name     string
		endpoint string
		body     string
		want     int
	}{
		{
			name:     "register missing projectId",
			endpoint: "/api/v1/runners/register",
			want:     http.StatusBadRequest,
			body: `{
			  "clusterId": "dev-us",
			  "runnerId": "runner-1",
			  "registrationToken": "token-value",
			  "deploymentMode": "helm",
			  "runnerNamespace": "envpilot-runner"
			}`,
		},
		{
			name:     "register missing registrationToken",
			endpoint: "/api/v1/runners/register",
			want:     http.StatusUnauthorized,
			body: `{
			  "projectId": "bootstrap-runner-visibility",
			  "clusterId": "dev-us",
			  "runnerId": "runner-1",
			  "deploymentMode": "helm",
			  "runnerNamespace": "envpilot-runner"
			}`,
		},
		{
			name:     "register both missing",
			endpoint: "/api/v1/runners/register",
			want:     http.StatusBadRequest,
			body: `{
			  "clusterId": "dev-us",
			  "runnerId": "runner-1",
			  "deploymentMode": "helm",
			  "runnerNamespace": "envpilot-runner"
			}`,
		},
		{
			name:     "heartbeat missing projectId",
			endpoint: "/api/v1/runners/heartbeat",
			want:     http.StatusBadRequest,
			body: `{
			  "clusterId": "dev-us",
			  "runnerId": "runner-1",
			  "runnerAuthToken": "token-value",
			  "status": "online"
			}`,
		},
		{
			name:     "heartbeat missing runnerAuthToken",
			endpoint: "/api/v1/runners/heartbeat",
			want:     http.StatusUnauthorized,
			body: `{
			  "projectId": "bootstrap-runner-visibility",
			  "clusterId": "dev-us",
			  "runnerId": "runner-1",
			  "status": "online"
			}`,
		},
		{
			name:     "heartbeat missing runnerId",
			endpoint: "/api/v1/runners/heartbeat",
			want:     http.StatusBadRequest,
			body: `{
			  "projectId": "bootstrap-runner-visibility",
			  "clusterId": "dev-us",
			  "runnerAuthToken": "token-value",
			  "status": "online"
			}`,
		},
		{
			name:     "heartbeat both missing",
			endpoint: "/api/v1/runners/heartbeat",
			want:     http.StatusBadRequest,
			body: `{
			  "clusterId": "dev-us",
			  "runnerId": "runner-1",
			  "status": "online"
			}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.endpoint, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}

func prepareRunnerConfigFixture(t *testing.T, projectID string) (*Server, domain.RunnerDeploymentInstructionsResponse) {
	t.Helper()
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               "Runner Config",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	sessionUpdateReq := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), bytes.NewReader([]byte(`{
	  "current_step": 1,
	  "status": "reviewed",
	  "step_data": {
	    "oauthToken": "super-secret-oauth-token",
	    "appToken": "super-secret-app-token"
	  }
	}`)))
	sessionUpdateReq.Header.Set("Content-Type", "application/json")
	sessionUpdateRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(sessionUpdateRec, sessionUpdateReq)
	if sessionUpdateRec.Code != http.StatusOK {
		t.Fatalf("update session status=%d body=%s", sessionUpdateRec.Code, sessionUpdateRec.Body.String())
	}
	project, err := application.projects.GetProject(projectID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	session, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	if _, err := application.projectConfigs.SaveFromBootstrapSession(project, session, "runner-config-tester"); err != nil {
		t.Fatalf("save project config: %v", err)
	}
	deployReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader([]byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner"
	}`)))
	deployReq.Header.Set("Content-Type", "application/json")
	deployRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(deployRec, deployReq)
	if deployRec.Code != http.StatusOK {
		t.Fatalf("runner deploy status=%d body=%s", deployRec.Code, deployRec.Body.String())
	}
	var deployResp domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(deployRec.Body.Bytes(), &deployResp); err != nil {
		t.Fatalf("decode runner deploy response: %v", err)
	}
	hydrateRunnerBootstrapTokensForTest(t, &deployResp)
	return application, deployResp
}

func hydrateRunnerBootstrapTokensForTest(t *testing.T, response *domain.RunnerDeploymentInstructionsResponse) {
	t.Helper()
	registrationToken, projectConfigToken := runnerBootstrapTokensFromCommand(t, response.BootstrapSecretCommand)
	response.RegistrationToken = registrationToken
	response.ProjectConfigToken = projectConfigToken
}

func runnerBootstrapTokensFromCommand(t *testing.T, command string) (string, string) {
	t.Helper()
	registrationToken := bootstrapLiteralFromCommand(t, command, "--from-literal=token=")
	projectConfigToken := bootstrapLiteralFromCommand(t, command, "--from-literal=project-config-token=")
	if registrationToken == "" || projectConfigToken == "" {
		t.Fatalf("expected bootstrap secret command to contain both token literals, got %q", command)
	}
	if registrationToken == "[masked]" || projectConfigToken == "[masked]" {
		t.Fatalf("expected raw bootstrap tokens, got command %q", command)
	}
	return registrationToken, projectConfigToken
}

func bootstrapLiteralFromCommand(t *testing.T, command string, marker string) string {
	t.Helper()
	start := strings.Index(command, marker)
	if start < 0 {
		t.Fatalf("bootstrap command missing %s: %q", marker, command)
	}
	value := command[start+len(marker):]
	if strings.HasPrefix(value, "\"") {
		value = strings.TrimPrefix(value, "\"")
		end := strings.Index(value, "\"")
		if end < 0 {
			t.Fatalf("bootstrap command has unterminated quoted literal for %s: %q", marker, command)
		}
		return value[:end]
	}
	end := strings.Index(value, " ")
	if end < 0 {
		return value
	}
	return value[:end]
}

func rawResponseIndex(responses []domain.RunnerDeploymentInstructionsResponse) int {
	for idx, response := range responses {
		if !strings.Contains(response.BootstrapSecretCommand, "[masked]") {
			return idx
		}
	}
	return 0
}

func newRunnerConfigRequest(t *testing.T, deployResp domain.RunnerDeploymentInstructionsResponse, token string) *http.Request {
	t.Helper()
	if token == "" {
		token = deployResp.ProjectConfigToken
	}
	configURL, err := url.Parse(deployResp.ProjectConfigURL)
	if err != nil {
		t.Fatalf("parse projectConfigURL: %v", err)
	}
	body := fmt.Sprintf(`{
	  "clusterId": %q,
	  "runnerId": %q,
	  "runnerNamespace": %q,
	  "deploymentMode": %q
	}`, deployResp.ClusterID, deployResp.ProjectID+"-runner", deployResp.RunnerNamespace, deployResp.DeploymentMode)
	req := httptest.NewRequest(http.MethodPost, configURL.RequestURI(), bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func runnerConfigRequestWithoutAuth(t *testing.T, deployResp domain.RunnerDeploymentInstructionsResponse) *http.Request {
	t.Helper()
	req := newRunnerConfigRequest(t, deployResp, deployResp.ProjectConfigToken)
	req.Header.Del("Authorization")
	return req
}

func runnerConfigRequestWithBody(t *testing.T, deployResp domain.RunnerDeploymentInstructionsResponse, token string, body map[string]any) *http.Request {
	t.Helper()
	configURL, err := url.Parse(deployResp.ProjectConfigURL)
	if err != nil {
		t.Fatalf("parse projectConfigURL: %v", err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal runner config body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, configURL.RequestURI(), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func jsonRequest(path string, body map[string]any) *http.Request {
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func authorizedJSONRequest(rawURL string, token string, body string) *http.Request {
	parsed, err := url.Parse(rawURL)
	path := rawURL
	if err == nil {
		path = parsed.RequestURI()
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func registerRunnerForTest(t *testing.T, application *Server, deployResp domain.RunnerDeploymentInstructionsResponse) domain.RunnerRegistrationResponse {
	t.Helper()
	body := fmt.Sprintf(`{
	  "projectId": %q,
	  "clusterId": %q,
	  "runnerId": %q,
	  "registrationToken": %q,
	  "deploymentMode": %q,
	  "runnerNamespace": %q
	}`, deployResp.ProjectID, deployResp.ClusterID, deployResp.ProjectID+"-runner", deployResp.RegistrationToken, deployResp.DeploymentMode, deployResp.RunnerNamespace)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("runner registration status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp domain.RunnerRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode runner registration response: %v", err)
	}
	if strings.TrimSpace(resp.RunnerAuthToken) == "" {
		t.Fatalf("runner registration did not return runnerAuthToken: %s", rec.Body.String())
	}
	return resp
}

func registerAgentForStatusContract(t *testing.T, application *Server, projectID string, tokenResp domain.AgentRegistrationTokenResponse, agentID string) string {
	t.Helper()
	req := jsonRequest("/api/v1/agents/register", map[string]any{
		"projectId":         projectID,
		"clusterId":         tokenResp.ClusterID,
		"agentId":           agentID,
		"registrationToken": tokenResp.RegistrationToken,
	})
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("agent register status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp domain.AgentRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode agent registration response: %v", err)
	}
	if strings.TrimSpace(resp.AgentAuthToken) == "" {
		t.Fatalf("agent registration did not return agentAuthToken: %s", rec.Body.String())
	}
	return resp.AgentAuthToken
}

func runnerHeartbeatRequestBody(projectID string, clusterID string, runnerID string, namespace string, mode any, token string, status string) string {
	return fmt.Sprintf(`{
	  "projectId": %q,
	  "clusterId": %q,
	  "runnerId": %q,
	  "runnerNamespace": %q,
	  "deploymentMode": %q,
	  "runnerAuthToken": %q,
	  "status": %q
	}`, projectID, clusterID, runnerID, namespace, mode, token, status)
}

func TestRunnerRegisterThenHeartbeatHappyPathAndRejectsOldRegistrationToken(t *testing.T) {
	projectID := "bootstrap-runner-register-heartbeat"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	registerResp := registerRunnerForTest(t, application, deployResp)

	oldTokenBody := fmt.Sprintf(`{
	  "projectId": %q,
	  "clusterId": "dev-us",
	  "runnerId": %q,
	  "runnerNamespace": "envpilot-runner",
	  "deploymentMode": "helm",
	  "registrationToken": %q,
	  "status": "online"
	}`, projectID, projectID+"-runner", deployResp.RegistrationToken)
	oldTokenReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(oldTokenBody)))
	oldTokenReq.Header.Set("Content-Type", "application/json")
	oldTokenRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(oldTokenRec, oldTokenReq)
	if oldTokenRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected old registration token heartbeat to be ignored as missing auth token, got %d body=%s", oldTokenRec.Code, oldTokenRec.Body.String())
	}

	body := runnerHeartbeatRequestBody(projectID, "dev-us", projectID+"-runner", "envpilot-runner", "helm", registerResp.RunnerAuthToken, "online")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("heartbeat after registration status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"online"`) {
		t.Fatalf("expected online heartbeat response, got %s", rec.Body.String())
	}
}

func TestRunnerRegistrationPersistenceFailureDoesNotConsumeBootstrapToken(t *testing.T) {
	projectID := "bootstrap-runner-register-retry"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	stored, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	failStore := &failOnceBootstrapSessionStore{session: stored, failOnSave: 1}
	application.bootstrapSessions = app.NewBootstrapSessionServiceWithEncryptor(failStore, app.MustNewAESGCMCredentialEncryptor("test-bootstrap-key", "test"))

	body := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": %q,
  "runnerId": %q,
  "registrationToken": %q,
  "deploymentMode": %q,
  "runnerNamespace": %q
}`, deployResp.ProjectID, deployResp.ClusterID, deployResp.ProjectID+"-runner", deployResp.RegistrationToken, deployResp.DeploymentMode, deployResp.RunnerNamespace))
	firstReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader(body))
	firstReq.Header.Set("Content-Type", "application/json")
	firstRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code == http.StatusAccepted {
		t.Fatalf("expected transient persistence failure, got %d body=%s", firstRec.Code, firstRec.Body.String())
	}
	if usedAt := asString(failStore.session.Data[bootstrapRunnerTokenUsedAtKey]); usedAt != "" {
		t.Fatalf("failed registration must not consume runner bootstrap token, usedAt=%q", usedAt)
	}
	if hash := asString(failStore.session.Data[bootstrapRunnerAuthTokenHashKey]); hash != "" {
		t.Fatalf("failed registration must not persist runner auth token hash")
	}

	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader(body))
	retryReq.Header.Set("Content-Type", "application/json")
	retryRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusAccepted {
		t.Fatalf("retry registration status=%d body=%s", retryRec.Code, retryRec.Body.String())
	}
	if usedAt := asString(failStore.session.Data[bootstrapRunnerTokenUsedAtKey]); usedAt == "" {
		t.Fatalf("successful registration must consume runner bootstrap token")
	}
	if hash := asString(failStore.session.Data[bootstrapRunnerAuthTokenHashKey]); hash == "" {
		t.Fatalf("successful registration must persist runner auth token hash")
	}

	reuseReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader(body))
	reuseReq.Header.Set("Content-Type", "application/json")
	reuseRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(reuseRec, reuseReq)
	if reuseRec.Code != http.StatusUnauthorized {
		t.Fatalf("reuse after successful registration must fail, got %d body=%s", reuseRec.Code, reuseRec.Body.String())
	}
}

func TestRunnerRegistrationConcurrentRequestsWithSameTokenIssueSingleAuthToken(t *testing.T) {
	projectID := "bootstrap-runner-register-concurrent-token"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	body := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": %q,
  "runnerId": %q,
  "registrationToken": %q,
  "deploymentMode": %q,
  "runnerNamespace": %q
}`, deployResp.ProjectID, deployResp.ClusterID, deployResp.ProjectID+"-runner", deployResp.RegistrationToken, deployResp.DeploymentMode, deployResp.RunnerNamespace))

	var wg sync.WaitGroup
	statuses := make([]int, 8)
	authTokens := make([]string, 8)
	for idx := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)
			statuses[i] = rec.Code
			if rec.Code == http.StatusAccepted {
				var resp domain.RunnerRegistrationResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Errorf("decode response %d: %v", i, err)
					return
				}
				authTokens[i] = strings.TrimSpace(resp.RunnerAuthToken)
			}
		}(idx)
	}
	wg.Wait()

	accepted := 0
	issued := map[string]struct{}{}
	for idx, status := range statuses {
		switch status {
		case http.StatusAccepted:
			accepted++
			if authTokens[idx] == "" {
				t.Fatalf("accepted response %d did not include runnerAuthToken", idx)
			}
			issued[authTokens[idx]] = struct{}{}
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unexpected status at %d: %d", idx, status)
		}
	}
	if accepted != 1 {
		t.Fatalf("expected exactly one accepted registration, got %d statuses=%v", accepted, statuses)
	}
	if len(issued) != 1 {
		t.Fatalf("expected exactly one unique runner auth token, got %d", len(issued))
	}

	replayReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader(body))
	replayReq.Header.Set("Content-Type", "application/json")
	replayRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed registration token must fail, got %d body=%s", replayRec.Code, replayRec.Body.String())
	}
}

func TestRunnerHeartbeatStatusEnumValidation(t *testing.T) {
	projectID := "bootstrap-runner-heartbeat-status"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	registerResp := registerRunnerForTest(t, application, deployResp)

	tests := []struct {
		name           string
		status         string
		expectedStatus string
	}{
		{name: "waiting", status: "waiting", expectedStatus: "waiting"},
		{name: "connected", status: "connected", expectedStatus: "connected"},
		{name: "online", status: "online", expectedStatus: "online"},
		{name: "failed", status: "failed", expectedStatus: "failed"},
		{name: "default empty status", status: "", expectedStatus: "connected"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := runnerHeartbeatRequestBody(projectID, "dev-us", projectID+"-runner", "envpilot-runner", "helm", registerResp.RunnerAuthToken, tc.status)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("heartbeat status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"status":"%s"`, tc.expectedStatus)) {
				t.Fatalf("expected response status %q, got body=%s", tc.expectedStatus, rec.Body.String())
			}

			statusReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-status", projectID), nil)
			statusRec := httptest.NewRecorder()
			application.Routes().ServeHTTP(statusRec, statusReq)
			if statusRec.Code != http.StatusOK {
				t.Fatalf("runner status endpoint code=%d body=%s", statusRec.Code, statusRec.Body.String())
			}
			var runnerStatus domain.RunnerStatusResponse
			if err := json.Unmarshal(statusRec.Body.Bytes(), &runnerStatus); err != nil {
				t.Fatalf("decode runner status response: %v", err)
			}
			if runnerStatus.Status != tc.expectedStatus {
				t.Fatalf("expected stored status %q, got %q", tc.expectedStatus, runnerStatus.Status)
			}
		})
	}
}

func TestRunnerHeartbeatRejectsUnknownStatus(t *testing.T) {
	projectID := "bootstrap-runner-heartbeat-unknown"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	registerResp := registerRunnerForTest(t, application, deployResp)

	body := runnerHeartbeatRequestBody(projectID, "dev-us", projectID+"-runner", "envpilot-runner", "helm", registerResp.RunnerAuthToken, "unsupported")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown status, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "status must be one of: waiting, connected, online, degraded, failed") {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}

func TestRunnerHeartbeatRejectsMismatchedIdentityExpiredAndReusedTokens(t *testing.T) {
	projectID := "bootstrap-runner-heartbeat-auth"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	registerResp := registerRunnerForTest(t, application, deployResp)
	validRunnerID := projectID + "-runner"

	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "wrong runnerId",
			body: runnerHeartbeatRequestBody(projectID, "dev-us", "wrong-runner", "envpilot-runner", "helm", registerResp.RunnerAuthToken, "online"),
			want: http.StatusForbidden,
		},
		{
			name: "wrong projectId",
			body: runnerHeartbeatRequestBody("wrong-project", "dev-us", validRunnerID, "envpilot-runner", "helm", registerResp.RunnerAuthToken, "online"),
			want: http.StatusUnauthorized,
		},
		{
			name: "wrong clusterId",
			body: runnerHeartbeatRequestBody(projectID, "dev-eu", validRunnerID, "envpilot-runner", "helm", registerResp.RunnerAuthToken, "online"),
			want: http.StatusForbidden,
		},
		{
			name: "wrong namespace",
			body: runnerHeartbeatRequestBody(projectID, "dev-us", validRunnerID, "other-namespace", "helm", registerResp.RunnerAuthToken, "online"),
			want: http.StatusForbidden,
		},
		{
			name: "wrong deploymentMode",
			body: runnerHeartbeatRequestBody(projectID, "dev-us", validRunnerID, "envpilot-runner", "gitops", registerResp.RunnerAuthToken, "online"),
			want: http.StatusForbidden,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			application.Routes().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d body=%s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}

	projectID = "bootstrap-runner-heartbeat-reused"
	application, deployResp = prepareRunnerConfigFixture(t, projectID)
	registerResp = registerRunnerForTest(t, application, deployResp)
	validRunnerID = projectID + "-runner"
	_, err := application.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapRunnerAuthTokenHashKey: hashToken("rotated-runner-auth-token"),
		},
	})
	if err != nil {
		t.Fatalf("rotate runner auth token: %v", err)
	}
	reusedBody := runnerHeartbeatRequestBody(projectID, "dev-us", validRunnerID, "envpilot-runner", "helm", registerResp.RunnerAuthToken, "online")
	reusedReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(reusedBody)))
	reusedReq.Header.Set("Content-Type", "application/json")
	reusedRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(reusedRec, reusedReq)
	if reusedRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected rotated auth token 401, got %d body=%s", reusedRec.Code, reusedRec.Body.String())
	}
}

func TestRunnerHeartbeatFailedAuthenticationIsAudited(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	projectID := "bootstrap-runner-heartbeat-audit"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	registerResp := registerRunnerForTest(t, application, deployResp)

	body := runnerHeartbeatRequestBody(projectID, "dev-us", "wrong-runner", "envpilot-runner", "helm", registerResp.RunnerAuthToken, "online")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"event":"runner_heartbeat_auth_failed"`)) {
		t.Fatalf("expected runner heartbeat auth failure audit event: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"runner_id":"wrong-runner"`)) {
		t.Fatalf("expected runner id in audit event: %s", string(raw))
	}
	if bytes.Contains(raw, []byte(registerResp.RunnerAuthToken)) || bytes.Contains(raw, []byte(deployResp.RegistrationToken)) {
		t.Fatalf("audit log leaked raw runner token: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventRunnerHeartbeatAuthFailed)
	assertStandardAuditEvent(t, entry, auditEventRunnerHeartbeatAuthFailed, auditEndpointRunnerHeartbeat, projectID, auditSubjectRunnerID, true)
}

func TestBootstrapInvalidAttemptsAreRateLimitedAndAudited(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	projectID := "bootstrap-rate-limit"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	makeInvalid := func() *httptest.ResponseRecorder {
		req := newRunnerConfigRequest(t, deployResp, "invalid-bootstrap-token")
		req.RemoteAddr = "198.51.100.10:12345"
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		return rec
	}

	if rec := makeInvalid(); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected first invalid attempt 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := makeInvalid(); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected second invalid attempt 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	limited := makeInvalid()
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("expected third invalid attempt 429, got %d body=%s", limited.Code, limited.Body.String())
	}
	if got := limited.Header().Get("Retry-After"); got == "" {
		t.Fatalf("expected Retry-After header")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"event":"runner_config_fetch_auth_failed"`)) {
		t.Fatalf("expected runner config auth failure audit event: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"event":"bootstrap_rate_limit_hit"`)) {
		t.Fatalf("expected bootstrap rate-limit audit event: %s", string(raw))
	}
	if bytes.Contains(raw, []byte("invalid-bootstrap-token")) {
		t.Fatalf("audit log leaked raw bootstrap token: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"token_fingerprint"`)) {
		t.Fatalf("expected token fingerprint in audit log: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	authEntry := findAuditEventEntry(t, entries, auditEventRunnerConfigFetchAuthFailed)
	assertStandardAuditEvent(t, authEntry, auditEventRunnerConfigFetchAuthFailed, auditEndpointRunnerConfigFetch, projectID, auditSubjectRunnerID, true)
	limitEntry := findAuditEventEntry(t, entries, auditEventBootstrapRateLimitHit)
	assertStandardAuditEvent(t, limitEntry, auditEventBootstrapRateLimitHit, auditEndpointRunnerConfigFetch, projectID, "", false)
}

func TestBootstrapRateLimitBlocksRotatingInvalidTokensFromSameIP(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	projectID := "bootstrap-rate-limit-rotating"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	makeInvalid := func(token string) *httptest.ResponseRecorder {
		req := newRunnerConfigRequest(t, deployResp, token)
		req.RemoteAddr = "198.51.100.11:12345"
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		return rec
	}

	if rec := makeInvalid("invalid-rotating-token-1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected first rotating invalid attempt 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := makeInvalid("invalid-rotating-token-2"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected second rotating invalid attempt 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	limited := makeInvalid("invalid-rotating-token-3")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("expected rotating invalid token storm to hit 429, got %d body=%s", limited.Code, limited.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"event":"bootstrap_rate_limit_hit"`)) {
		t.Fatalf("expected bootstrap rate-limit audit event: %s", string(raw))
	}
	assertAuditLogContainsNoRawTokens(t, raw, logPath, "invalid-rotating-token-1", "invalid-rotating-token-2", "invalid-rotating-token-3")
	if !bytes.Contains(raw, []byte(`"reason":"bootstrap token validation failed"`)) && !bytes.Contains(raw, []byte(`"reason":"`)) {
		t.Fatalf("expected rate-limit/auth failure reason in audit: %s", string(raw))
	}
}

func TestBootstrapRateLimitBlocksRotatingInvalidAgentScanNextTokensFromSameIP(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	projectID := "bootstrap-rate-limit-agent-scan-next"
	agentID := projectID + "-agent"
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)
	registerBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "registrationToken": %q,
  "agentVersion": "1.0.0",
  "agentNamespace": "envpilot",
  "kubernetesVersion": "v1.30.1",
  "heartbeatIntervalSeconds": 30,
  "status": "waiting",
  "capabilities": ["apps-v1"]
}`, projectID, agentID, tokenResp.RegistrationToken))
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("agent register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}

	makeInvalid := func(token string) *httptest.ResponseRecorder {
		nextURL := fmt.Sprintf("/api/v1/agents/resource-scan/next?projectId=%s&clusterId=dev-us&agentId=%s", projectID, agentID)
		req := httptest.NewRequest(http.MethodGet, nextURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "203.0.113.20:54321"
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		return rec
	}

	if rec := makeInvalid("invalid-next-token-a"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected first rotating invalid scan-next attempt 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := makeInvalid("invalid-next-token-b"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected second rotating invalid scan-next attempt 401, got %d body=%s", rec.Code, rec.Body.String())
	}
	limited := makeInvalid("invalid-next-token-c")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("expected rotating invalid scan-next attempts to hit 429, got %d body=%s", limited.Code, limited.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"event":"bootstrap_rate_limit_hit"`)) {
		t.Fatalf("expected bootstrap rate-limit audit event: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(auditEndpointAgentResourceScanNext)) {
		t.Fatalf("expected rate-limit audit event for resource-scan next endpoint: %s", string(raw))
	}
	assertAuditLogContainsNoRawTokens(t, raw, logPath, "invalid-next-token-a", "invalid-next-token-b", "invalid-next-token-c")
}

func TestAgentResourceScanNextWithValidTokenLowVolumePasses(t *testing.T) {
	projectID := "bootstrap-agent-scan-next-low-volume"
	agentID := projectID + "-agent"
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)
	registerBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "registrationToken": %q,
  "agentVersion": "1.0.0",
  "agentNamespace": "envpilot",
  "kubernetesVersion": "v1.30.1",
  "heartbeatIntervalSeconds": 30,
  "status": "waiting",
  "capabilities": ["apps-v1"]
}`, projectID, agentID, tokenResp.RegistrationToken))
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("agent register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registerResp); err != nil {
		t.Fatalf("decode agent registration response: %v", err)
	}
	if strings.TrimSpace(registerResp.AgentAuthToken) == "" {
		t.Fatalf("expected agent auth token: %s", registerRec.Body.String())
	}

	startReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/agents/resource-scan/next?projectId=%s&clusterId=dev-us&agentId=%s", projectID, agentID), nil)
	startReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	startReq.RemoteAddr = "203.0.113.30:54321"

	rec1 := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec1, startReq)
	if rec1.Code == http.StatusTooManyRequests {
		t.Fatalf("first valid scan-next request should not be rate-limited: %d body=%s", rec1.Code, rec1.Body.String())
	}
	rec2 := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec2, startReq)
	if rec2.Code == http.StatusTooManyRequests {
		t.Fatalf("second valid scan-next request should not be rate-limited: %d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestBootstrapMalformedJSONIsRateLimitedAndAuditedBeforeDecode(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	projectID := "bootstrap-malformed-predecode"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	makeMalformed := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(`{"malformedBodySecret":"do-not-log-body"`)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.20:12345"
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		return rec
	}

	if rec := makeMalformed(); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected first malformed request 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := makeMalformed(); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected second malformed request 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	limited := makeMalformed()
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("expected repeated malformed request to hit 429, got %d body=%s", limited.Code, limited.Body.String())
	}

	validBody := fmt.Sprintf(`{
	  "projectId": %q,
	  "clusterId": "dev-us",
	  "runnerId": %q,
	  "registrationToken": %q,
	  "deploymentMode": "helm",
	  "runnerNamespace": "envpilot-runner"
	}`, projectID, projectID+"-runner", deployResp.RegistrationToken)
	validReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(validBody)))
	validReq.Header.Set("Content-Type", "application/json")
	validReq.RemoteAddr = "198.51.100.21:12345"
	validRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(validRec, validReq)
	if validRec.Code != http.StatusAccepted {
		t.Fatalf("expected valid request from non-limited IP to pass, got %d body=%s", validRec.Code, validRec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"event":"bootstrap_rate_limit_hit"`)) {
		t.Fatalf("expected bootstrap rate-limit audit event: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte(`"endpoint":"runner_registration"`)) {
		t.Fatalf("expected runner_registration endpoint in audit event: %s", string(raw))
	}
	if bytes.Contains(raw, []byte("do-not-log-body")) {
		t.Fatalf("audit log leaked malformed request body: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventBootstrapRateLimitHit)
	assertStandardAuditEvent(t, entry, auditEventBootstrapRateLimitHit, auditEndpointRunnerRegistration, "", "", false)
}

func TestBootstrapSecurityLifecycleRunnerInstructionsRegisterConfigFetchHeartbeat(t *testing.T) {
	projectID := "bootstrap-lifecycle-runner"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	if !strings.Contains(deployResp.HelmCommand, "helm upgrade --install") {
		t.Fatalf("expected helm command, got %q", deployResp.HelmCommand)
	}
	if strings.Contains(deployResp.HelmCommand, deployResp.RegistrationToken) ||
		strings.Contains(deployResp.HelmCommand, deployResp.ProjectConfigToken) {
		t.Fatalf("helm command must not contain raw tokens: %q", deployResp.HelmCommand)
	}
	if !strings.Contains(deployResp.HelmCommand, "controlPlane.existingSecret") {
		t.Fatalf("helm command must reference existingSecret: %q", deployResp.HelmCommand)
	}

	registerResp := registerRunnerForTest(t, application, deployResp)

	configReq := newRunnerConfigRequest(t, deployResp, "")
	configRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(configRec, configReq)
	if configRec.Code != http.StatusOK {
		t.Fatalf("runner config fetch status=%d body=%s", configRec.Code, configRec.Body.String())
	}

	heartbeatBody := runnerHeartbeatRequestBody(projectID, deployResp.ClusterID, projectID+"-runner", deployResp.RunnerNamespace, string(deployResp.DeploymentMode), registerResp.RunnerAuthToken, "online")
	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(heartbeatBody)))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code != http.StatusAccepted {
		t.Fatalf("runner heartbeat status=%d body=%s", heartbeatRec.Code, heartbeatRec.Body.String())
	}
}

func TestBootstrapSecurityLifecycleAgentRegisterHeartbeatResourceScan(t *testing.T) {
	projectID := "bootstrap-lifecycle-agent"
	agentID := "agent-lifecycle"
	application, _, _ := newTestServer(t, "")
	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)

	registerBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "registrationToken": %q,
  "capabilityReport": {"namespaces": ["dev-base", "shared"]}
}`, projectID, agentID, tokenResp.RegistrationToken))
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("agent register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registerResp); err != nil {
		t.Fatalf("decode agent registration response: %v", err)
	}
	if strings.TrimSpace(registerResp.AgentAuthToken) == "" {
		t.Fatalf("expected agent auth token: %s", registerRec.Body.String())
	}

	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader([]byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "agentAuthToken": %q,
  "status": "online"
}`, projectID, agentID, registerResp.AgentAuthToken))))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code != http.StatusAccepted {
		t.Fatalf("agent heartbeat status=%d body=%s", heartbeatRec.Code, heartbeatRec.Body.String())
	}

	selectReq := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), bytes.NewReader([]byte(`{
  "current_step": 3,
  "status": "reviewed",
  "step_data": {"selectedBaseNamespaces": ["dev-base"]}
}`)))
	selectReq.Header.Set("Content-Type", "application/json")
	selectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(selectRec, selectReq)
	if selectRec.Code != http.StatusOK {
		t.Fatalf("select namespaces status=%d body=%s", selectRec.Code, selectRec.Body.String())
	}
	startReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/resource-scan/start", projectID), nil)
	startRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("start scan status=%d body=%s", startRec.Code, startRec.Body.String())
	}

	oldTokenNextURL := fmt.Sprintf("/api/v1/agents/resource-scan/next?projectId=%s&clusterId=dev-us&agentId=%s&registrationToken=%s", projectID, agentID, url.QueryEscape(tokenResp.RegistrationToken))
	oldTokenNextReq := httptest.NewRequest(http.MethodGet, oldTokenNextURL, nil)
	oldTokenNextRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(oldTokenNextRec, oldTokenNextReq)
	if oldTokenNextRec.Code != http.StatusUnauthorized {
		t.Fatalf("registration token must not authenticate resource scan next after registration, got %d body=%s", oldTokenNextRec.Code, oldTokenNextRec.Body.String())
	}

	queryAuthNextURL := fmt.Sprintf("/api/v1/agents/resource-scan/next?projectId=%s&clusterId=dev-us&agentId=%s&agentAuthToken=%s", projectID, agentID, url.QueryEscape(registerResp.AgentAuthToken))
	queryAuthNextReq := httptest.NewRequest(http.MethodGet, queryAuthNextURL, nil)
	queryAuthNextRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(queryAuthNextRec, queryAuthNextReq)
	if queryAuthNextRec.Code != http.StatusUnauthorized {
		t.Fatalf("agentAuthToken query must not authenticate resource scan next, got %d body=%s", queryAuthNextRec.Code, queryAuthNextRec.Body.String())
	}

	nextURL := fmt.Sprintf("/api/v1/agents/resource-scan/next?projectId=%s&clusterId=dev-us&agentId=%s", projectID, agentID)
	nextReq := httptest.NewRequest(http.MethodGet, nextURL, nil)
	nextReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	nextRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(nextRec, nextReq)
	if nextRec.Code != http.StatusOK {
		t.Fatalf("resource scan next status=%d body=%s", nextRec.Code, nextRec.Body.String())
	}

	scanBody := []byte(fmt.Sprintf(`{
  "projectId": %q,
  "clusterId": "dev-us",
  "agentId": %q,
  "agentAuthToken": %q,
  "resourceSnapshots": [
    {"kind":"Deployment","namespace":"dev-base","name":"orders"},
    {"kind":"Service","namespace":"dev-base","name":"orders"}
  ]
}`, projectID, agentID, registerResp.AgentAuthToken))
	scanReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/resource-scan", bytes.NewReader(scanBody))
	scanReq.Header.Set("Content-Type", "application/json")
	scanReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	scanRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(scanRec, scanReq)
	if scanRec.Code != http.StatusAccepted {
		t.Fatalf("resource scan ingest status=%d body=%s", scanRec.Code, scanRec.Body.String())
	}
}

func TestBootstrapFullLifecycleAgentHTTPStatusReporterRegisterFetchReportAndRestart(t *testing.T) {
	requireHTTPServer(t)
	projectID := "bootstrap-http-reporter-agent"
	agentID := "agent-http-reporter"
	application, _, _ := newTestServer(t, "")
	tokenResp := createBootstrapAgentTokenForTest(t, application, projectID)
	controlPlane := httptest.NewServer(application.Routes())
	defer controlPlane.Close()

	reporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "", "dev-us", agentID, 5*time.Second)
	cfg := agent.Config{
		ControlPlaneURL:    controlPlane.URL,
		BootstrapProjectID: projectID,
		ClusterID:          "dev-us",
		AgentID:            agentID,
		AgentVersion:       "test",
		RegistrationToken:  tokenResp.RegistrationToken,
		HeartbeatInterval:  30 * time.Second,
	}
	capabilities := agent.ClusterCapabilities{
		KubernetesVersion: "v1.29.0",
		Capabilities:      []string{"core-v1", "apps-v1"},
		Report: domain.ClusterCapabilityReport{
			KubernetesVersion: "v1.29.0",
			Namespaces:        []string{"dev-base", "shared"},
			CapabilityFlags:   []string{"core-v1", "apps-v1"},
		},
	}
	agentAuthToken, err := reporter.RegisterAgent(context.Background(), cfg, capabilities)
	if err != nil {
		t.Fatalf("register agent through HTTPStatusReporter: %v", err)
	}
	if strings.TrimSpace(agentAuthToken) == "" {
		t.Fatalf("expected agent auth token")
	}

	cfg.AgentAuthToken = agentAuthToken
	cfg.RegistrationToken = ""
	startBootstrapResourceScanForTest(t, application, projectID, []string{"dev-base"})

	task, err := reporter.FetchResourceScanTask(context.Background(), cfg)
	if err != nil {
		t.Fatalf("fetch resource scan task through HTTPStatusReporter: %v", err)
	}
	if task == nil || !reflect.DeepEqual(task.Namespaces, []string{"dev-base"}) {
		t.Fatalf("unexpected resource scan task: %#v", task)
	}
	result := agent.ResourceScanResult{
		Snapshots: []domain.ResourceSnapshot{
			{Kind: "Deployment", Namespace: "dev-base", Name: "orders"},
			{Kind: "Service", Namespace: "dev-base", Name: "orders"},
		},
		PermissionWarnings: []string{"pods list forbidden in shared"},
	}
	if err := reporter.ReportResourceScan(context.Background(), cfg, task, result); err != nil {
		t.Fatalf("report resource scan through HTTPStatusReporter: %v", err)
	}

	restartedReporter := agent.NewHTTPStatusReporterForAgent(controlPlane.URL, "", "dev-us", agentID, 5*time.Second)
	restartedCfg := cfg
	restartedCfg.RegistrationToken = ""
	restartedCfg.AgentAuthToken = agentAuthToken
	startBootstrapResourceScanForTest(t, application, projectID, []string{"dev-base"})
	restartedTask, err := restartedReporter.FetchResourceScanTask(context.Background(), restartedCfg)
	if err != nil {
		t.Fatalf("fetch resource scan task after restart with persisted auth token: %v", err)
	}
	if restartedTask == nil || len(restartedTask.Namespaces) != 1 || restartedTask.Namespaces[0] != "dev-base" {
		t.Fatalf("unexpected restarted resource scan task: %#v", restartedTask)
	}
	if err := restartedReporter.ReportResourceScan(context.Background(), restartedCfg, restartedTask, result); err != nil {
		t.Fatalf("report resource scan after restart with persisted auth token: %v", err)
	}
}

func TestBootstrapFullLifecycleRunnerRegisterConfigFetchHeartbeat(t *testing.T) {
	projectID := "bootstrap-full-lifecycle-runner"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)

	registerResp := registerRunnerForTest(t, application, deployResp)
	configReq := newRunnerConfigRequest(t, deployResp, "")
	configRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(configRec, configReq)
	if configRec.Code != http.StatusOK {
		t.Fatalf("runner config fetch status=%d body=%s", configRec.Code, configRec.Body.String())
	}
	if strings.Contains(configRec.Body.String(), deployResp.RegistrationToken) ||
		strings.Contains(configRec.Body.String(), deployResp.ProjectConfigToken) {
		t.Fatalf("runner config response leaked bootstrap tokens: %s", configRec.Body.String())
	}

	heartbeatBody := runnerHeartbeatRequestBody(projectID, deployResp.ClusterID, projectID+"-runner", deployResp.RunnerNamespace, string(deployResp.DeploymentMode), registerResp.RunnerAuthToken, "online")
	heartbeatReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", bytes.NewReader([]byte(heartbeatBody)))
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(heartbeatRec, heartbeatReq)
	if heartbeatRec.Code != http.StatusAccepted {
		t.Fatalf("runner heartbeat status=%d body=%s", heartbeatRec.Code, heartbeatRec.Body.String())
	}
}

func TestBootstrapSecurityLifecycleInvalidRotatingTokenStormReturns429(t *testing.T) {
	projectID := "bootstrap-lifecycle-rotating"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	for index, token := range []string{"invalid-lifecycle-token-a", "invalid-lifecycle-token-b", "invalid-lifecycle-token-c"} {
		req := newRunnerConfigRequest(t, deployResp, token)
		req.RemoteAddr = "198.51.100.30:12345"
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		if index < 2 && rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d body=%s", index+1, rec.Code, rec.Body.String())
		}
		if index == 2 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("rotating invalid token storm expected 429, got %d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestBootstrapSecurityLifecycleHelmCommandOmitsRawTokens(t *testing.T) {
	_, deployResp := prepareRunnerConfigFixture(t, "bootstrap-lifecycle-helm-token")
	if strings.TrimSpace(deployResp.RegistrationToken) == "" || strings.TrimSpace(deployResp.ProjectConfigToken) == "" {
		t.Fatalf("expected generated tokens")
	}
	if strings.Contains(deployResp.HelmCommand, deployResp.RegistrationToken) ||
		strings.Contains(deployResp.HelmCommand, deployResp.ProjectConfigToken) ||
		strings.Contains(deployResp.HelmCommand, "controlPlane.token") ||
		strings.Contains(deployResp.HelmCommand, "controlPlane.configToken") {
		t.Fatalf("helm command leaked raw token or unsafe set flag: %q", deployResp.HelmCommand)
	}
	if !strings.Contains(deployResp.BootstrapSecretCommand, deployResp.RegistrationToken) ||
		!strings.Contains(deployResp.BootstrapSecretCommand, deployResp.ProjectConfigToken) {
		t.Fatalf("expected separate bootstrap secret command to contain token literals")
	}
}

func TestBootstrapSecurityLifecycleMalformedUnauthenticatedJSONRateLimitedAndAudited(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	for index := 0; index < 3; index++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader([]byte(`{"bodySecret":"do-not-audit"`)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.31:12345"
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		if index < 2 && rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed attempt %d expected 400, got %d body=%s", index+1, rec.Code, rec.Body.String())
		}
		if index == 2 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("malformed attempt 3 expected 429, got %d body=%s", rec.Code, rec.Body.String())
		}
	}

	raw := mustReadFileString(t, logPath)
	if !strings.Contains(raw, `"event":"bootstrap_rate_limit_hit"`) {
		t.Fatalf("expected rate-limit audit event: %s", raw)
	}
	if strings.Contains(raw, "do-not-audit") {
		t.Fatalf("audit log leaked malformed request body: %s", raw)
	}
}

func TestRunnerConfigMalformedJSONIsPredecodeRateLimitedWithProjectContext(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	projectID := "bootstrap-runner-config-malformed"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	cfg := application.config()
	cfg.BootstrapRateLimitRequests = 2
	cfg.BootstrapRateLimitWindow = time.Minute
	cfg.RateLimitRequests = 0
	cfg.RateLimitWindow = 0
	application.ReloadConfig(cfg)

	makeMalformed := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/runner-config", projectID), bytes.NewReader([]byte(`{"bodySecret":"do-not-log"`)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.40:12345"
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := makeMalformed(); rec.Code != http.StatusBadRequest {
		t.Fatalf("first malformed runner-config expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := makeMalformed(); rec.Code != http.StatusBadRequest {
		t.Fatalf("second malformed runner-config expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	limited := makeMalformed()
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("third malformed runner-config expected 429, got %d body=%s", limited.Code, limited.Body.String())
	}

	validInvalidTokenReq := newRunnerConfigRequest(t, deployResp, "invalid-token-after-malformed")
	validInvalidTokenReq.RemoteAddr = "198.51.100.41:12345"
	validInvalidTokenRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(validInvalidTokenRec, validInvalidTokenReq)
	if validInvalidTokenRec.Code != http.StatusUnauthorized {
		t.Fatalf("valid JSON invalid-token flow should reach handler-level limiter/auth, got %d body=%s", validInvalidTokenRec.Code, validInvalidTokenRec.Body.String())
	}

	raw := mustReadFileString(t, logPath)
	if strings.Contains(raw, "do-not-log") {
		t.Fatalf("audit log leaked malformed request body: %s", raw)
	}
	entries := parseAuditLogEntries(t, logPath)
	entry := findAuditEventEntry(t, entries, auditEventBootstrapRateLimitHit)
	assertStandardAuditEvent(t, entry, auditEventBootstrapRateLimitHit, auditEndpointRunnerConfigFetch, projectID, "", false)
}

func TestSuccessfulBootstrapEventsAreAuditedWithoutRawTokens(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	projectID := "bootstrap-success-audit"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)

	configReq := newRunnerConfigRequest(t, deployResp, "")
	configRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(configRec, configReq)
	if configRec.Code != http.StatusOK {
		t.Fatalf("runner config status=%d body=%s", configRec.Code, configRec.Body.String())
	}

	registerBody := fmt.Sprintf(`{
	  "projectId": %q,
	  "clusterId": "dev-us",
	  "runnerId": %q,
	  "registrationToken": %q,
	  "deploymentMode": "helm",
	  "runnerNamespace": "envpilot-runner"
	}`, projectID, projectID+"-runner", deployResp.RegistrationToken)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", bytes.NewReader([]byte(registerBody)))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("runner register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	for _, expected := range []string{
		`"event":"runner_config_fetch_succeeded"`,
		`"event":"runner_registration_succeeded"`,
	} {
		if !bytes.Contains(raw, []byte(expected)) {
			t.Fatalf("missing audit event %s in %s", expected, string(raw))
		}
	}
	if bytes.Contains(raw, []byte(deployResp.ProjectConfigToken)) || bytes.Contains(raw, []byte(deployResp.RegistrationToken)) {
		t.Fatalf("audit log leaked raw bootstrap token: %s", string(raw))
	}
	entries := parseAuditLogEntries(t, logPath)
	configEntry := findAuditEventEntry(t, entries, auditEventRunnerConfigFetchSucceeded)
	assertStandardAuditEvent(t, configEntry, auditEventRunnerConfigFetchSucceeded, auditEndpointRunnerConfigFetch, projectID, auditSubjectRunnerID, true)
	registerEntry := findAuditEventEntry(t, entries, auditEventRunnerRegistrationSucceeded)
	assertStandardAuditEvent(t, registerEntry, auditEventRunnerRegistrationSucceeded, auditEndpointRunnerRegistration, projectID, auditSubjectRunnerID, true)
}

func TestCentralSecretRedactionSanitizesAuditFileAndLogger(t *testing.T) {
	application, _, logs := newTestServer(t, "")
	loggedSecret := "known-log-secret-token"
	application.writeAuditLog(map[string]any{
		"event":         "redaction_test",
		"endpoint":      "unit",
		"authorization": "Bearer " + loggedSecret,
		"nested": map[string]any{
			"registrationToken":  "known-registration-secret",
			"projectConfigToken": "known-project-config-secret",
			"password":           "known-password-secret",
		},
		"items": []any{
			map[string]any{"agentAuthToken": "known-agent-auth-secret"},
			`{"runnerAuthToken":"known-runner-auth-secret","token":"known-token-secret"}`,
		},
		"reason": `failed with Authorization: Bearer known-reason-bearer registrationToken="known-reason-registration"`,
	})
	rawLogs := logs.String()
	for _, secret := range []string{
		loggedSecret,
		"known-registration-secret",
		"known-project-config-secret",
		"known-password-secret",
		"known-agent-auth-secret",
		"known-runner-auth-secret",
		"known-token-secret",
		"known-reason-bearer",
		"known-reason-registration",
	} {
		if strings.Contains(rawLogs, secret) {
			t.Fatalf("structured audit log leaked secret %q: %s", secret, rawLogs)
		}
	}

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	cfg := application.config()
	cfg.AuditLogPath = auditPath
	application.ReloadConfig(cfg)
	application.writeAuditLog(map[string]any{
		"event":              "redaction_test_file",
		"endpoint":           "unit",
		"token_fingerprint":  tokenFingerprint("known-fingerprint-source"),
		"registration_token": "known-file-registration-secret",
		"configToken":        "known-file-config-secret",
		"secret":             "known-file-secret",
		"reason":             `token=known-file-token password=known-file-password`,
	})
	rawAudit := mustReadFileString(t, auditPath)
	for _, secret := range []string{
		"known-file-registration-secret",
		"known-file-config-secret",
		"known-file-secret",
		"known-file-token",
		"known-file-password",
	} {
		if strings.Contains(rawAudit, secret) {
			t.Fatalf("audit file leaked secret %q: %s", secret, rawAudit)
		}
	}
	if !strings.Contains(rawAudit, `"token_fingerprint"`) {
		t.Fatalf("audit file should preserve token fingerprint metadata: %s", rawAudit)
	}
}

func TestMalformedBootstrapDecodeRedactsSecretsFromResponseAuditAndLogs(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", auditPath)
	application, _, logs := newTestServer(t, "")
	bodySecret := "known-malformed-body-secret"
	authSecret := "known-authorization-secret"
	querySecret := "known-query-token-secret"
	body := fmt.Sprintf(`{"projectId":"demo","registrationToken":"%s","password":"known-malformed-password"`, bodySecret)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register?registrationToken="+url.QueryEscape(querySecret), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authSecret)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed request 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	rawResponse := rec.Body.String()
	rawAudit := mustReadFileString(t, auditPath)
	rawLogs := logs.String()
	for _, secret := range []string{bodySecret, authSecret, querySecret, "known-malformed-password"} {
		if strings.Contains(rawResponse, secret) {
			t.Fatalf("error response leaked secret %q: %s", secret, rawResponse)
		}
		if strings.Contains(rawAudit, secret) {
			t.Fatalf("audit log leaked secret %q: %s", secret, rawAudit)
		}
		if strings.Contains(rawLogs, secret) {
			t.Fatalf("structured logs leaked secret %q: %s", secret, rawLogs)
		}
	}
	if !strings.Contains(rawAudit, "registrationToken=[redacted]") {
		t.Fatalf("request audit should redact sensitive query values: %s", rawAudit)
	}
	if !strings.Contains(rawResponse, "invalid request body") {
		t.Fatalf("sanitized error response should remain useful: %s", rawResponse)
	}
}

func TestAuditLogSanitizesBootstrapQueryTokens(t *testing.T) {
	logPath := t.TempDir() + "/audit.log"
	t.Setenv("ENVPLANE_AUDIT_LOG_PATH", logPath)
	application, _, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/resource-scan/next?projectId=demo&agentId=agent-1&registrationToken=query-secret-token", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d body=%s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if bytes.Contains(raw, []byte("query-secret-token")) {
		t.Fatalf("audit log leaked query token: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte("registrationToken=[redacted]")) {
		t.Fatalf("expected redacted query token in audit log: %s", string(raw))
	}
}

func TestRunnerConfigEndpointRequiresTokenAndReturnsSanitizedProjectConfig(t *testing.T) {
	projectID := "bootstrap-runner-config"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)

	if strings.Contains(deployResp.ProjectConfigURL, "?") ||
		strings.Contains(deployResp.ProjectConfigURL, deployResp.ProjectConfigToken) ||
		strings.Contains(deployResp.ProjectConfigURL, deployResp.RegistrationToken) {
		t.Fatalf("projectConfigURL must not contain live tokens: %q", deployResp.ProjectConfigURL)
	}

	noTokenReq := newRunnerConfigRequest(t, deployResp, "")
	noTokenReq.Header.Del("Authorization")
	noTokenRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(noTokenRec, noTokenReq)
	if noTokenRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized without token, got %d body=%s", noTokenRec.Code, noTokenRec.Body.String())
	}

	firstReq := newRunnerConfigRequest(t, deployResp, "")
	firstRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("runner config first fetch status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	var config domain.ProjectConfig
	if err := json.Unmarshal(firstRec.Body.Bytes(), &config); err != nil {
		t.Fatalf("decode runner config: %v", err)
	}
	if config.ProjectID != projectID {
		t.Fatalf("unexpected project id: %q", config.ProjectID)
	}
	var payload map[string]any
	if err := json.Unmarshal(firstRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode runner config payload: %v", err)
	}
	if _, exists := payload["sensitive"]; exists {
		t.Fatalf("runner config must not include sensitive map: %s", firstRec.Body.String())
	}
	if _, exists := payload["sensitive_refs"]; !exists {
		t.Fatalf("runner config should include sensitive references, got %s", firstRec.Body.String())
	}
	raw := firstRec.Body.String()
	if strings.Contains(raw, "super-secret-oauth-token") ||
		strings.Contains(raw, "super-secret-app-token") ||
		strings.Contains(raw, `"ciphertext"`) {
		t.Fatalf("runner config leaked secret value: %s", raw)
	}

	secondReq := newRunnerConfigRequest(t, deployResp, "")
	secondRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected second fetch to fail with 401, got %d body=%s", secondRec.Code, secondRec.Body.String())
	}
}

func TestRunnerConfigEndpointRejectsExpiredToken(t *testing.T) {
	projectID := "bootstrap-runner-config-expired"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	_, err := application.bootstrapSessions.Update(projectID, app.BootstrapSessionUpdate{
		StepData: map[string]any{
			bootstrapRunnerConfigTokenExpiresAtKey: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatalf("expire runner config token: %v", err)
	}
	req := newRunnerConfigRequest(t, deployResp, "")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected expired token to fail with 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunnerConfigEndpointRejectsWrongRunnerIdentity(t *testing.T) {
	projectID := "bootstrap-runner-config-identity"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	configURL, err := url.Parse(deployResp.ProjectConfigURL)
	if err != nil {
		t.Fatalf("parse projectConfigURL: %v", err)
	}
	body := fmt.Sprintf(`{
	  "clusterId": %q,
	  "runnerId": "unexpected-runner",
	  "runnerNamespace": %q,
	  "deploymentMode": %q
	}`, deployResp.ClusterID, deployResp.RunnerNamespace, deployResp.DeploymentMode)
	req := httptest.NewRequest(http.MethodPost, configURL.RequestURI(), bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+deployResp.ProjectConfigToken)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected wrong runner identity to fail with 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunnerConfigEndpointDoesNotConsumeTokenOnConfigLoadError(t *testing.T) {
	projectID := "bootstrap-runner-config-transient"
	application, _, _ := newTestServer(t, "")
	project := domain.Project{
		ID:                 projectID,
		Name:               "Runner Config Transient",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}
	if _, err := application.projects.SaveProject(project); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	deployReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/runner-deployment-instructions", projectID), bytes.NewReader([]byte(`{
	  "deploymentMode":"helm",
	  "clusterId":"dev-us",
	  "runnerNamespace":"envpilot-runner"
	}`)))
	deployReq.Header.Set("Content-Type", "application/json")
	deployRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(deployRec, deployReq)
	if deployRec.Code != http.StatusOK {
		t.Fatalf("runner deploy status=%d body=%s", deployRec.Code, deployRec.Body.String())
	}
	var deployResp domain.RunnerDeploymentInstructionsResponse
	if err := json.Unmarshal(deployRec.Body.Bytes(), &deployResp); err != nil {
		t.Fatalf("decode runner deploy response: %v", err)
	}
	hydrateRunnerBootstrapTokensForTest(t, &deployResp)

	firstReq := newRunnerConfigRequest(t, deployResp, "")
	firstRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code == http.StatusOK {
		t.Fatalf("expected config load error before project config exists, got status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}

	session, err := application.bootstrapSessions.GetStored(projectID)
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	if _, err := application.projectConfigs.SaveFromBootstrapSession(project, session, "runner-config-tester"); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	retryReq := newRunnerConfigRequest(t, deployResp, "")
	retryRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(retryRec, retryReq)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("expected retry with same token to succeed, got %d body=%s", retryRec.Code, retryRec.Body.String())
	}

	reuseReq := newRunnerConfigRequest(t, deployResp, "")
	reuseRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(reuseRec, reuseReq)
	if reuseRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected reuse after successful response to fail with 401, got %d body=%s", reuseRec.Code, reuseRec.Body.String())
	}
}

func TestRunnerConfigEndpointRejectsProjectConfigTokenInRequestBody(t *testing.T) {
	projectID := "bootstrap-runner-config-body-token"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	configURL, err := url.Parse(deployResp.ProjectConfigURL)
	if err != nil {
		t.Fatalf("parse projectConfigURL: %v", err)
	}
	body := fmt.Sprintf(`{
	  "clusterId": %q,
	  "runnerId": %q,
	  "runnerNamespace": %q,
	  "deploymentMode": %q,
	  "projectConfigToken": %q
	}`, deployResp.ClusterID, projectID+"-runner", deployResp.RunnerNamespace, deployResp.DeploymentMode, deployResp.ProjectConfigToken)
	req := httptest.NewRequest(http.MethodPost, configURL.RequestURI(), bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected body token config fetch to fail, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project config token must be sent only in the Authorization header") {
		t.Fatalf("expected config-token-only header error, got %s", rec.Body.String())
	}
}

func TestRunnerConfigEndpointRejectsRegistrationTokenAliasesInBody(t *testing.T) {
	projectID := "bootstrap-runner-config-body-aliases"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	configURL, err := url.Parse(deployResp.ProjectConfigURL)
	if err != nil {
		t.Fatalf("parse projectConfigURL: %v", err)
	}
	for _, bodyTokenKey := range []string{
		`"registrationToken"`,
		`"registration_token"`,
	} {
		body := fmt.Sprintf(`{
  "clusterId": %q,
  "runnerId": %q,
  "runnerNamespace": %q,
  "deploymentMode": %q,
  %s: %q
}`, deployResp.ClusterID, projectID+"-runner", deployResp.RunnerNamespace, deployResp.DeploymentMode, bodyTokenKey, deployResp.ProjectConfigToken)
		req := httptest.NewRequest(http.MethodPost, configURL.RequestURI(), bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected registration alias %s to be rejected, got %d body=%s", bodyTokenKey, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "registration token") {
			t.Fatalf("runner-config error must not refer to registration token: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "project config token must be sent only in the Authorization header") {
			t.Fatalf("expected config-token-only header error, got %s", rec.Body.String())
		}
	}
}

func TestRunnerConfigEndpointMissingBearerMentionsProjectConfigToken(t *testing.T) {
	projectID := "bootstrap-runner-config-missing-bearer"
	application, deployResp := prepareRunnerConfigFixture(t, projectID)
	req := newRunnerConfigRequest(t, deployResp, "")
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected missing bearer token to fail, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "registration token") {
		t.Fatalf("runner-config error must not mention registration token: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project config token") {
		t.Fatalf("runner-config error should mention project config token: %s", rec.Body.String())
	}
}

func TestBootstrapSecurityEndpointStatusCodeContracts(t *testing.T) {
	application, deployResp := prepareRunnerConfigFixture(t, "status-contract-runner")
	expectStatus := func(name string, req *http.Request, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		application.Routes().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s status=%d want=%d body=%s", name, rec.Code, want, rec.Body.String())
		}
	}

	expectStatus("runner register malformed", httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", strings.NewReader(`{`)), http.StatusBadRequest)
	expectStatus("runner register missing auth", jsonRequest("/api/v1/runners/register", map[string]any{
		"projectId":       deployResp.ProjectID,
		"clusterId":       deployResp.ClusterID,
		"runnerId":        deployResp.ProjectID + "-runner",
		"deploymentMode":  deployResp.DeploymentMode,
		"runnerNamespace": deployResp.RunnerNamespace,
	}), http.StatusUnauthorized)
	expectStatus("runner register invalid auth", jsonRequest("/api/v1/runners/register", map[string]any{
		"projectId":         deployResp.ProjectID,
		"clusterId":         deployResp.ClusterID,
		"runnerId":          deployResp.ProjectID + "-runner",
		"registrationToken": "invalid-runner-registration-token",
		"deploymentMode":    deployResp.DeploymentMode,
		"runnerNamespace":   deployResp.RunnerNamespace,
		"runnerVersion":     "test",
	}), http.StatusUnauthorized)
	expectStatus("runner register binding mismatch", jsonRequest("/api/v1/runners/register", map[string]any{
		"projectId":         deployResp.ProjectID,
		"clusterId":         deployResp.ClusterID,
		"runnerId":          deployResp.ProjectID + "-runner",
		"registrationToken": deployResp.RegistrationToken,
		"deploymentMode":    deployResp.DeploymentMode,
		"runnerNamespace":   "other-namespace",
	}), http.StatusForbidden)
	runnerRegistration := registerRunnerForTest(t, application, deployResp)

	expectStatus("runner heartbeat malformed", httptest.NewRequest(http.MethodPost, "/api/v1/runners/heartbeat", strings.NewReader(`{`)), http.StatusBadRequest)
	expectStatus("runner heartbeat missing auth", jsonRequest("/api/v1/runners/heartbeat", map[string]any{
		"projectId": deployResp.ProjectID,
		"runnerId":  deployResp.ProjectID + "-runner",
	}), http.StatusUnauthorized)
	expectStatus("runner heartbeat invalid auth", jsonRequest("/api/v1/runners/heartbeat", map[string]any{
		"projectId":       deployResp.ProjectID,
		"clusterId":       deployResp.ClusterID,
		"runnerId":        deployResp.ProjectID + "-runner",
		"runnerAuthToken": "invalid-runner-auth-token",
	}), http.StatusUnauthorized)
	expectStatus("runner heartbeat binding mismatch", jsonRequest("/api/v1/runners/heartbeat", map[string]any{
		"projectId":       deployResp.ProjectID,
		"clusterId":       "wrong-cluster",
		"runnerId":        deployResp.ProjectID + "-runner",
		"runnerAuthToken": runnerRegistration.RunnerAuthToken,
	}), http.StatusForbidden)

	expectStatus("runner config malformed", authorizedJSONRequest(deployResp.ProjectConfigURL, "invalid-config-token", `{`), http.StatusBadRequest)
	expectStatus("runner config missing auth", runnerConfigRequestWithoutAuth(t, deployResp), http.StatusUnauthorized)
	expectStatus("runner config invalid auth", newRunnerConfigRequest(t, deployResp, "invalid-project-config-token"), http.StatusUnauthorized)
	expectStatus("runner config identity mismatch", runnerConfigRequestWithBody(t, deployResp, deployResp.ProjectConfigToken, map[string]any{
		"clusterId":       deployResp.ClusterID,
		"runnerId":        "wrong-runner",
		"runnerNamespace": deployResp.RunnerNamespace,
		"deploymentMode":  deployResp.DeploymentMode,
	}), http.StatusForbidden)

	agentProjectID := "status-contract-agent"
	tokenResp := createBootstrapAgentTokenForTest(t, application, agentProjectID)
	expectStatus("agent register malformed", httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", strings.NewReader(`{`)), http.StatusBadRequest)
	expectStatus("agent register missing agentId", jsonRequest("/api/v1/agents/register", map[string]any{
		"projectId":         agentProjectID,
		"clusterId":         tokenResp.ClusterID,
		"registrationToken": tokenResp.RegistrationToken,
	}), http.StatusBadRequest)
	expectStatus("agent register missing clusterId", jsonRequest("/api/v1/agents/register", map[string]any{
		"projectId":         agentProjectID,
		"agentId":           "agent-1",
		"registrationToken": tokenResp.RegistrationToken,
	}), http.StatusBadRequest)
	expectStatus("agent register missing auth", jsonRequest("/api/v1/agents/register", map[string]any{
		"projectId": agentProjectID,
		"clusterId": tokenResp.ClusterID,
		"agentId":   "agent-1",
	}), http.StatusUnauthorized)
	expectStatus("agent register invalid auth", jsonRequest("/api/v1/agents/register", map[string]any{
		"projectId":         agentProjectID,
		"clusterId":         tokenResp.ClusterID,
		"agentId":           "agent-1",
		"registrationToken": "invalid-agent-registration-token",
	}), http.StatusUnauthorized)
	expectStatus("agent register binding mismatch", jsonRequest("/api/v1/agents/register", map[string]any{
		"projectId":         agentProjectID,
		"clusterId":         "wrong-cluster",
		"agentId":           "agent-1",
		"registrationToken": tokenResp.RegistrationToken,
	}), http.StatusForbidden)
	agentAuthToken := registerAgentForStatusContract(t, application, agentProjectID, tokenResp, "agent-1")

	expectStatus("agent heartbeat malformed", httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(`{`)), http.StatusBadRequest)
	expectStatus("agent heartbeat missing auth", jsonRequest("/api/v1/agents/heartbeat", map[string]any{
		"projectId": agentProjectID,
		"clusterId": tokenResp.ClusterID,
		"agentId":   "agent-1",
	}), http.StatusUnauthorized)
	expectStatus("agent heartbeat invalid auth", jsonRequest("/api/v1/agents/heartbeat", map[string]any{
		"projectId":      agentProjectID,
		"clusterId":      tokenResp.ClusterID,
		"agentId":        "agent-1",
		"agentAuthToken": "invalid-agent-auth-token",
	}), http.StatusUnauthorized)
	expectStatus("agent heartbeat binding mismatch", jsonRequest("/api/v1/agents/heartbeat", map[string]any{
		"projectId":      agentProjectID,
		"clusterId":      tokenResp.ClusterID,
		"agentId":        "wrong-agent",
		"agentAuthToken": agentAuthToken,
	}), http.StatusForbidden)

	expectStatus("agent scan next malformed", httptest.NewRequest(http.MethodGet, "/api/v1/agents/resource-scan/next?clusterId="+url.QueryEscape(tokenResp.ClusterID)+"&agentId=agent-1", nil), http.StatusBadRequest)
	expectStatus("agent scan next missing auth", httptest.NewRequest(http.MethodGet, "/api/v1/agents/resource-scan/next?projectId="+agentProjectID+"&clusterId="+url.QueryEscape(tokenResp.ClusterID)+"&agentId=agent-1", nil), http.StatusUnauthorized)
	scanNextInvalid := httptest.NewRequest(http.MethodGet, "/api/v1/agents/resource-scan/next?projectId="+agentProjectID+"&clusterId="+url.QueryEscape(tokenResp.ClusterID)+"&agentId=agent-1", nil)
	scanNextInvalid.Header.Set("Authorization", "Bearer invalid-agent-auth-token")
	expectStatus("agent scan next invalid auth", scanNextInvalid, http.StatusUnauthorized)
	scanNextMismatch := httptest.NewRequest(http.MethodGet, "/api/v1/agents/resource-scan/next?projectId="+agentProjectID+"&clusterId="+url.QueryEscape(tokenResp.ClusterID)+"&agentId=wrong-agent", nil)
	scanNextMismatch.Header.Set("Authorization", "Bearer "+agentAuthToken)
	expectStatus("agent scan next binding mismatch", scanNextMismatch, http.StatusForbidden)

	expectStatus("agent scan ingest malformed", httptest.NewRequest(http.MethodPost, "/api/v1/agents/resource-scan", strings.NewReader(`{`)), http.StatusBadRequest)
	expectStatus("agent scan ingest missing auth", jsonRequest("/api/v1/agents/resource-scan", map[string]any{
		"projectId": agentProjectID,
		"clusterId": tokenResp.ClusterID,
		"agentId":   "agent-1",
	}), http.StatusUnauthorized)
	ingestInvalid := jsonRequest("/api/v1/agents/resource-scan", map[string]any{
		"projectId": agentProjectID,
		"clusterId": tokenResp.ClusterID,
		"agentId":   "agent-1",
	})
	ingestInvalid.Header.Set("Authorization", "Bearer invalid-agent-auth-token")
	expectStatus("agent scan ingest invalid auth", ingestInvalid, http.StatusUnauthorized)
	ingestMismatch := jsonRequest("/api/v1/agents/resource-scan", map[string]any{
		"projectId": agentProjectID,
		"clusterId": tokenResp.ClusterID,
		"agentId":   "wrong-agent",
	})
	ingestMismatch.Header.Set("Authorization", "Bearer "+agentAuthToken)
	expectStatus("agent scan ingest binding mismatch", ingestMismatch, http.StatusForbidden)
}

func TestBootstrapSecurityEndpointRateLimitStatusCodeContracts(t *testing.T) {
	withBootstrapLimit := func(application *Server) {
		cfg := application.config()
		cfg.BootstrapRateLimitRequests = 1
		cfg.BootstrapRateLimitWindow = time.Minute
		application.ReloadConfig(cfg)
	}
	assertRateLimited := func(name string, application *Server, makeRequest func() *http.Request) {
		t.Helper()
		first := httptest.NewRecorder()
		application.Routes().ServeHTTP(first, makeRequest())
		if first.Code == http.StatusTooManyRequests {
			t.Fatalf("%s first request was rate-limited: %d body=%s", name, first.Code, first.Body.String())
		}
		second := httptest.NewRecorder()
		application.Routes().ServeHTTP(second, makeRequest())
		if second.Code != http.StatusTooManyRequests {
			t.Fatalf("%s second request status=%d want=429 body=%s", name, second.Code, second.Body.String())
		}
	}

	t.Run("runner register", func(t *testing.T) {
		application, deployResp := prepareRunnerConfigFixture(t, "rate-contract-runner-register")
		withBootstrapLimit(application)
		assertRateLimited(t.Name(), application, func() *http.Request {
			return jsonRequest("/api/v1/runners/register", map[string]any{
				"projectId":         deployResp.ProjectID,
				"clusterId":         deployResp.ClusterID,
				"runnerId":          deployResp.ProjectID + "-runner",
				"registrationToken": "invalid-runner-registration-token",
				"deploymentMode":    deployResp.DeploymentMode,
				"runnerNamespace":   deployResp.RunnerNamespace,
			})
		})
	})

	t.Run("runner heartbeat", func(t *testing.T) {
		application, deployResp := prepareRunnerConfigFixture(t, "rate-contract-runner-heartbeat")
		withBootstrapLimit(application)
		assertRateLimited(t.Name(), application, func() *http.Request {
			return jsonRequest("/api/v1/runners/heartbeat", map[string]any{
				"projectId":       deployResp.ProjectID,
				"clusterId":       deployResp.ClusterID,
				"runnerId":        deployResp.ProjectID + "-runner",
				"runnerAuthToken": "invalid-runner-auth-token",
			})
		})
	})

	t.Run("runner config fetch", func(t *testing.T) {
		application, deployResp := prepareRunnerConfigFixture(t, "rate-contract-runner-config")
		withBootstrapLimit(application)
		assertRateLimited(t.Name(), application, func() *http.Request {
			return newRunnerConfigRequest(t, deployResp, "invalid-project-config-token")
		})
	})

	t.Run("agent register", func(t *testing.T) {
		application, _, _ := newTestServer(t, "")
		withBootstrapLimit(application)
		tokenResp := createBootstrapAgentTokenForTest(t, application, "rate-contract-agent-register")
		assertRateLimited(t.Name(), application, func() *http.Request {
			return jsonRequest("/api/v1/agents/register", map[string]any{
				"projectId":         "rate-contract-agent-register",
				"clusterId":         tokenResp.ClusterID,
				"agentId":           "agent-1",
				"registrationToken": "invalid-agent-registration-token",
			})
		})
	})

	t.Run("agent heartbeat", func(t *testing.T) {
		application, _, _ := newTestServer(t, "")
		withBootstrapLimit(application)
		tokenResp := createBootstrapAgentTokenForTest(t, application, "rate-contract-agent-heartbeat")
		assertRateLimited(t.Name(), application, func() *http.Request {
			return jsonRequest("/api/v1/agents/heartbeat", map[string]any{
				"projectId":      "rate-contract-agent-heartbeat",
				"clusterId":      tokenResp.ClusterID,
				"agentId":        "agent-1",
				"agentAuthToken": "invalid-agent-auth-token",
			})
		})
	})

	t.Run("agent scan next", func(t *testing.T) {
		application, _, _ := newTestServer(t, "")
		withBootstrapLimit(application)
		tokenResp := createBootstrapAgentTokenForTest(t, application, "rate-contract-agent-scan-next")
		assertRateLimited(t.Name(), application, func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/resource-scan/next?projectId=rate-contract-agent-scan-next&clusterId="+url.QueryEscape(tokenResp.ClusterID)+"&agentId=agent-1", nil)
			req.Header.Set("Authorization", "Bearer invalid-agent-auth-token")
			return req
		})
	})

	t.Run("agent scan ingest", func(t *testing.T) {
		application, _, _ := newTestServer(t, "")
		withBootstrapLimit(application)
		tokenResp := createBootstrapAgentTokenForTest(t, application, "rate-contract-agent-scan-ingest")
		assertRateLimited(t.Name(), application, func() *http.Request {
			req := jsonRequest("/api/v1/agents/resource-scan", map[string]any{
				"projectId": "rate-contract-agent-scan-ingest",
				"clusterId": tokenResp.ClusterID,
				"agentId":   "agent-1",
			})
			req.Header.Set("Authorization", "Bearer invalid-agent-auth-token")
			return req
		})
	})
}

func TestRunnersHealthcheckEndpoint(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/runners/health", nil)
	healthRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("healthcheck status=%d body=%s", healthRec.Code, healthRec.Body.String())
	}
	if !strings.Contains(healthRec.Body.String(), `"status":"ok"`) {
		t.Fatalf("expected ok status, got %s", healthRec.Body.String())
	}
}

func TestBootstrapResourceScanDispatchQueueFlow(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "bootstrap-queue",
		Name:               "Bootstrap Queue",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-queue/bootstrap-session", nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	tokenReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-queue/bootstrap-session/agent-token", bytes.NewReader([]byte(`{}`)))
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("token status=%d body=%s", tokenRec.Code, tokenRec.Body.String())
	}
	var tokenResp domain.AgentRegistrationTokenResponse
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}

	registerBody := []byte(fmt.Sprintf(`{
  "projectId": "bootstrap-queue",
  "clusterId": %q,
  "agentId": "agent-queue",
  "registrationToken": %q,
  "capabilityReport": {"namespaces": ["dev-base", "shared"]}
}`, tokenResp.ClusterID, tokenResp.RegistrationToken))
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusAccepted {
		t.Fatalf("register status=%d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerResp domain.AgentRegistrationResponse
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registerResp); err != nil {
		t.Fatalf("decode agent registration response: %v", err)
	}
	if strings.TrimSpace(registerResp.AgentAuthToken) == "" {
		t.Fatalf("expected agent auth token: %s", registerRec.Body.String())
	}

	selectNSBody := []byte(`{
  "current_step": 3,
  "status": "reviewed",
  "step_data": {"selectedBaseNamespaces": ["dev-base"]}
}`)
	selectReq := httptest.NewRequest(http.MethodPatch, "/api/projects/bootstrap-queue/bootstrap-session", bytes.NewReader(selectNSBody))
	selectReq.Header.Set("Content-Type", "application/json")
	selectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(selectRec, selectReq)
	if selectRec.Code != http.StatusOK {
		t.Fatalf("select namespaces status=%d body=%s", selectRec.Code, selectRec.Body.String())
	}

	startReq := httptest.NewRequest(http.MethodPost, "/api/projects/bootstrap-queue/bootstrap-session/resource-scan/start", nil)
	startRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("start scan status=%d body=%s", startRec.Code, startRec.Body.String())
	}

	nextURL := "/api/v1/agents/resource-scan/next?projectId=bootstrap-queue&clusterId=" + url.QueryEscape(tokenResp.ClusterID) + "&agentId=agent-queue"
	nextReq := httptest.NewRequest(http.MethodGet, nextURL, nil)
	nextReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	nextRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(nextRec, nextReq)
	if nextRec.Code != http.StatusOK {
		t.Fatalf("next task status=%d body=%s", nextRec.Code, nextRec.Body.String())
	}
	var taskResp domain.AgentResourceScanTaskResponse
	if err := json.Unmarshal(nextRec.Body.Bytes(), &taskResp); err != nil {
		t.Fatalf("decode task response: %v", err)
	}
	if len(taskResp.Namespaces) != 1 || taskResp.Namespaces[0] != "dev-base" {
		t.Fatalf("unexpected task namespaces=%v", taskResp.Namespaces)
	}

	nextAgainRec := httptest.NewRecorder()
	nextAgainReq := httptest.NewRequest(http.MethodGet, nextURL, nil)
	nextAgainReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	application.Routes().ServeHTTP(nextAgainRec, nextAgainReq)
	if nextAgainRec.Code != http.StatusNoContent {
		t.Fatalf("expected second dispatch 204, got %d body=%s", nextAgainRec.Code, nextAgainRec.Body.String())
	}

	scanBody := []byte(fmt.Sprintf(`{
  "projectId": "bootstrap-queue",
  "clusterId": %q,
  "agentId": "agent-queue",
  "agentAuthToken": %q,
  "resourceSnapshots": [
    {"kind":"Deployment","namespace":"dev-base","name":"orders"},
    {"kind":"Secret","namespace":"dev-base","name":"orders-token"}
  ]
}`, tokenResp.ClusterID, registerResp.AgentAuthToken))
	scanReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/resource-scan", bytes.NewReader(scanBody))
	scanReq.Header.Set("Content-Type", "application/json")
	scanReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	scanRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(scanRec, scanReq)
	if scanRec.Code != http.StatusAccepted {
		t.Fatalf("ingest scan status=%d body=%s", scanRec.Code, scanRec.Body.String())
	}

	nextCompletedRec := httptest.NewRecorder()
	nextCompletedReq := httptest.NewRequest(http.MethodGet, nextURL, nil)
	nextCompletedReq.Header.Set("Authorization", "Bearer "+registerResp.AgentAuthToken)
	application.Routes().ServeHTTP(nextCompletedRec, nextCompletedReq)
	if nextCompletedRec.Code != http.StatusNoContent {
		t.Fatalf("expected completed queue 204, got %d body=%s", nextCompletedRec.Code, nextCompletedRec.Body.String())
	}
}

func TestClusterScopedStatusRejectsWrongCluster(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	createBody := []byte(`{
  "id": "pr-91",
  "clusterId": "dev-us",
  "product": "generic",
  "source": {"pullRequestId": "91", "commit": "abc123"}
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	statusBody := []byte(`{"status": "ready", "message": "wrong cluster", "clusterId": "prod-eu"}`)
	statusReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/pr-91/status", bytes.NewReader(statusBody))
	statusReq.Header.Set("Content-Type", "application/json")
	statusRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusConflict {
		t.Fatalf("expected cluster conflict 409, got %d: %s", statusRec.Code, statusRec.Body.String())
	}
}

func TestClusterScopedStatusRejectsMissingClusterID(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	createBody := []byte(`{
  "id": "pr-92",
  "clusterId": "dev-us",
  "product": "generic",
  "source": {"pullRequestId": "92", "commit": "abc123"}
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	statusBody := []byte(`{"status": "ready", "message": "missing cluster"}`)
	statusReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/pr-92/status", bytes.NewReader(statusBody))
	statusReq.Header.Set("Content-Type", "application/json")
	statusRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusConflict {
		t.Fatalf("expected cluster conflict 409, got %d: %s", statusRec.Code, statusRec.Body.String())
	}
}

func TestClusterScopedStatusAcceptsMatchingClusterID(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	createBody := []byte(`{
  "id": "pr-93",
  "clusterId": "dev-us",
  "product": "generic",
  "source": {"pullRequestId": "93", "commit": "abc123"}
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	statusBody := []byte(`{"status": "ready", "message": "ready", "clusterId": "dev-us"}`)
	statusReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/pr-93/status", bytes.NewReader(statusBody))
	statusReq.Header.Set("Content-Type", "application/json")
	statusRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", statusRec.Code, statusRec.Body.String())
	}
}

func TestClusterScopedEventsRejectsWrongClusterID(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	createBody := []byte(`{
  "id": "pr-94",
  "clusterId": "dev-us",
  "product": "generic",
  "source": {"pullRequestId": "94", "commit": "abc123"}
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	eventsBody := []byte(`{
  "clusterId": "prod-eu",
  "events": [
    {
      "type": "Warning",
      "reason": "Failed",
      "message": "simulated"
    }
  ]
}`)
	eventsReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/pr-94/events", bytes.NewReader(eventsBody))
	eventsReq.Header.Set("Content-Type", "application/json")
	eventsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(eventsRec, eventsReq)
	if eventsRec.Code != http.StatusConflict {
		t.Fatalf("expected events conflict 409, got %d: %s", eventsRec.Code, eventsRec.Body.String())
	}
}

func TestClusterScopedEventsAcceptsMatchingClusterID(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	createBody := []byte(`{
  "id": "pr-96",
  "clusterId": "dev-us",
  "product": "generic",
  "source": {"pullRequestId": "96", "commit": "abc123"}
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	eventsBody := []byte(`{
  "clusterId": "dev-us",
  "events": [
    {
      "type": "Normal",
      "reason": "Started",
      "message": "ok"
    }
  ]
}`)
	eventsReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/pr-96/events", bytes.NewReader(eventsBody))
	eventsReq.Header.Set("Content-Type", "application/json")
	eventsRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(eventsRec, eventsReq)
	if eventsRec.Code != http.StatusAccepted {
		t.Fatalf("expected events accepted 202, got %d: %s", eventsRec.Code, eventsRec.Body.String())
	}
}

func TestClusterScopedFluxStatusRejectsWrongClusterID(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	createBody := []byte(`{
  "id": "pr-95",
  "clusterId": "dev-us",
  "product": "generic",
  "source": {"pullRequestId": "95", "commit": "abc123"}
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	fluxBody := []byte(`{
  "clusterId": "prod-eu",
  "fluxStatus": {
    "status": "ready",
    "message": "wrong cluster"
  }
}`)
	fluxReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/pr-95/flux-status", bytes.NewReader(fluxBody))
	fluxReq.Header.Set("Content-Type", "application/json")
	fluxRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(fluxRec, fluxReq)
	if fluxRec.Code != http.StatusConflict {
		t.Fatalf("expected flux status conflict 409, got %d: %s", fluxRec.Code, fluxRec.Body.String())
	}
}

func TestClusterScopedFluxStatusAcceptsMatchingClusterID(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	createBody := []byte(`{
  "id": "pr-97",
  "clusterId": "dev-us",
  "product": "generic",
  "source": {"pullRequestId": "97", "commit": "abc123"}
}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	fluxBody := []byte(`{
  "clusterId": "dev-us",
  "fluxStatus": {
    "status": "ready",
    "message": "correct cluster"
  }
}`)
	fluxReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/pr-97/flux-status", bytes.NewReader(fluxBody))
	fluxReq.Header.Set("Content-Type", "application/json")
	fluxRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(fluxRec, fluxReq)
	if fluxRec.Code != http.StatusAccepted {
		t.Fatalf("expected flux status accepted 202, got %d: %s", fluxRec.Code, fluxRec.Body.String())
	}
}

func TestSettingsEndpointsPersistUniversalConfiguration(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	body := []byte(`{
  "repositories": [
    {
      "id": "checkout-app",
      "name": "Checkout",
      "kind": "application",
      "provider": "github",
      "url": "https://github.com/example/checkout.git",
      "default_branch": "main",
      "secret_ref": "github-token"
    }
  ],
  "secret_refs": [
    {
      "id": "github-token",
      "provider": "env",
      "scope": "github",
      "reference": "ENVPLANE_GITHUB_TOKEN"
    }
  ],
  "manifest_sources": [
    {
      "id": "checkout-template",
      "kind": "helm",
      "repository_id": "checkout-app",
      "path": "deploy/helm/checkout",
      "values_path": "values-preview.yaml",
      "enabled": true
    }
  ],
  "clusters": [
    {
      "id": "dev-us",
      "provider": "eks",
      "context": "dev-us",
      "namespace_selector": "app.kubernetes.io/managed-by=envpilot",
      "secret_ref": "cluster-token",
      "enabled": true
    }
  ],
  "notifications": [
    {
      "id": "preview-slack",
      "provider": "slack",
      "channel": "#preview",
      "secret_ref": "slack-webhook",
      "enabled": true
    }
  ],
  "runtime": {
    "default_product": "generic",
    "default_project": "checkout",
    "default_mode": "full",
    "domain_root": "preview.example.com",
    "namespace_prefix": "envpilot-pr",
    "default_ttl_hours": 24,
    "ttl_check_seconds": 120,
    "job_retry_seconds": 10,
    "job_max_attempts": 5,
    "gitops_dir": "/gitops",
    "product_base_path": "apps",
    "flux_namespace": "flux-system",
    "source_ref_name": "apps",
    "depends_on_name": "infra",
    "health_check_name": "app",
    "enable_git_commit": true,
    "enable_git_push": false,
    "git_push_remote": "origin",
    "git_push_branch": "main"
  }
}`)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var settings domain.ControlPlaneSettings
	if err := json.Unmarshal(getRec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settings.Runtime.DefaultProject != "checkout" || settings.Runtime.DomainRoot != "preview.example.com" {
		t.Fatalf("unexpected runtime settings: %+v", settings.Runtime)
	}
	if len(settings.Repositories) != 1 || settings.Repositories[0].SecretRef != "github-token" {
		t.Fatalf("unexpected repositories: %+v", settings.Repositories)
	}
	if len(settings.SecretRefs) != 1 || settings.SecretRefs[0].Reference != "ENVPLANE_GITHUB_TOKEN" {
		t.Fatalf("unexpected secret refs: %+v", settings.SecretRefs)
	}
	if len(settings.ManifestSources) != 1 || settings.ManifestSources[0].Path != "deploy/helm/checkout" {
		t.Fatalf("unexpected manifest sources: %+v", settings.ManifestSources)
	}
	if len(settings.Clusters) != 1 || !settings.Clusters[0].Enabled {
		t.Fatalf("unexpected clusters: %+v", settings.Clusters)
	}
	if len(settings.Notifications) != 1 || settings.Notifications[0].Provider != "slack" {
		t.Fatalf("unexpected notifications: %+v", settings.Notifications)
	}
}

func TestValidateSecretReferenceEndpointDoesNotReturnSecretValue(t *testing.T) {
	t.Setenv("ENVPLANE_GITHUB_TOKEN", "super-secret-token")
	application, _, _ := newTestServer(t, "")
	payload := `{
  "secret_refs": [
    {
      "id": "github-token",
      "provider": "env",
      "scope": "github",
      "reference": "ENVPLANE_GITHUB_TOKEN"
    }
  ],
  "runtime": {"default_mode": "full"}
}`
	saveReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(payload))
	saveReq.Header.Set("Content-Type", "application/json")
	saveRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("expected settings save 200, got %d: %s", saveRec.Code, saveRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/secret-refs/github-token/validate", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected validate 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret-token") {
		t.Fatalf("validation leaked secret value: %s", rec.Body.String())
	}
	var result struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode validation: %v", err)
	}
	if !result.Valid {
		t.Fatalf("expected valid result: %s", rec.Body.String())
	}
}

func TestValidateBootstrapSCMConfigEndpoint(t *testing.T) {
	transport := fakeRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/repos/owner/app" && req.URL.Query().Get("per_page") == "":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"permissions":{"pull":true,"push":true}}`)),
				Request:    req,
			}, nil
		case req.URL.Path == "/repos/owner/gitops" && req.URL.Query().Get("per_page") == "":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"permissions":{"push":true}}`)),
				Request:    req,
			}, nil
		case req.URL.Path == "/repos/owner/gitops-denied" && req.URL.Query().Get("per_page") == "":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"permissions":{"push":false}}`)),
				Request:    req,
			}, nil
		case req.URL.Path == "/repos/owner/app/branches":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`[{"name":"main"},{"name":"feature"}]`)),
				Request:    req,
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    req,
			}, nil
		}
	})
	client := &http.Client{Transport: transport}

	tmp := t.TempDir()
	cfg := config.FromEnv()
	cfg.DataDir = tmp
	cfg.GitOpsDir = tmp

	envStore, err := store.NewJSONStore(tmp + "/environments.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer := gitops.NewFluxRenderer(cfg.GitOps)
	writer := gitops.NewFileWriter(tmp, false, "", "")
	envService := app.NewEnvironmentService(cfg, catalog.Default(), envStore, renderer, writer)
	productStore, err := store.NewJSONProductStore(tmp+"/products.json", catalog.Default().List())
	if err != nil {
		t.Fatalf("product store: %v", err)
	}
	productService := app.NewProductService(productStore)
	envService.SetProductProvider(productService)
	projectStore, err := store.NewJSONProjectStore(tmp+"/projects.json", catalog.DefaultProjects())
	if err != nil {
		t.Fatalf("project store: %v", err)
	}
	projectService := app.NewProjectService(projectStore)
	settingsStore, err := store.NewJSONSettingsStore(tmp+"/settings.json", app.DefaultControlPlaneSettings(cfg))
	if err != nil {
		t.Fatalf("settings store: %v", err)
	}
	settingsService := app.NewSettingsService(settingsStore)
	projectService.SetSettingsProvider(settingsService)
	envService.SetProjectStore(projectStore)
	envService.SetSettingsProvider(settingsService)
	jobManager := jobs.NewManager(envService, jobs.WithProjectResolver(projectService))
	validationService := app.NewSCMValidationServiceWithConfig(app.SCMValidationServiceConfig{GitHubAPI: "http://example.com", Client: client})

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	application := New(Dependencies{Config: cfg, Service: envService, Products: productService, Projects: projectService, Settings: settingsService, SCMValidation: validationService, Jobs: jobManager, Logger: logger})
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 "default",
		Name:               "Default",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/owner/app",
		GitOpsRepositoryID: "github.com/owner/gitops",
	}); err != nil {
		t.Fatalf("save default project: %v", err)
	}

	body := []byte(`{
	  "provider":"github",
	  "appRepoUrl":"https://github.com/owner/app.git",
	  "gitopsRepoUrl":"https://github.com/owner/gitops.git",
	  "defaultBranch":"main",
	  "authMethod":"OAuth",
	  "oauthToken":"token"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/default/bootstrap-session/validate-scm", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result struct {
		Valid                    bool     `json:"valid"`
		AppRepositoryReadable    bool     `json:"appRepositoryReadable"`
		GitopsRepositoryWritable bool     `json:"gitopsRepositoryWritable"`
		Branches                 []string `json:"branches"`
		Errors                   []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Valid {
		t.Fatalf("expected valid result, got: %s", rec.Body.String())
	}
	if !result.AppRepositoryReadable || !result.GitopsRepositoryWritable {
		t.Fatalf("expected readable and writable flags true: %+v", result)
	}
	if len(result.Branches) != 2 || result.Branches[0] != "main" {
		t.Fatalf("unexpected branches: %v", result.Branches)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected validation errors: %v", result.Errors)
	}

	body = []byte(`{
	  "provider":"github",
	  "appRepoUrl":"https://github.com/owner/app.git",
	  "gitopsRepoUrl":"https://github.com/owner/gitops-denied.git",
	  "defaultBranch":"main",
	  "authMethod":"OAuth",
	  "oauthToken":"token"
	}`)
	req = httptest.NewRequest(http.MethodPost, "/api/projects/default/bootstrap-session/validate-scm", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected denied 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var deniedResult struct {
		Valid                    bool `json:"valid"`
		GitopsRepositoryWritable bool `json:"gitopsRepositoryWritable"`
		Errors                   []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deniedResult); err != nil {
		t.Fatalf("decode denied result: %v", err)
	}
	if deniedResult.Valid {
		t.Fatalf("expected denied write result to be invalid: %s", rec.Body.String())
	}
	if deniedResult.GitopsRepositoryWritable {
		t.Fatalf("expected denied write access to be false: %s", rec.Body.String())
	}
	if len(deniedResult.Errors) == 0 || deniedResult.Errors[0].Code != "write_access_denied" {
		t.Fatalf("expected write_access_denied error: %v", deniedResult.Errors)
	}
}

func TestGetClusterHealthEndpoint(t *testing.T) {
	application, _, _ := newTestServer(t, "")
	payload := `{
  "clusters": [
    {
      "id": "dev-us",
      "provider": "eks",
      "context": "dev-us",
      "enabled": true
    }
  ],
  "runtime": {"default_mode": "full"}
}`
	saveReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(payload))
	saveReq.Header.Set("Content-Type", "application/json")
	saveRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("expected settings save 200, got %d: %s", saveRec.Code, saveRec.Body.String())
	}

	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/clusters/dev-us/health", nil)
	healthRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", healthRec.Code, healthRec.Body.String())
	}
	var cluster domain.ClusterTarget
	if err := json.Unmarshal(healthRec.Body.Bytes(), &cluster); err != nil {
		t.Fatalf("decode cluster response: %v", err)
	}
	if cluster.ID != "dev-us" || cluster.Provider != "eks" || !cluster.Enabled {
		t.Fatalf("unexpected cluster: %#v", cluster)
	}

	missingReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/clusters/unknown/health", nil)
	missingRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", missingRec.Code, missingRec.Body.String())
	}
}

func TestEnvironmentEventsEndpointStoresKubernetesEvents(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	err := envStore.Save(domain.Environment{
		ID:        "kan-404",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "envpilot-pr-kan-404",
		Status:    domain.StatusCreating,
	})
	if err != nil {
		t.Fatalf("save environment: %v", err)
	}
	body := []byte(`{
  "events": [
    {
      "uid": "event-1",
      "namespace": "envpilot-pr-kan-404",
      "type": "Warning",
      "reason": "FailedScheduling",
      "message": "0/3 nodes are available",
      "involvedKind": "Pod",
      "involvedName": "cms-api-abc",
      "count": 2,
      "lastSeen": "2026-05-01T10:00:00Z"
    }
  ]
}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/environments/kan-404/events", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	env, err := envStore.Get("kan-404")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if len(env.Events) != 1 || env.Events[0].Reason != "FailedScheduling" {
		t.Fatalf("events = %#v", env.Events)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/environments/kan-404/events", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "FailedScheduling") {
		t.Fatalf("expected event in response, got %s", getRec.Body.String())
	}
}

func TestEnvironmentFluxStatusEndpointStoresReconciliationStatus(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	err := envStore.Save(domain.Environment{
		ID:        "kan-405",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "envpilot-pr-kan-405",
		Status:    domain.StatusCreating,
	})
	if err != nil {
		t.Fatalf("save environment: %v", err)
	}
	body := []byte(`{
  "fluxStatus": {
    "status": "ready",
    "message": "flux ready",
    "kustomizations": [
      {
        "kind": "Kustomization",
        "name": "kan-405.bethunder",
        "namespace": "flux-system",
        "ready": true,
        "reason": "ReconciliationSucceeded",
        "lastAppliedRevision": "main/abc123"
      }
    ],
    "helmReleases": [
      {
        "kind": "HelmRelease",
        "name": "nginx",
        "namespace": "envpilot-pr-kan-405",
        "ready": true,
        "reason": "InstallSucceeded"
      }
    ]
  }
}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/environments/kan-405/flux-status", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	env, err := envStore.Get("kan-405")
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if env.FluxStatus == nil || env.FluxStatus.Status != domain.StatusReady {
		t.Fatalf("flux status = %#v", env.FluxStatus)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/environments/kan-405/flux-status", nil)
	getRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "ReconciliationSucceeded") {
		t.Fatalf("expected flux status in response, got %s", getRec.Body.String())
	}
}

func TestGetEnvironmentReturnsFluxBackendDeploymentStatus(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	_, err := application.projects.SaveProject(domain.Project{
		ID:                 "cms",
		Name:               "CMS",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/envpilot/cms",
		GitOpsRepositoryID: "github.com/envpilot/cms-gitops",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	project, err := application.projects.GetProject("cms")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if _, err := application.projectConfigs.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "sess-flux-406",
		ProjectID: "cms",
		Status:    domain.BootstrapSessionStatusCompiled,
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "fluxcd",
				"fluxcd": map[string]any{
					"gitopsRepo":        "https://github.com/example/flux-repo.git",
					"gitopsPath":        "clusters/dev",
					"fluxNamespace":     "flux-system",
					"kustomizationName": "kan-406",
					"commitMode":        "commit",
				},
			},
		},
	}, "test-user"); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	created, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-406",
		Project: "cms",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	created.FluxStatus = &domain.FluxStatus{
		Status:    domain.StatusReady,
		Message:   "flux ready",
		UpdatedAt: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
	}
	if err := envStore.Save(created); err != nil {
		t.Fatalf("save flux status on environment: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/environments/kan-406", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		DeploymentBackend string `json:"deploymentBackend"`
		DeploymentStatus  *struct {
			Backend          string             `json:"backend"`
			FluxStatus       *domain.FluxStatus `json:"fluxStatus"`
			HelmDirectStatus *struct {
				Status string `json:"status"`
				Ready  bool   `json:"ready"`
			} `json:"helmDirectStatus"`
		} `json:"deploymentStatus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.DeploymentBackend != "fluxcd" {
		t.Fatalf("expected fluxcd backend, got %q", payload.DeploymentBackend)
	}
	if payload.DeploymentStatus == nil || payload.DeploymentStatus.FluxStatus == nil {
		t.Fatalf("expected flux deployment status section")
	}
	if payload.DeploymentStatus.Backend != "fluxcd" {
		t.Fatalf("expected flux deployment status backend, got %q", payload.DeploymentStatus.Backend)
	}
	if payload.DeploymentStatus.HelmDirectStatus != nil {
		t.Fatalf("expected helm status section to be absent for flux backend")
	}
}

func TestGetEnvironmentReturnsHelmBackendDeploymentStatus(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	_, err := application.projects.SaveProject(domain.Project{
		ID:                 "cms",
		Name:               "CMS",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/envpilot/cms",
		GitOpsRepositoryID: "github.com/envpilot/cms-gitops",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	project, err := application.projects.GetProject("cms")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if _, err := application.projectConfigs.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "sess-helm-407",
		ProjectID: "cms",
		Status:    domain.BootstrapSessionStatusCompiled,
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}, "test-user"); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	created, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-407",
		Project: "cms",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	created.Status = domain.StatusCreating
	if err := envStore.Save(created); err != nil {
		t.Fatalf("save environment: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/environments/kan-407", nil)
	rec := httptest.NewRecorder()
	application.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		DeploymentBackend string `json:"deploymentBackend"`
		DeploymentStatus  *struct {
			Backend          string `json:"backend"`
			HelmDirectStatus *struct {
				Status string `json:"status"`
				Ready  bool   `json:"ready"`
			} `json:"helmDirectStatus"`
			FluxStatus *domain.FluxStatus `json:"fluxStatus"`
		} `json:"deploymentStatus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.DeploymentBackend != "helm_direct" {
		t.Fatalf("expected helm_direct backend, got %q", payload.DeploymentBackend)
	}
	if payload.DeploymentStatus == nil || payload.DeploymentStatus.HelmDirectStatus == nil {
		t.Fatalf("expected helm deployment status section")
	}
	if payload.DeploymentStatus.Backend != "helm_direct" {
		t.Fatalf("expected helm deployment status backend, got %q", payload.DeploymentStatus.Backend)
	}
	if payload.DeploymentStatus.HelmDirectStatus.Status != string(domain.StatusCreating) {
		t.Fatalf("unexpected helm status %q", payload.DeploymentStatus.HelmDirectStatus.Status)
	}
	if payload.DeploymentStatus.FluxStatus != nil {
		t.Fatalf("expected flux section to be absent for helm backend")
	}
}

func TestListEnvironmentsIncludesBackendAndDeploymentStatus(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	_, err := application.projects.SaveProject(domain.Project{
		ID:                 "cms-flux",
		Name:               "CMS Flux",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/envpilot/cms-flux",
		GitOpsRepositoryID: "github.com/envpilot/cms-flux-gitops",
	})
	if err != nil {
		t.Fatalf("create flux project: %v", err)
	}
	_, err = application.projects.SaveProject(domain.Project{
		ID:                 "cms-helm",
		Name:               "CMS Helm",
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/envpilot/cms-helm",
		GitOpsRepositoryID: "github.com/envpilot/cms-helm-gitops",
	})
	if err != nil {
		t.Fatalf("create helm project: %v", err)
	}

	projectFlux, err := application.projects.GetProject("cms-flux")
	if err != nil {
		t.Fatalf("get flux project: %v", err)
	}
	projectHelm, err := application.projects.GetProject("cms-helm")
	if err != nil {
		t.Fatalf("get helm project: %v", err)
	}
	if _, err := application.projectConfigs.SaveFromBootstrapSession(projectFlux, domain.BootstrapSession{
		ID:        "sess-flux-list",
		ProjectID: "cms-flux",
		Status:    domain.BootstrapSessionStatusCompiled,
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "fluxcd",
				"fluxcd": map[string]any{
					"gitopsRepo":        "https://github.com/example/flux-repo.git",
					"gitopsPath":        "clusters/dev",
					"fluxNamespace":     "flux-system",
					"kustomizationName": "cms-flux",
					"commitMode":        "commit",
				},
			},
		},
	}, "test-user"); err != nil {
		t.Fatalf("save flux project config: %v", err)
	}
	if _, err := application.projectConfigs.SaveFromBootstrapSession(projectHelm, domain.BootstrapSession{
		ID:        "sess-helm-list",
		ProjectID: "cms-helm",
		Status:    domain.BootstrapSessionStatusCompiled,
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
			},
		},
	}, "test-user"); err != nil {
		t.Fatalf("save helm project config: %v", err)
	}

	fluxEnv, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-flux",
		Project: "cms-flux",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create flux environment: %v", err)
	}
	fluxEnv.FluxStatus = &domain.FluxStatus{
		Status:    domain.StatusReady,
		Message:   "flux ready",
		UpdatedAt: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
	}
	if err := envStore.Save(fluxEnv); err != nil {
		t.Fatalf("save flux env status: %v", err)
	}

	helmEnv, err := application.service.CreateEnvironment(context.Background(), domain.CreateEnvironmentRequest{
		ID:      "kan-helm",
		Project: "cms-helm",
		Product: "bethunder",
	})
	if err != nil {
		t.Fatalf("create helm environment: %v", err)
	}
	helmEnv.Status = domain.StatusCreating
	if err := envStore.Save(helmEnv); err != nil {
		t.Fatalf("save helm env status: %v", err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/environments", nil)
	listRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	var payload []struct {
		ID                string `json:"id"`
		Project           string `json:"project"`
		DeploymentBackend string `json:"deploymentBackend"`
		DeploymentStatus  *struct {
			Backend          string             `json:"backend"`
			FluxStatus       *domain.FluxStatus `json:"fluxStatus"`
			HelmDirectStatus *struct {
				Status string `json:"status"`
				Ready  bool   `json:"ready"`
			} `json:"helmDirectStatus"`
		} `json:"deploymentStatus"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list environments: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("expected 2 environments, got %d", len(payload))
	}
	var fluxFound, helmFound bool
	for _, item := range payload {
		switch item.ID {
		case "kan-flux":
			fluxFound = true
			if item.DeploymentBackend != "fluxcd" {
				t.Fatalf("expected kan-flux backend fluxcd, got %q", item.DeploymentBackend)
			}
			if item.DeploymentStatus == nil || item.DeploymentStatus.FluxStatus == nil {
				t.Fatalf("expected flux status for kan-flux")
			}
			if item.DeploymentStatus.HelmDirectStatus != nil {
				t.Fatalf("expected missing helm status for kan-flux")
			}
		case "kan-helm":
			helmFound = true
			if item.DeploymentBackend != "helm_direct" {
				t.Fatalf("expected kan-helm backend helm_direct, got %q", item.DeploymentBackend)
			}
			if item.DeploymentStatus == nil || item.DeploymentStatus.HelmDirectStatus == nil {
				t.Fatalf("expected helm status for kan-helm")
			}
			if item.DeploymentStatus.FluxStatus != nil {
				t.Fatalf("expected missing flux status for kan-helm")
			}
		}
	}
	if !fluxFound || !helmFound {
		t.Fatalf("missing expected environments in list payload: flux=%t helm=%t", fluxFound, helmFound)
	}
}

func TestPinUnpinEnvironmentEndpointsTogglePinnedState(t *testing.T) {
	application, envStore, _ := newTestServer(t, "")
	expiresAt := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	err := envStore.Save(domain.Environment{
		ID:        "kan-603",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "envpilot-pr-kan-603",
		Status:    domain.StatusCreating,
		TTLHours:  24,
		ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatalf("save environment: %v", err)
	}

	pinReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/kan-603/pin", nil)
	pinRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(pinRec, pinReq)
	if pinRec.Code != http.StatusOK {
		t.Fatalf("expected 200 pin, got %d: %s", pinRec.Code, pinRec.Body.String())
	}
	var pinned domain.Environment
	if err := json.Unmarshal(pinRec.Body.Bytes(), &pinned); err != nil {
		t.Fatalf("decode pinned response: %v", err)
	}
	if !pinned.Pinned || pinned.ExpiresAt != nil {
		t.Fatalf("expected pinned env without expiration, got %+v", pinned)
	}

	unpinReq := httptest.NewRequest(http.MethodPost, "/api/v1/environments/kan-603/unpin", nil)
	unpinRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(unpinRec, unpinReq)
	if unpinRec.Code != http.StatusOK {
		t.Fatalf("expected 200 unpin, got %d: %s", unpinRec.Code, unpinRec.Body.String())
	}
	var unpinned domain.Environment
	if err := json.Unmarshal(unpinRec.Body.Bytes(), &unpinned); err != nil {
		t.Fatalf("decode unpinned response: %v", err)
	}
	if unpinned.Pinned || unpinned.ExpiresAt == nil {
		t.Fatalf("expected unpinned env with expiration, got %+v", unpinned)
	}
}

func oauthStateCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == oauthStateCookieName {
			return cookie
		}
	}
	return nil
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == apiSessionCookieName {
			return cookie
		}
	}
	return nil
}

func cookieFrom(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func newTestServer(t *testing.T, githubSecret string) (*Server, store.EnvironmentStore, *bytes.Buffer) {
	return newTestServerWithSecrets(t, githubSecret, "")
}

func parseAuditLogEntries(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw := mustReadFileString(t, path)
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("audit entry is not valid json: %v: %s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

func findAuditEventEntry(t *testing.T, entries []map[string]any, event string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry["event"] == event {
			return entry
		}
	}
	t.Fatalf("audit event %q not found in %#v", event, entries)
	return nil
}

func assertStandardAuditEvent(t *testing.T, entry map[string]any, event string, endpoint string, projectID string, subjectKey string, expectTokenFingerprint bool) {
	t.Helper()
	requiredStrings := map[string]string{
		"event":       event,
		"endpoint":    endpoint,
		"project_id":  normalizeSettingsID(projectID),
		"user_id":     "",
		"remote_addr": "",
		"trace_id":    "",
		"reason":      "",
		"outcome":     expectedAuditOutcome(event),
	}
	for key, expected := range requiredStrings {
		value, ok := entry[key].(string)
		if !ok {
			t.Fatalf("audit event %q missing string field %q: %#v", event, key, entry)
		}
		if expected != "" && value != expected {
			t.Fatalf("audit event %q field %q = %q, want %q: %#v", event, key, value, expected, entry)
		}
		if expected == "" && (key == "remote_addr" || key == "trace_id") && strings.TrimSpace(value) == "" {
			t.Fatalf("audit event %q field %q must be non-empty: %#v", event, key, entry)
		}
	}
	if subjectKey != "" {
		if _, ok := entry[subjectKey].(string); !ok {
			t.Fatalf("audit event %q missing subject field %q: %#v", event, subjectKey, entry)
		}
	}
	if expectTokenFingerprint {
		if value, ok := entry["token_fingerprint"].(string); !ok || strings.TrimSpace(value) == "" {
			t.Fatalf("audit event %q missing token_fingerprint: %#v", event, entry)
		}
	}
}

func assertStandardRequestAuditEvent(t *testing.T, entry map[string]any, method string, endpoint string, statusCode int, projectID string, outcome string, reason string) {
	t.Helper()
	requiredStrings := map[string]string{
		"event":       auditEventAPIRequest,
		"endpoint":    endpoint,
		"method":      method,
		"path":        endpoint,
		"project_id":  normalizeSettingsID(projectID),
		"user_id":     "",
		"remote_addr": "",
		"trace_id":    "",
		"reason":      reason,
		"outcome":     outcome,
	}
	for key, expected := range requiredStrings {
		value, ok := entry[key].(string)
		if !ok {
			t.Fatalf("request audit missing string field %q: %#v", key, entry)
		}
		if expected != "" && value != expected {
			t.Fatalf("request audit field %q = %q, want %q: %#v", key, value, expected, entry)
		}
		if expected == "" && (key == "remote_addr" || key == "trace_id") && strings.TrimSpace(value) == "" {
			t.Fatalf("request audit field %q must be non-empty: %#v", key, entry)
		}
	}
	if value, ok := entry["status_code"].(float64); !ok || int(value) != statusCode {
		t.Fatalf("request audit status_code = %#v, want %d: %#v", entry["status_code"], statusCode, entry)
	}
}

func assertAuditSchemaContract(t *testing.T, entries []map[string]any, rawSecrets ...string) {
	t.Helper()
	if len(entries) == 0 {
		t.Fatalf("expected audit entries")
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal audit entries: %v", err)
	}
	for _, secret := range rawSecrets {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("audit entries leaked raw secret %q: %s", secret, string(encoded))
		}
	}
	for index, entry := range entries {
		event := requiredAuditStringField(t, entry, "event", index)
		outcome := requiredAuditStringField(t, entry, "outcome", index)
		requiredAuditStringField(t, entry, "remote_addr", index)
		requiredAuditStringField(t, entry, "trace_id", index)
		if _, ok := entry["reason"].(string); !ok {
			t.Fatalf("audit entry %d missing string reason: %#v", index, entry)
		}
		if outcome == "failure" || outcome == "rate_limited" {
			if strings.TrimSpace(fmt.Sprint(entry["reason"])) == "" {
				t.Fatalf("audit entry %d failure/rate-limit missing reason: %#v", index, entry)
			}
		}
		if event == auditEventAPIRequest {
			requiredAuditStringField(t, entry, "endpoint", index)
			requiredAuditStringField(t, entry, "method", index)
			if _, ok := entry["status_code"].(float64); !ok {
				t.Fatalf("api_request audit entry %d missing numeric status_code: %#v", index, entry)
			}
			if _, ok := entry["project_id"].(string); !ok {
				t.Fatalf("api_request audit entry %d missing string project_id: %#v", index, entry)
			}
			continue
		}
		requiredAuditStringField(t, entry, "endpoint", index)
		if _, ok := entry["project_id"].(string); !ok {
			t.Fatalf("audit entry %d missing string project_id: %#v", index, entry)
		}
		if _, ok := entry["user_id"].(string); !ok {
			t.Fatalf("audit entry %d missing string user_id: %#v", index, entry)
		}
		if strings.Contains(event, "runner_") {
			if _, ok := entry[auditSubjectRunnerID].(string); !ok {
				t.Fatalf("runner audit entry %d missing runner_id: %#v", index, entry)
			}
		}
		if strings.Contains(event, "agent_") {
			if _, ok := entry[auditSubjectAgentID].(string); !ok {
				t.Fatalf("agent audit entry %d missing agent_id: %#v", index, entry)
			}
		}
	}
}

func requiredAuditStringField(t *testing.T, entry map[string]any, key string, index int) string {
	t.Helper()
	value, ok := entry[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		t.Fatalf("audit entry %d missing non-empty string %s: %#v", index, key, entry)
	}
	return value
}

func expectedAuditOutcome(event string) string {
	return auditOutcomeForEvent(event)
}

func mustReadFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	return string(raw)
}

func assertAuditLogContainsNoRawTokens(t *testing.T, raw []byte, logPath string, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if bytes.Contains(raw, []byte(token)) {
			t.Fatalf("audit log leaked raw token %q in %s: %s", token, logPath, string(raw))
		}
	}
}

func clusterByIDForTest(settings domain.ControlPlaneSettings, id string) domain.ClusterTarget {
	for _, item := range settings.Clusters {
		if item.ID == id {
			return item
		}
	}
	return domain.ClusterTarget{}
}

type failOnceBootstrapSessionStore struct {
	session    domain.BootstrapSession
	saveCount  int
	failOnSave int
}

type failOnceSettingsStore struct {
	settings   domain.ControlPlaneSettings
	saveCount  int
	failOnSave int
}

type failingWriteResponseWriter struct {
	inner http.ResponseWriter
}

func (w *failingWriteResponseWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *failingWriteResponseWriter) WriteHeader(statusCode int) {
	w.inner.WriteHeader(statusCode)
}

func (w *failingWriteResponseWriter) Write(p []byte) (int, error) {
	return 0, errors.New("simulated response write failure")
}

func (s *failOnceBootstrapSessionStore) GetByProject(projectID string) (domain.BootstrapSession, error) {
	if !strings.EqualFold(strings.TrimSpace(s.session.ProjectID), strings.TrimSpace(projectID)) {
		return domain.BootstrapSession{}, store.ErrBootstrapSessionNotFound
	}
	return cloneBootstrapSessionForTest(s.session), nil
}

func (s *failOnceBootstrapSessionStore) Save(session domain.BootstrapSession) error {
	s.saveCount++
	if s.failOnSave > 0 && s.saveCount == s.failOnSave {
		return fmt.Errorf("transient bootstrap session persistence failure")
	}
	s.session = cloneBootstrapSessionForTest(session)
	return nil
}

func (s *failOnceBootstrapSessionStore) ClaimBootstrapToken(request store.BootstrapTokenClaimRequest) (domain.BootstrapSession, error) {
	updated, err := store.ApplyBootstrapTokenClaim(cloneBootstrapSessionForTest(s.session), request)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	if err := s.Save(updated); err != nil {
		return domain.BootstrapSession{}, err
	}
	return cloneBootstrapSessionForTest(s.session), nil
}

func (s *failOnceSettingsStore) Get() (domain.ControlPlaneSettings, error) {
	payload, err := json.Marshal(s.settings)
	if err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	var cloned domain.ControlPlaneSettings
	if err := json.Unmarshal(payload, &cloned); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	return cloned, nil
}

func (s *failOnceSettingsStore) Save(settings domain.ControlPlaneSettings) error {
	s.saveCount++
	if s.failOnSave > 0 && s.saveCount == s.failOnSave {
		return fmt.Errorf("transient settings persistence failure")
	}
	payload, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, &s.settings)
}

func cloneBootstrapSessionForTest(session domain.BootstrapSession) domain.BootstrapSession {
	payload, err := json.Marshal(session)
	if err != nil {
		panic(err)
	}
	var cloned domain.BootstrapSession
	if err := json.Unmarshal(payload, &cloned); err != nil {
		panic(err)
	}
	if cloned.Data == nil {
		cloned.Data = map[string]any{}
	}
	return cloned
}

func createBootstrapAgentTokenForTest(t *testing.T, application *Server, projectID string) domain.AgentRegistrationTokenResponse {
	t.Helper()
	if _, err := application.projects.SaveProject(domain.Project{
		ID:                 projectID,
		Name:               projectID,
		ProductID:          "bethunder",
		AppRepositoryID:    "github.com/acme/app",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), nil)
	createRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create bootstrap session status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	tokenReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/agent-token", projectID), bytes.NewReader([]byte(`{"clusterId":"dev-us"}`)))
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("agent token status=%d body=%s", tokenRec.Code, tokenRec.Body.String())
	}
	var tokenResp domain.AgentRegistrationTokenResponse
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("decode agent token response: %v", err)
	}
	if strings.TrimSpace(tokenResp.RegistrationToken) == "" {
		t.Fatalf("expected non-empty agent registration token")
	}
	return tokenResp
}

func startBootstrapResourceScanForTest(t *testing.T, application *Server, projectID string, namespaces []string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"current_step": 3,
		"status":       "reviewed",
		"step_data": map[string]any{
			"selectedBaseNamespaces": namespaces,
		},
	})
	if err != nil {
		t.Fatalf("marshal namespace selection: %v", err)
	}
	selectReq := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/projects/%s/bootstrap-session", projectID), bytes.NewReader(payload))
	selectReq.Header.Set("Content-Type", "application/json")
	selectRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(selectRec, selectReq)
	if selectRec.Code != http.StatusOK {
		t.Fatalf("select namespaces status=%d body=%s", selectRec.Code, selectRec.Body.String())
	}
	startReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/projects/%s/bootstrap-session/resource-scan/start", projectID), nil)
	startRec := httptest.NewRecorder()
	application.Routes().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("start scan status=%d body=%s", startRec.Code, startRec.Body.String())
	}
}

func newTestServerWithSecrets(t *testing.T, githubSecret string, gitlabSecret string) (*Server, store.EnvironmentStore, *bytes.Buffer) {
	t.Helper()

	tmp := t.TempDir()
	cfg := config.FromEnv()
	cfg.DeploymentBackend = "fluxcd"
	cfg.DataDir = tmp
	cfg.GitOpsDir = tmp
	cfg.GitHubWebhookSecret = githubSecret
	cfg.GitLabWebhookSecret = gitlabSecret
	cfg.WebhookSecret = ""

	envStore, err := store.NewJSONStore(tmp + "/environments.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer := gitops.NewFluxRenderer(cfg.GitOps)
	writer := gitops.NewFileWriter(tmp, false, "", "")
	service := app.NewEnvironmentService(cfg, catalog.Default(), envStore, renderer, writer)
	productStore, err := store.NewJSONProductStore(tmp+"/products.json", catalog.Default().List())
	if err != nil {
		t.Fatalf("product store: %v", err)
	}
	productService := app.NewProductService(productStore)
	service.SetProductProvider(productService)
	projectStore, err := store.NewJSONProjectStore(tmp+"/projects.json", catalog.DefaultProjects())
	if err != nil {
		t.Fatalf("project store: %v", err)
	}
	service.SetProjectStore(projectStore)
	settingsStore, err := store.NewJSONSettingsStore(tmp+"/settings.json", app.DefaultControlPlaneSettings(cfg))
	if err != nil {
		t.Fatalf("settings store: %v", err)
	}
	bootstrapSessionStore, err := store.NewJSONBootstrapSessionStore(tmp + "/bootstrap-sessions.json")
	if err != nil {
		t.Fatalf("bootstrap session store: %v", err)
	}
	projectConfigStore, err := store.NewJSONProjectConfigStore(tmp + "/project-configs.json")
	if err != nil {
		t.Fatalf("project config store: %v", err)
	}
	projectService := app.NewProjectService(projectStore)
	settingsService := app.NewSettingsService(settingsStore)
	bootstrapSessionService := app.NewBootstrapSessionServiceWithEncryptor(bootstrapSessionStore, app.MustNewAESGCMCredentialEncryptor("test-bootstrap-key", "test"))
	projectConfigService := app.NewProjectConfigService(projectConfigStore)
	projectService.SetSettingsProvider(settingsService)
	service.SetSettingsProvider(settingsService)
	jobManager := jobs.NewManager(service, jobs.WithProjectResolver(projectService))

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	return New(Dependencies{Config: cfg, Service: service, Products: productService, Projects: projectService, Settings: settingsService, BootstrapSessions: bootstrapSessionService, ProjectConfigs: projectConfigService, Jobs: jobManager, Logger: logger}), envStore, &logs
}

func githubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type githubPullRequestPayloadOptions struct {
	Action         string
	Number         string
	Branch         string
	InstallationID string
	Labels         []string
	Draft          bool
}

func githubPullRequestPayload(action string, branch string) string {
	return githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action:         action,
		Number:         "1902",
		Branch:         branch,
		InstallationID: "",
		Labels:         nil,
		Draft:          false,
	})
}

func githubPullRequestPayloadWithNumber(action string, number string, branch string) string {
	return githubPullRequestPayloadWithOptions(githubPullRequestPayloadOptions{
		Action: action,
		Number: number,
		Branch: branch,
	})
}

func githubPullRequestPayloadWithOptions(options githubPullRequestPayloadOptions) string {
	number := strings.TrimSpace(options.Number)
	if number == "" {
		number = "1902"
	}
	action := strings.TrimSpace(options.Action)
	branch := strings.TrimSpace(options.Branch)

	draft := "false"
	if options.Draft {
		draft = "true"
	}
	labelsJSON := "[]"
	if options.Labels != nil {
		if encoded, err := json.Marshal(options.Labels); err == nil {
			labelsJSON = string(encoded)
		}
	}

	installationField := ""
	if strings.TrimSpace(options.InstallationID) != "" {
		installationField = `, 
  "installation": {
    "id": ` + strings.TrimSpace(options.InstallationID) + `
  }`
	}

	return `{
  "action": "` + action + `",
  "number": ` + number + `,
  "repository": {
    "full_name": "owner/repo",
    "html_url": "https://github.com/owner/repo"
  },
  "pull_request": {
    "number": ` + number + `,
    "html_url": "https://github.com/owner/repo/pull/` + number + `",
    "head": {
      "ref": "` + branch + `",
      "sha": "abc123",
      "repo": {
        "full_name": "owner/repo",
        "html_url": "https://github.com/owner/repo"
      }
    },
    "user": {
      "login": "octocat"
    },
    "labels": ` + labelsJSON + `,
    "draft": ` + draft + `
  },
  "sender": {
    "login": "octocat"
  }` + installationField + `
}`
}

func githubIssueCommentPayload(number string, body string) string {
	number = strings.TrimSpace(number)
	if number == "" {
		number = "2101"
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		encodedBody = []byte(`""`)
	}
	return `{
  "action": "created",
  "repository": {
    "full_name": "owner/repo",
    "html_url": "https://github.com/owner/repo"
  },
  "issue": {
    "number": ` + number + `,
    "html_url": "https://github.com/owner/repo/issues/` + number + `",
    "pull_request": {
      "url": "https://api.github.com/repos/owner/repo/pulls/` + number + `"
    }
  },
  "comment": {
    "body": ` + string(encodedBody) + `,
    "user": {
      "login": "octocat"
    }
  },
  "sender": {
    "login": "octocat"
  },
  "installation": {
    "id": 98765
  }
}`
}

func gitlabMergeRequestPayload(action string, state string, branch string) string {
	return gitlabMergeRequestPayloadWithIIDAndProjectID(action, state, "2002", branch, "2002")
}

func gitlabNotePayload(iid string, body string) string {
	iid = strings.TrimSpace(iid)
	if iid == "" {
		iid = "2201"
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		encodedBody = []byte(`""`)
	}
	return `{
  "object_kind": "note",
  "user": {
    "name": "Alex",
    "username": "alex"
  },
  "project": {
    "path_with_namespace": "group/repo",
    "id": 123,
    "web_url": "https://gitlab.example/group/repo"
  },
  "merge_request": {
    "iid": ` + iid + `,
    "url": "https://gitlab.example/group/repo/-/merge_requests/` + iid + `"
  },
  "object_attributes": {
    "note": ` + string(encodedBody) + `
  }
}`
}

func gitlabMergeRequestPayloadWithIID(action string, state string, iid string, branch string) string {
	return gitlabMergeRequestPayloadWithIIDAndProjectID(action, state, iid, branch, "2002")
}

func gitlabMergeRequestPayloadWithIIDAndProjectID(action string, state string, iid string, branch string, projectID string) string {
	return gitlabMergeRequestPayloadWithOptions(gitlabMergeRequestPayloadOptions{
		Action:    action,
		State:     state,
		IID:       iid,
		Branch:    branch,
		ProjectID: projectID,
	})
}

type gitlabMergeRequestPayloadOptions struct {
	Action         string
	State          string
	IID            string
	Branch         string
	ProjectID      string
	Labels         []string
	WorkInProgress bool
}

func gitlabMergeRequestPayloadWithOptions(options gitlabMergeRequestPayloadOptions) string {
	action := strings.TrimSpace(options.Action)
	state := strings.TrimSpace(options.State)
	projectID := strings.TrimSpace(options.ProjectID)
	if projectID == "" {
		projectID = "2002"
	}
	branch := strings.TrimSpace(options.Branch)
	iid := strings.TrimSpace(options.IID)
	if iid == "" {
		iid = "2002"
	}
	labelsJSON := "[]"
	if options.Labels != nil {
		if encoded, err := json.Marshal(options.Labels); err == nil {
			labelsJSON = string(encoded)
		}
	}
	draftValue := "false"
	if options.WorkInProgress {
		draftValue = "true"
	}
	return `{
  "object_kind": "merge_request",
  "user": {
    "name": "Alex",
    "username": "alex"
  },
  "project": {
    "path_with_namespace": "owner/repo",
    "id": ` + projectID + `,
    "web_url": "https://gitlab.example/owner/repo"
  },
  "object_attributes": {
    "iid": ` + iid + `,
    "action": "` + action + `",
    "state": "` + state + `",
    "source_branch": "` + branch + `",
    "work_in_progress": ` + draftValue + `,
    "labels": ` + labelsJSON + `,
    "last_commit": {
      "id": "abc123"
    },
    "url": "https://gitlab.example/owner/repo/-/merge_requests/` + iid + `"
  }
}`
}
