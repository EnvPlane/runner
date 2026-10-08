package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/envplane/contracts/domain"
)

type storageCaptureBackend struct {
	fakeRunnerCommandBackend
	deleted         bool
	captureObserved *atomic.Bool
	t               *testing.T
}

func (b *storageCaptureBackend) Delete(_ context.Context, _ domain.Environment, _ domain.ProjectConfig) error {
	if b.captureObserved != nil && !b.captureObserved.Load() {
		b.t.Error("delete ran before capture")
	}
	b.deleted = true
	return nil
}

type storageCaptureFluxExecutor struct {
	fakeFluxNamespaceCleanupExecutor
	observed *atomic.Bool
	t        *testing.T
}

func (f *storageCaptureFluxExecutor) DeleteNamespace(ctx context.Context, namespace string) error {
	if !f.observed.Load() {
		f.t.Error("namespace removed before capture")
	}
	return f.fakeFluxNamespaceCleanupExecutor.DeleteNamespace(ctx, namespace)
}

func storageCaptureFixture() (runnerConfig, domain.RunnerCommand) {
	cfg := runnerConfig{ProjectID: "project", ClusterID: "cluster", RunnerID: "runtime-runner", RunnerAuthToken: "opaque-runtime-test-token"}
	command := domain.RunnerCommand{ID: "command", ProjectID: cfg.ProjectID, ClusterID: cfg.ClusterID, RunnerID: cfg.RunnerID, Operation: "delete", Environment: domain.Environment{ID: "feature", Project: cfg.ProjectID, TenantID: "tenant", Namespace: "envplane-pr-feature"}}
	return cfg, command
}

func TestRunnerStorageCapturePrecedesEveryDeleteAndDoesNotBlockFailure(t *testing.T) {
	for _, operation := range []string{"delete", "force_cleanup", "cleanup_flux_namespace"} {
		for _, status := range []int{200, 202, 204, 404, 403, 503} {
			t.Run(operation+"/"+http.StatusText(status), func(t *testing.T) {
				var observed atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runner/storage-cleanup/capture" {
						t.Error("wrong capture route")
					}
					if r.Header.Get("Authorization") != "Bearer opaque-runtime-test-token" || r.Header.Get("X-envplane-Tenant") != "tenant" || r.Header.Get(runnerCommandAPIVersionHeader) != runnerCommandAPIVersion {
						t.Error("missing runtime/scoped headers")
					}
					var body map[string]string
					if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 5 || body["projectId"] != "project" || body["clusterId"] != "cluster" || body["runnerId"] != "runtime-runner" || body["commandId"] != "command" || body["environmentId"] != "feature" {
						t.Error("capture body must contain five identity fields only")
					}
					observed.Store(true)
					w.WriteHeader(status)
					if status == http.StatusOK {
						_, _ = io.WriteString(w, `{"snapshotId":"metadata-only","commandId":"command"}`)
					}
				}))
				defer server.Close()
				cfg, command := storageCaptureFixture()
				cfg.ControlPlaneURL = server.URL
				command.Operation = operation
				backend := &storageCaptureBackend{captureObserved: &observed, t: t}
				if operation != "cleanup_flux_namespace" {
					command = runnerCommandWithReleasePlan(command)
				} else {
					previous := newFluxNamespaceCleanupExecutor
					executor := &storageCaptureFluxExecutor{fakeFluxNamespaceCleanupExecutor: fakeFluxNamespaceCleanupExecutor{managed: true, absent: true}, observed: &observed, t: t}
					newFluxNamespaceCleanupExecutor = func() fluxNamespaceCleanupExecutor { return executor }
					t.Cleanup(func() { newFluxNamespaceCleanupExecutor = previous })
				}
				result := executeRunnerCommandWithNamespaceGuard(context.Background(), cfg, command, backend, func(string) bool { return true })
				if result.Status != "succeeded" || !result.CleanupVerified || !observed.Load() {
					t.Fatal("capture failure changed default cleanup semantics")
				}
				if operation != "cleanup_flux_namespace" && !backend.deleted {
					t.Fatal("backend delete not called")
				}
			})
		}
	}
}

