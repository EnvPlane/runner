package bootstrap

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/envpilot/contracts/domain"
)

type ResourceSelection struct {
	Include  bool
	Strategy string
}

type ManifestTemplateGeneratorOptions struct {
	FeatureNamespaceTemplate string
	CommitSHAPlaceholder     string
	ImagePattern             string
	PreviewDomain            string
	HostPatternTemplate      string
	Labels                   map[string]string
	Annotations              map[string]string
}

type ManifestTemplate struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	YAML      string `json:"yaml"`
}

// MarshalDeterministicYAML renders YAML with deterministic key ordering.
//
// It is intended for bundling and diff-friendly artifacts where stable output
// between runs is required.
func MarshalDeterministicYAML(value any) string {
	return marshalDeterministicYAML(value)
}

type ResourcePolicyConfig struct {
	DefaultTTLHours       int
	CPURequest            string
	CPULimit              string
	MemoryRequest         string
	MemoryLimit           string
	StorageQuota          string
	MaxActiveEnvironments int
}

type NetworkPolicyConfig struct {
	FeatureToBase              bool
	BaseToFeature              bool
	EgressMode                 string
	BaseNamespaces             []string
	AllowBaseNamespacePolicies bool
}

var sf801SupportedKinds = map[string]struct{}{
	"Namespace":     {},
	"Deployment":    {},
	"Service":       {},
	"Ingress":       {},
	"ConfigMap":     {},
	"ResourceQuota": {},
	"LimitRange":    {},
}

func GenerateManifestTemplates(
	snapshots []domain.ResourceSnapshot,
	selections map[string]ResourceSelection,
	options ManifestTemplateGeneratorOptions,
) ([]ManifestTemplate, error) {
	featureNamespace := strings.TrimSpace(options.FeatureNamespaceTemplate)
	if featureNamespace == "" {
		return nil, fmt.Errorf("feature namespace template is required")
	}
	commitPlaceholder := strings.TrimSpace(options.CommitSHAPlaceholder)
	if commitPlaceholder == "" {
		commitPlaceholder = "{{ .CommitSHA }}"
	}
	templates := make([]ManifestTemplate, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if _, ok := sf801SupportedKinds[snapshot.Kind]; !ok {
			continue
		}
		if !shouldGenerateTemplate(snapshot, selections) {
			continue
		}
		if snapshot.Manifest == nil {
			continue
		}
		manifest := cloneManifest(snapshot.Manifest)
		if manifest == nil {
			continue
		}
		rewriteManifestNamespace(manifest, snapshot.Kind, featureNamespace)
		rewriteManifestImages(manifest, snapshot.Kind, commitPlaceholder, strings.TrimSpace(options.ImagePattern))
		rewriteIngressHosts(manifest, snapshot, strings.TrimSpace(options.PreviewDomain), strings.TrimSpace(options.HostPatternTemplate))
		addEnvPlaneMetadata(manifest, options.Labels, options.Annotations)

		yamlText := marshalDeterministicYAML(manifest)
		rewrittenNamespace := metadataNamespace(manifest, snapshot.Kind, featureNamespace)
		rewrittenName := metadataName(manifest, snapshot.Name)
		templates = append(templates, ManifestTemplate{
			Kind:      snapshot.Kind,
			Namespace: rewrittenNamespace,
			Name:      rewrittenName,
			YAML:      yamlText,
		})
	}

	sort.Slice(templates, func(i, j int) bool {
		if templates[i].Kind != templates[j].Kind {
			return templates[i].Kind < templates[j].Kind
		}
		if templates[i].Namespace != templates[j].Namespace {
			return templates[i].Namespace < templates[j].Namespace
		}
		return templates[i].Name < templates[j].Name
	})
	return templates, nil
}

