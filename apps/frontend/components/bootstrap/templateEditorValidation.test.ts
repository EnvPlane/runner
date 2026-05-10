import * as assert from "node:assert/strict";
import { test } from "node:test";
import {
  extractTemplateVariables,
  simpleDiff,
  templatePath,
  templatesToPathMap,
  type ManifestTemplateFile
} from "./templateEditorValidation";

test("builds deterministic template path for namespaced resources", () => {
  const path = templatePath({ kind: "Deployment", namespace: "dev-base", name: "orders", yaml: "" });
  assert.equal(path, "dev-base/deployment-orders.yaml");
});

test("uses cluster namespace placeholder for cluster-scoped resources", () => {
  const path = templatePath({ kind: "Namespace", namespace: "", name: "envpilot-pr-123", yaml: "" });
  assert.equal(path, "cluster/namespace-envpilot-pr-123.yaml");
});

test("creates template tree map by path", () => {
  const templates: ManifestTemplateFile[] = [
    { kind: "Service", namespace: "dev", name: "orders", yaml: "apiVersion: v1" },
    { kind: "Ingress", namespace: "dev", name: "orders", yaml: "apiVersion: networking.k8s.io/v1" }
  ];
  const byPath = templatesToPathMap(templates);
  assert.equal(byPath.size, 2);
  assert.equal(byPath.get("dev/service-orders.yaml")?.kind, "Service");
});

test("computes line-by-line diff rows for edited template", () => {
  const rows = simpleDiff("kind: Service\nname: orders", "kind: Service\nname: payments");
  assert.equal(rows.length, 3);
  assert.equal(rows[0].type, "same");
  assert.equal(rows[1].type, "remove");
  assert.equal(rows[2].type, "add");
});

test("extracts template variables for highlighting", () => {
  const vars = extractTemplateVariables("host: pr-{{ .PRNumber }}-{{ .Service }}.preview.company.com");
  assert.deepEqual(vars, ["{{ .PRNumber }}", "{{ .Service }}"]);
});
