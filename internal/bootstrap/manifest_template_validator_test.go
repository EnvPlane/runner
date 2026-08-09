package bootstrap

import "testing"

func TestValidateManifestTemplatesRejectsInvalidYAML(t *testing.T) {
	result := ValidateManifestTemplates([]ManifestTemplate{
		{
			Kind:      "Deployment",
			Namespace: "envpilot-pr-{{ .PRNumber }}",
			Name:      "orders",
			YAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders
  namespace: envpilot-pr-{{ .PRNumber }}
spec:
  template:
    spec:
      containers:
        - name: orders
          image ghcr.io/acme/orders:{{ .CommitSHA }}`,
		},
	})
	if result.Valid {
		t.Fatalf("expected invalid result")
	}
	if len(result.Issues) == 0 {
		t.Fatalf("expected syntax issues")
	}
	if result.Issues[0].Code != "yaml.syntax" {
		t.Fatalf("expected yaml.syntax, got %q", result.Issues[0].Code)
	}
}

func TestValidateManifestTemplatesRejectsMissingVariables(t *testing.T) {
	result := ValidateManifestTemplates([]ManifestTemplate{
		{
			Kind:      "Deployment",
			Namespace: "dev-base",
			Name:      "orders",
			YAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders
  namespace: dev-base
spec:
  template:
    spec:
      containers:
        - name: orders
          image: ghcr.io/acme/orders:latest`,
		},
	})
	if result.Valid {
		t.Fatalf("expected invalid result")
	}
	foundPR := false
	foundCommit := false
	for _, issue := range result.Issues {
		if issue.Code == "template.required_variable" && issue.Message == "missing required EnvPlane variable {{ .PRNumber }}" {
			foundPR = true
		}
		if issue.Code == "template.required_variable" && issue.Message == "missing required EnvPlane variable {{ .CommitSHA }} for Deployment" {
			foundCommit = true
		}
	}
	if !foundPR || !foundCommit {
		t.Fatalf("expected missing PRNumber and CommitSHA issues, got %+v", result.Issues)
	}
}

func TestValidateManifestTemplatesRejectsSchemaError(t *testing.T) {
	result := ValidateManifestTemplates([]ManifestTemplate{
		{
			Kind:      "Service",
			Namespace: "envpilot-pr-{{ .PRNumber }}",
			Name:      "orders",
			YAML: `apiVersion: v1
kind: Service
metadata:
  name: orders
  namespace: envpilot-pr-{{ .PRNumber }}
spec:
  selector:
    app: orders`,
		},
	})
	if result.Valid {
		t.Fatalf("expected invalid result")
	}
	foundSchema := false
	for _, issue := range result.Issues {
		if issue.Code == "schema.service.ports" {
			foundSchema = true
			break
		}
	}
	if !foundSchema {
		t.Fatalf("expected schema.service.ports issue, got %+v", result.Issues)
	}
}

func TestValidateManifestTemplatesAcceptsValidTemplates(t *testing.T) {
	result := ValidateManifestTemplates([]ManifestTemplate{
		{
			Kind:      "Deployment",
			Namespace: "envpilot-pr-{{ .PRNumber }}",
			Name:      "orders",
			YAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders
  namespace: envpilot-pr-{{ .PRNumber }}
spec:
  template:
    spec:
      containers:
        - name: orders
          image: ghcr.io/acme/orders:{{ .CommitSHA }}`,
		},
		{
			Kind:      "Service",
			Namespace: "envpilot-pr-{{ .PRNumber }}",
			Name:      "orders",
			YAML: `apiVersion: v1
kind: Service
metadata:
  name: orders
  namespace: envpilot-pr-{{ .PRNumber }}
spec:
  ports:
    - port: 80`,
		},
	})
	if !result.Valid {
		t.Fatalf("expected valid result, got %+v", result.Issues)
	}
}
