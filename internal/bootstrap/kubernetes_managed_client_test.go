package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/envpilot/contracts/domain"
)

func TestKubernetesManagedResourceClientBlocksUnlabeledApplyUpdateAndDeleteForAllRunnerKinds(t *testing.T) {
	for _, kind := range runnerManagedResourceKinds() {
		t.Run(kind, func(t *testing.T) {
			paths, err := kubernetesResourcePaths(kind, "envpilot-pr-123", "manual")
			if err != nil {
				t.Fatal(err)
			}
			patchCalled := false
			deleteCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != paths.resource {
					http.NotFound(w, r)
					return
				}
				switch r.Method {
				case http.MethodGet:
					writeKubernetesManagedClientTestManifest(t, w, kind, "manual", nil)
				case http.MethodPatch:
					patchCalled = true
					w.WriteHeader(http.StatusOK)
				case http.MethodDelete:
					deleteCalled = true
					w.WriteHeader(http.StatusOK)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client := kubernetesManagedClientForTest(server)
			if err := client.Apply(context.Background(), resourceSnapshot(kind, "manual", envPilotLabels())); !errors.Is(err, ErrResourceNotEnvPlaneManaged) {
				t.Fatalf("unlabeled existing %s update should be rejected, got %v", kind, err)
			}
			if patchCalled {
				t.Fatalf("unlabeled existing %s was patched", kind)
			}

			if err := client.Delete(context.Background(), kind, "envpilot-pr-123", "manual"); !errors.Is(err, ErrResourceNotEnvPlaneManaged) {
				t.Fatalf("unlabeled existing %s delete should be rejected, got %v", kind, err)
			}
			if deleteCalled {
				t.Fatalf("unlabeled existing %s was deleted", kind)
			}
		})
	}
}

func TestKubernetesManagedResourceClientAllowsEnvPlaneLabeledApplyUpdateAndDeleteForAllRunnerKinds(t *testing.T) {
	for _, kind := range runnerManagedResourceKinds() {
		t.Run(kind, func(t *testing.T) {
			paths, err := kubernetesResourcePaths(kind, "envpilot-pr-123", "orders")
			if err != nil {
				t.Fatal(err)
			}
			patchCalled := false
			deleteCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != paths.resource {
					http.NotFound(w, r)
					return
				}
				switch r.Method {
				case http.MethodGet:
					writeKubernetesManagedClientTestManifest(t, w, kind, "orders", envPilotLabels())
				case http.MethodPatch:
					patchCalled = true
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatalf("decode patch payload: %v", err)
					}
					assertKubernetesManagedClientOwnershipLabels(t, payload)
					w.WriteHeader(http.StatusOK)
				case http.MethodDelete:
					deleteCalled = true
					w.WriteHeader(http.StatusOK)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client := kubernetesManagedClientForTest(server)
			if err := client.Apply(context.Background(), resourceSnapshot(kind, "orders", envPilotLabels())); err != nil {
				t.Fatalf("EnvPlane-labeled existing %s update should be allowed: %v", kind, err)
			}
			if !patchCalled {
				t.Fatalf("EnvPlane-labeled existing %s was not patched", kind)
			}

			if err := client.Delete(context.Background(), kind, "envpilot-pr-123", "orders"); err != nil {
				t.Fatalf("EnvPlane-labeled existing %s delete should be allowed: %v", kind, err)
			}
			if !deleteCalled {
				t.Fatalf("EnvPlane-labeled existing %s was not deleted", kind)
			}
		})
	}
}

func TestKubernetesManagedResourceClientCreatesNewResourcesWithEnvPlaneOwnershipLabelsForAllRunnerKinds(t *testing.T) {
	for _, kind := range runnerManagedResourceKinds() {
		t.Run(kind, func(t *testing.T) {
			paths, err := kubernetesResourcePaths(kind, "envpilot-pr-123", "orders")
			if err != nil {
				t.Fatal(err)
			}
			postCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == paths.resource:
					http.NotFound(w, r)
				case r.Method == http.MethodPost && r.URL.Path == paths.collection:
					postCalled = true
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatalf("decode post payload: %v", err)
					}
					assertKubernetesManagedClientOwnershipLabels(t, payload)
					w.WriteHeader(http.StatusCreated)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client := kubernetesManagedClientForTest(server)
			if err := client.Apply(context.Background(), resourceSnapshot(kind, "orders", nil)); err != nil {
				t.Fatalf("new %s create should be allowed with EnvPlane labels applied: %v", kind, err)
			}
			if !postCalled {
				t.Fatalf("new %s was not posted", kind)
			}
		})
	}
}

func TestKubernetesManagedResourceClientDeleteAlwaysRequiresOwnershipLabelsEvenWithUnsafeCleanupConfig(t *testing.T) {
	paths, err := kubernetesResourcePaths("Secret", "envpilot-pr-123", "manual")
	if err != nil {
		t.Fatal(err)
	}
	deleteCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != paths.resource {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeKubernetesManagedClientTestManifest(t, w, "Secret", "manual", nil)
		case http.MethodDelete:
			deleteCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := kubernetesManagedClientForTest(server)
	client.CleanupSafety = CleanupSafetyConfig{
		ProtectedNamespaces:       []string{"default"},
		DeleteEnvPlaneLabeledOnly: false,
	}
	if err := client.Delete(context.Background(), "Secret", "envpilot-pr-123", "manual"); !errors.Is(err, ErrResourceNotEnvPlaneManaged) {
		t.Fatalf("unlabeled existing Secret delete should be rejected regardless of cleanup config, got %v", err)
	}
	if deleteCalled {
		t.Fatalf("unlabeled existing Secret was deleted with unsafe cleanup config")
	}
}

func runnerManagedResourceKinds() []string {
	return []string{
		"Deployment",
		"Service",
		"Ingress",
		"ConfigMap",
		"Secret",
		"NetworkPolicy",
		"HelmRelease",
		"Kustomization",
		"GitRepository",
	}
}

func kubernetesManagedClientForTest(server *httptest.Server) KubernetesManagedResourceClient {
	return KubernetesManagedResourceClient{
		BaseURL:       server.URL,
		HTTPClient:    server.Client(),
		ProjectID:     "checkout",
		EnvironmentID: "pr-123",
	}
}

func writeKubernetesManagedClientTestManifest(t *testing.T, w http.ResponseWriter, kind string, name string, labels map[string]string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	payload := domain.ResourceSnapshot{
		Kind:      kind,
		Namespace: "envpilot-pr-123",
		Name:      name,
		Labels:    labels,
	}
	if err := json.NewEncoder(w).Encode(KubernetesManagedResourceClient{}.manifestFor(payload)); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
}

func assertKubernetesManagedClientOwnershipLabels(t *testing.T, manifest map[string]any) {
	t.Helper()
	metadata, ok := manifest["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing from manifest: %+v", manifest)
	}
	rawLabels, ok := metadata["labels"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.labels missing from manifest: %+v", manifest)
	}
	labels := stringMap(rawLabels)
	expected := envPilotLabels()
	for key, value := range expected {
		if labels[key] != value {
			t.Fatalf("label %s = %q, want %q in %+v", key, labels[key], value, labels)
		}
	}
}
