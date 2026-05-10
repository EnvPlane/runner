export type ConfigMapStrategy = "clone" | "template" | "reference" | "ignore";

export type EditableConfigMapKey = {
  selected: boolean;
  value: string;
};

export type EditableConfigMapConfig = {
  strategy: ConfigMapStrategy;
  keys: Record<string, EditableConfigMapKey>;
};

export type ConfigMapValidationResource = {
  name: string;
  configMapKeys?: string[];
};

export function buildDefaultConfigMapConfig(resource: ConfigMapValidationResource): EditableConfigMapConfig {
  const keys = (resource.configMapKeys ?? []).reduce<Record<string, EditableConfigMapKey>>((acc, item) => {
    const keyName = item.trim();
    if (keyName) {
      acc[keyName] = { selected: true, value: "{{ .Branch }}" };
    }
    return acc;
  }, {});
  return {
    strategy: "clone",
    keys
  };
}

export function resolveConfigMapConfig(
  resource: ConfigMapValidationResource,
  existing: EditableConfigMapConfig | undefined
): EditableConfigMapConfig {
  const fallback = buildDefaultConfigMapConfig(resource);
  if (!existing) return fallback;
  return {
    strategy: existing.strategy,
    keys: {
      ...fallback.keys,
      ...existing.keys
    }
  };
}

export function validateConfigMapStrategies<T extends ConfigMapValidationResource>(
  resources: T[],
  configMapStrategies: Record<string, EditableConfigMapConfig>,
  getResourceKey: (resource: T) => string,
  validateTemplate: (value: string) => string
): { errors: string[] } {
  const errors: string[] = [];
  for (const resource of resources) {
    const resourceKey = getResourceKey(resource);
    const cfg = resolveConfigMapConfig(resource, configMapStrategies[resourceKey]);
    if (cfg.strategy !== "template") continue;
    const selectedKeys = Object.entries(cfg.keys).filter(([, value]) => value.selected);
    if (selectedKeys.length === 0) {
      errors.push(`${resource.name}: choose at least one key for template strategy.`);
      continue;
    }
    for (const [key, keyCfg] of selectedKeys) {
      const templateError = validateTemplate(keyCfg.value);
      if (templateError) {
        errors.push(`${resource.name}/${key}: ${templateError}`);
      }
    }
  }
  return { errors };
}
