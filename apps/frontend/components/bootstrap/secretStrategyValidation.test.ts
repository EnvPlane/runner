import * as assert from "node:assert/strict";
import { test } from "node:test";
import { resolveSecretStrategy, validateSecretStrategies, type DiscoveredSecretRef, type EditableSecretStrategy } from "./secretStrategyValidation";

const secret: DiscoveredSecretRef = {
  id: "dev/db-password",
  namespace: "dev",
  secretName: "db-password",
  required: true,
  source: "env.secretKeyRef",
  serviceId: "Service/dev/orders",
  serviceName: "orders",
  container: "api",
  variable: "DB_PASSWORD",
};

test("required secret cannot be unresolved", () => {
  const result = validateSecretStrategies([secret], {
    [secret.id]: {
      strategy: "",
      required: true,
      backend: "",
      reference: "",
      manualValue: "",
      namespace: "dev",
      secretName: "db-password",
      source: "env.secretKeyRef",
      serviceId: "Service/dev/orders",
      serviceName: "orders",
      container: "api",
      variable: "DB_PASSWORD",
    },
  });
  assert.equal(result.errors.some((item) => item.includes("required secret has no strategy")), true);
});

test("manual strategy requires value unless stored marker exists", () => {
  const missing = validateSecretStrategies([secret], {
    [secret.id]: {
      strategy: "manual input",
      required: true,
      backend: "kubernetes",
      reference: "",
      manualValue: "",
      namespace: "dev",
      secretName: "db-password",
      source: "env.secretKeyRef",
      serviceId: "Service/dev/orders",
      serviceName: "orders",
      container: "api",
      variable: "DB_PASSWORD",
    },
  });
  assert.equal(missing.errors.some((item) => item.includes("manual input strategy requires value")), true);

  const stored = validateSecretStrategies([secret], {
    [secret.id]: {
      strategy: "manual input",
      required: true,
      backend: "kubernetes",
      reference: "",
      manualValue: "",
      manualValueStored: true,
      namespace: "dev",
      secretName: "db-password",
      source: "env.secretKeyRef",
      serviceId: "Service/dev/orders",
      serviceName: "orders",
      container: "api",
      variable: "DB_PASSWORD",
    },
  });
  assert.equal(stored.errors.length, 0);
});

test("resolve merges discovered defaults with existing strategy", () => {
  const existing: EditableSecretStrategy = {
    strategy: "external secret",
    required: true,
    backend: "vault",
    reference: "kv/data/app/db#password",
    manualValue: "",
    namespace: "dev",
    secretName: "db-password",
    source: "env.secretKeyRef",
    serviceId: "Service/dev/orders",
    serviceName: "orders",
    container: "api",
    variable: "DB_PASSWORD",
  };

  const resolved = resolveSecretStrategy(secret, existing);

  assert.equal(resolved.strategy, "external secret");
  assert.equal(resolved.backend, "vault");
  assert.equal(resolved.reference, "kv/data/app/db#password");
});
