export type EnvVarType = "static" | "dynamic" | "secret" | "reference";

export type ServiceDiscoveredEnvVar = {
  name: string;
  value?: string;
  valueFrom?: string;
  valueFromKind?: string;
  valueFromName?: string;
  valueFromKey?: string;
  valueFromField?: string;
  valueFromPath?: string;
  sourceType?: string;
};

export const envVarTypes: EnvVarType[] = ["static", "dynamic", "secret", "reference"];
export const allowedDynamicTemplates = [".PRNumber", ".Branch", ".CommitSHA"] as const;

export function inferEnvVarType(item: ServiceDiscoveredEnvVar): EnvVarType {
  const kind = String(item.valueFromKind ?? "").trim();
  if (kind === "secretKeyRef") return "secret";
  if (item.valueFrom) return "dynamic";
  if ((item.value ?? "").includes("{{")) return "dynamic";
  return "static";
}

export function inferEnvVarValue(item: ServiceDiscoveredEnvVar): string {
  if (item.value) return item.value;
  if (item.valueFromKind === "secretKeyRef") return `${item.valueFromName ?? ""}:${item.valueFromKey ?? ""}`;
  if (item.valueFromKind === "configMapKeyRef") return `${item.valueFromName ?? ""}:${item.valueFromKey ?? ""}`;
  if (item.valueFromKind === "fieldRef" || item.valueFromKind === "resourceFieldRef") {
    return item.valueFromField ?? "";
  }
  return "";
}

export function isRequiredDynamicVar(name: string): boolean {
  const normalized = name.trim().toUpperCase();
  return normalized === "PR_NUMBER" || normalized === "BRANCH" || normalized === "COMMIT_SHA";
}

export function validateDynamicTemplate(value: string): string {
  const trimmed = value.trim();
  if (!trimmed.includes("{{") || !trimmed.includes("}}")) {
    return "dynamic value must include template token like {{ .PRNumber }}.";
  }
  const tokenRegex = /\{\{\s*(\.[A-Za-z0-9]+)\s*\}\}/g;
  let match: RegExpExecArray | null;
  const usedTokens = new Set<string>();
  while ((match = tokenRegex.exec(trimmed)) !== null) {
    usedTokens.add(match[1]);
  }
  if (usedTokens.size === 0) {
    return "template syntax is invalid.";
  }
  for (const token of usedTokens) {
    if (!allowedDynamicTemplates.includes(token as typeof allowedDynamicTemplates[number])) {
      return `template token ${token} is not allowed. Use ${allowedDynamicTemplates.join(", ")}.`;
    }
  }
  const stripped = trimmed.replace(tokenRegex, "");
  if (stripped.includes("{{") || stripped.includes("}}")) {
    return "template syntax is invalid.";
  }
  return "";
}