func ValidateResourcePolicyConfig(policy ResourcePolicyConfig) error {
	if strings.TrimSpace(policy.CPURequest) == "" {
		return fmt.Errorf("cpu request is required")
	}
	if strings.TrimSpace(policy.CPULimit) == "" {
		return fmt.Errorf("cpu limit is required")
	}
	if strings.TrimSpace(policy.MemoryRequest) == "" {
		return fmt.Errorf("memory request is required")
	}
	if strings.TrimSpace(policy.MemoryLimit) == "" {
		return fmt.Errorf("memory limit is required")
	}
	if strings.TrimSpace(policy.StorageQuota) == "" {
		return fmt.Errorf("storage quota is required")
	}
	if !isValidCPUMilli(policy.CPURequest) {
		return fmt.Errorf("cpu request must be a valid Kubernetes quantity")
	}
	if !isValidCPUMilli(policy.CPULimit) {
		return fmt.Errorf("cpu limit must be a valid Kubernetes quantity")
	}
	if !isValidBinaryBytes(policy.MemoryRequest) {
		return fmt.Errorf("memory request must be a valid Kubernetes quantity like 256Mi")
	}
	if !isValidBinaryBytes(policy.MemoryLimit) {
		return fmt.Errorf("memory limit must be a valid Kubernetes quantity like 1Gi")
	}
	if !isValidBinaryBytes(policy.StorageQuota) {
		return fmt.Errorf("storage quota must be a valid Kubernetes quantity like 10Gi")
	}
	if policy.MaxActiveEnvironments <= 0 {
		return fmt.Errorf("max active environments must be greater than 0")
	}
	if policy.DefaultTTLHours <= 0 {
		return fmt.Errorf("default TTL must be greater than 0")
	}
	return nil
}

func GenerateResourcePolicyTemplates(
	policy ResourcePolicyConfig,
	featureNamespace string,
	labels map[string]string,
	annotations map[string]string,
) ([]ManifestTemplate, error) {
	if err := ValidateResourcePolicyConfig(policy); err != nil {
		return nil, err
	}
	ns := strings.TrimSpace(featureNamespace)
	if ns == "" {
		return nil, fmt.Errorf("feature namespace template is required")
	}

	resourceQuota := map[string]any{
		"apiVersion": "v1",
		"kind":       "ResourceQuota",
		"metadata": map[string]any{
			"name":      "envpilot-resource-quota",
			"namespace": ns,
		},
		"spec": map[string]any{
			"hard": map[string]any{
				"requests.cpu":                 strings.TrimSpace(policy.CPURequest),
				"limits.cpu":                   strings.TrimSpace(policy.CPULimit),
				"requests.memory":              strings.TrimSpace(policy.MemoryRequest),
				"limits.memory":                strings.TrimSpace(policy.MemoryLimit),
				"requests.storage":             strings.TrimSpace(policy.StorageQuota),
				"count/persistentvolumeclaims": "20",
				"count/pods":                   "100",
			},
		},
	}
	addEnvPlaneMetadata(resourceQuota, labels, annotations)

	limitRange := map[string]any{
		"apiVersion": "v1",
		"kind":       "LimitRange",
		"metadata": map[string]any{
			"name":      "envpilot-resource-limits",
			"namespace": ns,
		},
		"spec": map[string]any{
			"limits": []any{
				map[string]any{
					"type": "Container",
					"defaultRequest": map[string]any{
						"cpu":    strings.TrimSpace(policy.CPURequest),
						"memory": strings.TrimSpace(policy.MemoryRequest),
					},
					"default": map[string]any{
						"cpu":    strings.TrimSpace(policy.CPULimit),
						"memory": strings.TrimSpace(policy.MemoryLimit),
					},
				},
			},
		},
	}
	addEnvPlaneMetadata(limitRange, labels, annotations)

	return []ManifestTemplate{
		{
			Kind:      "LimitRange",
			Namespace: ns,
			Name:      "envpilot-resource-limits",
			YAML:      marshalDeterministicYAML(limitRange),
		},
		{
			Kind:      "ResourceQuota",
			Namespace: ns,
			Name:      "envpilot-resource-quota",
			YAML:      marshalDeterministicYAML(resourceQuota),
		},
	}, nil
}

func ValidateNetworkPolicyConfig(config NetworkPolicyConfig) error {
	mode := normalizeNetworkEgressMode(config.EgressMode)
	switch mode {
	case "allow all", "restricted", "deny all":
	default:
		return fmt.Errorf("egress policy must be one of: allow all, restricted, deny all")
	}
	if config.BaseToFeature || mode == "restricted" || (config.FeatureToBase && config.AllowBaseNamespacePolicies) {
		if len(nonEmptyStrings(config.BaseNamespaces)) == 0 {
			return fmt.Errorf("at least one base namespace is required")
		}
	}
	return nil
}

