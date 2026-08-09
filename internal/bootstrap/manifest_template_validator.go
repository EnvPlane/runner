package bootstrap

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type ManifestTemplateValidationIssue struct {
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ManifestTemplateValidationResult struct {
	Valid  bool                              `json:"valid"`
	Issues []ManifestTemplateValidationIssue `json:"issues"`
}

var (
	templateExpressionPattern = regexp.MustCompile(`\{\{[^}]*\}\}`)
	templateTokenPattern      = regexp.MustCompile(`^\{\{\s*\.([A-Za-z0-9_]+)\s*\}\}$`)
	yamlLinePattern           = regexp.MustCompile(`line\s+([0-9]+)`)
)

var allowedTemplateVariables = map[string]struct{}{
	"PRNumber":  {},
	"Branch":    {},
	"CommitSHA": {},
	"Service":   {},
}

func ValidateManifestTemplates(templates []ManifestTemplate) ManifestTemplateValidationResult {
	issues := make([]ManifestTemplateValidationIssue, 0)
	if len(templates) == 0 {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    "",
			Line:    1,
			Code:    "templates.missing",
			Message: "at least one manifest template is required",
		})
		return ManifestTemplateValidationResult{Valid: false, Issues: issues}
	}

	for _, item := range templates {
		file := manifestTemplateFile(item)
		yamlBody := strings.TrimSpace(item.YAML)
		if yamlBody == "" {
			issues = append(issues, ManifestTemplateValidationIssue{
				File:    file,
				Line:    1,
				Code:    "yaml.empty",
				Message: "template YAML is empty",
			})
			continue
		}

		var root yaml.Node
		if err := yaml.Unmarshal([]byte(item.YAML), &root); err != nil {
			line := parseYAMLLine(err.Error())
			if line == 0 {
				line = 1
			}
			issues = append(issues, ManifestTemplateValidationIssue{
				File:    file,
				Line:    line,
				Code:    "yaml.syntax",
				Message: err.Error(),
			})
			continue
		}

		issues = append(issues, validateTemplateVariables(file, item.Kind, item.YAML)...)
		issues = append(issues, validateKubernetesSchema(file, item.Kind, &root)...)
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].File != issues[j].File {
			return issues[i].File < issues[j].File
		}
		if issues[i].Line != issues[j].Line {
			return issues[i].Line < issues[j].Line
		}
		if issues[i].Column != issues[j].Column {
			return issues[i].Column < issues[j].Column
		}
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Message < issues[j].Message
	})
	return ManifestTemplateValidationResult{
		Valid:  len(issues) == 0,
		Issues: issues,
	}
}

func validateTemplateVariables(file string, kind string, yamlBody string) []ManifestTemplateValidationIssue {
	issues := make([]ManifestTemplateValidationIssue, 0)
	expressions := templateExpressionPattern.FindAllString(yamlBody, -1)
	hasPRNumber := false
	hasCommitSHA := false
	for _, expression := range expressions {
		match := templateTokenPattern.FindStringSubmatch(expression)
		if len(match) != 2 {
			issues = append(issues, ManifestTemplateValidationIssue{
				File:    file,
				Line:    lineOfFirst(yamlBody, expression),
				Code:    "template.syntax",
				Message: fmt.Sprintf("unsupported template expression %q", expression),
			})
			continue
		}
		name := match[1]
		if name == "PRNumber" {
			hasPRNumber = true
		}
		if name == "CommitSHA" {
			hasCommitSHA = true
		}
		if _, ok := allowedTemplateVariables[name]; !ok {
			issues = append(issues, ManifestTemplateValidationIssue{
				File:    file,
				Line:    lineOfFirst(yamlBody, expression),
				Code:    "template.variable",
				Message: fmt.Sprintf("unsupported EnvPlane variable %q", name),
			})
		}
	}

	if !hasPRNumber {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    1,
			Code:    "template.required_variable",
			Message: "missing required EnvPlane variable {{ .PRNumber }}",
		})
	}
	if strings.EqualFold(strings.TrimSpace(kind), "Deployment") && !hasCommitSHA {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    1,
			Code:    "template.required_variable",
			Message: "missing required EnvPlane variable {{ .CommitSHA }} for Deployment",
		})
	}
	return issues
}

