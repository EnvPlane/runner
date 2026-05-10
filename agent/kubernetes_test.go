package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestKubernetesNamespaceSourceListsSelectedNamespaces(t *testing.T) {
	var gotAuth string
	var gotSelector string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSelector = r.URL.Query().Get("labelSelector")
		_ = json.NewEncoder(w).Encode(namespaceList{
			Items: []Namespace{
				{Metadata: NamespaceMetadata{Name: "envpilot-pr-kan-402"}},
				{Metadata: NamespaceMetadata{Name: "envpilot-pr-kan-999"}},
			},
		})
	}))
	defer server.Close()

	source := NewKubernetesNamespaceSource(server.URL, "kube-token", "app.kubernetes.io/managed-by=envpilot", []string{"envpilot-pr-kan-402"}, server.Client())
	items, err := source.ListNamespaces(context.Background())
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}

	if gotAuth != "Bearer kube-token" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if gotSelector != "app.kubernetes.io/managed-by=envpilot" {
		t.Fatalf("label selector = %q", gotSelector)
	}
	if len(items) != 1 || items[0].Metadata.Name != "envpilot-pr-kan-402" {
		t.Fatalf("unexpected namespaces: %#v", items)
	}
}

func TestKubernetesNamespaceSourceListsDeploymentsPodsAndIngresses(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/envpilot-pr-kan-403/deployments":
			_ = json.NewEncoder(w).Encode(deploymentList{
				Items: []Deployment{{Metadata: DeploymentMetadata{Name: "cms-api"}}},
			})
		case "/api/v1/namespaces/envpilot-pr-kan-403/pods":
			_ = json.NewEncoder(w).Encode(podList{
				Items: []Pod{{Metadata: PodMetadata{Name: "cms-api-abc"}}},
			})
		case "/apis/networking.k8s.io/v1/namespaces/envpilot-pr-kan-403/ingresses":
			_ = json.NewEncoder(w).Encode(ingressList{
				Items: []Ingress{{
					Metadata: IngressMetadata{Name: "preview"},
					Spec:     IngressSpec{Rules: []IngressRule{{Host: "kan-403.preview.local"}}},
					Status:   IngressStatus{LoadBalancer: IngressLoadBalancerStatus{Ingress: []LoadBalancerIngress{{IP: "10.0.0.15"}}}},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	source := NewKubernetesNamespaceSource(server.URL, "kube-token", "", nil, server.Client())
	deployments, err := source.ListDeployments(context.Background(), "envpilot-pr-kan-403")
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	pods, err := source.ListPods(context.Background(), "envpilot-pr-kan-403")
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	ingresses, err := source.ListIngresses(context.Background(), "envpilot-pr-kan-403")
	if err != nil {
		t.Fatalf("list ingresses: %v", err)
	}

	if len(deployments) != 1 || deployments[0].Metadata.Name != "cms-api" {
		t.Fatalf("unexpected deployments: %#v", deployments)
	}
	if len(pods) != 1 || pods[0].Metadata.Name != "cms-api-abc" {
		t.Fatalf("unexpected pods: %#v", pods)
	}
	if len(ingresses) != 1 || ingresses[0].Metadata.Name != "preview" {
		t.Fatalf("unexpected ingresses: %#v", ingresses)
	}
	if len(paths) != 3 {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestKubernetesNamespaceSourceListsEvents(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(eventList{
			Items: []KubernetesEvent{
				{
					Metadata: EventMetadata{Name: "event-1", Namespace: "envpilot-pr-kan-404"},
					Type:     "Warning",
					Reason:   "FailedScheduling",
					Message:  "0/3 nodes are available",
				},
			},
		})
	}))
	defer server.Close()

	source := NewKubernetesNamespaceSource(server.URL, "kube-token", "", nil, server.Client())
	events, err := source.ListEvents(context.Background(), "envpilot-pr-kan-404")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if gotPath != "/api/v1/namespaces/envpilot-pr-kan-404/events" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(events) != 1 || events[0].Reason != "FailedScheduling" {
		t.Fatalf("events = %#v", events)
	}
}

func TestKubernetesNamespaceSourceListsFluxResources(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations":
			_ = json.NewEncoder(w).Encode(fluxKustomizationList{
				Items: []FluxKustomization{{Metadata: FluxMetadata{Name: "kan-405.bethunder", Namespace: "flux-system"}}},
			})
		case "/apis/helm.toolkit.fluxcd.io/v2/namespaces/envpilot-pr-kan-405/helmreleases":
			_ = json.NewEncoder(w).Encode(helmReleaseList{
				Items: []HelmRelease{{Metadata: FluxMetadata{Name: "nginx", Namespace: "envpilot-pr-kan-405"}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	source := NewKubernetesNamespaceSource(server.URL, "kube-token", "", nil, server.Client())
	kustomizations, err := source.ListFluxKustomizations(context.Background(), "flux-system")
	if err != nil {
		t.Fatalf("list flux kustomizations: %v", err)
	}
	helmReleases, err := source.ListHelmReleases(context.Background(), "envpilot-pr-kan-405")
	if err != nil {
		t.Fatalf("list helm releases: %v", err)
	}

	if len(kustomizations) != 1 || kustomizations[0].Metadata.Name != "kan-405.bethunder" {
		t.Fatalf("kustomizations = %#v", kustomizations)
	}
	if len(helmReleases) != 1 || helmReleases[0].Metadata.Name != "nginx" {
		t.Fatalf("helm releases = %#v", helmReleases)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestKubernetesNamespaceSourceDiscoversCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.30.1"})
		case "/api/v1", "/apis/apps/v1", "/apis/kustomize.toolkit.fluxcd.io/v1", "/apis/helm.toolkit.fluxcd.io/v2":
			_ = json.NewEncoder(w).Encode(map[string]string{"kind": "APIResourceList"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	source := NewKubernetesNamespaceSource(server.URL, "kube-token", "", nil, server.Client())
	capabilities, err := source.DiscoverCapabilities(context.Background())
	if err != nil {
		t.Fatalf("discover capabilities: %v", err)
	}

	expected := []string{"apps-v1", "core-v1", "flux-helm-v2", "flux-kustomize-v1"}
	if capabilities.KubernetesVersion != "v1.30.1" {
		t.Fatalf("version = %q", capabilities.KubernetesVersion)
	}
	if !reflect.DeepEqual(capabilities.Capabilities, expected) {
		t.Fatalf("capabilities = %#v", capabilities.Capabilities)
	}
}