func GenerateNetworkPolicyTemplates(
	config NetworkPolicyConfig,
	featureNamespace string,
	labels map[string]string,
	annotations map[string]string,
) ([]ManifestTemplate, error) {
	if err := ValidateNetworkPolicyConfig(config); err != nil {
		return nil, err
	}
	ns := strings.TrimSpace(featureNamespace)
	if ns == "" {
		return nil, fmt.Errorf("feature namespace template is required")
	}
	mode := normalizeNetworkEgressMode(config.EgressMode)
	baseNamespaces := nonEmptyStrings(config.BaseNamespaces)
	templates := make([]ManifestTemplate, 0, 3+len(baseNamespaces))

	if config.BaseToFeature {
		manifest := map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"metadata": map[string]any{
				"name":      "envpilot-allow-base-to-feature",
				"namespace": ns,
			},
			"spec": map[string]any{
				"podSelector": map[string]any{},
				"policyTypes": []any{"Ingress"},
				"ingress": []any{
					map[string]any{
						"from": namespacePeerRules(baseNamespaces),
					},
				},
			},
		}
		addEnvPlaneMetadata(manifest, labels, annotations)
		templates = append(templates, ManifestTemplate{
			Kind:      "NetworkPolicy",
			Namespace: ns,
			Name:      "envpilot-allow-base-to-feature",
			YAML:      marshalDeterministicYAML(manifest),
		})
	}

	if mode != "" {
		manifest := map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"metadata": map[string]any{
				"name":      "envpilot-feature-egress",
				"namespace": ns,
			},
			"spec": map[string]any{
				"podSelector": map[string]any{},
				"policyTypes": []any{"Egress"},
			},
		}
		spec := manifest["spec"].(map[string]any)
		switch mode {
		case "allow all":
			spec["egress"] = []any{map[string]any{}}
		case "restricted":
			spec["egress"] = restrictedEgressRules(baseNamespaces)
		case "deny all":
			spec["egress"] = []any{}
		}
		addEnvPlaneMetadata(manifest, labels, annotations)
		templates = append(templates, ManifestTemplate{
			Kind:      "NetworkPolicy",
			Namespace: ns,
			Name:      "envpilot-feature-egress",
			YAML:      marshalDeterministicYAML(manifest),
		})
	}

	if config.FeatureToBase && config.AllowBaseNamespacePolicies {
		for _, baseNamespace := range baseNamespaces {
			manifest := map[string]any{
				"apiVersion": "networking.k8s.io/v1",
				"kind":       "NetworkPolicy",
				"metadata": map[string]any{
					"name":      "envpilot-allow-feature-to-base",
					"namespace": baseNamespace,
				},
				"spec": map[string]any{
					"podSelector": map[string]any{},
					"policyTypes": []any{"Ingress"},
					"ingress": []any{
						map[string]any{
							"from": []any{
								namespacePeerRule(ns),
							},
						},
					},
				},
			}
			addEnvPlaneMetadata(manifest, labels, annotations)
			templates = append(templates, ManifestTemplate{
				Kind:      "NetworkPolicy",
				Namespace: baseNamespace,
				Name:      "envpilot-allow-feature-to-base",
				YAML:      marshalDeterministicYAML(manifest),
			})
		}
	}

	sort.Slice(templates, func(i, j int) bool {
		if templates[i].Namespace != templates[j].Namespace {
			return templates[i].Namespace < templates[j].Namespace
		}
		return templates[i].Name < templates[j].Name
	})
	return templates, nil
}

func ResourceSnapshotKey(snapshot domain.ResourceSnapshot) string {
	return snapshot.Kind + "/" + snapshot.Namespace + "/" + snapshot.Name
}

func shouldGenerateTemplate(snapshot domain.ResourceSnapshot, selections map[string]ResourceSelection) bool {
	selection, ok := selections[ResourceSnapshotKey(snapshot)]
	if !ok {
		return true
	}
	if !selection.Include {
		return false
	}
	strategy := strings.ToLower(strings.TrimSpace(selection.Strategy))
	switch strategy {
	case "", "override per pr", "clone":
		return true
	case "use base", "reference", "mock", "ignore", "external dependency":
		return false
	default:
		return true
	}
}

