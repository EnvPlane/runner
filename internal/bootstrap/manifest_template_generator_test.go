package bootstrap

import (
	"strings"
	"testing"

	"github.com/envpilot/runner/internal/domain"
)

func TestGenerateManifestTemplatesRewritesAndIsDeterministic(t *testing.T) {
	snapshots := []domain.ResourceSnapshot{
		{
			Kind:      "Namespace",
			Namespace: "dev-base",
			Name:      "dev-base",
			Manifest: map[string]any{
				"apiVersion": "v1",
				"kind":       "Namespace",
				"metadata": map[string]any{
					"name": "dev-base",
				},
			},
		},
		{
			Kind:      "Deployment",
			Namespace: "dev-base",
			Name:      "orders",
			Manifest: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name":      "orders",
					"namespace": "dev-base",
				},
				"spec": map[string]any{
					"replicas": float64(1),
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":  "orders",
									"image": "ghcr.io/acme/orders:abc123",
								},
							},
						},
					},
				},
			},
		},
		{
			Kind:      "Service",
			Namespace: "dev-base",
			Name:      "orders",
			Manifest: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]any{
					"name":      "orders",
					"namespace": "dev-base",
				},
				"spec": map[string]any{
					"ports": []any{
						map[string]any{
							"name":       "http",
							"port":       float64(80),
							"targetPort": float64(8080),
						},
					},
				},
			},
		},
		{
			Kind:      "Ingress",
			Namespace: "dev-base",
			Name:      "orders",
			Manifest: map[string]any{
				"apiVersion": "networking.k8s.io/v1",
				"kind":       "Ingress",
				"metadata": map[string]any{
					"name":      "orders",
					"namespace": "dev-base",
				},
				"spec": map[string]any{
					"rules": []any{
						map[string]any{
							"host": "orders.dev-base.local",
							"http": map[string]any{
								"paths": []any{
									map[string]any{
										"path": "/",
										"backend": map[string]any{
											"service": map[string]any{
												"name": "orders",
												"port": map[string]any{
													"number": float64(80),
												},
											},
										},
									},
								},
							},
						},
					},
					"tls": []any{
						map[string]any{
							"hosts": []any{"orders.dev-base.local"},
						},
					},
				},
			},
		},
		{
			Kind:      "ConfigMap",
			Namespace: "dev-base",
			Name:      "orders-config",
			Manifest: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "orders-config",
					"namespace": "dev-base",
				},
				"data": map[string]any{
					"FEATURE_X": "on",
				},
			},
		},
		{
			Kind:      "ResourceQuota",
			Namespace: "dev-base",
			Name:      "rq",
			Manifest: map[string]any{
				"apiVersion": "v1",
				"kind":       "ResourceQuota",
				"metadata": map[string]any{
					"name":      "rq",
					"namespace": "dev-base",
				},
				"spec": map[string]any{
					"hard": map[string]any{
						"limits.cpu": "2",
					},
				},
			},
		},
		{
			Kind:      "LimitRange",
			Namespace: "dev-base",
			Name:      "limits",
			Manifest: map[string]any{
				"apiVersion": "v1",
				"kind":       "LimitRange",
				"metadata": map[string]any{
					"name":      "limits",
					"namespace": "dev-base",
				},
				"spec": map[string]any{
					"limits": []any{
						map[string]any{
							"type": "Container",
						},
					},
				},
			},
		},
		{
			Kind:      "Job",
			Namespace: "dev-base",
			Name:      "cleanup",
			Manifest: map[string]any{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"metadata": map[string]any{
					"name":      "cleanup",
					"namespace": "dev-base",
				},
			},
		},
	}

	selections := map[string]ResourceSelection{
		"Service/dev-base/orders": {Include: true, Strategy: "clone"},
		"Job/dev-base/cleanup":    {Include: true, Strategy: "override per PR"},
	}
	options := ManifestTemplateGeneratorOptions{
		FeatureNamespaceTemplate: "envpilot-pr-{{ .PRNumber }}",
		CommitSHAPlaceholder:     "{{ .CommitSHA }}",
		PreviewDomain:            "preview.example.com",
		Labels: map[string]string{
			"envpilot.io/project": "checkout",
		},
		Annotations: map[string]string{
			"team": "platform",
		},
	}

	first, err := GenerateManifestTemplates(snapshots, selections, options)
	if err != nil {
		t.Fatalf("generate templates: %v", err)
	}
	second, err := GenerateManifestTemplates(snapshots, selections, options)
	if err != nil {
		t.Fatalf("generate templates second run: %v", err)
	}

	if len(first) != 7 {
		t.Fatalf("expected 7 templates, got %d", len(first))
	}
	if strings.Join(extractYAML(first), "\n---\n") != strings.Join(extractYAML(second), "\n---\n") {
		t.Fatalf("templates are not deterministic")
	}

	byKind := map[string]ManifestTemplate{}
	for _, item := range first {
		byKind[item.Kind] = item
	}

	deploymentYAML := byKind["Deployment"].YAML
	if !strings.Contains(deploymentYAML, `namespace: "envpilot-pr-{{ .PRNumber }}"`) {
		t.Fatalf("deployment namespace rewrite missing: %s", deploymentYAML)
	}
	if !strings.Contains(deploymentYAML, `image: "ghcr.io/acme/orders:{{ .CommitSHA }}"`) {
		t.Fatalf("deployment image rewrite missing: %s", deploymentYAML)
	}
	if !strings.Contains(deploymentYAML, "envpilot.io/managed: true") {
		t.Fatalf("deployment envpilot label missing: %s", deploymentYAML)
	}

	expectedIngressYAML := "" +
		"apiVersion: networking.k8s.io/v1\n" +
		"kind: Ingress\n" +
		"metadata:\n" +
		"  annotations:\n" +
		"    envpilot.io/generated-from-discovery: true\n" +
		"    team: platform\n" +
		"  labels:\n" +
		"    app.kubernetes.io/managed-by: envpilot\n" +
		"    envpilot.io/managed: true\n" +
		"    envpilot.io/project: checkout\n" +
		"  name: orders\n" +
		"  namespace: \"envpilot-pr-{{ .PRNumber }}\"\n" +
		"spec:\n" +
		"  rules:\n" +
		"    -\n" +
		"      host: orders.preview.example.com\n" +
		"      http:\n" +
		"        paths:\n" +
		"          -\n" +
		"            backend:\n" +
		"              service:\n" +
		"                name: orders\n" +
		"                port:\n" +
		"                  number: 80\n" +
		"            path: /\n" +
		"  tls:\n" +
		"    -\n" +
		"      hosts:\n" +
		"        - orders.preview.example.com\n"
	if byKind["Ingress"].YAML != expectedIngressYAML {
		t.Fatalf("unexpected ingress yaml:\n%s", byKind["Ingress"].YAML)
	}

	expectedNamespaceYAML := "" +
		"apiVersion: v1\n" +
		"kind: Namespace\n" +
		"metadata:\n" +
		"  annotations:\n" +
		"    envpilot.io/generated-from-discovery: true\n" +
		"    team: platform\n" +
		"  labels:\n" +
		"    app.kubernetes.io/managed-by: envpilot\n" +
		"    envpilot.io/managed: true\n" +
		"    envpilot.io/project: checkout\n" +
		"  name: \"envpilot-pr-{{ .PRNumber }}\"\n"
	if byKind["Namespace"].YAML != expectedNamespaceYAML {
		t.Fatalf("unexpected namespace yaml:\n%s", byKind["Namespace"].YAML)
	}
}

