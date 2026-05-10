package agent

import (
	"sort"
	"strings"

	"envpilot/internal/domain"
)

func BuildServiceGraph(snapshots []domain.ResourceSnapshot) domain.ServiceGraph {
	nodes := make(map[string]domain.ServiceGraphNode)
	services := make([]domain.ResourceSnapshot, 0)
	workloads := make([]domain.ResourceSnapshot, 0)
	ingresses := make([]domain.ResourceSnapshot, 0)

	byKindNamespaceName := make(map[string]domain.ResourceSnapshot, len(snapshots))
	servicesByNamespace := make(map[string][]domain.ResourceSnapshot)

	for _, snapshot := range snapshots {
		if snapshot.Kind == "" || snapshot.Name == "" || snapshot.Namespace == "" {
			continue
		}
		id := serviceGraphNodeID(snapshot.Kind, snapshot.Namespace, snapshot.Name)
		nodes[id] = domain.ServiceGraphNode{
			ID:        id,
			Kind:      snapshot.Kind,
			Namespace: snapshot.Namespace,
			Name:      snapshot.Name,
			Labels:    snapshot.Labels,
		}
		byKindNamespaceName[id] = snapshot
		switch snapshot.Kind {
		case "Service":
			services = append(services, snapshot)
			servicesByNamespace[snapshot.Namespace] = append(servicesByNamespace[snapshot.Namespace], snapshot)
		case "Deployment", "StatefulSet", "Job", "CronJob":
			workloads = append(workloads, snapshot)
		case "Ingress":
			ingresses = append(ingresses, snapshot)
		}
	}

	edgeSet := make(map[string]domain.ServiceGraphEdge)
	addEdge := func(edge domain.ServiceGraphEdge) {
		if edge.From == "" || edge.To == "" || edge.Type == "" {
			return
		}
		if edge.Confidence <= 0 {
			edge.Confidence = 0.5
		}
		key := edge.From + "\x00" + edge.To + "\x00" + edge.Type + "\x00" + edge.Reason
		if existing, ok := edgeSet[key]; ok && existing.Confidence >= edge.Confidence {
			return
		}
		edgeSet[key] = edge
	}

	for _, ingress := range ingresses {
		from := serviceGraphNodeID(ingress.Kind, ingress.Namespace, ingress.Name)
		for _, rule := range ingress.IngressRules {
			target, ok := findServiceByName(servicesByNamespace[ingress.Namespace], rule.ServiceName)
			if !ok {
				continue
			}
			addEdge(domain.ServiceGraphEdge{
				From:       from,
				To:         serviceGraphNodeID(target.Kind, target.Namespace, target.Name),
				Type:       "routes-to",
				Reason:     ingressRouteReason(rule),
				Confidence: 1,
			})
		}
	}

	for _, service := range services {
		if len(service.Selector) == 0 {
			continue
		}
		from := serviceGraphNodeID(service.Kind, service.Namespace, service.Name)
		for _, workload := range workloads {
			if service.Namespace != workload.Namespace {
				continue
			}
			if selectorMatchesLabels(service.Selector, firstNonEmptyMap(workload.PodLabels, workload.Labels)) {
				addEdge(domain.ServiceGraphEdge{
					From:       from,
					To:         serviceGraphNodeID(workload.Kind, workload.Namespace, workload.Name),
					Type:       "selects",
					Reason:     "service selector matches workload labels",
					Confidence: 1,
				})
			}
		}
	}

	for _, snapshot := range snapshots {
		from := serviceGraphNodeID(snapshot.Kind, snapshot.Namespace, snapshot.Name)
		for _, owner := range snapshot.OwnerReferences {
			targetID := serviceGraphNodeID(owner.Kind, snapshot.Namespace, owner.Name)
			if _, ok := byKindNamespaceName[targetID]; !ok {
				continue
			}
			addEdge(domain.ServiceGraphEdge{
				From:       from,
				To:         targetID,
				Type:       "owned-by",
				Reason:     "ownerReference",
				Confidence: 1,
			})
		}
	}

	for _, workload := range workloads {
		from := serviceGraphNodeID(workload.Kind, workload.Namespace, workload.Name)
		for _, service := range servicesByNamespace[workload.Namespace] {
			confidence, reason := inferDependencyConfidence(workload, service)
			if confidence <= 0 {
				continue
			}
			addEdge(domain.ServiceGraphEdge{
				From:       from,
				To:         serviceGraphNodeID(service.Kind, service.Namespace, service.Name),
				Type:       "depends-on",
				Reason:     reason,
				Confidence: confidence,
			})
		}
	}

	graphNodes := make([]domain.ServiceGraphNode, 0, len(nodes))
	for _, node := range nodes {
		graphNodes = append(graphNodes, node)
	}
	sort.Slice(graphNodes, func(i, j int) bool { return graphNodes[i].ID < graphNodes[j].ID })

	graphEdges := make([]domain.ServiceGraphEdge, 0, len(edgeSet))
	for _, edge := range edgeSet {
		graphEdges = append(graphEdges, edge)
	}
	sort.Slice(graphEdges, func(i, j int) bool {
		if graphEdges[i].From != graphEdges[j].From {
			return graphEdges[i].From < graphEdges[j].From
		}
		if graphEdges[i].To != graphEdges[j].To {
			return graphEdges[i].To < graphEdges[j].To
		}
		if graphEdges[i].Type != graphEdges[j].Type {
			return graphEdges[i].Type < graphEdges[j].Type
		}
		return graphEdges[i].Reason < graphEdges[j].Reason
	})

	return domain.ServiceGraph{Nodes: graphNodes, Edges: graphEdges}
}

