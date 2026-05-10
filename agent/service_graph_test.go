package agent

import (
	"testing"

	"envpilot/internal/domain"
)

func TestBuildServiceGraphMapsServicesIngressAndEnvDependencies(t *testing.T) {
	graph := BuildServiceGraph([]domain.ResourceSnapshot{
		{
			Kind:      "Service",
			Namespace: "dev-base",
			Name:      "orders",
			Selector:  map[string]string{"app": "orders"},
		},
		{
			Kind:      "Service",
			Namespace: "dev-base",
			Name:      "payments",
			Selector:  map[string]string{"app": "payments"},
		},
		{
			Kind:      "Deployment",
			Namespace: "dev-base",
			Name:      "orders",
			PodLabels: map[string]string{"app": "orders"},
			EnvVars: []domain.ResourceEnvVar{
				{Name: "PAYMENTS_URL", Value: "http://payments.dev-base.svc.cluster.local:8080"},
			},
		},
		{
			Kind:      "Ingress",
			Namespace: "dev-base",
			Name:      "orders-public",
			IngressRules: []domain.ResourceIngressRule{
				{Host: "preview.example.com", Path: "/orders", ServiceName: "orders", ServicePort: "80"},
			},
		},
	})

	assertEdge(t, graph, "Service/dev-base/orders", "Deployment/dev-base/orders", "selects", 1)
	assertEdge(t, graph, "Ingress/dev-base/orders-public", "Service/dev-base/orders", "routes-to", 1)
	assertEdge(t, graph, "Deployment/dev-base/orders", "Service/dev-base/payments", "depends-on", 0.95)
}

func TestBuildServiceGraphMarksAmbiguousEnvDependenciesWithConfidence(t *testing.T) {
	graph := BuildServiceGraph([]domain.ResourceSnapshot{
		{
			Kind:      "Service",
			Namespace: "dev-base",
			Name:      "redis",
		},
		{
			Kind:      "Deployment",
			Namespace: "dev-base",
			Name:      "worker",
			EnvVars: []domain.ResourceEnvVar{
				{Name: "REDIS_HOST"},
			},
			EnvFrom: []domain.ResourceEnvFromRef{
				{Kind: "ConfigMap", Name: "redis-client-config"},
			},
		},
	})

	assertEdge(t, graph, "Deployment/dev-base/worker", "Service/dev-base/redis", "depends-on", 0.7)
}

func TestBuildServiceGraphUsesOwnerReferences(t *testing.T) {
	graph := BuildServiceGraph([]domain.ResourceSnapshot{
		{
			Kind:      "ReplicaSet",
			Namespace: "dev-base",
			Name:      "orders-abc",
			OwnerReferences: []domain.ResourceOwnerReference{
				{Kind: "Deployment", Name: "orders"},
			},
		},
		{
			Kind:      "Deployment",
			Namespace: "dev-base",
			Name:      "orders",
		},
	})

	assertEdge(t, graph, "ReplicaSet/dev-base/orders-abc", "Deployment/dev-base/orders", "owned-by", 1)
}

func assertEdge(t *testing.T, graph domain.ServiceGraph, from string, to string, edgeType string, minConfidence float64) {
	t.Helper()
	for _, edge := range graph.Edges {
		if edge.From == from && edge.To == to && edge.Type == edgeType {
			if edge.Confidence < minConfidence {
				t.Fatalf("edge %s -> %s type=%s confidence=%f, want >= %f", from, to, edgeType, edge.Confidence, minConfidence)
			}
			return
		}
	}
	t.Fatalf("edge %s -> %s type=%s missing; edges=%#v", from, to, edgeType, graph.Edges)
}