func TestGenerateManifestTemplatesSkipsBaseStrategies(t *testing.T) {
	snapshots := []domain.ResourceSnapshot{
		{
			Kind:      "Service",
			Namespace: "dev-base",
			Name:      "auth",
			Manifest: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]any{
					"name":      "auth",
					"namespace": "dev-base",
				},
			},
		},
	}
	selections := map[string]ResourceSelection{
		"Service/dev-base/auth": {Include: true, Strategy: "use base"},
	}
	options := ManifestTemplateGeneratorOptions{
		FeatureNamespaceTemplate: "envpilot-pr-{{ .PRNumber }}",
	}
	items, err := GenerateManifestTemplates(snapshots, selections, options)
	if err != nil {
		t.Fatalf("generate templates: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no templates for use base strategy, got %d", len(items))
	}
}

func TestGenerateNetworkPolicyTemplatesForRestrictedMode(t *testing.T) {
	items, err := GenerateNetworkPolicyTemplates(
		NetworkPolicyConfig{
			FeatureToBase:  true,
			BaseToFeature:  true,
			EgressMode:     "restricted",
			BaseNamespaces: []string{"dev-base"},
		},
		"envpilot-pr-{{ .PRNumber }}",
		map[string]string{"envpilot.io/project": "checkout"},
		nil,
	)
	if err != nil {
		t.Fatalf("generate network policies: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 network policies without base namespace writes, got %d", len(items))
	}
	byName := map[string]ManifestTemplate{}
	for _, item := range items {
		byName[item.Namespace+"/"+item.Name] = item
	}
	featureIngress := byName["envpilot-pr-{{ .PRNumber }}/envpilot-allow-base-to-feature"].YAML
	if !strings.Contains(featureIngress, "kubernetes.io/metadata.name: dev-base") {
		t.Fatalf("feature ingress policy missing base namespace selector:\n%s", featureIngress)
	}
	if _, exists := byName["dev-base/envpilot-allow-feature-to-base"]; exists {
		t.Fatalf("did not expect base namespace policy without explicit opt-in")
	}
	egress := byName["envpilot-pr-{{ .PRNumber }}/envpilot-feature-egress"].YAML
	if !strings.Contains(egress, "k8s-app: kube-dns") {
		t.Fatalf("restricted egress policy missing DNS rule:\n%s", egress)
	}
	if !strings.Contains(egress, "kubernetes.io/metadata.name: dev-base") {
		t.Fatalf("restricted egress policy missing base namespace rule:\n%s", egress)
	}
}

