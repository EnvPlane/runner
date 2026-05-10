package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"envpilot/internal/domain"
)

type KubernetesManagedResourceClient struct {
	BaseURL       string
	BearerToken   string
	HTTPClient    *http.Client
	Logger        *slog.Logger
	ProjectID     string
	EnvironmentID string
	CleanupSafety CleanupSafetyConfig
}

func (c KubernetesManagedResourceClient) Apply(ctx context.Context, resource domain.ResourceSnapshot) error {
	resource = c.withOwnershipLabels(resource)
	paths, err := kubernetesResourcePaths(resource.Kind, resource.Namespace, resource.Name)
	if err != nil {
		return err
	}
	existing, exists, err := c.getExisting(ctx, paths.resource)
	if err != nil {
		return err
	}
	if exists {
		if err := ValidateModifyManagedResource(existing, c.ProjectID, c.EnvironmentID); err != nil {
			c.warnUnsafeResourceOperation("apply_refused", existing, err)
			return err
		}
		return c.writeJSON(ctx, http.MethodPatch, paths.resource, c.manifestFor(resource), "application/merge-patch+json")
	}
	return c.writeJSON(ctx, http.MethodPost, paths.collection, c.manifestFor(resource), "application/json")
}

func (c KubernetesManagedResourceClient) Delete(ctx context.Context, kind string, namespace string, name string) error {
	paths, err := kubernetesResourcePaths(kind, namespace, name)
	if err != nil {
		return err
	}
	existing, exists, err := c.getExisting(ctx, paths.resource)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	config := c.CleanupSafety
	if len(config.ProtectedNamespaces) == 0 {
		config = DefaultCleanupSafetyConfig()
	}
	if err := ValidateDeleteManagedResource(existing, config, c.ProjectID, c.EnvironmentID); err != nil {
		c.warnUnsafeResourceOperation("delete_refused", existing, err)
		return err
	}
	return c.do(ctx, http.MethodDelete, paths.resource, nil, "")
}

func (c KubernetesManagedResourceClient) withOwnershipLabels(resource domain.ResourceSnapshot) domain.ResourceSnapshot {
	if resource.Labels == nil {
		resource.Labels = map[string]string{}
	}
	resource.Labels[EnvPilotManagedByLabel] = "envpilot"
	resource.Labels[EnvPilotManagedLabel] = "true"
	if strings.TrimSpace(c.ProjectID) != "" {
		resource.Labels[EnvPilotProjectLabel] = strings.TrimSpace(c.ProjectID)
	}
	if strings.TrimSpace(c.EnvironmentID) != "" {
		resource.Labels[EnvPilotEnvironmentIDLabel] = strings.TrimSpace(c.EnvironmentID)
	}
	return resource
}

