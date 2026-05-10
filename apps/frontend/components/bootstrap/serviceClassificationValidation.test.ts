import * as assert from "node:assert/strict";
import { test } from "node:test";
import {
  buildEffectiveServiceClassifications,
  deriveAppServiceIDs,
  validateServiceClassifications,
  type ServiceGraph
} from "./serviceClassificationValidation";

test("detects override service with unresolved required dependency", () => {
  const graph: ServiceGraph = {
    nodes: [
      { id: "Service/dev/orders", kind: "Service", namespace: "dev", name: "orders" },
      { id: "Service/dev/payments", kind: "Service", namespace: "dev", name: "payments" }
    ],
    edges: [
      { from: "Service/dev/orders", to: "Deployment/dev/orders", type: "selects", confidence: 1 },
      { from: "Deployment/dev/orders", to: "Service/dev/payments", type: "depends-on", confidence: 0.95 }
    ]
  };
  const services = graph.nodes.filter((node) => node.kind === "Service");
  const classifications = buildEffectiveServiceClassifications(services, {
    "Service/dev/orders": "override",
    "Service/dev/payments": "ignore"
  }, deriveAppServiceIDs(graph));

  const result = validateServiceClassifications(graph, services, classifications);

  assert.equal(result.errors.some((error) => error.code === "unresolved_required_dependency"), true);
  assert.equal(result.warnings.length, 0);
});

test("warns when base service is not reachable from override service", () => {
  const graph: ServiceGraph = {
    nodes: [
      { id: "Service/dev/orders", kind: "Service", namespace: "dev", name: "orders" },
      { id: "Service/dev/auth", kind: "Service", namespace: "dev", name: "auth" }
    ],
    edges: [
      { from: "Service/dev/orders", to: "Deployment/dev/orders", type: "selects", confidence: 1 }
    ]
  };
  const services = graph.nodes.filter((node) => node.kind === "Service");
  const classifications = buildEffectiveServiceClassifications(services, {
    "Service/dev/orders": "override",
    "Service/dev/auth": "base"
  }, deriveAppServiceIDs(graph));

  const result = validateServiceClassifications(graph, services, classifications);

  assert.equal(result.errors.length, 0);
  assert.equal(result.warnings.some((warning) => warning.code === "base_not_reachable"), true);
});
