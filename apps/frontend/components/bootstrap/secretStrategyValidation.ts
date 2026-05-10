export type SecretStrategy = "reference existing secret" | "external secret" | "encrypted clone" | "manual input";

export type DiscoveredSecretRef = {
  id: string;
  namespace: string;
  secretName: string;
  required: boolean;
  source: string;
  serviceId: string;
  serviceName: string;
  container: string;
  variable: string;
};

export type EditableSecretStrategy = {
  strategy: SecretStrategy | "";
  required: boolean;
  backend: string;
  reference: string;
  manualValue: string;
  manualValueStored?: boolean;
  manualValueMasked?: boolean;
  namespace: string;
  secretName: string;
  source: string;
  serviceId: string;
  serviceName: string;
  container: string;
  variable: string;
};

export type SecretStrategyValidationResult = {
  errors: string[];
  warnings: string[];
};

export const secretStrategies: SecretStrategy[] = [
  "reference existing secret",
  "external secret",
  "encrypted clone",
  "manual input",
];

export const secretBackends = [
  "kubernetes",
  "vault",
  "aws-secrets-manager",
  "gcp-secret-manager",
  "azure-key-vault",
  "external-secrets",
] as const;

export function defaultSecretStrategy(secret: DiscoveredSecretRef): EditableSecretStrategy {
  return {
    strategy: "reference existing secret",
    required: secret.required,
    backend: "kubernetes",
    reference: `${secret.namespace}/${secret.secretName}`,
    manualValue: "",
    namespace: secret.namespace,
    secretName: secret.secretName,
    source: secret.source,
    serviceId: secret.serviceId,
    serviceName: secret.serviceName,
    container: secret.container,
    variable: secret.variable,
  };
}

export function resolveSecretStrategy(
  secret: DiscoveredSecretRef,
  existing: EditableSecretStrategy | undefined,
): EditableSecretStrategy {
  const fallback = defaultSecretStrategy(secret);
  if (!existing) return fallback;
  return {
    ...fallback,
    ...existing,
    required: secret.required || Boolean(existing.required),
    manualValue: typeof existing.manualValue === "string" ? existing.manualValue : "",
    reference: typeof existing.reference === "string" ? existing.reference : fallback.reference,
    backend: typeof existing.backend === "string" ? existing.backend : fallback.backend,
  };
}

export function validateSecretStrategies(
  secrets: DiscoveredSecretRef[],
  strategies: Record<string, EditableSecretStrategy>,
): SecretStrategyValidationResult {
  const errors: string[] = [];
  const warnings: string[] = [];
  for (const secret of secrets) {
    const cfg = resolveSecretStrategy(secret, strategies[secret.id]);
    const prefix = `${secret.serviceName}/${secret.container}/${secret.variable}`;
    if (cfg.required && cfg.strategy.trim() === "") {
      errors.push(`${prefix}: required secret has no strategy.`);
      continue;
    }
    if (cfg.strategy === "reference existing secret") {
      if (cfg.reference.trim() === "") {
        errors.push(`${prefix}: reference existing secret strategy requires reference.`);
      }
      continue;
    }
    if (cfg.strategy === "external secret") {
      if (cfg.backend.trim() === "") {
        errors.push(`${prefix}: external secret strategy requires backend.`);
      }
      if (cfg.reference.trim() === "") {
        errors.push(`${prefix}: external secret strategy requires reference.`);
      }
      continue;
    }
    if (cfg.strategy === "manual input") {
      const hasNewValue = cfg.manualValue.trim() !== "" && cfg.manualValue !== "********";
      const hasStoredValue = Boolean(cfg.manualValueStored);
      if (cfg.required && !hasNewValue && !hasStoredValue) {
        errors.push(`${prefix}: manual input strategy requires value.`);
      }
      if (!cfg.required && !hasNewValue && !hasStoredValue) {
        warnings.push(`${prefix}: manual input selected without value yet.`);
      }
      continue;
    }
  }
  return { errors, warnings };
}