func cloneManifest(manifest map[string]any) map[string]any {
	if manifest == nil {
		return nil
	}
	cloned := make(map[string]any, len(manifest))
	for key, value := range manifest {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, item := range typed {
			copied[key] = cloneValue(item)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index := range typed {
			copied[index] = cloneValue(typed[index])
		}
		return copied
	default:
		return typed
	}
}

func rewriteManifestNamespace(manifest map[string]any, kind string, featureNamespace string) {
	metadata := ensureStringAnyMap(manifest, "metadata")
	if kind == "Namespace" {
		metadata["name"] = featureNamespace
		delete(metadata, "namespace")
		return
	}
	metadata["namespace"] = featureNamespace
}

func rewriteManifestImages(manifest map[string]any, kind string, commitPlaceholder string, imagePattern string) {
	if kind != "Deployment" {
		return
	}
	spec, ok := manifest["spec"].(map[string]any)
	if !ok {
		return
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		return
	}
	podSpec, ok := template["spec"].(map[string]any)
	if !ok {
		return
	}
	rewriteContainerImageSlice(podSpec, "containers", commitPlaceholder, imagePattern)
	rewriteContainerImageSlice(podSpec, "initContainers", commitPlaceholder, imagePattern)
}

func rewriteContainerImageSlice(spec map[string]any, key string, commitPlaceholder string, imagePattern string) {
	items, ok := spec[key].([]any)
	if !ok {
		return
	}
	for index := range items {
		container, ok := items[index].(map[string]any)
		if !ok {
			continue
		}
		image := strings.TrimSpace(asString(container["image"]))
		if image == "" {
			continue
		}
		repository := imageRepository(image)
		if repository == "" {
			continue
		}
		if imagePattern != "" {
			rewritten := strings.ReplaceAll(imagePattern, "{{ .Repository }}", repository)
			rewritten = strings.ReplaceAll(rewritten, "{{ .CommitSHA }}", commitPlaceholder)
			container["image"] = rewritten
			continue
		}
		container["image"] = repository + ":" + commitPlaceholder
	}
}

func imageRepository(image string) string {
	trimmed := strings.TrimSpace(image)
	if trimmed == "" {
		return ""
	}
	if digest := strings.Index(trimmed, "@"); digest >= 0 {
		trimmed = trimmed[:digest]
	}
	lastSlash := strings.LastIndex(trimmed, "/")
	lastColon := strings.LastIndex(trimmed, ":")
	if lastColon > lastSlash {
		return trimmed[:lastColon]
	}
	return trimmed
}

func rewriteIngressHosts(manifest map[string]any, snapshot domain.ResourceSnapshot, previewDomain string, hostPatternTemplate string) {
	if snapshot.Kind != "Ingress" {
		return
	}
	spec, ok := manifest["spec"].(map[string]any)
	if !ok {
		return
	}
	rewriteRuleHosts(spec, snapshot.Name, previewDomain, hostPatternTemplate)
	rewriteTLSHosts(spec, snapshot.Name, previewDomain, hostPatternTemplate)
}

func rewriteRuleHosts(spec map[string]any, resourceName string, previewDomain string, hostPatternTemplate string) {
	rules, ok := spec["rules"].([]any)
	if !ok {
		return
	}
	for _, rawRule := range rules {
		rule, ok := rawRule.(map[string]any)
		if !ok {
			continue
		}
		serviceName := firstIngressServiceName(rule)
		rule["host"] = rewriteHostValue(asString(rule["host"]), previewDomain, hostPatternTemplate, serviceName, resourceName)
	}
}

func rewriteTLSHosts(spec map[string]any, resourceName string, previewDomain string, hostPatternTemplate string) {
	entries, ok := spec["tls"].([]any)
	if !ok {
		return
	}
	for _, rawTLS := range entries {
		tlsEntry, ok := rawTLS.(map[string]any)
		if !ok {
			continue
		}
		hosts, ok := tlsEntry["hosts"].([]any)
		if !ok {
			continue
		}
		for index := range hosts {
			hosts[index] = rewriteHostValue(asString(hosts[index]), previewDomain, hostPatternTemplate, "", resourceName)
		}
		tlsEntry["hosts"] = hosts
	}
}

func firstIngressServiceName(rule map[string]any) string {
	httpSection, ok := rule["http"].(map[string]any)
	if !ok {
		return ""
	}
	paths, ok := httpSection["paths"].([]any)
	if !ok {
		return ""
	}
	for _, rawPath := range paths {
		path, ok := rawPath.(map[string]any)
		if !ok {
			continue
		}
		backend, ok := path["backend"].(map[string]any)
		if !ok {
			continue
		}
		service, ok := backend["service"].(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(asString(service["name"]))
		if name != "" {
			return name
		}
	}
	return ""
}

func rewriteHostValue(host string, previewDomain string, hostPatternTemplate string, serviceName string, resourceName string) string {
	trimmedHost := strings.TrimSpace(host)
	if hostPatternTemplate != "" {
		hostValue := strings.ReplaceAll(hostPatternTemplate, "{{ .Service }}", sanitizeHostLabel(firstNonEmpty(serviceName, resourceName)))
		if strings.Contains(hostValue, "{{ .PRNumber }}") {
			return hostValue
		}
		return strings.TrimSpace(hostValue)
	}
	if previewDomain == "" {
		return trimmedHost
	}
	prefix := sanitizeHostLabel(firstNonEmpty(serviceName, hostPrefix(trimmedHost), resourceName))
	if prefix == "" {
		prefix = "preview"
	}
	return prefix + "." + strings.TrimSpace(previewDomain)
}

func hostPrefix(host string) string {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return ""
	}
	if dot := strings.Index(trimmed, "."); dot > 0 {
		return trimmed[:dot]
	}
	return trimmed
}

func sanitizeHostLabel(label string) string {
	normalized := strings.ToLower(strings.TrimSpace(label))
	if normalized == "" {
		return ""
	}
	var builder strings.Builder
	for _, char := range normalized {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteRune('-')
		}
	}
	value := strings.Trim(builder.String(), "-")
	if value == "" {
		return ""
	}
	return value
}

