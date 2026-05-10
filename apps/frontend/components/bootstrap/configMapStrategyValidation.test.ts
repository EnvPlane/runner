import * as assert from "node:assert/strict";
import { test } from "node:test";
import { resolveConfigMapConfig, validateConfigMapStrategies, type EditableConfigMapConfig } from "./configMapStrategyValidation";
import { validateDynamicTemplate } from "./envEditorValidation";

test("template strategy requires at least one selected key", () => {
  const resources = [{ name: "app-config", configMapKeys: ["feature_flag"] }];
  const strategies: Record<string, EditableConfigMapConfig> = {
    "ConfigMap/dev/app-config": {
      strategy: "template",
      keys: {
        feature_flag: { selected: false, value: "{{ .Branch }}" }
      }
    }
  };

  const result = validateConfigMapStrategies(resources, strategies, () => "ConfigMap/dev/app-config", validateDynamicTemplate);

  assert.equal(result.errors.some((item) => item.includes("choose at least one key")), true);
});

test("template strategy validates templated key values", () => {
  const resources = [{ name: "app-config", configMapKeys: ["commit"] }];
  const strategies: Record<string, EditableConfigMapConfig> = {
    "ConfigMap/dev/app-config": {
      strategy: "template",
      keys: {
        commit: { selected: true, value: "{{ .Unknown }}" }
      }
    }
  };

  const result = validateConfigMapStrategies(resources, strategies, () => "ConfigMap/dev/app-config", validateDynamicTemplate);

  assert.equal(result.errors.length, 1);
  assert.equal(result.errors[0].includes("not allowed"), true);
});

test("saved config keeps existing keys and merges newly discovered keys", () => {
  const resolved = resolveConfigMapConfig(
    { name: "app-config", configMapKeys: ["feature_flag", "region"] },
    {
      strategy: "template",
      keys: {
        feature_flag: { selected: true, value: "{{ .PRNumber }}" }
      }
    }
  );

  assert.equal(resolved.strategy, "template");
  assert.equal(resolved.keys.feature_flag.value, "{{ .PRNumber }}");
  assert.equal(resolved.keys.region.selected, true);
  assert.equal(resolved.keys.region.value, "{{ .Branch }}");
});
