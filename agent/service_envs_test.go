package agent

import (
	"testing"

	"envpilot/internal/domain"
)

func TestBuildServiceEnvironmentVariablesGroupsByServiceAndContainer(t *testing.T) {
	snapshots := []domain.ResourceSnapshot{
		{
			Kind:      "Service",
			Namespace: "dev",
			Name:      "orders",
		},
		{
			Kind:      "Deployment",
			Namespace: "dev",
			Name:      "orders",
			Containers: []domain.ResourceContainerEnv{
				{
					Name: "api",
					EnvVars: []domain.ResourceEnvVar{
						{Name: "STATIC", Value: "1"},
						{Name: "POD_NAME", ValueFrom: "valueFrom", ValueFromKind: "fieldRef", ValueFromField: "metadata.name"},
						{Name: "DB_PASSWORD", ValueFrom: "valueFrom", ValueFromKind: "secretKeyRef", ValueFromName: "orders-secret", ValueFromKey: "password"},
					},
					EnvFrom: []domain.ResourceEnvFromRef{
						{Kind: "ConfigMap", Name: "orders-config"},
						{Kind: "Secret", Name: "orders-secret"},
					},
				},
				{
					Name: "worker",
					EnvVars: []domain.ResourceEnvVar{
						{Name: "QUEUE_URL", Value: "nats://shared"},
					},
				},
			},
			SourceMapping: &domain.ResourceSourceMapping{Kind: "HelmRelease", Name: "orders"},
		},
	}
	graph := domain.ServiceGraph{
		Nodes: []domain.ServiceGraphNode{
			{ID: "Service/dev/orders", Kind: "Service", Namespace: "dev", Name: "orders"},
			{ID: "Deployment/dev/orders", Kind: "Deployment", Namespace: "dev", Name: "orders"},
		},
		Edges: []domain.ServiceGraphEdge{
			{From: "Service/dev/orders", To: "Deployment/dev/orders", Type: "selects", Confidence: 1},
		},
	}
	envs := BuildServiceEnvironmentVariables(snapshots, graph)
	if len(envs.Services) != 1 {
		t.Fatalf("services len=%d", len(envs.Services))
	}
	service := envs.Services[0]
	if service.ServiceName != "orders" || service.Namespace != "dev" {
		t.Fatalf("service=%#v", service)
	}
	if len(service.Containers) != 2 {
		t.Fatalf("containers=%#v", service.Containers)
	}
	api := service.Containers[0]
	if api.Container != "api" {
		api = service.Containers[1]
	}
	if api.Container != "api" {
		t.Fatalf("api container missing: %#v", service.Containers)
	}
	if len(api.EnvFrom) != 2 {
		t.Fatalf("envFrom=%#v", api.EnvFrom)
	}
	if api.EnvFrom[0].SourceType == "" || api.EnvFrom[1].SourceType == "" {
		t.Fatalf("envFrom source types not set: %#v", api.EnvFrom)
	}
	var secretVar domain.ResourceEnvVar
	foundSecret := false
	for _, item := range api.Vars {
		if item.Name == "DB_PASSWORD" {
			secretVar = item
			foundSecret = true
			break
		}
	}
	if !foundSecret {
		t.Fatalf("secret env var not found: %#v", api.Vars)
	}
	if secretVar.Value != "" {
		t.Fatalf("secret value must not be resolved, got value=%q", secretVar.Value)
	}
	if secretVar.ValueFromKind != "secretKeyRef" || secretVar.ValueFromName != "orders-secret" || secretVar.ValueFromKey != "password" {
		t.Fatalf("secret valueFrom not preserved: %#v", secretVar)
	}
}

func TestBuildServiceEnvironmentVariablesPreservesDynamicValueFrom(t *testing.T) {
	snapshots := []domain.ResourceSnapshot{
		{Kind: "Service", Namespace: "dev", Name: "api"},
		{
			Kind:      "Deployment",
			Namespace: "dev",
			Name:      "api",
			Containers: []domain.ResourceContainerEnv{
				{
					Name: "api",
					EnvVars: []domain.ResourceEnvVar{
						{Name: "POD_IP", ValueFrom: "valueFrom", ValueFromKind: "fieldRef", ValueFromField: "status.podIP"},
						{Name: "CPU_LIMIT", ValueFrom: "valueFrom", ValueFromKind: "resourceFieldRef", ValueFromField: "limits.cpu", ValueFromPath: "1m"},
						{Name: "CONFIG_VERSION", ValueFrom: "valueFrom", ValueFromKind: "configMapKeyRef", ValueFromName: "api-config", ValueFromKey: "version"},
					},
				},
			},
		},
	}
	graph := domain.ServiceGraph{
		Nodes: []domain.ServiceGraphNode{
			{ID: "Service/dev/api", Kind: "Service", Namespace: "dev", Name: "api"},
			{ID: "Deployment/dev/api", Kind: "Deployment", Namespace: "dev", Name: "api"},
		},
		Edges: []domain.ServiceGraphEdge{
			{From: "Service/dev/api", To: "Deployment/dev/api", Type: "selects", Confidence: 1},
		},
	}
	envs := BuildServiceEnvironmentVariables(snapshots, graph)
	if len(envs.Services) != 1 || len(envs.Services[0].Containers) != 1 {
		t.Fatalf("envs=%#v", envs)
	}
	vars := envs.Services[0].Containers[0].Vars
	if len(vars) != 3 {
		t.Fatalf("vars=%#v", vars)
	}
	assertHasValueFrom := func(kind, field, name, key string) {
		t.Helper()
		for _, item := range vars {
			if item.ValueFromKind == kind {
				if field != "" && item.ValueFromField != field {
					t.Fatalf("kind=%s field=%q got=%q", kind, field, item.ValueFromField)
				}
				if name != "" && item.ValueFromName != name {
					t.Fatalf("kind=%s name=%q got=%q", kind, name, item.ValueFromName)
				}
				if key != "" && item.ValueFromKey != key {
					t.Fatalf("kind=%s key=%q got=%q", kind, key, item.ValueFromKey)
				}
				return
			}
		}
		t.Fatalf("kind=%s not found in %#v", kind, vars)
	}
	assertHasValueFrom("fieldRef", "status.podIP", "", "")
	assertHasValueFrom("resourceFieldRef", "limits.cpu", "", "")
	assertHasValueFrom("configMapKeyRef", "", "api-config", "version")
}