func serviceGraphNodeID(kind string, namespace string, name string) string {
	return strings.TrimSpace(kind) + "/" + strings.TrimSpace(namespace) + "/" + strings.TrimSpace(name)
}

func selectorMatchesLabels(selector map[string]string, labels map[string]string) bool {
	if len(selector) == 0 || len(labels) == 0 {
		return false
	}
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func firstNonEmptyMap(items ...map[string]string) map[string]string {
	for _, item := range items {
		if len(item) > 0 {
			return item
		}
	}
	return nil
}

func findServiceByName(services []domain.ResourceSnapshot, name string) (domain.ResourceSnapshot, bool) {
	name = strings.TrimSpace(name)
	for _, service := range services {
		if service.Name == name {
			return service, true
		}
	}
	return domain.ResourceSnapshot{}, false
}

func ingressRouteReason(rule domain.ResourceIngressRule) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(rule.Host) != "" {
		parts = append(parts, "host="+strings.TrimSpace(rule.Host))
	}
	if strings.TrimSpace(rule.Path) != "" {
		parts = append(parts, "path="+strings.TrimSpace(rule.Path))
	}
	if strings.TrimSpace(rule.ServicePort) != "" {
		parts = append(parts, "port="+strings.TrimSpace(rule.ServicePort))
	}
	if len(parts) == 0 {
		return "ingress backend service"
	}
	return "ingress route " + strings.Join(parts, " ")
}

func inferDependencyConfidence(workload domain.ResourceSnapshot, service domain.ResourceSnapshot) (float64, string) {
	serviceName := strings.ToLower(strings.TrimSpace(service.Name))
	if serviceName == "" || strings.EqualFold(workload.Name, service.Name) {
		return 0, ""
	}
	for _, env := range workload.EnvVars {
		name := strings.ToLower(strings.TrimSpace(env.Name))
		value := strings.ToLower(strings.TrimSpace(env.Value))
		if envValueReferencesService(value, serviceName, service.Namespace) {
			return 0.95, "env var value references service name"
		}
		if tokenContainsServiceName(name, serviceName) {
			return 0.7, "env var name references service name"
		}
	}
	for _, ref := range workload.EnvFrom {
		refName := strings.ToLower(strings.TrimSpace(ref.Name))
		if tokenContainsServiceName(refName, serviceName) {
			return 0.55, "envFrom reference name resembles service name"
		}
	}
	return 0, ""
}

func envValueReferencesService(value string, serviceName string, namespace string) bool {
	if value == "" {
		return false
	}
	candidates := []string{
		serviceName,
		serviceName + "." + strings.ToLower(strings.TrimSpace(namespace)),
		serviceName + "." + strings.ToLower(strings.TrimSpace(namespace)) + ".svc",
	}
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func tokenContainsServiceName(token string, serviceName string) bool {
	if token == "" || serviceName == "" {
		return false
	}
	normalizedToken := strings.NewReplacer("_", "-", ".", "-", "/", "-").Replace(token)
	normalizedService := strings.NewReplacer("_", "-", ".", "-", "/", "-").Replace(serviceName)
	return strings.Contains(normalizedToken, normalizedService)
}