func addEnvPlaneMetadata(manifest map[string]any, labels map[string]string, annotations map[string]string) {
	metadata := ensureStringAnyMap(manifest, "metadata")
	manifestLabels := ensureNestedStringMap(metadata, "labels")
	manifestAnnotations := ensureNestedStringMap(metadata, "annotations")

	manifestLabels["app.kubernetes.io/managed-by"] = "envpilot"
	manifestLabels["envpilot.io/managed"] = "true"
	manifestAnnotations["envpilot.io/generated-from-discovery"] = "true"

	for key, value := range labels {
		trimmedKey := strings.TrimSpace(key)
		trimmedValue := strings.TrimSpace(value)
		if trimmedKey == "" || trimmedValue == "" {
			continue
		}
		manifestLabels[trimmedKey] = trimmedValue
	}
	for key, value := range annotations {
		trimmedKey := strings.TrimSpace(key)
		trimmedValue := strings.TrimSpace(value)
		if trimmedKey == "" || trimmedValue == "" {
			continue
		}
		manifestAnnotations[trimmedKey] = trimmedValue
	}
}

func metadataNamespace(manifest map[string]any, kind string, fallback string) string {
	metadata, ok := manifest["metadata"].(map[string]any)
	if !ok {
		if kind == "Namespace" {
			return fallback
		}
		return fallback
	}
	if kind == "Namespace" {
		name := strings.TrimSpace(asString(metadata["name"]))
		if name == "" {
			return fallback
		}
		return name
	}
	namespace := strings.TrimSpace(asString(metadata["namespace"]))
	if namespace == "" {
		return fallback
	}
	return namespace
}

func metadataName(manifest map[string]any, fallback string) string {
	metadata, ok := manifest["metadata"].(map[string]any)
	if !ok {
		return fallback
	}
	name := strings.TrimSpace(asString(metadata["name"]))
	if name == "" {
		return fallback
	}
	return name
}

func ensureStringAnyMap(parent map[string]any, key string) map[string]any {
	current, ok := parent[key].(map[string]any)
	if ok {
		return current
	}
	current = map[string]any{}
	parent[key] = current
	return current
}

func ensureNestedStringMap(parent map[string]any, key string) map[string]any {
	current, ok := parent[key].(map[string]any)
	if ok {
		return current
	}
	current = map[string]any{}
	parent[key] = current
	return current
}

