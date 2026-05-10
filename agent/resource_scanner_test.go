package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"envpilot/internal/domain"
)

func TestResourceDiscoveryScannerFluxSourceMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/dev-base/deployments":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{
					map[string]any{
						"metadata": map[string]any{
							"name":      "orders",
							"namespace": "dev-base",
							"labels": map[string]string{
								"helm.toolkit.fluxcd.io/name":      "orders-release",
								"helm.toolkit.fluxcd.io/namespace": "dev-base",
							},
						},
					},
					map[string]any{
						"metadata": map[string]any{
							"name":      "unmapped",
							"namespace": "dev-base",
						},
					},
				},
			})
			return
		case "/apis/helm.toolkit.fluxcd.io/v2/namespaces/dev-base/helmreleases":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{
					map[string]any{
						"metadata": map[string]any{
							"name":      "orders-release",
							"namespace": "dev-base",
						},
						"spec": map[string]any{
							"chart": map[string]any{
								"spec": map[string]any{
									"sourceRef": map[string]any{
										"kind":      "GitRepository",
										"name":      "app-config",
										"namespace": "flux-system",
									},
								},
							},
						},
					},
				},
			})
			return
		case "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{
					map[string]any{
						"metadata": map[string]any{
							"name":      "platform-kustomization",
							"namespace": "flux-system",
						},
						"spec": map[string]any{
							"sourceRef": map[string]any{
								"kind":      "GitRepository",
								"name":      "missing-repo",
								"namespace": "flux-system",
							},
						},
					},
				},
			})
			return
		case "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{
					map[string]any{
						"metadata": map[string]any{
							"name":      "app-config",
							"namespace": "flux-system",
						},
					},
				},
			})
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
			return
		}
	}))
	defer server.Close()

	source := NewKubernetesNamespaceSource(server.URL, "token", "", []string{"dev-base"}, server.Client())
	scanner := NewResourceDiscoveryScanner(source)
	result, err := scanner.Scan(context.Background(), []string{"dev-base"})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	byKindName := make(map[string]domain.ResourceSnapshot)
	for _, snapshot := range result.Snapshots {
		byKindName[snapshot.Kind+"/"+snapshot.Namespace+"/"+snapshot.Name] = snapshot
	}

	helmKey := "HelmRelease/dev-base/orders-release"
	if _, ok := byKindName[helmKey]; !ok {
		t.Fatalf("helm release snapshot missing")
	}
	kustomizationKey := "Kustomization/flux-system/platform-kustomization"
	kustomizationSnapshot, ok := byKindName[kustomizationKey]
	if !ok {
		t.Fatalf("kustomization snapshot missing")
	}
	if kustomizationSnapshot.SourceMapping == nil || kustomizationSnapshot.SourceMapping.Status != "unresolved" {
		t.Fatalf("kustomization source mapping = %#v", kustomizationSnapshot.SourceMapping)
	}

	deploymentKey := "Deployment/dev-base/orders"
	deploymentSnapshot, ok := byKindName[deploymentKey]
	if !ok {
		t.Fatalf("mapped deployment snapshot missing")
	}
	if deploymentSnapshot.SourceMapping == nil || deploymentSnapshot.SourceMapping.Status != "resolved" {
		t.Fatalf("deployment source mapping = %#v", deploymentSnapshot.SourceMapping)
	}
	if deploymentSnapshot.SourceMapping.Kind != "HelmRelease" || deploymentSnapshot.SourceMapping.Name != "orders-release" {
		t.Fatalf("deployment source mapping details = %#v", deploymentSnapshot.SourceMapping)
	}
	if deploymentSnapshot.SourceMapping.GitRepositoryName != "app-config" {
		t.Fatalf("deployment git source mapping = %#v", deploymentSnapshot.SourceMapping)
	}

	unresolvedKey := "Deployment/dev-base/unmapped"
	unresolvedSnapshot, ok := byKindName[unresolvedKey]
	if !ok {
		t.Fatalf("unmapped deployment snapshot missing")
	}
	if unresolvedSnapshot.SourceMapping == nil || unresolvedSnapshot.SourceMapping.Status != "unresolved" {
		t.Fatalf("unmapped deployment source mapping = %#v", unresolvedSnapshot.SourceMapping)
	}
}