func (c KubernetesManagedResourceClient) manifestFor(resource domain.ResourceSnapshot) map[string]any {
	manifest := cloneKubernetesManifest(resource.Manifest)
	if manifest == nil {
		manifest = map[string]any{
			"apiVersion": apiVersionForKind(resource.Kind),
			"kind":       strings.TrimSpace(resource.Kind),
		}
	}
	if strings.TrimSpace(resource.Kind) != "" {
		manifest["kind"] = strings.TrimSpace(resource.Kind)
	}
	metadata := ensureMap(manifest, "metadata")
	if strings.TrimSpace(resource.Name) != "" {
		metadata["name"] = strings.TrimSpace(resource.Name)
	}
	if strings.TrimSpace(resource.Namespace) != "" {
		metadata["namespace"] = strings.TrimSpace(resource.Namespace)
	}
	labels := ensureMap(metadata, "labels")
	for key, value := range resource.Labels {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			labels[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return manifest
}

func (c KubernetesManagedResourceClient) getExisting(ctx context.Context, path string) (domain.ResourceSnapshot, bool, error) {
	req, err := c.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return domain.ResourceSnapshot{}, false, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return domain.ResourceSnapshot{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return domain.ResourceSnapshot{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return domain.ResourceSnapshot{}, false, fmt.Errorf("get existing Kubernetes resource failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var manifest map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest); err != nil {
		return domain.ResourceSnapshot{}, false, err
	}
	return resourceSnapshotFromManifest(manifest), true, nil
}

func (c KubernetesManagedResourceClient) writeJSON(ctx context.Context, method string, path string, payload map[string]any, contentType string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, body, contentType)
}

func (c KubernetesManagedResourceClient) do(ctx context.Context, method string, path string, body []byte, contentType string) error {
	req, err := c.request(ctx, method, path, body, contentType)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Kubernetes %s failed: status=%d body=%s", method, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (c KubernetesManagedResourceClient) request(ctx context.Context, method string, path string, body []byte, contentType string) (*http.Request, error) {
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("kubernetes base URL is required")
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token := strings.TrimSpace(c.BearerToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func (c KubernetesManagedResourceClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c KubernetesManagedResourceClient) warnUnsafeResourceOperation(operation string, resource domain.ResourceSnapshot, err error) {
	if c.Logger == nil {
		return
	}
	c.Logger.Warn("refusing Kubernetes resource operation for non-EnvPilot-managed resource",
		"operation", operation,
		"kind", resource.Kind,
		"namespace", resource.Namespace,
		"name", resource.Name,
		"project_id", c.ProjectID,
		"environment_id", c.EnvironmentID,
		"error", err)
}

type kubernetesManagedResourcePaths struct {
	collection string
	resource   string
}

func kubernetesResourcePaths(kind string, namespace string, name string) (kubernetesManagedResourcePaths, error) {
	kind = strings.TrimSpace(kind)
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	if kind == "" || namespace == "" || name == "" {
		return kubernetesManagedResourcePaths{}, fmt.Errorf("kind, namespace, and name are required")
	}
	plural, apiPrefix, ok := kubernetesResourceAPI(kind)
	if !ok {
		return kubernetesManagedResourcePaths{}, fmt.Errorf("unsupported Kubernetes resource kind %q", kind)
	}
	collection := fmt.Sprintf("%s/namespaces/%s/%s", apiPrefix, url.PathEscape(namespace), plural)
	return kubernetesManagedResourcePaths{
		collection: collection,
		resource:   collection + "/" + url.PathEscape(name),
	}, nil
}

func kubernetesResourceAPI(kind string) (plural string, apiPrefix string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "deployment":
		return "deployments", "/apis/apps/v1", true
	case "service":
		return "services", "/api/v1", true
	case "ingress":
		return "ingresses", "/apis/networking.k8s.io/v1", true
	case "configmap":
		return "configmaps", "/api/v1", true
	case "secret":
		return "secrets", "/api/v1", true
	case "networkpolicy":
		return "networkpolicies", "/apis/networking.k8s.io/v1", true
	case "helmrelease":
		return "helmreleases", "/apis/helm.toolkit.fluxcd.io/v2", true
	case "kustomization":
		return "kustomizations", "/apis/kustomize.toolkit.fluxcd.io/v1", true
	case "gitrepository":
		return "gitrepositories", "/apis/source.toolkit.fluxcd.io/v1", true
	default:
		return "", "", false
	}
}

func apiVersionForKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "deployment":
		return "apps/v1"
	case "service", "configmap", "secret":
		return "v1"
	case "ingress", "networkpolicy":
		return "networking.k8s.io/v1"
	case "helmrelease":
		return "helm.toolkit.fluxcd.io/v2"
	case "kustomization":
		return "kustomize.toolkit.fluxcd.io/v1"
	case "gitrepository":
		return "source.toolkit.fluxcd.io/v1"
	default:
		return "v1"
	}
}

func resourceSnapshotFromManifest(manifest map[string]any) domain.ResourceSnapshot {
	metadata, _ := manifest["metadata"].(map[string]any)
	return domain.ResourceSnapshot{
		Kind:      strings.TrimSpace(kubernetesManifestString(manifest["kind"])),
		Namespace: strings.TrimSpace(kubernetesManifestString(metadata["namespace"])),
		Name:      strings.TrimSpace(kubernetesManifestString(metadata["name"])),
		Labels:    stringMap(metadata["labels"]),
		Manifest:  manifest,
	}
}

func cloneKubernetesManifest(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var output map[string]any
	if err := json.Unmarshal(payload, &output); err != nil {
		return nil
	}
	return output
}

func ensureMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	parent[key] = created
	return created
}

func stringMap(value any) map[string]string {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	output := make(map[string]string, len(raw))
	for key, item := range raw {
		output[key] = strings.TrimSpace(kubernetesManifestString(item))
	}
	return output
}

func kubernetesManifestString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}