func TestRunnerStorageCaptureHonorsValidationAndNamespaceOwnershipGuards(t *testing.T) {
	for _, mode := range []string{"invalid release plan", "invalid inventory", "denied namespace", "unmanaged Flux namespace"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(204) }))
			defer server.Close()
			cfg, command := storageCaptureFixture()
			cfg.ControlPlaneURL = server.URL
			if mode != "invalid release plan" && mode != "unmanaged Flux namespace" {
				command = runnerCommandWithReleasePlan(command)
			}
			if mode == "invalid inventory" {
				command.CleanupInventory = []domain.ReleasePlanInventoryItem{{Kind: "PersistentVolumeClaim", Namespace: "foreign", Name: "data", Owned: true}}
			}
			if mode == "unmanaged Flux namespace" {
				command.Operation = "cleanup_flux_namespace"
				previous := newFluxNamespaceCleanupExecutor
				newFluxNamespaceCleanupExecutor = func() fluxNamespaceCleanupExecutor { return &fakeFluxNamespaceCleanupExecutor{} }
				t.Cleanup(func() { newFluxNamespaceCleanupExecutor = previous })
			}
			backend := &storageCaptureBackend{t: t}
			result := executeRunnerCommandWithNamespaceGuard(context.Background(), cfg, command, backend, func(string) bool { return mode != "denied namespace" })
			if result.Status != "failed" || calls.Load() != 0 || backend.deleted {
				t.Fatal("capture/deletion preceded authorization checks")
			}
		})
	}
}

func TestRunnerStorageCaptureCannotRedirectCredentialsOrExceedResponseBound(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { leaked.Store(true); w.WriteHeader(204) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	cfg, command := storageCaptureFixture()
	cfg.ControlPlaneURL = server.URL
	if code := captureRunnerStorageCleanup(context.Background(), cfg, command); code != "capture_not_acknowledged" || leaked.Load() {
		t.Fatal("capture redirect followed")
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > runnerStorageCaptureTimeout {
			t.Error("capture is not time-bounded")
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", runnerStorageCaptureResponseLimit+100)))}, nil
	})}
	if code := captureRunnerStorageCleanupWithClient(context.Background(), cfg, command, client); code != "capture_response_too_large" {
		t.Fatal("unbounded response accepted")
	}
}

func TestRunnerStorageCaptureSkipsMissingIdentityAndDisallowedEndpoints(t *testing.T) {
	for _, mode := range []string{"empty config", "missing command", "missing token", "foreign identity", "remote HTTP", "URL credential", "query", "fragment", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			cfg, command := storageCaptureFixture()
			cfg.ControlPlaneURL = "http://control-plane.invalid"
			ctx := context.Background()
			switch mode {
			case "empty config":
				cfg = runnerConfig{}
			case "missing command":
				command.ID = ""
			case "missing token":
				cfg.RunnerAuthToken = ""
			case "foreign identity":
				command.ProjectID = "other"
			case "remote HTTP":
				cfg.ControlPlaneEndpointMode = "remote"
			case "URL credential":
				cfg.ControlPlaneURL = "http://user:secret@control-plane.invalid"
			case "query":
				cfg.ControlPlaneURL += "?untrusted=true"
			case "fragment":
				cfg.ControlPlaneURL += "#untrusted"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unsafe capture made request")
				return nil, context.Canceled
			})}
			if code := captureRunnerStorageCleanupWithClient(ctx, cfg, command, client); code == "" {
				t.Fatal("capture skipped without diagnostic")
			}
		})
	}
}

func TestRunnerStorageCaptureCancellationDoesNotCancelCleanupParent(t *testing.T) {
	cfg, command := storageCaptureFixture()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	cfg.ControlPlaneURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	backend := &storageCaptureBackend{t: t}
	command = runnerCommandWithReleasePlan(command)
	result := executeRunnerCommandWithNamespaceGuard(ctx, cfg, command, backend, func(string) bool { return true })
	if !backend.deleted || result.Status != "succeeded" {
		t.Fatal("capture cancellation blocked backend invocation")
	}
	parent := context.Background()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })}
	if code := captureRunnerStorageCleanupWithClient(parent, cfg, command, client); code == "" || parent.Err() != nil {
		t.Fatal("capture failure canceled parent")
	}
}

func TestRunnerStorageCaptureFailureIsOneSanitizedWarningAndNoResultMutation(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	cfg, command := storageCaptureFixture()
	command = runnerCommandWithReleasePlan(command)
	baseline := executeRunnerCommandWithNamespaceGuard(context.Background(), cfg, command, &storageCaptureBackend{t: t}, func(string) bool { return true })
	logs.Reset()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "sensitive-response-credential-and-pv-path")
	}))
	defer server.Close()
	cfg.ControlPlaneURL = server.URL
	result := executeRunnerCommandWithNamespaceGuard(context.Background(), cfg, command, &storageCaptureBackend{t: t}, func(string) bool { return true })
	if !reflect.DeepEqual(result, baseline) {
		t.Fatal("capture mutated cleanup result/proof semantics")
	}
	if strings.Count(logs.String(), "runner storage cleanup capture unavailable") != 1 {
		t.Fatal("capture diagnostic not bounded to one warning")
	}
	for _, forbidden := range []string{cfg.RunnerAuthToken, server.URL, "sensitive-response-credential-and-pv-path"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatal("capture logged credentials, URL or response body")
		}
	}
}