func validateKubernetesSchema(file string, expectedKind string, root *yaml.Node) []ManifestTemplateValidationIssue {
	issues := make([]ManifestTemplateValidationIssue, 0)
	manifest := rootDocumentMapping(root)
	if manifest == nil {
		return append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(root, 1),
			Code:    "schema.root",
			Message: "manifest root must be a YAML object",
		})
	}

	apiVersionNode := lookupMapValue(manifest, "apiVersion")
	if !isNonEmptyScalar(apiVersionNode) {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(apiVersionNode, lineFromNode(manifest, 1)),
			Code:    "schema.apiVersion",
			Message: "apiVersion is required",
		})
	}

	kindNode := lookupMapValue(manifest, "kind")
	if !isNonEmptyScalar(kindNode) {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(kindNode, lineFromNode(manifest, 1)),
			Code:    "schema.kind",
			Message: "kind is required",
		})
	} else if !strings.EqualFold(strings.TrimSpace(kindNode.Value), strings.TrimSpace(expectedKind)) {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(kindNode, 1),
			Code:    "schema.kind_mismatch",
			Message: fmt.Sprintf("kind %q does not match template kind %q", strings.TrimSpace(kindNode.Value), strings.TrimSpace(expectedKind)),
		})
	}

	metadataNode := lookupMapValue(manifest, "metadata")
	if metadataNode == nil || metadataNode.Kind != yaml.MappingNode {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(metadataNode, lineFromNode(manifest, 1)),
			Code:    "schema.metadata",
			Message: "metadata object is required",
		})
		return issues
	}
	nameNode := lookupMapValue(metadataNode, "name")
	if !isNonEmptyScalar(nameNode) {
		issues = append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(nameNode, lineFromNode(metadataNode, 1)),
			Code:    "schema.metadata_name",
			Message: "metadata.name is required",
		})
	}
	if !strings.EqualFold(strings.TrimSpace(expectedKind), "Namespace") {
		namespaceNode := lookupMapValue(metadataNode, "namespace")
		if !isNonEmptyScalar(namespaceNode) {
			issues = append(issues, ManifestTemplateValidationIssue{
				File:    file,
				Line:    lineFromNode(namespaceNode, lineFromNode(metadataNode, 1)),
				Code:    "schema.metadata_namespace",
				Message: "metadata.namespace is required for namespaced resources",
			})
		}
	}

	switch strings.TrimSpace(expectedKind) {
	case "Deployment":
		issues = append(issues, validateDeploymentSchema(file, manifest)...)
	case "Service":
		issues = append(issues, validateServiceSchema(file, manifest)...)
	case "Ingress":
		issues = append(issues, validateIngressSchema(file, manifest)...)
	case "ResourceQuota":
		issues = append(issues, validateResourceQuotaSchema(file, manifest)...)
	case "LimitRange":
		issues = append(issues, validateLimitRangeSchema(file, manifest)...)
	case "NetworkPolicy":
		issues = append(issues, validateNetworkPolicySchema(file, manifest)...)
	}
	return issues
}

func validateDeploymentSchema(file string, manifest *yaml.Node) []ManifestTemplateValidationIssue {
	issues := make([]ManifestTemplateValidationIssue, 0)
	specNode := lookupMapValue(manifest, "spec")
	if specNode == nil || specNode.Kind != yaml.MappingNode {
		return append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(specNode, lineFromNode(manifest, 1)),
			Code:    "schema.deployment.spec",
			Message: "Deployment spec is required",
		})
	}
	templateNode := lookupMapValue(specNode, "template")
	templateSpecNode := lookupMapValue(templateNode, "spec")
	containersNode := lookupMapValue(templateSpecNode, "containers")
	if containersNode == nil || containersNode.Kind != yaml.SequenceNode || len(containersNode.Content) == 0 {
		return append(issues, ManifestTemplateValidationIssue{
			File:    file,
			Line:    lineFromNode(containersNode, lineFromNode(templateSpecNode, lineFromNode(specNode, 1))),
			Code:    "schema.deployment.containers",
			Message: "Deployment spec.template.spec.containers must contain at least one container",
		})
	}
	for _, item := range containersNode.Content {
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}
		nameNode := lookupMapValue(item, "name")
		if !isNonEmptyScalar(nameNode) {
			issues = append(issues, ManifestTemplateValidationIssue{
				File:    file,
				Line:    lineFromNode(nameNode, lineFromNode(item, 1)),
				Code:    "schema.deployment.container_name",
				Message: "container name is required",
			})
		}
		imageNode := lookupMapValue(item, "image")
		if !isNonEmptyScalar(imageNode) {
			issues = append(issues, ManifestTemplateValidationIssue{
				File:    file,
				Line:    lineFromNode(imageNode, lineFromNode(item, 1)),
				Code:    "schema.deployment.container_image",
				Message: "container image is required",
			})
		}
	}
	return issues
}

func validateServiceSchema(file string, manifest *yaml.Node) []ManifestTemplateValidationIssue {
	specNode := lookupMapValue(manifest, "spec")
	portsNode := lookupMapValue(specNode, "ports")
	if portsNode == nil || portsNode.Kind != yaml.SequenceNode || len(portsNode.Content) == 0 {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(portsNode, lineFromNode(specNode, lineFromNode(manifest, 1))),
			Code:    "schema.service.ports",
			Message: "Service spec.ports must contain at least one entry",
		}}
	}
	return nil
}

