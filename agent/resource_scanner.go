package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"envpilot/internal/domain"
)

type ResourceDiscoveryScanner struct {
	source *KubernetesNamespaceSource
}

type ResourceScanResult struct {
	Snapshots          []domain.ResourceSnapshot
	ServiceGraph       domain.ServiceGraph
	ServiceEnvs        domain.ServiceEnvironmentVariables
	PermissionWarnings []string
}

func NewResourceDiscoveryScanner(source *KubernetesNamespaceSource) *ResourceDiscoveryScanner {
	return &ResourceDiscoveryScanner{source: source}
}

func (s *ResourceDiscoveryScanner) Scan(ctx context.Context, namespaces []string) (ResourceScanResult, error) {
	if s == nil || s.source == nil {
		return ResourceScanResult{}, fmt.Errorf("resource scanner source is required")
	}
	normalizedNamespaces := normalizeNamespaces(namespaces)
	items := make([]domain.ResourceSnapshot, 0)
	warnings := make([]string, 0)

	fluxKustomizations := make([]fluxResource, 0)
	helmReleases := make([]fluxResource, 0)
	gitRepositories := make([]fluxResource, 0)

	for _, namespace := range normalizedNamespaces {
		namespaceSnapshot, warning, err := s.getNamespaceResource(ctx, namespace)
		if err != nil {
			return ResourceScanResult{}, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		if namespaceSnapshot.Name != "" {
			items = append(items, namespaceSnapshot)
		}
		for _, target := range []struct {
			kind     string
			endpoint string
		}{
			{kind: "Deployment", endpoint: "/apis/apps/v1/namespaces/%s/deployments"},
			{kind: "StatefulSet", endpoint: "/apis/apps/v1/namespaces/%s/statefulsets"},
			{kind: "Service", endpoint: "/api/v1/namespaces/%s/services"},
			{kind: "Ingress", endpoint: "/apis/networking.k8s.io/v1/namespaces/%s/ingresses"},
			{kind: "ConfigMap", endpoint: "/api/v1/namespaces/%s/configmaps"},
			{kind: "ResourceQuota", endpoint: "/api/v1/namespaces/%s/resourcequotas"},
			{kind: "LimitRange", endpoint: "/api/v1/namespaces/%s/limitranges"},
			{kind: "Secret", endpoint: "/api/v1/namespaces/%s/secrets"},
			{kind: "PersistentVolumeClaim", endpoint: "/api/v1/namespaces/%s/persistentvolumeclaims"},
			{kind: "Job", endpoint: "/apis/batch/v1/namespaces/%s/jobs"},
			{kind: "CronJob", endpoint: "/apis/batch/v1/namespaces/%s/cronjobs"},
			{kind: "ServiceAccount", endpoint: "/api/v1/namespaces/%s/serviceaccounts"},
		} {
			snapshots, warning, err := s.listNamespaceResources(ctx, namespace, target.kind, fmt.Sprintf(target.endpoint, url.PathEscape(namespace)))
			if err != nil {
				return ResourceScanResult{}, err
			}
			if warning != "" {
				warnings = append(warnings, warning)
			}
			items = append(items, snapshots...)
		}
	}

	fluxNamespaces := normalizeNamespaces(append(append([]string(nil), normalizedNamespaces...), s.source.FluxNamespace()))
	for _, namespace := range fluxNamespaces {
		releases, warning, err := s.listHelmReleases(ctx, namespace)
		if err != nil {
			return ResourceScanResult{}, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		helmReleases = append(helmReleases, releases...)

		kustomizations, warning, err := s.listFluxKustomizations(ctx, namespace)
		if err != nil {
			return ResourceScanResult{}, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		fluxKustomizations = append(fluxKustomizations, kustomizations...)

		gitRepos, warning, err := s.listGitRepositories(ctx, namespace)
		if err != nil {
			return ResourceScanResult{}, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		gitRepositories = append(gitRepositories, gitRepos...)
	}

	helmByKey := make(map[string]fluxResource, len(helmReleases))
	for _, item := range helmReleases {
		helmByKey[fluxKey(item.Namespace, item.Name)] = item
	}
	kustomizationByKey := make(map[string]fluxResource, len(fluxKustomizations))
	for _, item := range fluxKustomizations {
		kustomizationByKey[fluxKey(item.Namespace, item.Name)] = item
	}
	gitRepositoryByKey := make(map[string]fluxResource, len(gitRepositories))
	for _, item := range gitRepositories {
		gitRepositoryByKey[fluxKey(item.Namespace, item.Name)] = item
	}

	for _, item := range fluxKustomizations {
		source := sourceMappingFromFluxSourceRef(item.SourceRef, item.Namespace, gitRepositoryByKey)
		items = append(items, domain.ResourceSnapshot{
			Kind:          "Kustomization",
			Namespace:     item.Namespace,
			Name:          item.Name,
			Labels:        item.Labels,
			Annotations:   item.Annotations,
			SourceMapping: source,
		})
	}
	for _, item := range helmReleases {
		source := sourceMappingFromFluxSourceRef(item.SourceRef, item.Namespace, gitRepositoryByKey)
		items = append(items, domain.ResourceSnapshot{
			Kind:          "HelmRelease",
			Namespace:     item.Namespace,
			Name:          item.Name,
			Labels:        item.Labels,
			Annotations:   item.Annotations,
			SourceMapping: source,
		})
	}
	for _, item := range gitRepositories {
		items = append(items, domain.ResourceSnapshot{
			Kind:        "GitRepository",
			Namespace:   item.Namespace,
			Name:        item.Name,
			Labels:      item.Labels,
			Annotations: item.Annotations,
		})
	}
	for index := range items {
		if isFluxSourceKind(items[index].Kind) {
			continue
		}
		items[index].SourceMapping = resolveWorkloadSource(items[index], helmByKey, kustomizationByKey, gitRepositoryByKey)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
	sort.Strings(warnings)
	graph := BuildServiceGraph(items)
	return ResourceScanResult{
		Snapshots:          items,
		ServiceGraph:       graph,
		ServiceEnvs:        BuildServiceEnvironmentVariables(items, graph),
		PermissionWarnings: deduplicateStrings(warnings),
	}, nil
}

func (s *ResourceDiscoveryScanner) getNamespaceResource(ctx context.Context, namespace string) (domain.ResourceSnapshot, string, error) {
	endpoint := s.source.apiURL + fmt.Sprintf("/api/v1/namespaces/%s", url.PathEscape(namespace))
	req, err := s.source.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return domain.ResourceSnapshot{}, "", err
	}
	resp, err := s.source.client.Do(req)
	if err != nil {
		return domain.ResourceSnapshot{}, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return domain.ResourceSnapshot{}, fmt.Sprintf("%s Namespace: forbidden (%s)", namespace, strings.TrimSpace(string(body))), nil
	case http.StatusNotFound:
		return domain.ResourceSnapshot{}, "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return domain.ResourceSnapshot{}, "", fmt.Errorf("get Namespace failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return domain.ResourceSnapshot{}, "", fmt.Errorf("read Namespace response: %w", err)
	}
	var payload struct {
		Metadata struct {
			Name        string            `json:"name"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return domain.ResourceSnapshot{}, "", fmt.Errorf("decode Namespace: %w", err)
	}
	name := strings.TrimSpace(payload.Metadata.Name)
	if name == "" {
		return domain.ResourceSnapshot{}, "", nil
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return domain.ResourceSnapshot{}, "", fmt.Errorf("decode Namespace raw: %w", err)
	}
	return domain.ResourceSnapshot{
		Kind:        "Namespace",
		Namespace:   name,
		Name:        name,
		Labels:      payload.Metadata.Labels,
		Annotations: payload.Metadata.Annotations,
		Manifest:    sanitizeResourceManifest("Namespace", raw, name, name),
	}, "", nil
}

func (s *ResourceDiscoveryScanner) listNamespaceResources(ctx context.Context, namespace string, kind string, endpointTemplate string) ([]domain.ResourceSnapshot, string, error) {
	endpoint := s.source.apiURL + endpointTemplate
	req, err := s.source.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.source.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Sprintf("%s %s: forbidden (%s)", namespace, kind, strings.TrimSpace(string(body))), nil
	case http.StatusNotFound:
		return nil, "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("list %s failed: namespace=%s status=%d body=%s", kind, namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read %s list: %w", kind, err)
	}
	var payload struct {
		Items []struct {
			Metadata struct {
				Name            string                          `json:"name"`
				Namespace       string                          `json:"namespace"`
				Labels          map[string]string               `json:"labels"`
				Annotations     map[string]string               `json:"annotations"`
				OwnerReferences []domain.ResourceOwnerReference `json:"ownerReferences"`
			} `json:"metadata"`
			Data       map[string]any `json:"data"`
			BinaryData map[string]any `json:"binaryData"`
			Spec       struct {
				Selector map[string]string `json:"selector"`
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
					Spec struct {
						Containers []struct {
							Name string `json:"name"`
							Env  []struct {
								Name      string         `json:"name"`
								Value     string         `json:"value"`
								ValueFrom map[string]any `json:"valueFrom"`
							} `json:"env"`
							EnvFrom []struct {
								ConfigMapRef *struct {
									Name string `json:"name"`
								} `json:"configMapRef"`
								SecretRef *struct {
									Name string `json:"name"`
								} `json:"secretRef"`
							} `json:"envFrom"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
				Rules []struct {
					Host string `json:"host"`
					HTTP struct {
						Paths []struct {
							Path    string `json:"path"`
							Backend struct {
								Service struct {
									Name string `json:"name"`
									Port struct {
										Name   string `json:"name"`
										Number int    `json:"number"`
									} `json:"port"`
								} `json:"service"`
							} `json:"backend"`
						} `json:"paths"`
					} `json:"http"`
				} `json:"rules"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return nil, "", fmt.Errorf("decode %s list: %w", kind, err)
	}
	var rawPayload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(responseBody, &rawPayload); err != nil {
		return nil, "", fmt.Errorf("decode %s raw list: %w", kind, err)
	}
	snapshots := make([]domain.ResourceSnapshot, 0, len(payload.Items))
	for index, item := range payload.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		ns := strings.TrimSpace(item.Metadata.Namespace)
		if ns == "" {
			ns = namespace
		}
		if name == "" {
			continue
		}
		rawManifest := map[string]any(nil)
		if index < len(rawPayload.Items) {
			rawManifest = sanitizeResourceManifest(kind, rawPayload.Items[index], ns, name)
		}
		snapshots = append(snapshots, domain.ResourceSnapshot{
			Kind:            kind,
			Namespace:       ns,
			Name:            name,
			Labels:          item.Metadata.Labels,
			Annotations:     item.Metadata.Annotations,
			Manifest:        rawManifest,
			OwnerReferences: item.Metadata.OwnerReferences,
			Selector:        normalizeStringMap(item.Spec.Selector),
			PodLabels:       normalizeStringMap(item.Spec.Template.Metadata.Labels),
			EnvVars:         resourceEnvVarsFromContainers(item.Spec.Template.Spec.Containers),
			EnvFrom:         resourceEnvFromRefsFromContainers(item.Spec.Template.Spec.Containers),
			Containers:      resourceContainersFromSpec(item.Spec.Template.Spec.Containers),
			ConfigMapKeys:   configMapKeysFromResource(kind, item.Data, item.BinaryData),
			IngressRules:    resourceIngressRulesFromSpec(item.Spec.Rules),
		})
	}
	return snapshots, "", nil
}

func sanitizeResourceManifest(kind string, raw map[string]any, namespace string, name string) map[string]any {
	if raw == nil {
		return nil
	}
	manifest := map[string]any{}
	if apiVersion := strings.TrimSpace(stringifyAny(raw["apiVersion"])); apiVersion != "" {
		manifest["apiVersion"] = apiVersion
	}
	manifest["kind"] = kind
	metadata := map[string]any{
		"name": name,
	}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	if metaRaw, ok := raw["metadata"].(map[string]any); ok {
		if labels := normalizeStringAnyMap(metaRaw["labels"]); len(labels) > 0 {
			metadata["labels"] = labels
		}
		if annotations := normalizeStringAnyMap(metaRaw["annotations"]); len(annotations) > 0 {
			metadata["annotations"] = annotations
		}
	}
	manifest["metadata"] = metadata

	if kind == "Secret" {
		if secretType := strings.TrimSpace(stringifyAny(raw["type"])); secretType != "" {
			manifest["type"] = secretType
		}
		if immutable, ok := raw["immutable"].(bool); ok {
			manifest["immutable"] = immutable
		}
		return manifest
	}

	if spec, ok := deepCopyJSONValue(raw["spec"]).(map[string]any); ok && len(spec) > 0 {
		manifest["spec"] = spec
	}
	if kind == "ConfigMap" {
		if data, ok := deepCopyJSONValue(raw["data"]).(map[string]any); ok && len(data) > 0 {
			manifest["data"] = data
		}
		if binaryData, ok := deepCopyJSONValue(raw["binaryData"]).(map[string]any); ok && len(binaryData) > 0 {
			manifest["binaryData"] = binaryData
		}
	}
	return manifest
}

func deepCopyJSONValue(value any) any {
	if value == nil {
		return nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var copied any
	if err := json.Unmarshal(payload, &copied); err != nil {
		return nil
	}
	return copied
}

func normalizeStringAnyMap(value any) map[string]any {
	raw, ok := value.(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	items := make([]string, 0, len(raw))
	for key, rawValue := range raw {
		trimmedKey := strings.TrimSpace(key)
		trimmedValue := strings.TrimSpace(stringifyAny(rawValue))
		if trimmedKey == "" || trimmedValue == "" {
			continue
		}
		items = append(items, trimmedKey)
	}
	if len(items) == 0 {
		return nil
	}
	sort.Strings(items)
	normalized := make(map[string]any, len(items))
	for _, key := range items {
		normalized[key] = strings.TrimSpace(stringifyAny(raw[key]))
	}
	return normalized
}

func stringifyAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func configMapKeysFromResource(kind string, data map[string]any, binaryData map[string]any) []string {
	if kind != "ConfigMap" {
		return nil
	}
	keys := make([]string, 0, len(data)+len(binaryData))
	for key := range data {
		trimmed := strings.TrimSpace(key)
		if trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	for key := range binaryData {
		trimmed := strings.TrimSpace(key)
		if trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return deduplicateStrings(keys)
}

func resourceEnvVarsFromContainers(containers []struct {
	Name string `json:"name"`
	Env  []struct {
		Name      string         `json:"name"`
		Value     string         `json:"value"`
		ValueFrom map[string]any `json:"valueFrom"`
	} `json:"env"`
	EnvFrom []struct {
		ConfigMapRef *struct {
			Name string `json:"name"`
		} `json:"configMapRef"`
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}) []domain.ResourceEnvVar {
	var envVars []domain.ResourceEnvVar
	for _, container := range containers {
		for _, item := range container.Env {
			parsed, ok := parseResourceEnvVar(item)
			if !ok {
				continue
			}
			envVars = append(envVars, parsed)
		}
	}
	return envVars
}

func resourceEnvFromRefsFromContainers(containers []struct {
	Name string `json:"name"`
	Env  []struct {
		Name      string         `json:"name"`
		Value     string         `json:"value"`
		ValueFrom map[string]any `json:"valueFrom"`
	} `json:"env"`
	EnvFrom []struct {
		ConfigMapRef *struct {
			Name string `json:"name"`
		} `json:"configMapRef"`
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}) []domain.ResourceEnvFromRef {
	var refs []domain.ResourceEnvFromRef
	for _, container := range containers {
		for _, item := range container.EnvFrom {
			if item.ConfigMapRef != nil && strings.TrimSpace(item.ConfigMapRef.Name) != "" {
				refs = append(refs, domain.ResourceEnvFromRef{Kind: "ConfigMap", Name: strings.TrimSpace(item.ConfigMapRef.Name)})
			}
			if item.SecretRef != nil && strings.TrimSpace(item.SecretRef.Name) != "" {
				refs = append(refs, domain.ResourceEnvFromRef{Kind: "Secret", Name: strings.TrimSpace(item.SecretRef.Name)})
			}
		}
	}
	return refs
}

func resourceContainersFromSpec(containers []struct {
	Name string `json:"name"`
	Env  []struct {
		Name      string         `json:"name"`
		Value     string         `json:"value"`
		ValueFrom map[string]any `json:"valueFrom"`
	} `json:"env"`
	EnvFrom []struct {
		ConfigMapRef *struct {
			Name string `json:"name"`
		} `json:"configMapRef"`
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}) []domain.ResourceContainerEnv {
	result := make([]domain.ResourceContainerEnv, 0, len(containers))
	for _, container := range containers {
		entry := domain.ResourceContainerEnv{
			Name: strings.TrimSpace(container.Name),
			EnvVars: resourceEnvVarsFromContainers([]struct {
				Name string `json:"name"`
				Env  []struct {
					Name      string         `json:"name"`
					Value     string         `json:"value"`
					ValueFrom map[string]any `json:"valueFrom"`
				} `json:"env"`
				EnvFrom []struct {
					ConfigMapRef *struct {
						Name string `json:"name"`
					} `json:"configMapRef"`
					SecretRef *struct {
						Name string `json:"name"`
					} `json:"secretRef"`
				} `json:"envFrom"`
			}{container}),
			EnvFrom: resourceEnvFromRefsFromContainers([]struct {
				Name string `json:"name"`
				Env  []struct {
					Name      string         `json:"name"`
					Value     string         `json:"value"`
					ValueFrom map[string]any `json:"valueFrom"`
				} `json:"env"`
				EnvFrom []struct {
					ConfigMapRef *struct {
						Name string `json:"name"`
					} `json:"configMapRef"`
					SecretRef *struct {
						Name string `json:"name"`
					} `json:"secretRef"`
				} `json:"envFrom"`
			}{container}),
		}
		if entry.Name == "" {
			entry.Name = "container"
		}
		result = append(result, entry)
	}
	return result
}

func parseResourceEnvVar(item struct {
	Name      string         `json:"name"`
	Value     string         `json:"value"`
	ValueFrom map[string]any `json:"valueFrom"`
}) (domain.ResourceEnvVar, bool) {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		return domain.ResourceEnvVar{}, false
	}
	result := domain.ResourceEnvVar{
		Name:  name,
		Value: strings.TrimSpace(item.Value),
	}
	if len(item.ValueFrom) == 0 {
		return result, true
	}
	result.ValueFrom = "valueFrom"
	if ref, ok := item.ValueFrom["secretKeyRef"].(map[string]any); ok {
		result.ValueFromKind = "secretKeyRef"
		result.ValueFromName = strings.TrimSpace(asAnyString(ref["name"]))
		result.ValueFromKey = strings.TrimSpace(asAnyString(ref["key"]))
		return result, true
	}
	if ref, ok := item.ValueFrom["configMapKeyRef"].(map[string]any); ok {
		result.ValueFromKind = "configMapKeyRef"
		result.ValueFromName = strings.TrimSpace(asAnyString(ref["name"]))
		result.ValueFromKey = strings.TrimSpace(asAnyString(ref["key"]))
		return result, true
	}
	if ref, ok := item.ValueFrom["fieldRef"].(map[string]any); ok {
		result.ValueFromKind = "fieldRef"
		result.ValueFromField = strings.TrimSpace(asAnyString(ref["fieldPath"]))
		return result, true
	}
	if ref, ok := item.ValueFrom["resourceFieldRef"].(map[string]any); ok {
		result.ValueFromKind = "resourceFieldRef"
		result.ValueFromField = strings.TrimSpace(asAnyString(ref["resource"]))
		result.ValueFromPath = strings.TrimSpace(asAnyString(ref["divisor"]))
		return result, true
	}
	return result, true
}

func asAnyString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func resourceIngressRulesFromSpec(rules []struct {
	Host string `json:"host"`
	HTTP struct {
		Paths []struct {
			Path    string `json:"path"`
			Backend struct {
				Service struct {
					Name string `json:"name"`
					Port struct {
						Name   string `json:"name"`
						Number int    `json:"number"`
					} `json:"port"`
				} `json:"service"`
			} `json:"backend"`
		} `json:"paths"`
	} `json:"http"`
}) []domain.ResourceIngressRule {
	var result []domain.ResourceIngressRule
	for _, rule := range rules {
		for _, path := range rule.HTTP.Paths {
			serviceName := strings.TrimSpace(path.Backend.Service.Name)
			if serviceName == "" {
				continue
			}
			servicePort := strings.TrimSpace(path.Backend.Service.Port.Name)
			if servicePort == "" && path.Backend.Service.Port.Number > 0 {
				servicePort = fmt.Sprintf("%d", path.Backend.Service.Port.Number)
			}
			result = append(result, domain.ResourceIngressRule{
				Host:        strings.TrimSpace(rule.Host),
				Path:        strings.TrimSpace(path.Path),
				ServiceName: serviceName,
				ServicePort: servicePort,
			})
		}
	}
	return result
}

func (s *ResourceDiscoveryScanner) listFluxKustomizations(ctx context.Context, namespace string) ([]fluxResource, string, error) {
	endpoint := s.source.apiURL + "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/" + url.PathEscape(namespace) + "/kustomizations"
	req, err := s.source.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.source.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Sprintf("%s Kustomization: forbidden (%s)", namespace, strings.TrimSpace(string(body))), nil
	case http.StatusNotFound:
		return nil, "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("list Kustomization failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				SourceRef fluxSourceRef `json:"sourceRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("decode Kustomization list: %w", err)
	}
	items := make([]fluxResource, 0, len(payload.Items))
	for _, item := range payload.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		if name == "" {
			continue
		}
		ns := strings.TrimSpace(item.Metadata.Namespace)
		if ns == "" {
			ns = namespace
		}
		items = append(items, fluxResource{
			Namespace:   ns,
			Name:        name,
			Labels:      item.Metadata.Labels,
			Annotations: item.Metadata.Annotations,
			SourceRef:   normalizeFluxSourceRef(item.Spec.SourceRef, ns),
		})
	}
	return items, "", nil
}

func (s *ResourceDiscoveryScanner) listHelmReleases(ctx context.Context, namespace string) ([]fluxResource, string, error) {
	endpoint := s.source.apiURL + "/apis/helm.toolkit.fluxcd.io/v2/namespaces/" + url.PathEscape(namespace) + "/helmreleases"
	req, err := s.source.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.source.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Sprintf("%s HelmRelease: forbidden (%s)", namespace, strings.TrimSpace(string(body))), nil
	case http.StatusNotFound:
		return nil, "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("list HelmRelease failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Chart struct {
					Spec struct {
						SourceRef fluxSourceRef `json:"sourceRef"`
					} `json:"spec"`
				} `json:"chart"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("decode HelmRelease list: %w", err)
	}
	items := make([]fluxResource, 0, len(payload.Items))
	for _, item := range payload.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		if name == "" {
			continue
		}
		ns := strings.TrimSpace(item.Metadata.Namespace)
		if ns == "" {
			ns = namespace
		}
		items = append(items, fluxResource{
			Namespace:   ns,
			Name:        name,
			Labels:      item.Metadata.Labels,
			Annotations: item.Metadata.Annotations,
			SourceRef:   normalizeFluxSourceRef(item.Spec.Chart.Spec.SourceRef, ns),
		})
	}
	return items, "", nil
}

func (s *ResourceDiscoveryScanner) listGitRepositories(ctx context.Context, namespace string) ([]fluxResource, string, error) {
	endpoint := s.source.apiURL + "/apis/source.toolkit.fluxcd.io/v1/namespaces/" + url.PathEscape(namespace) + "/gitrepositories"
	req, err := s.source.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.source.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Sprintf("%s GitRepository: forbidden (%s)", namespace, strings.TrimSpace(string(body))), nil
	case http.StatusNotFound:
		return nil, "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("list GitRepository failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("decode GitRepository list: %w", err)
	}
	items := make([]fluxResource, 0, len(payload.Items))
	for _, item := range payload.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		if name == "" {
			continue
		}
		ns := strings.TrimSpace(item.Metadata.Namespace)
		if ns == "" {
			ns = namespace
		}
		items = append(items, fluxResource{
			Namespace:   ns,
			Name:        name,
			Labels:      item.Metadata.Labels,
			Annotations: item.Metadata.Annotations,
		})
	}
	return items, "", nil
}

func resolveWorkloadSource(
	snapshot domain.ResourceSnapshot,
	helmByKey map[string]fluxResource,
	kustomizationByKey map[string]fluxResource,
	gitRepositoryByKey map[string]fluxResource,
) *domain.ResourceSourceMapping {
	helmName := firstNonEmptyString(
		mapLookup(snapshot.Labels, "helm.toolkit.fluxcd.io/name"),
		mapLookup(snapshot.Annotations, "meta.helm.sh/release-name"),
	)
	helmNamespace := firstNonEmptyString(
		mapLookup(snapshot.Labels, "helm.toolkit.fluxcd.io/namespace"),
		mapLookup(snapshot.Annotations, "meta.helm.sh/release-namespace"),
		snapshot.Namespace,
	)
	if helmName != "" {
		if release, ok := helmByKey[fluxKey(helmNamespace, helmName)]; ok {
			mapping := &domain.ResourceSourceMapping{
				Status:    "resolved",
				Kind:      "HelmRelease",
				Namespace: release.Namespace,
				Name:      release.Name,
			}
			attachGitRepositoryMapping(mapping, release.SourceRef, release.Namespace, gitRepositoryByKey)
			return mapping
		}
		return &domain.ResourceSourceMapping{
			Status:    "unresolved",
			Kind:      "HelmRelease",
			Namespace: helmNamespace,
			Name:      helmName,
			Reason:    "referenced helm release not found",
		}
	}

	kustomizationName := firstNonEmptyString(
		mapLookup(snapshot.Labels, "kustomize.toolkit.fluxcd.io/name"),
		mapLookup(snapshot.Annotations, "kustomize.toolkit.fluxcd.io/name"),
	)
	kustomizationNamespace := firstNonEmptyString(
		mapLookup(snapshot.Labels, "kustomize.toolkit.fluxcd.io/namespace"),
		mapLookup(snapshot.Annotations, "kustomize.toolkit.fluxcd.io/namespace"),
		snapshot.Namespace,
	)
	if kustomizationName != "" {
		if item, ok := kustomizationByKey[fluxKey(kustomizationNamespace, kustomizationName)]; ok {
			mapping := &domain.ResourceSourceMapping{
				Status:    "resolved",
				Kind:      "Kustomization",
				Namespace: item.Namespace,
				Name:      item.Name,
			}
			attachGitRepositoryMapping(mapping, item.SourceRef, item.Namespace, gitRepositoryByKey)
			return mapping
		}
		return &domain.ResourceSourceMapping{
			Status:    "unresolved",
			Kind:      "Kustomization",
			Namespace: kustomizationNamespace,
			Name:      kustomizationName,
			Reason:    "referenced kustomization not found",
		}
	}

	return &domain.ResourceSourceMapping{
		Status: "unresolved",
		Reason: "owner labels or annotations not found",
	}
}

func sourceMappingFromFluxSourceRef(
	ref fluxSourceRef,
	defaultNamespace string,
	gitRepositoryByKey map[string]fluxResource,
) *domain.ResourceSourceMapping {
	if !strings.EqualFold(strings.TrimSpace(ref.Kind), "GitRepository") {
		return &domain.ResourceSourceMapping{
			Status: "unresolved",
			Reason: "sourceRef kind is not GitRepository",
		}
	}
	namespace := firstNonEmptyString(strings.TrimSpace(ref.Namespace), defaultNamespace)
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return &domain.ResourceSourceMapping{
			Status:    "unresolved",
			Kind:      "GitRepository",
			Namespace: namespace,
			Reason:    "sourceRef name is empty",
		}
	}
	if _, ok := gitRepositoryByKey[fluxKey(namespace, name)]; ok {
		return &domain.ResourceSourceMapping{
			Status:                 "resolved",
			Kind:                   "GitRepository",
			Namespace:              namespace,
			Name:                   name,
			GitRepositoryNamespace: namespace,
			GitRepositoryName:      name,
		}
	}
	return &domain.ResourceSourceMapping{
		Status:                 "unresolved",
		Kind:                   "GitRepository",
		Namespace:              namespace,
		Name:                   name,
		GitRepositoryNamespace: namespace,
		GitRepositoryName:      name,
		Reason:                 "referenced git repository not found",
	}
}

func attachGitRepositoryMapping(
	mapping *domain.ResourceSourceMapping,
	ref fluxSourceRef,
	defaultNamespace string,
	gitRepositoryByKey map[string]fluxResource,
) {
	if mapping == nil || !strings.EqualFold(strings.TrimSpace(ref.Kind), "GitRepository") {
		return
	}
	namespace := firstNonEmptyString(strings.TrimSpace(ref.Namespace), defaultNamespace)
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return
	}
	mapping.GitRepositoryNamespace = namespace
	mapping.GitRepositoryName = name
	if _, ok := gitRepositoryByKey[fluxKey(namespace, name)]; !ok && mapping.Status == "resolved" {
		mapping.Status = "unresolved"
		mapping.Reason = "referenced git repository not found"
	}
}

func fluxKey(namespace string, name string) string {
	return strings.TrimSpace(namespace) + "/" + strings.TrimSpace(name)
}

func mapLookup(values map[string]string, key string) string {
	if values == nil {
		return ""
	}
	return strings.TrimSpace(values[key])
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isFluxSourceKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "HelmRelease", "Kustomization", "GitRepository":
		return true
	default:
		return false
	}
}

func normalizeFluxSourceRef(ref fluxSourceRef, defaultNamespace string) fluxSourceRef {
	ref.Kind = strings.TrimSpace(ref.Kind)
	ref.Name = strings.TrimSpace(ref.Name)
	ref.Namespace = firstNonEmptyString(ref.Namespace, defaultNamespace)
	return ref
}

type fluxResource struct {
	Namespace   string
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	SourceRef   fluxSourceRef
}

type fluxSourceRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func normalizeNamespaces(items []string) []string {
	result := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func normalizeStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		trimmedKey := strings.TrimSpace(key)
		trimmedValue := strings.TrimSpace(value)
		if trimmedKey == "" || trimmedValue == "" {
			continue
		}
		result[trimmedKey] = trimmedValue
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func deduplicateStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	result := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