func TestGenerateNetworkPolicyTemplatesIncludesBaseNamespacePoliciesWhenExplicitlyAllowed(t *testing.T) {
	items, err := GenerateNetworkPolicyTemplates(
		NetworkPolicyConfig{
			FeatureToBase:              true,
			BaseToFeature:              true,
			EgressMode:                 "restricted",
			BaseNamespaces:             []string{"dev-base"},
			AllowBaseNamespacePolicies: true,
		},
		"envpilot-pr-{{ .PRNumber }}",
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("generate network policies: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 network policies with explicit base namespace opt-in, got %d", len(items))
	}
	byName := map[string]ManifestTemplate{}
	for _, item := range items {
		byName[item.Namespace+"/"+item.Name] = item
	}
	baseIngress := byName["dev-base/envpilot-allow-feature-to-base"].YAML
	if !strings.Contains(baseIngress, `kubernetes.io/metadata.name: "envpilot-pr-{{ .PRNumber }}"`) {
		t.Fatalf("base ingress policy missing feature namespace selector:\n%s", baseIngress)
	}
}

func TestGenerateNetworkPolicyTemplatesForDenyAllEgress(t *testing.T) {
	items, err := GenerateNetworkPolicyTemplates(
		NetworkPolicyConfig{
			FeatureToBase:  false,
			BaseToFeature:  false,
			EgressMode:     "deny all",
			BaseNamespaces: []string{"dev-base"},
		},
		"envpilot-pr-{{ .PRNumber }}",
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("generate network policies: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one egress policy, got %d", len(items))
	}
	if items[0].Name != "envpilot-feature-egress" {
		t.Fatalf("expected egress policy, got %q", items[0].Name)
	}
	if !strings.Contains(items[0].YAML, "egress: []") {
		t.Fatalf("deny all egress policy should render empty egress list:\n%s", items[0].YAML)
	}
}

func TestGenerateNetworkPolicyTemplatesRejectsInvalidMode(t *testing.T) {
	_, err := GenerateNetworkPolicyTemplates(
		NetworkPolicyConfig{
			FeatureToBase:  true,
			EgressMode:     "invalid",
			BaseNamespaces: []string{"dev-base"},
		},
		"envpilot-pr-{{ .PRNumber }}",
		nil,
		nil,
	)
	if err == nil {
		t.Fatalf("expected invalid egress mode error")
	}
}

func extractYAML(items []ManifestTemplate) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.YAML)
	}
	return result
}
