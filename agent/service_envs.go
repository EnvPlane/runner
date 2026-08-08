package agent

import (
	"sort"
	"strings"

	"github.com/envpilot/contracts/domain"
)

func BuildServiceEnvironmentVariables(snapshots []domain.ResourceSnapshot, graph domain.ServiceGraph) domain.ServiceEnvironmentVariables {
	serviceNodes := make(map[string]domain.ServiceGraphNode)
	workloadByID := make(map[string]domain.ResourceSnapshot)
	for _, node := range graph.Nodes {
		if node.Kind == "Service" {
			serviceNodes[node.ID] = node
		}
	}
	for _, snapshot := range snapshots {
		if snapshot.Kind == "Deployment" || snapshot.Kind == "StatefulSet" || snapshot.Kind == "Job" || snapshot.Kind == "CronJob" {
			workloadByID[serviceGraphNodeID(snapshot.Kind, snapshot.Namespace, snapshot.Name)] = snapshot
		}
	}

	serviceToWorkloads := make(map[string][]domain.ResourceSnapshot)
	for _, edge := range graph.Edges {
		if edge.Type != "selects" {
			continue
		}
		if !strings.HasPrefix(edge.From, "Service/") {
			continue
		}
		workload, ok := workloadByID[edge.To]
		if !ok {
			continue
		}
		serviceToWorkloads[edge.From] = append(serviceToWorkloads[edge.From], workload)
	}

	result := make([]domain.ServiceEnvironmentGroup, 0, len(serviceToWorkloads))
	for serviceID, workloads := range serviceToWorkloads {
		serviceNode, ok := serviceNodes[serviceID]
		if !ok {
			continue
		}
		containerMap := make(map[string]domain.ServiceContainerEnvSet)
		for _, workload := range workloads {
			containerSets := workload.Containers
			if len(containerSets) == 0 {
				containerSets = []domain.ResourceContainerEnv{{
					Name:    workload.Name,
					EnvVars: workload.EnvVars,
					EnvFrom: workload.EnvFrom,
				}}
			}
			for _, container := range containerSets {
				name := strings.TrimSpace(container.Name)
				if name == "" {
					name = workload.Name
				}
				existing := containerMap[name]
				existing.Container = name
				existing.Vars = append(existing.Vars, markSourceType(container.EnvVars, workload)...)
				existing.EnvFrom = append(existing.EnvFrom, markEnvFromSourceType(container.EnvFrom, workload)...)
				containerMap[name] = existing
			}
		}

		containerNames := make([]string, 0, len(containerMap))
		for name := range containerMap {
			containerNames = append(containerNames, name)
		}
		sort.Strings(containerNames)
		containers := make([]domain.ServiceContainerEnvSet, 0, len(containerNames))
		for _, name := range containerNames {
			item := containerMap[name]
			item.Vars = dedupeEnvVars(item.Vars)
			item.EnvFrom = dedupeEnvFrom(item.EnvFrom)
			containers = append(containers, item)
		}

		result = append(result, domain.ServiceEnvironmentGroup{
			ServiceID:   serviceID,
			ServiceName: serviceNode.Name,
			Namespace:   serviceNode.Namespace,
			Containers:  containers,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Namespace != result[j].Namespace {
			return result[i].Namespace < result[j].Namespace
		}
		return result[i].ServiceName < result[j].ServiceName
	})
	return domain.ServiceEnvironmentVariables{Services: result}
}

func markSourceType(vars []domain.ResourceEnvVar, workload domain.ResourceSnapshot) []domain.ResourceEnvVar {
	result := make([]domain.ResourceEnvVar, 0, len(vars))
	defaultSource := "container.env"
	if workload.SourceMapping != nil && workload.SourceMapping.Kind == "HelmRelease" {
		defaultSource = "helm-values"
	}
	for _, item := range vars {
		next := item
		if strings.TrimSpace(next.SourceType) == "" {
			next.SourceType = defaultSource
		}
		result = append(result, next)
	}
	return result
}

func markEnvFromSourceType(refs []domain.ResourceEnvFromRef, workload domain.ResourceSnapshot) []domain.ResourceEnvFromRef {
	result := make([]domain.ResourceEnvFromRef, 0, len(refs))
	defaultSource := "container.envFrom"
	if workload.SourceMapping != nil && workload.SourceMapping.Kind == "HelmRelease" {
		defaultSource = "helm-values"
	}
	for _, item := range refs {
		next := item
		if strings.TrimSpace(next.SourceType) == "" {
			next.SourceType = defaultSource
		}
		result = append(result, next)
	}
	return result
}

func dedupeEnvVars(items []domain.ResourceEnvVar) []domain.ResourceEnvVar {
	if len(items) == 0 {
		return nil
	}
	seen := map[string]domain.ResourceEnvVar{}
	for _, item := range items {
		key := strings.Join([]string{
			item.Name,
			item.Value,
			item.ValueFrom,
			item.ValueFromKind,
			item.ValueFromName,
			item.ValueFromKey,
			item.ValueFromField,
			item.ValueFromPath,
			item.SourceType,
		}, "\x00")
		seen[key] = item
	}
	result := make([]domain.ResourceEnvVar, 0, len(seen))
	for _, item := range seen {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Value < result[j].Value
	})
	return result
}

func dedupeEnvFrom(items []domain.ResourceEnvFromRef) []domain.ResourceEnvFromRef {
	if len(items) == 0 {
		return nil
	}
	seen := map[string]domain.ResourceEnvFromRef{}
	for _, item := range items {
		key := item.Kind + "\x00" + item.Name + "\x00" + item.SourceType
		seen[key] = item
	}
	result := make([]domain.ResourceEnvFromRef, 0, len(seen))
	for _, item := range seen {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].Name < result[j].Name
	})
	return result
}
