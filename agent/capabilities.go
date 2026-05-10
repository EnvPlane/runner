package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"envpilot/internal/domain"
)

type ClusterCapabilities struct {
	KubernetesVersion string
	Capabilities      []string
	Report            domain.ClusterCapabilityReport
}

type CapabilitySource interface {
	DiscoverCapabilities(ctx context.Context) (ClusterCapabilities, error)
}

type versionResponse struct {
	GitVersion string `json:"gitVersion"`
}

func (s *KubernetesNamespaceSource) DiscoverCapabilities(ctx context.Context) (ClusterCapabilities, error) {
	version := s.discoverKubernetesVersion(ctx)
	capabilities := map[string]struct{}{}
	report := domain.ClusterCapabilityReport{
		KubernetesVersion: version,
	}
	for path, capability := range map[string]string{
		"/api/v1":                                 "core-v1",
		"/apis/apps/v1":                           "apps-v1",
		"/apis/networking.k8s.io/v1":              "networking-v1",
		"/apis/batch/v1":                          "batch-v1",
		"/apis/kustomize.toolkit.fluxcd.io/v1":    "flux-kustomize-v1",
		"/apis/helm.toolkit.fluxcd.io/v2":         "flux-helm-v2",
		"/apis/source.toolkit.fluxcd.io/v1":       "flux-source-v1",
		"/apis/external-secrets.io/v1beta1":       "external-secrets-v1beta1",
		"/apis/networking.istio.io/v1beta1":       "istio-networking-v1beta1",
		"/apis/gateway.networking.k8s.io/v1beta1": "gateway-api-v1beta1",
	} {
		available, err := s.apiEndpointAvailable(ctx, path)
		if err != nil {
			return ClusterCapabilities{}, err
		}
		if available {
			capabilities[capability] = struct{}{}
		}
	}
	namespaces, err := s.ListNamespaces(ctx)
	if err != nil {
		report.PermissionWarnings = append(report.PermissionWarnings, fmt.Sprintf("list namespaces failed: %v", err))
	} else {
		items := make([]string, 0, len(namespaces))
		for _, namespace := range namespaces {
			if name := strings.TrimSpace(namespace.Metadata.Name); name != "" {
				items = append(items, name)
			}
		}
		sort.Strings(items)
		report.Namespaces = items
	}
	ingressControllers, err := s.ListIngressControllers(ctx)
	if err != nil {
		report.PermissionWarnings = append(report.PermissionWarnings, fmt.Sprintf("list ingress classes failed: %v", err))
	} else {
		report.IngressControllers = ingressControllers
	}
	crds, err := s.ListCRDNames(ctx)
	if err != nil {
		report.PermissionWarnings = append(report.PermissionWarnings, fmt.Sprintf("list CRDs failed: %v", err))
	} else {
		report.FluxCRDs = filterBySuffix(crds, ".toolkit.fluxcd.io")
		report.CertManagerCRDs = filterBySuffix(crds, ".cert-manager.io")
		report.ExternalDNSPresent = containsValue(crds, "dnsendpoints.externaldns.k8s.io")
	}
	storageClasses, err := s.ListStorageClasses(ctx)
	if err != nil {
		report.PermissionWarnings = append(report.PermissionWarnings, fmt.Sprintf("list storage classes failed: %v", err))
	} else {
		report.StorageClasses = storageClasses
	}
	items := make([]string, 0, len(capabilities))
	for capability := range capabilities {
		items = append(items, capability)
	}
	sort.Strings(items)
	report.CapabilityFlags = append(report.CapabilityFlags, items...)
	return ClusterCapabilities{KubernetesVersion: version, Capabilities: items, Report: report}, nil
}

func (s *KubernetesNamespaceSource) discoverKubernetesVersion(ctx context.Context) string {
	req, err := s.newKubernetesGET(ctx, s.apiURL+"/version")
	if err != nil {
		return ""
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var version versionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&version); err != nil {
		return ""
	}
	return strings.TrimSpace(version.GitVersion)
}

func (s *KubernetesNamespaceSource) apiEndpointAvailable(ctx context.Context, path string) (bool, error) {
	req, err := s.newKubernetesGET(ctx, s.apiURL+path)
	if err != nil {
		return false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil
	}
	return true, nil
}

func filterBySuffix(values []string, suffix string) []string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(value)), strings.ToLower(strings.TrimSpace(suffix))) {
			items = append(items, strings.TrimSpace(value))
		}
	}
	sort.Strings(items)
	return items
}

func containsValue(values []string, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == target {
			return true
		}
	}
	return false
}
