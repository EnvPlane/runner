export type ServiceClassification = "override" | "base" | "shared dependency" | "external" | "mock" | "ignore";

export type ServiceGraphNode = {
  id: string;
  kind: string;
  namespace: string;
  name: string;
  labels?: Record<string, string>;
};

export type ServiceGraphEdge = {
  from: string;
  to: string;
  type: string;
  reason?: string;
  confidence?: number;
};

export type ServiceGraph = {
  nodes: ServiceGraphNode[];
  edges: ServiceGraphEdge[];
};

export type ServiceClassificationValidationIssue = {
  serviceId: string;
  message: string;
  code: string;
};

export type ServiceClassificationValidationResult = {
  errors: ServiceClassificationValidationIssue[];
  warnings: ServiceClassificationValidationIssue[];
};

export const serviceClassifications: ServiceClassification[] = ["override", "base", "shared dependency", "external", "mock", "ignore"];

export function deriveAppServiceIDs(graph: ServiceGraph): Set<string> {
  const ids = new Set<string>();
  for (const edge of graph.edges) {
    if (edge.type === "routes-to" && edge.to.startsWith("Service/")) {
      ids.add(edge.to);
    }
    if (edge.type === "selects" && edge.from.startsWith("Service/")) {
      ids.add(edge.from);
    }
  }
  return ids;
}

export function defaultServiceClassification(serviceID: string, appServiceIDs: Set<string>): ServiceClassification {
  return appServiceIDs.has(serviceID) ? "override" : "shared dependency";
}

export function buildEffectiveServiceClassifications(
  services: ServiceGraphNode[],
  classifications: Record<string, ServiceClassification>,
  appServiceIDs: Set<string>
): Record<string, ServiceClassification> {
  const effective: Record<string, ServiceClassification> = {};
  for (const service of services) {
    effective[service.id] = classifications[service.id] ?? defaultServiceClassification(service.id, appServiceIDs);
  }
  return effective;
}

export function validateServiceClassifications(
  graph: ServiceGraph,
  services: ServiceGraphNode[],
  classifications: Record<string, ServiceClassification>
): ServiceClassificationValidationResult {
  const serviceIDs = new Set(services.map((service) => service.id));
  const errors: ServiceClassificationValidationIssue[] = [];
  const warnings: ServiceClassificationValidationIssue[] = [];
  const overrideIDs = services
    .filter((service) => classifications[service.id] === "override")
    .map((service) => service.id);

  if (services.length === 0) {
    errors.push({
      serviceId: "",
      code: "no_services",
      message: "No discovered services available yet. Run resource scan first."
    });
  }

  for (const service of services) {
    if (!classifications[service.id]) {
      errors.push({
        serviceId: service.id,
        code: "missing_classification",
        message: `${service.name} has no classification. Choose how EnvPlane should handle it.`
      });
    }
  }

  if (overrideIDs.length === 0 && services.length > 0) {
    errors.push({
      serviceId: "",
      code: "missing_override",
      message: "At least one service must be classified as override."
    });
  }

  const workloadToService = new Map<string, string>();
  for (const edge of graph.edges) {
    if (edge.type === "selects" && edge.from.startsWith("Service/")) {
      workloadToService.set(edge.to, edge.from);
    }
  }

  const dependencyEdges = graph.edges.filter((edge) => edge.type === "depends-on" && edge.to.startsWith("Service/"));
  for (const edge of dependencyEdges) {
    const sourceServiceID = edge.from.startsWith("Service/") ? edge.from : workloadToService.get(edge.from);
    if (!sourceServiceID) {
      continue;
    }
    if (!serviceIDs.has(edge.to)) {
      errors.push({
        serviceId: sourceServiceID,
        code: "missing_dependency_mapping",
        message: `${serviceNameFromID(sourceServiceID)} depends on ${serviceNameFromID(edge.to)}, but that service is missing from the discovered graph.`
      });
      continue;
    }
    if (classifications[sourceServiceID] !== "override") {
      continue;
    }
    const targetClassification = classifications[edge.to];
    if (!targetClassification || targetClassification === "ignore") {
      errors.push({
        serviceId: sourceServiceID,
        code: "unresolved_required_dependency",
        message: `${serviceNameFromID(sourceServiceID)} is override but required dependency ${serviceNameFromID(edge.to)} is not reachable. Classify it as base, shared dependency, external, mock, or override.`
      });
    }
    if ((edge.confidence ?? 1) < 0.8 && targetClassification && targetClassification !== "ignore") {
      warnings.push({
        serviceId: sourceServiceID,
        code: "ambiguous_dependency",
        message: `${serviceNameFromID(sourceServiceID)} may depend on ${serviceNameFromID(edge.to)}. Check this inferred dependency before continuing.`
      });
    }
  }

  const overrideReachableTargets = new Set(
    dependencyEdges
      .filter((edge) => {
        const sourceServiceID = edge.from.startsWith("Service/") ? edge.from : workloadToService.get(edge.from);
        return Boolean(sourceServiceID && classifications[sourceServiceID] === "override" && serviceIDs.has(edge.to));
      })
      .map((edge) => edge.to)
  );
  for (const service of services) {
    if (classifications[service.id] !== "base") {
      continue;
    }
    if (!overrideReachableTargets.has(service.id)) {
      warnings.push({
        serviceId: service.id,
        code: "base_not_reachable",
        message: `${service.name} is marked base but no override service has a dependency edge to it. Verify routing/network access from the feature namespace.`
      });
    }
  }

  return { errors, warnings };
}

function serviceNameFromID(id: string): string {
  const parts = id.split("/");
  return parts[2] || id;
}