func validateIngressSchema(file string, manifest *yaml.Node) []ManifestTemplateValidationIssue {
	specNode := lookupMapValue(manifest, "spec")
	if specNode == nil || specNode.Kind != yaml.MappingNode {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(specNode, lineFromNode(manifest, 1)),
			Code:    "schema.ingress.spec",
			Message: "Ingress spec is required",
		}}
	}
	rulesNode := lookupMapValue(specNode, "rules")
	tlsNode := lookupMapValue(specNode, "tls")
	hasRules := rulesNode != nil && rulesNode.Kind == yaml.SequenceNode && len(rulesNode.Content) > 0
	hasTLS := tlsNode != nil && tlsNode.Kind == yaml.SequenceNode && len(tlsNode.Content) > 0
	if !hasRules && !hasTLS {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(specNode, 1),
			Code:    "schema.ingress.routes",
			Message: "Ingress must include at least one rule or TLS host entry",
		}}
	}
	return nil
}

func validateResourceQuotaSchema(file string, manifest *yaml.Node) []ManifestTemplateValidationIssue {
	specNode := lookupMapValue(manifest, "spec")
	hardNode := lookupMapValue(specNode, "hard")
	if hardNode == nil || hardNode.Kind != yaml.MappingNode || len(hardNode.Content) == 0 {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(hardNode, lineFromNode(specNode, lineFromNode(manifest, 1))),
			Code:    "schema.resourcequota.hard",
			Message: "ResourceQuota spec.hard must define at least one quota",
		}}
	}
	return nil
}

func validateLimitRangeSchema(file string, manifest *yaml.Node) []ManifestTemplateValidationIssue {
	specNode := lookupMapValue(manifest, "spec")
	limitsNode := lookupMapValue(specNode, "limits")
	if limitsNode == nil || limitsNode.Kind != yaml.SequenceNode || len(limitsNode.Content) == 0 {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(limitsNode, lineFromNode(specNode, lineFromNode(manifest, 1))),
			Code:    "schema.limitrange.limits",
			Message: "LimitRange spec.limits must contain at least one limit",
		}}
	}
	return nil
}

func validateNetworkPolicySchema(file string, manifest *yaml.Node) []ManifestTemplateValidationIssue {
	specNode := lookupMapValue(manifest, "spec")
	if specNode == nil || specNode.Kind != yaml.MappingNode {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(specNode, lineFromNode(manifest, 1)),
			Code:    "schema.networkpolicy.spec",
			Message: "NetworkPolicy spec is required",
		}}
	}
	podSelectorNode := lookupMapValue(specNode, "podSelector")
	if podSelectorNode == nil || podSelectorNode.Kind != yaml.MappingNode {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(podSelectorNode, lineFromNode(specNode, 1)),
			Code:    "schema.networkpolicy.pod_selector",
			Message: "NetworkPolicy spec.podSelector is required",
		}}
	}
	policyTypesNode := lookupMapValue(specNode, "policyTypes")
	if policyTypesNode == nil || policyTypesNode.Kind != yaml.SequenceNode || len(policyTypesNode.Content) == 0 {
		return []ManifestTemplateValidationIssue{{
			File:    file,
			Line:    lineFromNode(policyTypesNode, lineFromNode(specNode, 1)),
			Code:    "schema.networkpolicy.policy_types",
			Message: "NetworkPolicy spec.policyTypes must contain at least one type",
		}}
	}
	return nil
}

func rootDocumentMapping(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}
	node := root
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		node = root.Content[0]
	}
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func lookupMapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		keyNode := node.Content[index]
		valueNode := node.Content[index+1]
		if keyNode != nil && keyNode.Kind == yaml.ScalarNode && strings.TrimSpace(keyNode.Value) == key {
			return valueNode
		}
	}
	return nil
}

func isNonEmptyScalar(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.ScalarNode {
		return false
	}
	return strings.TrimSpace(node.Value) != ""
}

func lineFromNode(node *yaml.Node, fallback int) int {
	if node != nil && node.Line > 0 {
		return node.Line
	}
	if fallback > 0 {
		return fallback
	}
	return 1
}

func parseYAMLLine(message string) int {
	match := yamlLinePattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func lineOfFirst(text string, fragment string) int {
	if strings.TrimSpace(fragment) == "" {
		return 1
	}
	index := strings.Index(text, fragment)
	if index < 0 {
		return 1
	}
	return 1 + strings.Count(text[:index], "\n")
}

func manifestTemplateFile(item ManifestTemplate) string {
	namespace := strings.TrimSpace(item.Namespace)
	if namespace == "" {
		namespace = "cluster"
	}
	kind := strings.ToLower(strings.TrimSpace(item.Kind))
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = "resource"
	}
	if kind == "" {
		kind = "resource"
	}
	return namespace + "/" + kind + "-" + name + ".yaml"
}