func marshalDeterministicYAML(value any) string {
	var builder strings.Builder
	writeYAMLValue(&builder, value, 0, true)
	result := builder.String()
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

func writeYAMLValue(builder *strings.Builder, value any, indent int, root bool) {
	switch typed := value.(type) {
	case map[string]any:
		writeYAMLMap(builder, typed, indent)
	case []any:
		writeYAMLList(builder, typed, indent)
	default:
		if !root {
			builder.WriteString(renderScalar(typed))
			builder.WriteString("\n")
		}
	}
}

func writeYAMLMap(builder *strings.Builder, value map[string]any, indent int) {
	if len(value) == 0 {
		builder.WriteString("{}\n")
		return
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		item := value[key]
		builder.WriteString(strings.Repeat(" ", indent))
		builder.WriteString(key)
		switch typed := item.(type) {
		case map[string]any:
			if len(typed) == 0 {
				builder.WriteString(": {}\n")
				continue
			}
			builder.WriteString(":\n")
			writeYAMLMap(builder, typed, indent+2)
		case []any:
			if len(typed) == 0 {
				builder.WriteString(": []\n")
				continue
			}
			builder.WriteString(":\n")
			writeYAMLList(builder, typed, indent+2)
		default:
			builder.WriteString(": ")
			builder.WriteString(renderScalar(typed))
			builder.WriteString("\n")
		}
	}
}

func writeYAMLList(builder *strings.Builder, value []any, indent int) {
	if len(value) == 0 {
		builder.WriteString("[]\n")
		return
	}
	for _, item := range value {
		builder.WriteString(strings.Repeat(" ", indent))
		builder.WriteString("-")
		switch typed := item.(type) {
		case map[string]any:
			if len(typed) == 0 {
				builder.WriteString(" {}\n")
				continue
			}
			builder.WriteString("\n")
			writeYAMLMap(builder, typed, indent+2)
		case []any:
			if len(typed) == 0 {
				builder.WriteString(" []\n")
				continue
			}
			builder.WriteString("\n")
			writeYAMLList(builder, typed, indent+2)
		default:
			builder.WriteString(" ")
			builder.WriteString(renderScalar(typed))
			builder.WriteString("\n")
		}
	}
}

func renderScalar(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case string:
		return quoteYAMLString(typed)
	default:
		return quoteYAMLString(asString(typed))
	}
}

func quoteYAMLString(value string) string {
	if value == "" {
		return `""`
	}
	if isPlainYAMLString(value) {
		return value
	}
	return strconv.Quote(value)
}

func isPlainYAMLString(value string) bool {
	if strings.HasPrefix(value, "{{") && strings.HasSuffix(value, "}}") {
		return true
	}
	for _, char := range value {
		switch char {
		case ':', '{', '}', '[', ']', ',', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`':
			return false
		}
		if char < 32 {
			return false
		}
	}
	return true
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeNetworkEgressMode(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "restricted"
	}
	return normalized
}

func namespacePeerRules(namespaces []string) []any {
	result := make([]any, 0, len(namespaces))
	for _, namespace := range nonEmptyStrings(namespaces) {
		result = append(result, namespacePeerRule(namespace))
	}
	return result
}

func namespacePeerRule(namespace string) map[string]any {
	return map[string]any{
		"namespaceSelector": map[string]any{
			"matchLabels": map[string]any{
				"kubernetes.io/metadata.name": strings.TrimSpace(namespace),
			},
		},
	}
}

func restrictedEgressRules(baseNamespaces []string) []any {
	rules := []any{
		map[string]any{
			"to": namespacePeerRules(baseNamespaces),
		},
		map[string]any{
			"to": []any{
				map[string]any{
					"namespaceSelector": map[string]any{
						"matchLabels": map[string]any{
							"kubernetes.io/metadata.name": "kube-system",
						},
					},
					"podSelector": map[string]any{
						"matchLabels": map[string]any{
							"k8s-app": "kube-dns",
						},
					},
				},
			},
			"ports": []any{
				map[string]any{"protocol": "UDP", "port": 53},
				map[string]any{"protocol": "TCP", "port": 53},
			},
		},
	}
	return rules
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	sort.Strings(result)
	return result
}

func isValidCPUMilli(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if strings.HasSuffix(trimmed, "m") {
		number := strings.TrimSuffix(trimmed, "m")
		if number == "" {
			return false
		}
		for _, char := range number {
			if char < '0' || char > '9' {
				return false
			}
		}
		return number != "0"
	}
	for _, char := range trimmed {
		if char < '0' || char > '9' {
			return false
		}
	}
	return trimmed != "0"
}

func isValidBinaryBytes(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	units := []string{"Ki", "Mi", "Gi", "Ti"}
	for _, unit := range units {
		if !strings.HasSuffix(trimmed, unit) {
			continue
		}
		number := strings.TrimSuffix(trimmed, unit)
		if number == "" {
			return false
		}
		for _, char := range number {
			if char < '0' || char > '9' {
				return false
			}
		}
		return number != "0"
	}
	return false
}
