"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Button, Card, CardHeader, Toast } from "../ui";
import {
  buildEffectiveServiceClassifications,
  deriveAppServiceIDs,
  serviceClassifications,
  validateServiceClassifications,
  type ServiceClassification,
  type ServiceClassificationValidationResult,
  type ServiceGraph,
  type ServiceGraphEdge,
  type ServiceGraphNode
} from "./serviceClassificationValidation";
import {
  allowedDynamicTemplates,
  envVarTypes,
  inferEnvVarType,
  inferEnvVarValue,
  isRequiredDynamicVar,
  validateDynamicTemplate,
  type EnvVarType,
  type ServiceDiscoveredEnvVar
} from "./envEditorValidation";
import {
  resolveConfigMapConfig,
  validateConfigMapStrategies,
  type ConfigMapStrategy,
  type EditableConfigMapConfig
} from "./configMapStrategyValidation";
import {
  resolveSecretStrategy,
  secretBackends,
  secretStrategies,
  validateSecretStrategies,
  type DiscoveredSecretRef,
  type EditableSecretStrategy,
  type SecretStrategy
} from "./secretStrategyValidation";
import {
  validateGitOpsConfiguration,
  type GitOpsCommitMode
} from "./gitopsConfigValidation";

type SessionStatus = "draft" | "scanning" | "reviewed" | "compiled" | "deployed";
type AuthMethod = "OAuth" | "App token" | "Deploy token" | "SSH key";
type SCMProvider = "github" | "gitlab";

type WizardPayload = {
  createdBy?: string;
  current_step?: number;
  status?: SessionStatus;
  step_data?: Record<string, any>;
  stepData?: Record<string, any>;
};

type BootstrapSession = {
  id: string;
  project_id: string;
  current_step: number;
  status: SessionStatus;
  created_by: string;
  data: Record<string, any>;
};

type SCMValidationError = {
  field: string;
  code: string;
  message: string;
};

type SCMValidationResult = {
  appRepoUrl: string;
  gitopsRepoUrl: string;
  defaultBranch: string;
  appRepositoryReadable: boolean;
  gitopsRepositoryWritable: boolean;
  branches: string[];
  errors: SCMValidationError[];
  warnings: SCMValidationError[];
  validationProvider: string;
  hasAuthenticationValidated: boolean;
  valid: boolean;
};

type AgentInstallResponse = {
  projectId: string;
  clusterId: string;
  registrationToken: string;
  expiresAt: string;
  helmCommand: string;
  bootstrapSecretCommand?: string;
  bootstrapSecretCommandSensitive?: boolean;
  status: "waiting" | "connected" | "failed";
};

type RunnerDeploymentMode = "helm" | "gitops";

type RunnerDeploymentForm = {
  mode: RunnerDeploymentMode;
  clusterId: string;
  runnerNamespace: string;
  releaseName: string;
  gitOpsPath: string;
};

type RunnerDeploymentInstructionsResponse = {
  projectId: string;
  clusterId: string;
  deploymentMode: RunnerDeploymentMode;
  runnerNamespace: string;
  releaseName: string;
  projectConfigUrl: string;
  expiresAt: string;
  helmCommand?: string;
  bootstrapSecretCommand?: string;
  bootstrapSecretCommandSensitive?: boolean;
  gitOpsPath?: string;
  gitOpsManifest?: string;
  status: string;
};

type DeploymentBackend = "helm_direct" | "fluxcd";

type RunnerStatusResponse = {
  status: string;
  deploymentMode?: string;
  clusterId?: string;
  runnerId?: string;
  runnerNamespace?: string;
  projectConfigUrl?: string;
  error?: string;
  lastSeenAt?: string;
  tokenExpiresAt?: string;
  tokenIssuedAt?: string;
};

type RunnerHealthResponse = {
  status: string;
  component?: string;
  at?: string;
};

type CapabilityReport = {
  namespaces: string[];
  permissionWarnings: string[];
  ingressClasses?: string[];
  ingress_classes?: string[];
  ingressControllers?: string[];
  ingress_controllers?: string[];
  capabilityFlags?: string[];
  capability_flags?: string[];
};

type ResourceSourceMapping = {
  status: "resolved" | "unresolved";
  kind?: string;
  namespace?: string;
  name?: string;
  gitRepositoryNamespace?: string;
  gitRepositoryName?: string;
  reason?: string;
};

type DiscoveredResource = {
  kind: string;
  name: string;
  namespace: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  manifest?: Record<string, unknown>;
  configMapKeys?: string[];
  sourceMapping?: ResourceSourceMapping;
};

type ResourceStrategy =
  | "override per PR"
  | "use base"
  | "clone"
  | "reference"
  | "mock"
  | "ignore"
  | "external dependency";

type ResourceReviewItem = {
  include: boolean;
  strategy: ResourceStrategy;
};

type AgentStatusResponse = {
  status: "waiting" | "connected" | "failed";
  clusterId?: string;
  agentId?: string;
  lastSeenAt?: string;
  tokenExpiresAt?: string;
  capabilityReport?: CapabilityReport;
  selectedNamespaces?: string[];
  resourceScanStatus?: "idle" | "running" | "completed" | "failed";
  resourceCount?: number;
  error?: string;
};

type ServiceDiscoveredEnvFrom = {
  kind: string;
  name: string;
  sourceType?: string;
};

type ServiceDiscoveredContainerEnv = {
  container: string;
  vars?: ServiceDiscoveredEnvVar[];
  envFrom?: ServiceDiscoveredEnvFrom[];
};

type ServiceDiscoveredEnvGroup = {
  serviceId: string;
  serviceName: string;
  namespace: string;
  containers: ServiceDiscoveredContainerEnv[];
};

type ServiceEnvsPayload = {
  services: ServiceDiscoveredEnvGroup[];
};

type EditableServiceEnvVar = {
  id: string;
  name: string;
  type: EnvVarType;
  value: string;
  required: boolean;
  sourceType?: string;
};

type EditableServiceContainerEnv = {
  container: string;
  variables: EditableServiceEnvVar[];
};

type EditableServiceEnvGroup = {
  serviceId: string;
  serviceName: string;
  namespace: string;
  containers: EditableServiceContainerEnv[];
};

type WizardValues = {
  repositoryUrl: string;
  gitopsRepoUrl: string;
  defaultBranch: string;
  scmProvider: SCMProvider;
  defaultMode: "Hybrid" | "Full";
  defaultTTLHours: number;
  cpuRequest: string;
  cpuLimit: string;
  memoryRequest: string;
  memoryLimit: string;
  storageQuota: string;
  maxActiveEnvironments: number;
  networkFeatureToBase: boolean;
  networkBaseToFeature: boolean;
  networkEgressMode: NetworkEgressMode;
  cleanupProtectedNamespaces: string;
  cleanupDeleteEnvPlaneLabelsOnly: boolean;
  cleanupFinalizerStrategy: CleanupFinalizerStrategy;
  authMethod: AuthMethod;
  deploymentBackend: DeploymentBackend;
  helmDirectChartRef: string;
  helmDirectReleaseNamePattern: string;
  helmDirectNamespacePattern: string;
  helmDirectWait: boolean;
  helmDirectTimeout: number;
  helmDirectValuesOverrideStrategy: string;
  helmDirectImageTagValuePath: string;
  helmDirectCreateNamespace: boolean;
  gitOpsOutputPath: string;
  fluxNamespace: string;
  fluxGitRepositoryRef: string;
  fluxKustomizationRef: string;
  gitOpsCommitMode: GitOpsCommitMode;
  previewDomain: string;
  hostPatternTemplate: string;
  selectedIngressClass: string;
  manualIngressClass: string;
  routingMode: RoutingMode;
  oauthToken: string;
  appToken: string;
  deployToken: string;
  sshPrivateKey: string;
  selectedBaseNamespaces: string[];
  resourceReview: Record<string, ResourceReviewItem>;
  configMapStrategies: Record<string, EditableConfigMapConfig>;
  serviceClassifications: Record<string, ServiceClassification>;
  envEditor: Record<string, EditableServiceContainerEnv[]>;
  secretStrategies: Record<string, EditableSecretStrategy>;
};

type RoutingMode = "host-based" | "path-based" | "hybrid fallback";
type NetworkEgressMode = "allow all" | "restricted" | "deny all";
type CleanupFinalizerStrategy = "none" | "foreground" | "orphan";

type RoutingPreviewRow = {
  source: string;
  targetNamespace: string;
  targetService: string;
  classification: ServiceClassification;
};

type ManifestTemplateFile = {
  kind: string;
  namespace: string;
  name: string;
  yaml: string;
};

type ManifestTemplateValidationIssue = {
  file: string;
  line?: number;
  column?: number;
  code: string;
  message: string;
};

type ManifestTemplateValidationResult = {
  valid: boolean;
  issues: ManifestTemplateValidationIssue[];
};

type BootstrapSimulatePRDryRun = {
  enabled: boolean;
  status: string;
  message: string;
  commitPath: string;
  fileCount: number;
  files: string[];
  simulatedAt: string;
};

type BootstrapSimulatePRResponse = {
  validation: ManifestTemplateValidationResult;
  manifestTemplates: ManifestTemplateFile[];
  dryRun?: BootstrapSimulatePRDryRun;
};

const steps = [
  { id: "scm", label: "SCM connection", description: "Connect application and GitOps repositories" },
  { id: "settings", label: "Project settings", description: "Default mode and TTL" },
  { id: "agent", label: "Cluster agent", description: "Install discovery agent and wait for connection" },
  { id: "namespaces", label: "Namespaces", description: "Select base namespaces and start resource scan" },
  { id: "resources", label: "Resource review", description: "Configure include and strategy per discovered resource" },
  { id: "configmaps", label: "ConfigMap strategy", description: "Configure ConfigMap handling strategy and keys" },
  { id: "classification", label: "Service classification", description: "Classify discovered services" },
  { id: "env", label: "Env variables", description: "Manage variables per service and container" },
  { id: "secrets", label: "Secret strategy", description: "Choose strategy for required and optional secrets" },
  { id: "templates", label: "Template editor", description: "Review and edit generated YAML templates" },
  { id: "review", label: "Review", description: "Review and continue" },
];

const wizardStatusByStep = ["draft", "scanning", "reviewed", "compiled", "compiled", "compiled", "compiled", "compiled", "compiled", "compiled", "deployed"] as const;
const storageKey = (projectId: string) => `envpilot:bootstrap:${projectId}:wizard-v1`;
const authMethods: AuthMethod[] = ["OAuth", "App token", "Deploy token", "SSH key"];
const resourceStrategies: ResourceStrategy[] = ["override per PR", "use base", "clone", "reference", "mock", "ignore", "external dependency"];
const configMapStrategies: ConfigMapStrategy[] = ["clone", "template", "reference", "ignore"];
const routingModes: RoutingMode[] = ["host-based", "path-based", "hybrid fallback"];
const networkEgressModes: NetworkEgressMode[] = ["restricted", "allow all", "deny all"];
const cleanupFinalizerStrategies: CleanupFinalizerStrategy[] = ["foreground", "none", "orphan"];
const deploymentBackends = ["helm_direct", "fluxcd"] as const;
const helmValuesOverrideStrategies = ["merge", "append", "set"];

const branchByScmMethod: Record<AuthMethod, string> = {
  OAuth: "oauthToken",
  "App token": "appToken",
  "Deploy token": "deployToken",
  "SSH key": "sshPrivateKey"
};

const sanitizeStepData = (values: WizardValues): Record<string, any> => ({
  repositoryUrl: values.repositoryUrl,
  gitopsRepoUrl: values.gitopsRepoUrl,
  defaultBranch: values.defaultBranch,
  scmProvider: values.scmProvider,
  defaultMode: values.defaultMode,
  defaultTTLHours: values.defaultTTLHours,
  cpuRequest: values.cpuRequest,
  cpuLimit: values.cpuLimit,
  memoryRequest: values.memoryRequest,
  memoryLimit: values.memoryLimit,
  storageQuota: values.storageQuota,
  maxActiveEnvironments: values.maxActiveEnvironments,
  networkFeatureToBase: values.networkFeatureToBase,
  networkBaseToFeature: values.networkBaseToFeature,
  networkEgressMode: values.networkEgressMode,
  cleanupProtectedNamespaces: values.cleanupProtectedNamespaces,
  cleanupDeleteEnvPlaneLabelsOnly: values.cleanupDeleteEnvPlaneLabelsOnly,
  cleanupFinalizerStrategy: values.cleanupFinalizerStrategy,
  authMethod: values.authMethod,
  deployment: {
    backend: values.deploymentBackend,
    helmDirect: {
      chartRef: values.helmDirectChartRef,
      releaseNamePattern: values.helmDirectReleaseNamePattern,
      namespacePattern: values.helmDirectNamespacePattern,
      wait: values.helmDirectWait,
      timeout: values.helmDirectTimeout,
      createNamespace: values.helmDirectCreateNamespace,
      valuesOverrideStrategy: values.helmDirectValuesOverrideStrategy,
      imageTagValuePath: values.helmDirectImageTagValuePath
    },
  },
  gitOpsOutputPath: values.gitOpsOutputPath,
  fluxNamespace: values.fluxNamespace,
  fluxGitRepositoryRef: values.fluxGitRepositoryRef,
  fluxKustomizationRef: values.fluxKustomizationRef,
  gitOpsCommitMode: values.gitOpsCommitMode,
  previewDomain: values.previewDomain,
  hostPatternTemplate: values.hostPatternTemplate,
  selectedIngressClass: values.selectedIngressClass,
  manualIngressClass: values.manualIngressClass,
  routingMode: values.routingMode,
  oauthToken: values.oauthToken,
  appToken: values.appToken,
  deployToken: values.deployToken,
  sshPrivateKey: values.sshPrivateKey,
  selectedBaseNamespaces: values.selectedBaseNamespaces,
  resourceReview: values.resourceReview,
  configMapStrategies: values.configMapStrategies,
  serviceClassifications: values.serviceClassifications,
  envEditor: values.envEditor,
  secretStrategies: values.secretStrategies
});

const sanitizeValuesForStorage = (values: WizardValues): Record<string, string | number> => ({
  repositoryUrl: values.repositoryUrl,
  gitopsRepoUrl: values.gitopsRepoUrl,
  defaultBranch: values.defaultBranch,
  scmProvider: values.scmProvider,
  defaultMode: values.defaultMode,
  deploymentBackend: values.deploymentBackend,
  defaultTTLHours: values.defaultTTLHours,
  helmDirectChartRef: values.helmDirectChartRef,
  helmDirectReleaseNamePattern: values.helmDirectReleaseNamePattern,
  helmDirectNamespacePattern: values.helmDirectNamespacePattern,
  helmDirectWait: String(values.helmDirectWait),
  helmDirectTimeout: values.helmDirectTimeout,
  helmDirectValuesOverrideStrategy: values.helmDirectValuesOverrideStrategy,
  helmDirectImageTagValuePath: values.helmDirectImageTagValuePath,
  helmDirectCreateNamespace: String(values.helmDirectCreateNamespace),
  cpuRequest: values.cpuRequest,
  cpuLimit: values.cpuLimit,
  memoryRequest: values.memoryRequest,
  memoryLimit: values.memoryLimit,
  storageQuota: values.storageQuota,
  maxActiveEnvironments: values.maxActiveEnvironments,
  networkFeatureToBase: String(values.networkFeatureToBase),
  networkBaseToFeature: String(values.networkBaseToFeature),
  networkEgressMode: values.networkEgressMode,
  cleanupProtectedNamespaces: values.cleanupProtectedNamespaces,
  cleanupDeleteEnvPlaneLabelsOnly: String(values.cleanupDeleteEnvPlaneLabelsOnly),
  cleanupFinalizerStrategy: values.cleanupFinalizerStrategy,
  authMethod: values.authMethod,
  gitOpsOutputPath: values.gitOpsOutputPath,
  fluxNamespace: values.fluxNamespace,
  fluxGitRepositoryRef: values.fluxGitRepositoryRef,
  fluxKustomizationRef: values.fluxKustomizationRef,
  gitOpsCommitMode: values.gitOpsCommitMode,
  previewDomain: values.previewDomain,
  hostPatternTemplate: values.hostPatternTemplate,
  selectedIngressClass: values.selectedIngressClass,
  manualIngressClass: values.manualIngressClass,
  routingMode: values.routingMode,
  selectedBaseNamespaces: values.selectedBaseNamespaces.join(","),
  resourceReview: JSON.stringify(values.resourceReview),
  configMapStrategies: JSON.stringify(values.configMapStrategies),
  serviceClassifications: JSON.stringify(values.serviceClassifications),
  envEditor: JSON.stringify(values.envEditor),
  secretStrategies: JSON.stringify(values.secretStrategies)
});

type BootstrapWizardClientProps = {
  projectId: string;
};

const asString = (value: unknown): string => {
  if (typeof value === "string") {
    return value.trim();
  }
  return "";
};

const asStringAnyMap = (value: unknown): Record<string, any> => {
  if (value && typeof value === "object" && !Array.isArray(value)) {
    return value as Record<string, any>;
  }
  return {};
};

const asNumber = (value: unknown, fallback: number): number => {
  if (typeof value === "number" && Number.isFinite(value)) {
    return value;
  }
  if (typeof value === "string") {
    const parsed = Number(value);
    if (Number.isFinite(parsed)) {
      return parsed;
    }
  }
  return fallback;
};

const asOptionalBoolean = (value: unknown): boolean | null => {
  if (typeof value === "boolean") {
    return value;
  }
  if (typeof value === "number") {
    return value !== 0;
  }
  if (typeof value === "string") {
    const normalized = value.trim().toLowerCase();
    if (normalized === "") return null;
    return normalized === "true" || normalized === "1" || normalized === "yes";
  }
  return null;
};

const stableYAML = (value: unknown, depth = 0): string => {
  const indent = " ".repeat(depth);
  if (Array.isArray(value)) {
    if (value.length === 0) return `${indent}[]`;
    return value
      .map((item) => {
        if (item && typeof item === "object") {
          return `${indent}-\n${stableYAML(item, depth + 2)}`;
        }
        return `${indent}- ${scalarYAML(item)}`;
      })
      .join("\n");
  }
  if (value && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>).sort(([a], [b]) => a.localeCompare(b));
    if (entries.length === 0) return `${indent}{}`;
    return entries
      .map(([key, item]) => {
        if (item && typeof item === "object") {
          return `${indent}${key}:\n${stableYAML(item, depth + 2)}`;
        }
        return `${indent}${key}: ${scalarYAML(item)}`;
      })
      .join("\n");
  }
  return `${indent}${scalarYAML(value)}`;
};

const scalarYAML = (value: unknown): string => {
  if (value === null || value === undefined) return "null";
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  const raw = String(value);
  if (raw === "") return `""`;
  if (/^[A-Za-z0-9._/\-{} ]+$/.test(raw) && !raw.includes(": ")) {
    return raw;
  }
  return JSON.stringify(raw);
};

const splitLines = (input: string): string[] => input.replace(/\r\n/g, "\n").split("\n");

const simpleDiff = (originalText: string, editedText: string): Array<{ type: "same" | "add" | "remove"; text: string }> => {
  const original = splitLines(originalText);
  const edited = splitLines(editedText);
  const max = Math.max(original.length, edited.length);
  const rows: Array<{ type: "same" | "add" | "remove"; text: string }> = [];
  for (let index = 0; index < max; index += 1) {
    const left = original[index];
    const right = edited[index];
    if (left === right) {
      rows.push({ type: "same", text: left ?? "" });
      continue;
    }
    if (left !== undefined) rows.push({ type: "remove", text: left });
    if (right !== undefined) rows.push({ type: "add", text: right });
  }
  return rows;
};


const isValidRepositoryURL = (value: string): boolean => {
  const trimmed = value.trim();
  if (!trimmed) {
    return false;
  }
  if (trimmed.includes("://")) {
    try {
      const parsed = new URL(trimmed);
      return Boolean(parsed.protocol && parsed.host && parsed.pathname && parsed.pathname !== "/");
    } catch {
      return false;
    }
  }
  if (trimmed.includes(":") && trimmed.includes("@")) {
    const parts = trimmed.split(":", 2);
    const repo = parts[1]?.trim().replace(/^\/+/, "");
    return Boolean(repo && repo.includes("/") && repo.includes("."));
  }
  return false;
};

const isValidDomain = (value: string): boolean => {
  const domain = value.trim().toLowerCase();
  if (!domain || domain.length > 253 || domain.endsWith(".")) {
    return false;
  }
  const labels = domain.split(".");
  if (labels.length < 2) {
    return false;
  }
  return labels.every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label));
};

const renderHostTemplate = (template: string, prNumber: string, service: string, domain: string): string => {
  const normalizedTemplate = template.trim();
  const fallback = `pr-${prNumber}-${service}.${domain.trim().toLowerCase()}`;
  if (!normalizedTemplate) {
    return fallback;
  }
  let host = normalizedTemplate
    .replace(/\{\{\s*\.PRNumber\s*\}\}/g, prNumber)
    .replace(/\{\{\s*\.Service\s*\}\}/g, service)
    .toLowerCase();
  if (!host.includes(".") && domain.trim() !== "") {
    host = `${host}.${domain.trim().toLowerCase()}`;
  }
  return host;
};

const renderDeploymentTemplatePreview = (
  template: string,
  fallback: string,
  values: Record<string, string> = {}
): string => {
  const resolved = template.trim()
    .replace(/\{\{\s*\.project\.id\s*\}\}/g, values.projectId ?? "checkout")
    .replace(/\{\{\s*\.environment\.name\s*\}\}/g, values.environmentName ?? "pr-42")
    .replace(/\{\{\s*\.environment\.id\s*\}\}/g, values.environmentId ?? "42")
    .replace(/\{\{\s*\.environment\.projectId\s*\}\}/g, values.environmentProjectId ?? values.projectId ?? "checkout")
    .replace(/\{\{\s*\.source\.pr\s*\}\}/g, values.prNumber ?? "42")
    .replace(/\{\{\s*\.source\.mr\s*\}\}/g, values.prNumber ?? "42")
    .replace(/\{\{\s*\.source\.branch\s*\}\}/g, values.branch ?? "feature/demo")
    .replace(/\{\{\s*\.source\.commit\s*\}\}/g, values.commit ?? "abc123")
    .replace(/\{\{\s*\.PRNumber\s*\}\}/g, values.prNumber ?? "42")
    .replace(/\{\{\s*\.Service\s*\}\}/g, values.service ?? "orders")
    .replace(/\{\{\s*\.ProjectID\s*\}\}/g, values.projectId ?? "checkout")
    .replace(/\{\{\s*\.EnvironmentID\s*\}\}/g, values.environmentId ?? "42");
  return resolved.includes("{{") ? fallback : resolved;
};

const validateHostPatternTemplate = (template: string, domain: string): string => {
  const trimmed = template.trim();
  if (trimmed === "") {
    return "";
  }
  if (!trimmed.includes("{{ .PRNumber }}") && !trimmed.includes("{{.PRNumber}}")) {
    return "Host pattern template must include {{ .PRNumber }}.";
  }
  const unresolved = trimmed
    .replace(/\{\{\s*\.PRNumber\s*\}\}/g, "123")
    .replace(/\{\{\s*\.Service\s*\}\}/g, "orders");
  if (unresolved.includes("{{") || unresolved.includes("}}")) {
    return "Host pattern template contains unsupported placeholders.";
  }
  const sampleHost = renderHostTemplate(trimmed, "123", "orders", domain);
  if (!isValidDomain(sampleHost)) {
    return "Host pattern template produces invalid host name.";
  }
  return "";
};

const hasRequiredAuthSecret = (values: WizardValues): boolean => {
  switch (values.authMethod) {
    case "OAuth":
      return values.oauthToken.trim() !== "";
    case "App token":
      return values.appToken.trim() !== "";
    case "Deploy token":
      return values.deployToken.trim() !== "";
    case "SSH key":
      return values.sshPrivateKey.trim() !== "";
  }
  return false;
};

const isValidCPUQuantity = (value: string): boolean => /^[1-9]\d*(m)?$/.test(value.trim());
const isValidBinaryQuantity = (value: string): boolean => /^[1-9]\d*(Ki|Mi|Gi|Ti)$/.test(value.trim());

const resourcePolicyValidationMessage = (values: WizardValues): string => {
  if (!Number.isFinite(values.maxActiveEnvironments) || values.maxActiveEnvironments <= 0) {
    return "Max active environments must be greater than 0.";
  }
  if (!isValidCPUQuantity(values.cpuRequest)) {
    return "CPU request must be a valid quantity (for example 250m or 1).";
  }
  if (!isValidCPUQuantity(values.cpuLimit)) {
    return "CPU limit must be a valid quantity (for example 1000m or 2).";
  }
  if (!isValidBinaryQuantity(values.memoryRequest)) {
    return "Memory request must be a valid quantity (for example 256Mi).";
  }
  if (!isValidBinaryQuantity(values.memoryLimit)) {
    return "Memory limit must be a valid quantity (for example 1Gi).";
  }
  if (!isValidBinaryQuantity(values.storageQuota)) {
    return "Storage quota must be a valid quantity (for example 10Gi).";
  }
  return "";
};

const networkPolicyValidationMessage = (values: WizardValues): string => {
  if (!networkEgressModes.includes(values.networkEgressMode)) {
    return "Select egress policy.";
  }
  return "";
};

const cleanupSafetyValidationMessage = (values: WizardValues): string => {
  const protectedNamespaces = values.cleanupProtectedNamespaces
    .split(",")
    .map((item) => item.trim())
    .filter((item) => item !== "");
  if (protectedNamespaces.length === 0) {
    return "Protected namespaces list must not be empty.";
  }
  if (!values.cleanupDeleteEnvPlaneLabelsOnly) {
    return "Cleanup must delete only resources with EnvPlane labels.";
  }
  if (!cleanupFinalizerStrategies.includes(values.cleanupFinalizerStrategy)) {
    return "Select cleanup finalizer strategy.";
  }
  if (protectedNamespaces.includes("envpilot-pr-{{ .PRNumber }}")) {
    return "Protected namespaces cannot include the feature namespace template.";
  }
  return "";
};

const scmFingerprint = (values: WizardValues): string => {
  return JSON.stringify({
    provider: values.scmProvider,
    app: values.repositoryUrl.trim(),
    gitops: values.gitopsRepoUrl.trim(),
    branch: values.defaultBranch.trim(),
    authMethod: values.authMethod,
  });
};

export function BootstrapWizardClient({ projectId }: BootstrapWizardClientProps) {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [values, setValues] = useState<WizardValues>({
    repositoryUrl: "",
    gitopsRepoUrl: "",
    defaultBranch: "",
    scmProvider: "github",
    defaultMode: "Hybrid",
    defaultTTLHours: 48,
    cpuRequest: "250m",
    cpuLimit: "1000m",
    memoryRequest: "256Mi",
    memoryLimit: "1Gi",
    storageQuota: "10Gi",
    maxActiveEnvironments: 10,
    networkFeatureToBase: true,
    networkBaseToFeature: false,
    networkEgressMode: "restricted",
    cleanupProtectedNamespaces: "default,kube-system,kube-public,kube-node-lease,flux-system,cert-manager",
    cleanupDeleteEnvPlaneLabelsOnly: true,
    cleanupFinalizerStrategy: "foreground",
    authMethod: "OAuth",
    deploymentBackend: "helm_direct",
    helmDirectChartRef: "deploy/helm/{{ .project.id }}",
    helmDirectReleaseNamePattern: "{{ .project.id }}-{{ .environment.name }}",
    helmDirectNamespacePattern: "envpilot-pr-{{ .PRNumber }}",
    helmDirectWait: true,
    helmDirectTimeout: 300,
    helmDirectValuesOverrideStrategy: "merge",
    helmDirectImageTagValuePath: "imageTag",
    helmDirectCreateNamespace: true,
    gitOpsOutputPath: "environments/{{ .PRNumber }}",
    fluxNamespace: "flux-system",
    fluxGitRepositoryRef: "envpilot-gitops",
    fluxKustomizationRef: "envpilot-prs",
    gitOpsCommitMode: "direct",
    previewDomain: "preview.company.com",
    hostPatternTemplate: "pr-{{ .PRNumber }}-{{ .Service }}.preview.company.com",
    selectedIngressClass: "",
    manualIngressClass: "",
    routingMode: "host-based",
    oauthToken: "",
    appToken: "",
    deployToken: "",
    sshPrivateKey: "",
    selectedBaseNamespaces: [],
    resourceReview: {},
    configMapStrategies: {},
    serviceClassifications: {},
    envEditor: {},
    secretStrategies: {}
  });
  const [currentStep, setCurrentStep] = useState(0);
  const [sessionID, setSessionID] = useState("");
  const [scmValidation, setScmValidation] = useState<SCMValidationResult | null>(null);
  const [scmValidationInFlight, setScmValidationInFlight] = useState(false);
  const [lastScmValidationFingerprint, setLastScmValidationFingerprint] = useState("");
  const [agentInstall, setAgentInstall] = useState<AgentInstallResponse | null>(null);
  const [agentStatus, setAgentStatus] = useState<AgentStatusResponse | null>(null);
  const [runnerForm, setRunnerForm] = useState<RunnerDeploymentForm>({
    mode: "helm",
    clusterId: "default",
    runnerNamespace: "envpilot-runner",
    releaseName: "envpilot-runner-${projectId}".replace(/[^a-zA-Z0-9._-]/g, ""),
    gitOpsPath: "gitops/runners"
  });
  const [runnerDeploymentInstructions, setRunnerDeploymentInstructions] = useState<RunnerDeploymentInstructionsResponse | null>(null);
  const [runnerStatus, setRunnerStatus] = useState<RunnerStatusResponse | null>(null);
  const [runnerHealth, setRunnerHealth] = useState<RunnerHealthResponse | null>(null);
  const [runnerBusy, setRunnerBusy] = useState(false);
  const [discoveredResources, setDiscoveredResources] = useState<DiscoveredResource[]>([]);
  const [serviceGraph, setServiceGraph] = useState<ServiceGraph>({ nodes: [], edges: [] });
  const [discoveredServiceEnvs, setDiscoveredServiceEnvs] = useState<ServiceDiscoveredEnvGroup[]>([]);
  const [manifestTemplates, setManifestTemplates] = useState<ManifestTemplateFile[]>([]);
  const [activeTemplatePath, setActiveTemplatePath] = useState("");
  const [editedTemplateYAML, setEditedTemplateYAML] = useState<Record<string, string>>({});
  const [templateSaving, setTemplateSaving] = useState(false);
  const [agentBusy, setAgentBusy] = useState(false);
  const [compileSaving, setCompileSaving] = useState(false);
  const [simulateSaving, setSimulateSaving] = useState(false);
  const [simulateDryRun, setSimulateDryRun] = useState(true);
  const [simulateResult, setSimulateResult] = useState<BootstrapSimulatePRResponse | null>(null);

  const currentScmFingerprint = useMemo(() => scmFingerprint(values), [values]);
  const hasValidSCMConnection = useMemo(() => Boolean(scmValidation?.valid && lastScmValidationFingerprint === currentScmFingerprint), [lastScmValidationFingerprint, scmValidation, currentScmFingerprint]);
  const detectedIngressClasses = useMemo(() => {
    const report = agentStatus?.capabilityReport;
    const raw = report?.ingressClasses ?? report?.ingress_classes ?? report?.ingressControllers ?? report?.ingress_controllers ?? [];
    const items = Array.isArray(raw) ? raw : [];
    const normalized = items
      .map((item) => asString(item))
      .filter((item) => item !== "");
    return Array.from(new Set(normalized)).sort((a, b) => a.localeCompare(b));
  }, [agentStatus?.capabilityReport]);
  const networkPolicyWarning = useMemo(() => {
    const report = agentStatus?.capabilityReport;
    const rawFlags = report?.capabilityFlags ?? report?.capability_flags ?? [];
    const flags = Array.isArray(rawFlags) ? rawFlags.map((item) => asString(item).toLowerCase()) : [];
    const hasNetworkPolicySupport = flags.some((flag) =>
      flag.includes("networkpolicy") ||
      flag.includes("network-policy") ||
      flag.includes("cni:calico") ||
      flag.includes("cni:cilium") ||
      flag.includes("cni:canal")
    );
    return hasNetworkPolicySupport ? "" : "Cluster capability report does not confirm NetworkPolicy enforcement by the CNI.";
  }, [agentStatus?.capabilityReport]);
  const effectiveIngressClass = useMemo(() => {
    if (values.selectedIngressClass === "__manual__") {
      return values.manualIngressClass.trim();
    }
    return values.selectedIngressClass.trim();
  }, [values.manualIngressClass, values.selectedIngressClass]);
  const domainError = useMemo(() => {
    if (!isValidDomain(values.previewDomain)) {
      return "Base preview domain must be a valid domain.";
    }
    return "";
  }, [values.previewDomain]);
  const isFluxBackend = useMemo(() => values.deploymentBackend === "fluxcd", [values.deploymentBackend]);
  const isHelmDirectBackend = useMemo(() => values.deploymentBackend === "helm_direct", [values.deploymentBackend]);
  const hostTemplateError = useMemo(() => validateHostPatternTemplate(values.hostPatternTemplate, values.previewDomain), [values.hostPatternTemplate, values.previewDomain]);
  const gitOpsConfigValidation = useMemo(() => {
    return validateGitOpsConfiguration({
      gitOpsOutputPath: values.gitOpsOutputPath,
      fluxNamespace: values.fluxNamespace,
      fluxGitRepositoryRef: values.fluxGitRepositoryRef,
      fluxKustomizationRef: values.fluxKustomizationRef,
      gitOpsCommitMode: values.gitOpsCommitMode
    });
  }, [values.fluxGitRepositoryRef, values.fluxKustomizationRef, values.fluxNamespace, values.gitOpsCommitMode, values.gitOpsOutputPath]);
  const samplePreviewURL = useMemo(() => {
    if (!isValidDomain(values.previewDomain)) {
      return "";
    }
    const host = renderHostTemplate(values.hostPatternTemplate, "123", "orders", values.previewDomain);
    return `https://${host}`;
  }, [values.hostPatternTemplate, values.previewDomain]);
  const previewReleaseName = useMemo(() => {
    return renderDeploymentTemplatePreview(values.helmDirectReleaseNamePattern, `${values.helmDirectNamespacePattern || "envpilot-pr-42"}`, {
      projectId,
      environmentProjectId: projectId,
      environmentName: "feature/demo",
      environmentId: "42",
      prNumber: "42",
      branch: "feature/demo",
      commit: "abc123"
    });
  }, [values.helmDirectReleaseNamePattern]);
  const previewNamespace = useMemo(() => {
    return renderDeploymentTemplatePreview(values.helmDirectNamespacePattern, "envpilot-pr-42", {
      projectId,
      environmentProjectId: projectId,
      environmentName: "feature/demo",
      environmentId: "42",
      prNumber: "42",
      branch: "feature/demo",
      commit: "abc123"
    });
  }, [values.helmDirectNamespacePattern]);

  const templatePath = useCallback((item: ManifestTemplateFile) => {
    const namespace = item.namespace || "default";
    return `${namespace}/${item.kind.toLowerCase()}/${item.name}.yaml`;
  }, []);

  const templatesByPath = useMemo(() => {
    const map = new Map<string, ManifestTemplateFile>();
    for (const item of manifestTemplates) {
      map.set(templatePath(item), item);
    }
    return map;
  }, [manifestTemplates, templatePath]);

  const sortedTemplatePaths = useMemo(() => Array.from(templatesByPath.keys()).sort((a, b) => a.localeCompare(b)), [templatesByPath]);

  const effectiveActiveTemplatePath = useMemo(() => {
    if (activeTemplatePath && templatesByPath.has(activeTemplatePath)) {
      return activeTemplatePath;
    }
    return sortedTemplatePaths[0] ?? "";
  }, [activeTemplatePath, sortedTemplatePaths, templatesByPath]);

  const activeTemplate = useMemo(() => {
    if (!effectiveActiveTemplatePath) return null;
    return templatesByPath.get(effectiveActiveTemplatePath) ?? null;
  }, [effectiveActiveTemplatePath, templatesByPath]);

  const activeTemplateEditedYAML = useMemo(() => {
    if (!activeTemplate || !effectiveActiveTemplatePath) return "";
    return editedTemplateYAML[effectiveActiveTemplatePath] ?? activeTemplate.yaml;
  }, [activeTemplate, editedTemplateYAML, effectiveActiveTemplatePath]);

  const activeOriginalYAML = useMemo(() => {
    if (!activeTemplate) return "";
    const related = discoveredResources.find((resource) =>
      resource.kind === activeTemplate.kind &&
      resource.name === activeTemplate.name &&
      resource.namespace === activeTemplate.namespace
    );
    if (!related?.manifest) return "";
    return `${stableYAML(related.manifest)}\n`;
  }, [activeTemplate, discoveredResources]);

  const activeTemplateDiff = useMemo(() => simpleDiff(activeOriginalYAML, activeTemplateEditedYAML), [activeOriginalYAML, activeTemplateEditedYAML]);

  const hasTemplateChanges = useMemo(() => {
    for (const path of sortedTemplatePaths) {
      const source = templatesByPath.get(path);
      if (!source) continue;
      const edited = editedTemplateYAML[path];
      if (edited !== undefined && edited !== source.yaml) {
        return true;
      }
    }
    return false;
  }, [editedTemplateYAML, sortedTemplatePaths, templatesByPath]);

  const resourceKey = useCallback((resource: DiscoveredResource) => `${resource.kind}/${resource.namespace}/${resource.name}`, []);

  const resolveDefaultStrategy = useCallback((resource: DiscoveredResource): ResourceStrategy => {
    if (resource.kind === "Secret") {
      return "reference";
    }
    if (resource.kind === "Service" || resource.kind === "Ingress") {
      return "use base";
    }
    if (resource.kind === "Deployment" || resource.kind === "StatefulSet" || resource.kind === "HelmRelease" || resource.kind === "Kustomization") {
      return "override per PR";
    }
    return "clone";
  }, []);

  const getResourceReviewItem = useCallback((resource: DiscoveredResource, currentValues: WizardValues): ResourceReviewItem => {
    const key = resourceKey(resource);
    const existing = currentValues.resourceReview[key];
    if (existing) {
      return existing;
    }
    return {
      include: true,
      strategy: resolveDefaultStrategy(resource)
    };
  }, [resourceKey, resolveDefaultStrategy]);

  const validateResourceSelection = useCallback((resource: DiscoveredResource, item: ResourceReviewItem): string => {
    if (!item.include && item.strategy !== "ignore") {
      return "Excluded resources must use strategy 'ignore'.";
    }
    if (item.include && item.strategy === "ignore") {
      return "Included resources cannot use strategy 'ignore'.";
    }
    if ((resource.kind === "Secret" || resource.kind === "ConfigMap") && item.strategy === "mock") {
      return "Strategy 'mock' is not allowed for Secret/ConfigMap.";
    }
    if (resource.kind === "GitRepository" && (item.strategy === "override per PR" || item.strategy === "clone" || item.strategy === "mock")) {
      return "GitRepository supports only 'reference', 'use base', 'external dependency', or 'ignore'.";
    }
    return "";
  }, []);

  const invalidResourceItems = useMemo(() => {
    const invalid = new Map<string, string>();
    for (const resource of discoveredResources) {
      const item = getResourceReviewItem(resource, values);
      const reason = validateResourceSelection(resource, item);
      if (reason) {
        invalid.set(resourceKey(resource), reason);
      }
    }
    return invalid;
  }, [discoveredResources, getResourceReviewItem, resourceKey, validateResourceSelection, values]);

  const discoveredConfigMaps = useMemo(() => discoveredResources.filter((resource) => resource.kind === "ConfigMap"), [discoveredResources]);

  const getConfigMapConfig = useCallback((resource: DiscoveredResource, currentValues: WizardValues): EditableConfigMapConfig => {
    const key = resourceKey(resource);
    return resolveConfigMapConfig(resource, currentValues.configMapStrategies[key]);
  }, [resourceKey]);

  const configMapValidation = useMemo(() => {
    return validateConfigMapStrategies(discoveredConfigMaps, values.configMapStrategies, resourceKey, validateDynamicTemplate);
  }, [discoveredConfigMaps, resourceKey, values.configMapStrategies]);

  const discoveredServices = useMemo(() => serviceGraph.nodes.filter((node) => node.kind === "Service"), [serviceGraph.nodes]);

  const appServiceIDs = useMemo(() => deriveAppServiceIDs(serviceGraph), [serviceGraph]);

  const sortedServices = useMemo(() => {
    return [...discoveredServices].sort((a, b) => {
      const aApp = appServiceIDs.has(a.id) ? 0 : 1;
      const bApp = appServiceIDs.has(b.id) ? 0 : 1;
      if (aApp !== bApp) return aApp - bApp;
      if (a.namespace !== b.namespace) return a.namespace.localeCompare(b.namespace);
      return a.name.localeCompare(b.name);
    });
  }, [appServiceIDs, discoveredServices]);

  const getServiceClassification = useCallback((serviceID: string, currentValues: WizardValues): ServiceClassification => {
    return buildEffectiveServiceClassifications(sortedServices, currentValues.serviceClassifications, appServiceIDs)[serviceID];
  }, [appServiceIDs, sortedServices]);

  const effectiveServiceClassifications = useMemo(() => {
    return buildEffectiveServiceClassifications(sortedServices, values.serviceClassifications, appServiceIDs);
  }, [appServiceIDs, sortedServices, values.serviceClassifications]);

  const serviceClassificationValidation = useMemo<ServiceClassificationValidationResult>(() => {
    return validateServiceClassifications(serviceGraph, sortedServices, effectiveServiceClassifications);
  }, [effectiveServiceClassifications, serviceGraph, sortedServices]);

  const serviceClassificationValidationMessage = useMemo(() => {
    return serviceClassificationValidation.errors[0]?.message ?? "";
  }, [serviceClassificationValidation.errors]);

  const routingPreviewRows = useMemo<RoutingPreviewRow[]>(() => {
    const baseNamespace = values.selectedBaseNamespaces[0] ?? "dev-base";
    return sortedServices
      .filter((service) => effectiveServiceClassifications[service.id] !== "ignore")
      .map((service) => {
        const classification = effectiveServiceClassifications[service.id] ?? "shared dependency";
        const targetNamespace = classification === "override"
          ? "envpilot-pr-123"
          : classification === "external"
            ? "external"
            : classification === "mock"
              ? "mock"
              : baseNamespace;
        const source = values.routingMode === "path-based"
          ? `https://${values.previewDomain}/api/${service.name}`
          : `https://${renderHostTemplate(values.hostPatternTemplate, "123", service.name, values.previewDomain)}`;
        return {
          source,
          targetNamespace,
          targetService: service.name,
          classification
        };
      });
  }, [effectiveServiceClassifications, sortedServices, values.hostPatternTemplate, values.previewDomain, values.routingMode, values.selectedBaseNamespaces]);

  const routingValidation = useMemo(() => {
    const errors: string[] = [];
    const warnings: string[] = [];
    if (!routingModes.includes(values.routingMode)) {
      errors.push("Select routing mode.");
    }
    if (effectiveIngressClass === "") {
      errors.push("Routing requires selected ingress class.");
    }
    if (values.routingMode === "host-based" || values.routingMode === "hybrid fallback") {
      if (domainError !== "") errors.push(domainError);
      if (hostTemplateError !== "") errors.push(hostTemplateError);
    }
    if (values.routingMode === "hybrid fallback") {
      const hasFallbackTarget = sortedServices.some((service) => {
        const classification = effectiveServiceClassifications[service.id];
        return classification === "base" || classification === "shared dependency";
      });
      if (!hasFallbackTarget) {
        warnings.push("Hybrid fallback has no base/shared dependency routes yet.");
      }
    }
    if (detectedIngressClasses.length === 0 && values.selectedIngressClass === "__manual__") {
      warnings.push("Ingress class is manual because no detected ingress controller is available.");
    }
    return { errors, warnings };
  }, [detectedIngressClasses.length, domainError, effectiveIngressClass, effectiveServiceClassifications, hostTemplateError, sortedServices, values.routingMode, values.selectedIngressClass]);

  const sortedDiscoveredServiceEnvs = useMemo(() => {
    return [...discoveredServiceEnvs].sort((a, b) => {
      const aApp = appServiceIDs.has(a.serviceId) ? 0 : 1;
      const bApp = appServiceIDs.has(b.serviceId) ? 0 : 1;
      if (aApp !== bApp) return aApp - bApp;
      if (a.namespace !== b.namespace) return a.namespace.localeCompare(b.namespace);
      return a.serviceName.localeCompare(b.serviceName);
    });
  }, [appServiceIDs, discoveredServiceEnvs]);

  const editorForService = useCallback((service: ServiceDiscoveredEnvGroup, currentValues: WizardValues): EditableServiceContainerEnv[] => {
    const existing = currentValues.envEditor[service.serviceId];
    if (existing && existing.length > 0) {
      return existing;
    }
    const containers: EditableServiceContainerEnv[] = service.containers.map((container) => ({
      container: container.container,
      variables: [
        ...(container.vars ?? []).map((item) => ({
          id: `${container.container}:${item.name}:var`,
          name: item.name,
          type: inferEnvVarType(item),
          value: inferEnvVarValue(item),
          required: isRequiredDynamicVar(item.name),
          sourceType: item.sourceType
        })),
        ...(container.envFrom ?? []).map((item) => ({
          id: `${container.container}:${item.kind}:${item.name}:envfrom`,
          name: `${item.kind}:${item.name}`,
          type: "reference" as const,
          value: `${item.kind}:${item.name}`,
          required: false,
          sourceType: item.sourceType
        }))
      ]
    }));
    return containers;
  }, []);

  const envTemplateValidation = useMemo(() => {
    const errors: string[] = [];
    const warnings: string[] = [];
    for (const service of sortedDiscoveredServiceEnvs) {
      const containers = editorForService(service, values);
      for (const container of containers) {
        for (const variable of container.variables) {
          const varName = variable.name.trim();
          if (!varName) {
            errors.push(`${service.serviceName}/${container.container}: variable name is required.`);
            continue;
          }
          if (variable.type === "dynamic") {
            if (!variable.value.trim()) {
              errors.push(`${service.serviceName}/${container.container}/${varName}: dynamic variable must have a template value.`);
              continue;
            }
            const templateError = validateDynamicTemplate(variable.value);
            if (templateError) {
              errors.push(`${service.serviceName}/${container.container}/${varName}: ${templateError}`);
            }
          }
          if (variable.required && variable.type !== "dynamic") {
            errors.push(`${service.serviceName}/${container.container}/${varName}: required variable must use dynamic type.`);
          }
          if (variable.type === "secret" && variable.value && variable.value.startsWith("{{")) {
            warnings.push(`${service.serviceName}/${container.container}/${varName}: secret variable currently uses template syntax.`);
          }
        }
      }
    }
    return { errors, warnings };
  }, [editorForService, sortedDiscoveredServiceEnvs, values]);

  const discoveredSecrets = useMemo<DiscoveredSecretRef[]>(() => {
    const items = new Map<string, DiscoveredSecretRef>();
    for (const service of sortedDiscoveredServiceEnvs) {
      const containers = editorForService(service, values);
      for (const container of containers) {
        for (const variable of container.variables) {
          const source = (variable.sourceType ?? "").toLowerCase();
          const isSecretVar = variable.type === "secret" || source.includes("secret");
          const isSecretEnvFrom = variable.type === "reference" && variable.value.toLowerCase().startsWith("secret:");
          if (!isSecretVar && !isSecretEnvFrom) {
            continue;
          }
          let secretName = "";
          if (isSecretEnvFrom) {
            const parts = variable.value.split(":", 2);
            secretName = (parts[1] ?? "").trim();
          } else if (variable.value.includes(":")) {
            secretName = variable.value.split(":", 2)[0].trim();
          }
          if (!secretName) {
            secretName = variable.name.trim() || "secret";
          }
          const id = `${service.namespace}/${secretName}`;
          const existing = items.get(id);
          if (!existing) {
            items.set(id, {
              id,
              namespace: service.namespace,
              secretName,
              required: appServiceIDs.has(service.serviceId),
              source: variable.sourceType ?? (isSecretEnvFrom ? "envFrom.secretRef" : "env.secretKeyRef"),
              serviceId: service.serviceId,
              serviceName: service.serviceName,
              container: container.container,
              variable: variable.name || secretName
            });
            continue;
          }
          existing.required = existing.required || appServiceIDs.has(service.serviceId);
        }
      }
    }
    return Array.from(items.values()).sort((a, b) => a.id.localeCompare(b.id));
  }, [appServiceIDs, editorForService, sortedDiscoveredServiceEnvs, values]);

  const getSecretStrategy = useCallback((secret: DiscoveredSecretRef, currentValues: WizardValues): EditableSecretStrategy => {
    return resolveSecretStrategy(secret, currentValues.secretStrategies[secret.id]);
  }, []);

  const secretStrategyValidation = useMemo(() => {
    return validateSecretStrategies(discoveredSecrets, values.secretStrategies);
  }, [discoveredSecrets, values.secretStrategies]);

  const reviewBlockingErrors = useMemo(() => {
    const issues: string[] = [];
    if (!isValidRepositoryURL(values.repositoryUrl.trim()) || values.repositoryUrl.trim() === "") {
      issues.push("SCM: valid application repository URL is required.");
    }
    if (!isValidRepositoryURL(values.gitopsRepoUrl.trim()) || values.gitopsRepoUrl.trim() === "") {
      issues.push("SCM: valid GitOps repository URL is required.");
    }
    if (values.defaultBranch.trim() === "") {
      issues.push("SCM: default branch is required.");
    }
    if (!hasRequiredAuthSecret(values)) {
      issues.push("SCM: authentication credential is required for selected auth method.");
    }
    if (!hasValidSCMConnection) {
      issues.push("SCM: repository validation must succeed.");
    }
    if (isFluxBackend && !scmValidation?.gitopsRepositoryWritable) {
      issues.push("GitOps: repository must be writable.");
    }
    if (values.defaultTTLHours <= 0) {
      issues.push("Policies: default TTL must be greater than 0.");
    }
    const policyError = resourcePolicyValidationMessage(values);
    if (policyError !== "") {
      issues.push(`Policies: ${policyError}`);
    }
    const networkPolicyError = networkPolicyValidationMessage(values);
    if (networkPolicyError !== "") {
      issues.push(`Network policy: ${networkPolicyError}`);
    }
    const cleanupPolicyError = cleanupSafetyValidationMessage(values);
    if (cleanupPolicyError !== "") {
      issues.push(`Cleanup safety: ${cleanupPolicyError}`);
    }
    if (isFluxBackend) {
      if (gitOpsConfigValidation.outputPath !== "") issues.push(`GitOps: ${gitOpsConfigValidation.outputPath}`);
      if (gitOpsConfigValidation.namespace !== "") issues.push(`GitOps: ${gitOpsConfigValidation.namespace}`);
      if (gitOpsConfigValidation.gitRepositoryRef !== "") issues.push(`GitOps: ${gitOpsConfigValidation.gitRepositoryRef}`);
      if (gitOpsConfigValidation.kustomizationRef !== "") issues.push(`GitOps: ${gitOpsConfigValidation.kustomizationRef}`);
      if (gitOpsConfigValidation.commitMode !== "") issues.push(`GitOps: ${gitOpsConfigValidation.commitMode}`);
    }
    if (isHelmDirectBackend) {
      if (values.helmDirectChartRef.trim() === "") {
        issues.push("Helm: chart reference is required.");
      }
      if (values.helmDirectReleaseNamePattern.trim() === "") {
        issues.push("Helm: release name pattern is required.");
      }
      if (values.helmDirectNamespacePattern.trim() === "") {
        issues.push("Helm: namespace pattern is required.");
      }
      if (!helmValuesOverrideStrategies.includes(values.helmDirectValuesOverrideStrategy)) {
        issues.push("Helm: values override strategy is required.");
      }
      if (values.helmDirectImageTagValuePath.trim() === "") {
        issues.push("Helm: image tag value path is required.");
      }
      if (!Number.isFinite(values.helmDirectTimeout) || values.helmDirectTimeout <= 0) {
        issues.push("Helm: timeout must be greater than 0.");
      }
    }
    if (domainError !== "") issues.push(`Domain: ${domainError}`);
    if (hostTemplateError !== "") issues.push(`Domain: ${hostTemplateError}`);
    if (agentStatus?.status !== "connected") {
      issues.push("Cluster: agent must be connected.");
    }
    if (effectiveIngressClass === "") {
      issues.push("Cluster: ingress class must be selected.");
    }
    if (values.selectedBaseNamespaces.length === 0) {
      issues.push("Namespaces: at least one base namespace is required.");
    }
    if (discoveredResources.length === 0) {
      issues.push("Resources: discovery scan result is required.");
    }
    if (invalidResourceItems.size > 0) {
      issues.push("Resources: fix invalid include/strategy combinations.");
    }
    if (configMapValidation.errors.length > 0) {
      issues.push(...configMapValidation.errors.map((item) => `ConfigMaps: ${item}`));
    }
    if (serviceClassificationValidation.errors.length > 0) {
      issues.push(...serviceClassificationValidation.errors.map((item) => `Services: ${item.message}`));
    }
    if (routingValidation.errors.length > 0) {
      issues.push(...routingValidation.errors.map((item) => `Routing: ${item}`));
    }
    if (envTemplateValidation.errors.length > 0) {
      issues.push(...envTemplateValidation.errors.map((item) => `Env vars: ${item}`));
    }
    if (secretStrategyValidation.errors.length > 0) {
      issues.push(...secretStrategyValidation.errors.map((item) => `Secrets: ${item}`));
    }
    if (manifestTemplates.length === 0) {
      issues.push("Templates: at least one generated template is required.");
    }
    return Array.from(new Set(issues));
  }, [
    agentStatus?.status,
    cleanupSafetyValidationMessage,
    configMapValidation.errors,
    discoveredResources.length,
    domainError,
    effectiveIngressClass,
    envTemplateValidation.errors,
    gitOpsConfigValidation.commitMode,
    gitOpsConfigValidation.gitRepositoryRef,
    gitOpsConfigValidation.kustomizationRef,
    gitOpsConfigValidation.namespace,
    gitOpsConfigValidation.outputPath,
    hasValidSCMConnection,
    hostTemplateError,
    invalidResourceItems.size,
    manifestTemplates.length,
    networkPolicyValidationMessage,
    resourcePolicyValidationMessage,
    routingValidation.errors,
    isFluxBackend,
    isHelmDirectBackend,
    scmValidation?.gitopsRepositoryWritable,
    secretStrategyValidation.errors,
    serviceClassificationValidation.errors,
    values,
  ]);

  const reviewWarnings = useMemo(() => {
    const warnings: string[] = [];
    warnings.push(...serviceClassificationValidation.warnings.map((item) => item.message));
    warnings.push(...routingValidation.warnings);
    warnings.push(...envTemplateValidation.warnings);
    warnings.push(...secretStrategyValidation.warnings);
    if (networkPolicyWarning !== "") {
      warnings.push(networkPolicyWarning);
    }
    return Array.from(new Set(warnings.filter((item) => item.trim() !== "")));
  }, [
    envTemplateValidation.warnings,
    networkPolicyWarning,
    routingValidation.warnings,
    secretStrategyValidation.warnings,
    serviceClassificationValidation.warnings,
  ]);

  const isStepComplete = useCallback(
    (step: number, nextValues: WizardValues = values) => {
      if (step === 0) {
        return nextValues.repositoryUrl.trim() !== "" &&
          nextValues.gitopsRepoUrl.trim() !== "" &&
          isValidRepositoryURL(nextValues.repositoryUrl) &&
          isValidRepositoryURL(nextValues.gitopsRepoUrl) &&
          nextValues.defaultBranch.trim() !== "" &&
          hasRequiredAuthSecret(nextValues) &&
          hasValidSCMConnection;
      }
      if (step === 1) {
        const policyError = resourcePolicyValidationMessage(nextValues);
        const networkPolicyError = networkPolicyValidationMessage(nextValues);
        const cleanupSafetyError = cleanupSafetyValidationMessage(nextValues);
        const baseChecks = nextValues.defaultMode.trim() !== "" &&
          nextValues.defaultTTLHours > 0 &&
          policyError === "" &&
          networkPolicyError === "" &&
          cleanupSafetyError === "" &&
          domainError === "" &&
          hostTemplateError === "";
        if (!isFluxBackend) {
          return baseChecks;
        }
        return nextValues.defaultMode.trim() !== "" &&
          nextValues.defaultTTLHours > 0 &&
          policyError === "" &&
          networkPolicyError === "" &&
          cleanupSafetyError === "" &&
          nextValues.gitOpsOutputPath.trim() !== "" &&
          nextValues.fluxNamespace.trim() !== "" &&
          nextValues.fluxGitRepositoryRef.trim() !== "" &&
          nextValues.fluxKustomizationRef.trim() !== "" &&
          nextValues.gitOpsCommitMode.trim() !== "" &&
          gitOpsConfigValidation.commitMode === "" &&
          gitOpsConfigValidation.outputPath === "" &&
          gitOpsConfigValidation.namespace === "" &&
          gitOpsConfigValidation.kustomizationRef === "" &&
          gitOpsConfigValidation.gitRepositoryRef === "" &&
          hasValidSCMConnection &&
          scmValidation?.gitopsRepositoryWritable === true &&
          domainError === "" &&
          hostTemplateError === "";
      }
      if (step === 2) {
        return agentStatus?.status === "connected" && effectiveIngressClass !== "";
      }
      if (step === 3) {
        return nextValues.selectedBaseNamespaces.length > 0;
      }
      if (step === 4) {
        if (discoveredResources.length === 0) {
          return false;
        }
        for (const resource of discoveredResources) {
          const item = getResourceReviewItem(resource, nextValues);
          if (validateResourceSelection(resource, item) !== "") {
            return false;
          }
        }
      }
      if (step === 5) {
        return configMapValidation.errors.length === 0;
      }
      if (step === 6) {
        return serviceClassificationValidationMessage === "" && routingValidation.errors.length === 0;
      }
      if (step === 7) {
        return envTemplateValidation.errors.length === 0;
      }
      if (step === 8) {
        return secretStrategyValidation.errors.length === 0;
      }
      if (step === 9) {
        return manifestTemplates.length > 0;
      }
      if (step === 10) {
        return reviewBlockingErrors.length === 0;
      }
      return true;
    },
    [
      values,
      hasValidSCMConnection,
      scmValidation?.gitopsRepositoryWritable,
      agentStatus,
      discoveredResources,
      getResourceReviewItem,
      validateResourceSelection,
      configMapValidation.errors.length,
      serviceClassificationValidationMessage,
      routingValidation.errors.length,
      envTemplateValidation.errors.length,
      secretStrategyValidation.errors.length,
      manifestTemplates.length,
      domainError,
      hostTemplateError,
      isFluxBackend,
      gitOpsConfigValidation.outputPath,
      gitOpsConfigValidation.namespace,
      gitOpsConfigValidation.kustomizationRef,
      gitOpsConfigValidation.gitRepositoryRef,
      gitOpsConfigValidation.commitMode,
      effectiveIngressClass,
      reviewBlockingErrors.length
    ]
  );

  const canProceedFromCurrentStep = useMemo(() => isStepComplete(currentStep), [currentStep, isStepComplete]);

  const canNavigateToStep = useCallback(
    (targetStep: number) => {
      if (targetStep < 0 || targetStep >= steps.length) {
        return false;
      }
      if (targetStep <= currentStep) {
        return true;
      }
      for (let step = 0; step < targetStep; step += 1) {
        if (!isStepComplete(step)) {
          return false;
        }
      }
      return true;
    },
    [currentStep, isStepComplete]
  );

  const getStepStatus = useCallback(
    (stepIndex: number): "completed" | "current" | "ready" | "locked" => {
      if (stepIndex === currentStep) {
        return "current";
      }
      if (stepIndex < currentStep) {
        return isStepComplete(stepIndex) ? "completed" : "locked";
      }
      return canNavigateToStep(stepIndex) ? "ready" : "locked";
    },
    [canNavigateToStep, currentStep, isStepComplete]
  );

  const getStepIcon = useCallback((status: "completed" | "current" | "ready" | "locked") => {
    switch (status) {
      case "completed":
        return "✓";
      case "current":
        return "→";
      case "ready":
        return "◯";
      case "locked":
      default:
        return "×";
    }
  }, []);

  const getStepStatusLabel = useCallback(
    (status: "completed" | "current" | "ready" | "locked", index: number) => {
      const label = steps[index]?.label ?? `Step ${index + 1}`;
      if (status === "current") {
        return `${label}: in progress`;
      }
      if (status === "completed") {
        return `${label}: completed`;
      }
      if (status === "ready") {
        return `${label}: ready`;
      }
      return `${label}: locked`;
    },
    []
  );

  const getStepDisabledHint = useCallback((stepIndex: number) => {
    if (canNavigateToStep(stepIndex)) {
      return "";
    }
    for (let prev = 0; prev < stepIndex; prev += 1) {
      if (!isStepComplete(prev)) {
        return `Step ${prev + 1} must be completed before moving here.`;
      }
    }
    return "Complete required fields on earlier steps first.";
  }, [canNavigateToStep, isStepComplete]);

  const stepValidationMessage = useCallback(
    (step: number, nextValues: WizardValues = values) => {
      if (step === 0) {
        if (!isValidRepositoryURL(nextValues.repositoryUrl.trim()) || nextValues.repositoryUrl.trim() === "") {
          return "A valid application repository URL is required.";
        }
        if (!isValidRepositoryURL(nextValues.gitopsRepoUrl.trim()) || nextValues.gitopsRepoUrl.trim() === "") {
          return "A valid GitOps repository URL is required.";
        }
        if (nextValues.defaultBranch.trim() === "") {
          return "Default branch is required.";
        }
        if (!hasRequiredAuthSecret(nextValues)) {
          return "Authentication credential is required for selected auth method.";
        }
        if (!hasValidSCMConnection) {
          return "Validate repositories before continuing.";
        }
      }
      if (step === 1) {
        const policyError = resourcePolicyValidationMessage(nextValues);
        const networkPolicyError = networkPolicyValidationMessage(nextValues);
        const cleanupSafetyError = cleanupSafetyValidationMessage(nextValues);
        if (nextValues.defaultMode.trim() === "") {
          return "Default mode is required.";
        }
        if (nextValues.defaultTTLHours <= 0) {
          return "Default TTL must be greater than 0.";
        }
        if (policyError !== "") {
          return policyError;
        }
        if (networkPolicyError !== "") {
          return networkPolicyError;
        }
        if (cleanupSafetyError !== "") {
          return cleanupSafetyError;
        }
        if (domainError !== "") {
          return domainError;
        }
        if (hostTemplateError !== "") {
          return hostTemplateError;
        }
        if (isFluxBackend) {
          if (gitOpsConfigValidation.outputPath !== "") {
            return gitOpsConfigValidation.outputPath;
          }
          if (gitOpsConfigValidation.namespace !== "") {
            return gitOpsConfigValidation.namespace;
          }
          if (gitOpsConfigValidation.gitRepositoryRef !== "") {
            return gitOpsConfigValidation.gitRepositoryRef;
          }
          if (gitOpsConfigValidation.kustomizationRef !== "") {
            return gitOpsConfigValidation.kustomizationRef;
          }
          if (gitOpsConfigValidation.commitMode !== "") {
            return gitOpsConfigValidation.commitMode;
          }
          if (!scmValidation?.gitopsRepositoryWritable) {
            return "GitOps repository must be writable.";
          }
          if (!hasValidSCMConnection) {
            return "Re-validate repositories before continuing.";
          }
          return "";
        }
        if (nextValues.helmDirectChartRef.trim() === "") {
          return "Helm chart reference is required.";
        }
        if (nextValues.helmDirectReleaseNamePattern.trim() === "") {
          return "Release name pattern is required.";
        }
        if (nextValues.helmDirectNamespacePattern.trim() === "") {
          return "Namespace pattern is required.";
        }
        if (!helmValuesOverrideStrategies.includes(nextValues.helmDirectValuesOverrideStrategy)) {
          return "Select a values override strategy.";
        }
        if (nextValues.helmDirectImageTagValuePath.trim() === "") {
          return "Image tag value path is required.";
        }
        if (!Number.isFinite(nextValues.helmDirectTimeout) || nextValues.helmDirectTimeout <= 0) {
          return "Helm timeout must be greater than 0.";
        }
        return "";
      }
      if (step === 2) {
        if (agentStatus?.status !== "connected") {
          return "Agent must connect before continuing.";
        }
        if (effectiveIngressClass === "") {
          return "Select or enter ingress class before continuing.";
        }
      }
      if (step === 3) {
        if (nextValues.selectedBaseNamespaces.length === 0) {
          return "Select at least one namespace.";
        }
      }
      if (step === 4) {
        if (discoveredResources.length === 0) {
          return "No discovered resources available yet. Run scan and refresh status.";
        }
        if (invalidResourceItems.size > 0) {
          return "Fix invalid include/strategy combinations in Resource review.";
        }
      }
      if (step === 5) {
        if (configMapValidation.errors.length > 0) {
          return configMapValidation.errors[0];
        }
      }
      if (step === 6) {
        if (serviceClassificationValidationMessage !== "") {
          return serviceClassificationValidationMessage;
        }
        if (routingValidation.errors.length > 0) {
          return routingValidation.errors[0];
        }
      }
      if (step === 7) {
        if (envTemplateValidation.errors.length > 0) {
          return envTemplateValidation.errors[0];
        }
      }
      if (step === 8) {
        if (secretStrategyValidation.errors.length > 0) {
          return secretStrategyValidation.errors[0];
        }
      }
      if (step === 9) {
        if (manifestTemplates.length === 0) {
          return "No generated templates found. Run resource scan and ensure resources are selected.";
        }
      }
      return "This step is locked. Complete previous steps first.";
    },
      [
        hasValidSCMConnection,
        scmValidation?.gitopsRepositoryWritable,
        isFluxBackend,
        values,
        agentStatus,
      discoveredResources.length,
      invalidResourceItems.size,
      configMapValidation.errors,
      serviceClassificationValidationMessage,
      routingValidation.errors,
      envTemplateValidation.errors,
      secretStrategyValidation.errors,
      manifestTemplates.length,
      domainError,
      hostTemplateError,
      gitOpsConfigValidation.outputPath,
      gitOpsConfigValidation.namespace,
      gitOpsConfigValidation.kustomizationRef,
      gitOpsConfigValidation.gitRepositoryRef,
      gitOpsConfigValidation.commitMode,
      effectiveIngressClass
    ]
  );

  const currentStepMessage = useMemo(() => {
    if (currentStep < steps.length - 1 && canProceedFromCurrentStep) {
      return "";
    }
    if (currentStep >= steps.length - 1) {
      return "";
    }
    return stepValidationMessage(currentStep);
  }, [canProceedFromCurrentStep, currentStep, stepValidationMessage]);

  const navigationBlockers = useMemo(() => {
    const blockedIndexes = steps
      .map((_, index) => index)
      .filter((index) => !canNavigateToStep(index) && index > 0 && index <= currentStep + 1);
    if (blockedIndexes.length === 0) {
      return "";
    }
    return blockedIndexes
      .map((index) => `Step ${index + 1}: ${getStepDisabledHint(index)}`)
      .join(" ");
  }, [canNavigateToStep, currentStep, getStepDisabledHint, steps]);

  const persistLocally = useCallback(
    (nextValues: WizardValues, nextStep: number) => {
      try {
        const payload = {
          projectId,
          currentStep: nextStep,
          values: sanitizeValuesForStorage(nextValues)
        };
        window.localStorage.setItem(storageKey(projectId), JSON.stringify(payload));
      } catch {
        // keep behavior stable when localStorage is unavailable
      }
    },
    [projectId]
  );

  const clearSensitiveInputs = useCallback((nextValues: WizardValues): WizardValues => ({
    ...nextValues,
    oauthToken: "",
    appToken: "",
    deployToken: "",
    sshPrivateKey: ""
  }), []);

  const normalizeStoredValues = useCallback((rawValues: Record<string, any>): Partial<WizardValues> => {
    const repositoryUrl = asString(rawValues.repositoryUrl);
    const gitopsRepoUrl = asString(rawValues.gitopsRepoUrl);
    const defaultBranch = asString(rawValues.defaultBranch);
    const defaultTTLHours = asNumber(rawValues.defaultTTLHours, 48);
    const cpuRequest = asString(rawValues.cpuRequest) || asString(rawValues.cpu_request) || "250m";
    const cpuLimit = asString(rawValues.cpuLimit) || asString(rawValues.cpu_limit) || "1000m";
    const memoryRequest = asString(rawValues.memoryRequest) || asString(rawValues.memory_request) || "256Mi";
    const memoryLimit = asString(rawValues.memoryLimit) || asString(rawValues.memory_limit) || "1Gi";
    const storageQuota = asString(rawValues.storageQuota) || asString(rawValues.storage_quota) || "10Gi";
    const maxActiveEnvironments = asNumber(rawValues.maxActiveEnvironments ?? rawValues.max_active_environments, 10);
    const networkFeatureToBaseRaw = asOptionalBoolean(rawValues.networkFeatureToBase ?? rawValues.featureToBase ?? rawValues.feature_to_base);
    const networkBaseToFeatureRaw = asOptionalBoolean(rawValues.networkBaseToFeature ?? rawValues.baseToFeature ?? rawValues.base_to_feature);
    const networkEgressModeRaw = asString(rawValues.networkEgressMode) || asString(rawValues.egressMode) || asString(rawValues.egress_mode);
    const networkEgressMode: NetworkEgressMode = networkEgressModes.includes(networkEgressModeRaw as NetworkEgressMode)
      ? (networkEgressModeRaw as NetworkEgressMode)
      : "restricted";
    const cleanupProtectedRaw = rawValues.cleanupProtectedNamespaces ?? rawValues.protectedNamespaces ?? rawValues.protected_namespaces;
    const cleanupProtectedNamespaces = Array.isArray(cleanupProtectedRaw)
      ? cleanupProtectedRaw.map((item) => asString(item)).filter((item) => item !== "").join(",")
      : asString(cleanupProtectedRaw) || "default,kube-system,kube-public,kube-node-lease,flux-system,cert-manager";
    const cleanupDeleteEnvPlaneLabelsOnlyRaw = asOptionalBoolean(rawValues.cleanupDeleteEnvPlaneLabelsOnly ?? rawValues.deleteEnvPlaneLabeledOnly ?? rawValues.delete_envpilot_labeled_only);
    const cleanupFinalizerStrategyRaw = asString(rawValues.cleanupFinalizerStrategy) || asString(rawValues.finalizerStrategy) || asString(rawValues.finalizer_strategy);
    const cleanupFinalizerStrategy: CleanupFinalizerStrategy = cleanupFinalizerStrategies.includes(cleanupFinalizerStrategyRaw as CleanupFinalizerStrategy)
      ? (cleanupFinalizerStrategyRaw as CleanupFinalizerStrategy)
      : "foreground";
  const defaultModeRaw = asString(rawValues.defaultMode);
  const defaultMode: "Hybrid" | "Full" = defaultModeRaw === "Full" ? "Full" : "Hybrid";
  const scmProviderRaw = asString(rawValues.scmProvider).toLowerCase();
  const scmProvider = scmProviderRaw === "gitlab" ? "gitlab" : "github";
  const authMethodRaw = asString(rawValues.authMethod);
  const authMethod: AuthMethod = authMethods.includes(authMethodRaw as AuthMethod)
    ? (authMethodRaw as AuthMethod)
    : "OAuth";
  const deploymentData = (() => {
    const fromSessionData = asStringAnyMap(rawValues.deployment);
    const fromValues = asString(rawValues.deploymentBackend);
    return fromSessionData["backend"] ?? fromSessionData["backend_name"] ?? fromValues;
  })();
  const deploymentBackendRaw = asString(deploymentData);
  const deploymentBackend = deploymentBackends.includes(deploymentBackendRaw as (typeof deploymentBackends)[number])
    ? (deploymentBackendRaw as DeploymentBackend)
    : "helm_direct";
  const fromSessionDeploymentData = asStringAnyMap(rawValues.deployment);
  const fromSessionHelmDirect = asStringAnyMap(fromSessionDeploymentData.helmDirect);
  const helmDirectSessionChartRef = asString(fromSessionHelmDirect.chartRef);
  const helmDirectSessionReleaseNamePattern = asString(fromSessionHelmDirect.releaseNamePattern);
  const helmDirectSessionNamespacePattern = asString(fromSessionHelmDirect.namespacePattern);
  const helmDirectSessionValuesOverrideStrategy = asString(fromSessionHelmDirect.valuesOverrideStrategy);
  const helmDirectSessionImageTagValuePath = asString(fromSessionHelmDirect.imageTagValuePath);
  const helmDirectSessionWait = asOptionalBoolean(fromSessionHelmDirect.wait);
  const helmDirectSessionCreateNamespace = asOptionalBoolean(fromSessionHelmDirect.createNamespace);
  const helmDirectSessionTimeout = asNumber(fromSessionHelmDirect.timeout, 0);
  const gitOpsOutputPath = asString(rawValues.gitOpsOutputPath) || asString(rawValues.gitOps_output_path) || "environments/{{ .PRNumber }}";
  const fluxNamespace = asString(rawValues.fluxNamespace) || asString(rawValues.flux_namespace) || "flux-system";
  const fluxGitRepositoryRef = asString(rawValues.fluxGitRepositoryRef) || asString(rawValues.flux_git_repository_ref) || "envpilot-gitops";
  const fluxKustomizationRef = asString(rawValues.fluxKustomizationRef) || asString(rawValues.flux_kustomization_ref) || "envpilot-prs";
    const gitOpsCommitModeRaw = asString(rawValues.gitOpsCommitMode);
    const commitModeFromSnake = asString(rawValues.git_ops_commit_mode);
    const gitOpsCommitMode = (["direct", "pull request"] as const).includes(gitOpsCommitModeRaw as GitOpsCommitMode)
      ? (gitOpsCommitModeRaw as GitOpsCommitMode)
      : (["direct", "pull request"] as const).includes(commitModeFromSnake as GitOpsCommitMode)
        ? (commitModeFromSnake as GitOpsCommitMode)
        : "direct";
    const helmDirectChartRef = helmDirectSessionChartRef || asString(rawValues.helmDirectChartRef) || "deploy/helm/{{ .project.id }}";
    const helmDirectReleaseNamePattern = helmDirectSessionReleaseNamePattern || asString(rawValues.helmDirectReleaseNamePattern) || "{{ .project.id }}-{{ .environment.name }}";
    const helmDirectNamespacePattern = helmDirectSessionNamespacePattern || asString(rawValues.helmDirectNamespacePattern) || "envpilot-pr-{{ .PRNumber }}";
    const helmDirectValuesOverrideStrategyRaw = helmDirectSessionValuesOverrideStrategy || asString(rawValues.helmDirectValuesOverrideStrategy);
    const helmDirectValuesOverrideStrategy = helmValuesOverrideStrategies.includes(helmDirectValuesOverrideStrategyRaw as (typeof helmValuesOverrideStrategies)[number])
      ? helmDirectValuesOverrideStrategyRaw
      : "merge";
    const helmDirectImageTagValuePath = helmDirectSessionImageTagValuePath || asString(rawValues.helmDirectImageTagValuePath) || "imageTag";
    const helmDirectWait = helmDirectSessionWait ?? asOptionalBoolean(rawValues.helmDirectWait) ?? true;
    const helmDirectCreateNamespace = helmDirectSessionCreateNamespace ?? asOptionalBoolean(rawValues.helmDirectCreateNamespace) ?? true;
    const helmDirectTimeout = asNumber(rawValues.helmDirectTimeout, helmDirectSessionTimeout || 300);
    const previewDomain = asString(rawValues.previewDomain) || "preview.company.com";
    const hostPatternTemplate = asString(rawValues.hostPatternTemplate) || "pr-{{ .PRNumber }}-{{ .Service }}.preview.company.com";
    const selectedIngressClass = asString(rawValues.selectedIngressClass);
    const manualIngressClass = asString(rawValues.manualIngressClass);
    const routingModeRaw = asString(rawValues.routingMode);
    const routingMode: RoutingMode = routingModes.includes(routingModeRaw as RoutingMode)
      ? (routingModeRaw as RoutingMode)
      : "host-based";
    let selectedBaseNamespaces: string[] = [];
    if (Array.isArray(rawValues.selectedBaseNamespaces)) {
      selectedBaseNamespaces = rawValues.selectedBaseNamespaces
        .map((item) => asString(item))
        .filter((item) => item !== "");
    } else {
      selectedBaseNamespaces = asString(rawValues.selectedBaseNamespaces)
        .split(",")
        .map((item) => item.trim())
        .filter((item) => item !== "");
    }
    let resourceReview: Record<string, ResourceReviewItem> = {};
    if (rawValues.resourceReview && typeof rawValues.resourceReview === "object" && !Array.isArray(rawValues.resourceReview)) {
      resourceReview = rawValues.resourceReview as Record<string, ResourceReviewItem>;
    } else if (typeof rawValues.resourceReview === "string" && rawValues.resourceReview.trim() !== "") {
      try {
        const parsed = JSON.parse(rawValues.resourceReview);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          resourceReview = parsed as Record<string, ResourceReviewItem>;
        }
      } catch {
        resourceReview = {};
      }
    }
    let parsedConfigMapStrategies: Record<string, EditableConfigMapConfig> = {};
    if (rawValues.configMapStrategies && typeof rawValues.configMapStrategies === "object" && !Array.isArray(rawValues.configMapStrategies)) {
      parsedConfigMapStrategies = rawValues.configMapStrategies as Record<string, EditableConfigMapConfig>;
    } else if (typeof rawValues.configMapStrategies === "string" && rawValues.configMapStrategies.trim() !== "") {
      try {
        const parsed = JSON.parse(rawValues.configMapStrategies);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          parsedConfigMapStrategies = parsed as Record<string, EditableConfigMapConfig>;
        }
      } catch {
        parsedConfigMapStrategies = {};
      }
    }
    let parsedServiceClassifications: Record<string, ServiceClassification> = {};
    if (rawValues.serviceClassifications && typeof rawValues.serviceClassifications === "object" && !Array.isArray(rawValues.serviceClassifications)) {
      parsedServiceClassifications = rawValues.serviceClassifications as Record<string, ServiceClassification>;
    } else if (typeof rawValues.serviceClassifications === "string" && rawValues.serviceClassifications.trim() !== "") {
      try {
        const parsed = JSON.parse(rawValues.serviceClassifications);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          parsedServiceClassifications = parsed as Record<string, ServiceClassification>;
        }
      } catch {
        parsedServiceClassifications = {};
      }
    }
    let parsedEnvEditor: Record<string, EditableServiceContainerEnv[]> = {};
    if (rawValues.envEditor && typeof rawValues.envEditor === "object" && !Array.isArray(rawValues.envEditor)) {
      parsedEnvEditor = rawValues.envEditor as Record<string, EditableServiceContainerEnv[]>;
    } else if (typeof rawValues.envEditor === "string" && rawValues.envEditor.trim() !== "") {
      try {
        const parsed = JSON.parse(rawValues.envEditor);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          parsedEnvEditor = parsed as Record<string, EditableServiceContainerEnv[]>;
        }
      } catch {
        parsedEnvEditor = {};
      }
    }
    let parsedSecretStrategies: Record<string, EditableSecretStrategy> = {};
    if (rawValues.secretStrategies && typeof rawValues.secretStrategies === "object" && !Array.isArray(rawValues.secretStrategies)) {
      parsedSecretStrategies = rawValues.secretStrategies as Record<string, EditableSecretStrategy>;
    } else if (rawValues.secret_strategies && typeof rawValues.secret_strategies === "object" && !Array.isArray(rawValues.secret_strategies)) {
      parsedSecretStrategies = rawValues.secret_strategies as Record<string, EditableSecretStrategy>;
    } else if (typeof rawValues.secretStrategies === "string" && rawValues.secretStrategies.trim() !== "") {
      try {
        const parsed = JSON.parse(rawValues.secretStrategies);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          parsedSecretStrategies = parsed as Record<string, EditableSecretStrategy>;
        }
      } catch {
        parsedSecretStrategies = {};
      }
    } else if (typeof rawValues.secret_strategies === "string" && rawValues.secret_strategies.trim() !== "") {
      try {
        const parsed = JSON.parse(rawValues.secret_strategies);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          parsedSecretStrategies = parsed as Record<string, EditableSecretStrategy>;
        }
      } catch {
        parsedSecretStrategies = {};
      }
    }
    return {
      repositoryUrl,
      gitopsRepoUrl,
      defaultBranch,
      defaultMode,
      defaultTTLHours,
      cpuRequest,
      cpuLimit,
      memoryRequest,
      memoryLimit,
      storageQuota,
      maxActiveEnvironments,
      networkFeatureToBase: networkFeatureToBaseRaw ?? true,
      networkBaseToFeature: networkBaseToFeatureRaw ?? false,
      networkEgressMode,
      cleanupProtectedNamespaces,
      cleanupDeleteEnvPlaneLabelsOnly: cleanupDeleteEnvPlaneLabelsOnlyRaw ?? true,
      cleanupFinalizerStrategy,
      scmProvider,
      authMethod,
      deploymentBackend,
      helmDirectChartRef,
      helmDirectReleaseNamePattern,
      helmDirectNamespacePattern,
      helmDirectWait,
      helmDirectTimeout,
      helmDirectValuesOverrideStrategy,
      helmDirectImageTagValuePath,
      helmDirectCreateNamespace,
      gitOpsOutputPath,
      fluxNamespace,
      fluxGitRepositoryRef,
      fluxKustomizationRef,
      gitOpsCommitMode,
      previewDomain,
      hostPatternTemplate,
      selectedIngressClass,
      manualIngressClass,
      routingMode,
      oauthToken: asString(rawValues.oauthToken),
      appToken: asString(rawValues.appToken),
      deployToken: asString(rawValues.deployToken),
      sshPrivateKey: asString(rawValues.sshPrivateKey),
      selectedBaseNamespaces,
      resourceReview,
      configMapStrategies: parsedConfigMapStrategies,
      serviceClassifications: parsedServiceClassifications,
      envEditor: parsedEnvEditor,
      secretStrategies: parsedSecretStrategies
    };
  }, []);

  const extractDiscoveredResourcesFromSessionData = useCallback((data: Record<string, any>): DiscoveredResource[] => {
    const raw = data?.resourceScanReport;
    if (!Array.isArray(raw)) {
      return [];
    }
    const resources: DiscoveredResource[] = [];
    for (const entry of raw) {
      if (!entry || typeof entry !== "object") {
        continue;
      }
      const item = entry as Record<string, unknown>;
      const kind = asString(item.kind);
      const name = asString(item.name);
      const namespace = asString(item.namespace);
      if (!kind || !name || !namespace) {
        continue;
      }
      const labels = item.labels && typeof item.labels === "object" ? (item.labels as Record<string, string>) : undefined;
      const annotations = item.annotations && typeof item.annotations === "object" ? (item.annotations as Record<string, string>) : undefined;
      const manifest = item.manifest && typeof item.manifest === "object" && !Array.isArray(item.manifest)
        ? (item.manifest as Record<string, unknown>)
        : undefined;
      const sourceMapping = item.sourceMapping && typeof item.sourceMapping === "object"
        ? (item.sourceMapping as ResourceSourceMapping)
        : undefined;
      const configMapKeys = Array.isArray(item.configMapKeys)
        ? (item.configMapKeys as unknown[]).map((key) => asString(key)).filter((key) => key !== "")
        : undefined;
      resources.push({
        kind,
        name,
        namespace,
        labels,
        annotations,
        manifest,
        configMapKeys,
        sourceMapping
      });
    }
    resources.sort((a, b) => {
      if (a.namespace !== b.namespace) return a.namespace.localeCompare(b.namespace);
      if (a.kind !== b.kind) return a.kind.localeCompare(b.kind);
      return a.name.localeCompare(b.name);
    });
    return resources;
  }, []);

  const extractManifestTemplatesFromSessionData = useCallback((data: Record<string, any>): ManifestTemplateFile[] => {
    const raw = data?.manifestTemplates;
    if (!Array.isArray(raw)) {
      return [];
    }
    const items: ManifestTemplateFile[] = [];
    for (const entry of raw) {
      if (!entry || typeof entry !== "object") continue;
      const item = entry as Record<string, unknown>;
      const kind = asString(item.kind);
      const namespace = asString(item.namespace);
      const name = asString(item.name);
      const yaml = typeof item.yaml === "string" ? item.yaml : "";
      if (!kind || !name || !yaml) continue;
      items.push({ kind, namespace, name, yaml });
    }
    items.sort((a, b) => {
      const pa = `${a.namespace}/${a.kind}/${a.name}`;
      const pb = `${b.namespace}/${b.kind}/${b.name}`;
      return pa.localeCompare(pb);
    });
    return items;
  }, []);

  const extractServiceGraphFromSessionData = useCallback((data: Record<string, any>): ServiceGraph => {
    const graph = data?.serviceGraph;
    if (!graph || typeof graph !== "object") {
      return { nodes: [], edges: [] };
    }
    const rawNodes = Array.isArray((graph as { nodes?: unknown[] }).nodes) ? (graph as { nodes: unknown[] }).nodes : [];
    const rawEdges = Array.isArray((graph as { edges?: unknown[] }).edges) ? (graph as { edges: unknown[] }).edges : [];
    const nodes: ServiceGraphNode[] = [];
    for (const entry of rawNodes) {
      if (!entry || typeof entry !== "object") continue;
      const node = entry as Record<string, unknown>;
      const id = asString(node.id);
      const kind = asString(node.kind);
      const namespace = asString(node.namespace);
      const name = asString(node.name);
      if (!id || !kind || !namespace || !name) continue;
      const labels = node.labels && typeof node.labels === "object" ? (node.labels as Record<string, string>) : undefined;
      nodes.push({ id, kind, namespace, name, labels });
    }
    const edges: ServiceGraphEdge[] = [];
    for (const entry of rawEdges) {
      if (!entry || typeof entry !== "object") continue;
      const edge = entry as Record<string, unknown>;
      const from = asString(edge.from);
      const to = asString(edge.to);
      const type = asString(edge.type);
      if (!from || !to || !type) continue;
      const reason = asString(edge.reason);
      edges.push({
        from,
        to,
        type,
        reason: reason || undefined,
        confidence: typeof edge.confidence === "number" ? edge.confidence : undefined
      });
    }
    return { nodes, edges };
  }, []);

  const extractServiceEnvsFromSessionData = useCallback((data: Record<string, any>): ServiceDiscoveredEnvGroup[] => {
    const payload = data?.serviceEnvs as ServiceEnvsPayload | undefined;
    if (!payload || !Array.isArray(payload.services)) {
      return [];
    }
    const result: ServiceDiscoveredEnvGroup[] = [];
    for (const service of payload.services) {
      if (!service || typeof service !== "object") continue;
      const serviceId = asString((service as Record<string, unknown>).serviceId);
      const serviceName = asString((service as Record<string, unknown>).serviceName);
      const namespace = asString((service as Record<string, unknown>).namespace);
      const rawContainers = Array.isArray((service as Record<string, unknown>).containers)
        ? ((service as Record<string, unknown>).containers as unknown[])
        : [];
      if (!serviceId || !serviceName || !namespace) continue;
      const containers: ServiceDiscoveredContainerEnv[] = [];
      for (const container of rawContainers) {
        if (!container || typeof container !== "object") continue;
        const item = container as Record<string, unknown>;
        const name = asString(item.container);
        if (!name) continue;
        const vars = Array.isArray(item.vars) ? (item.vars as ServiceDiscoveredEnvVar[]) : [];
        const envFrom = Array.isArray(item.envFrom) ? (item.envFrom as ServiceDiscoveredEnvFrom[]) : [];
        containers.push({ container: name, vars, envFrom });
      }
      result.push({ serviceId, serviceName, namespace, containers });
    }
    result.sort((a, b) => {
      if (a.namespace !== b.namespace) return a.namespace.localeCompare(b.namespace);
      return a.serviceName.localeCompare(b.serviceName);
    });
    return result;
  }, []);

  const patchSession = useCallback(async (nextStep: number, nextValues: WizardValues) => {
    if (!projectId) {
      return;
    }
    try {
      setSaving(true);
      const stepData = sanitizeStepData(nextValues);
      stepData.routingConfig = {
        mode: nextValues.routingMode,
        previewDomain: nextValues.previewDomain,
        hostPatternTemplate: nextValues.hostPatternTemplate,
        ingressClass: effectiveIngressClass,
        hybridFallback: nextValues.routingMode === "hybrid fallback",
        routes: routingPreviewRows.map((row) => ({
          source: row.source,
          targetNamespace: row.targetNamespace,
          targetService: row.targetService,
          classification: row.classification
        }))
      };
      const payload: WizardPayload = {
        current_step: nextStep,
        status: wizardStatusByStep[nextStep] ?? "deployed",
        step_data: stepData
      };
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
      });
      if (!response.ok) {
        throw new Error(`PATCH failed: ${response.status}`);
      }
      const session = (await response.json()) as BootstrapSession;
      const restored = normalizeStoredValues(session.data ?? {});
      setValues((current) => ({ ...current, ...restored }));
      setError("");
      persistLocally({ ...nextValues, ...restored }, nextStep);
    } catch {
      setError("Could not save progress to API. Changes are saved locally.");
      persistLocally(nextValues, nextStep);
    } finally {
      setSaving(false);
    }
  }, [effectiveIngressClass, normalizeStoredValues, persistLocally, projectId, routingPreviewRows]);

  const compileSession = useCallback(async (nextValues: WizardValues) => {
    if (!projectId) {
      return;
    }
    try {
      setCompileSaving(true);
      setSimulateResult(null);
      setError("");
      await patchSession(currentStep, nextValues);
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/compile`, {
        method: "POST",
        headers: { "Content-Type": "application/json" }
      });
      if (!response.ok) {
        const payload = await response.json().catch(() => ({ error: "Compile failed." })) as { error?: string };
        throw new Error(payload.error || "Compile failed.");
      }
      const session = (await response.json()) as BootstrapSession;
      const restored = normalizeStoredValues(session.data ?? {});
      setSessionID(session.id);
      setValues((current) => ({ ...current, ...restored }));
      persistLocally({ ...nextValues, ...restored }, currentStep);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not compile bootstrap session.");
    } finally {
      setCompileSaving(false);
    }
  }, [currentStep, normalizeStoredValues, patchSession, persistLocally, projectId]);

  const simulatePR = useCallback(async (nextValues: WizardValues) => {
    if (!projectId) {
      return;
    }
    try {
      setSimulateSaving(true);
      setError("");
      setSimulateResult(null);
      await patchSession(currentStep, nextValues);
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/simulate-pr`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          dryRunCommit: simulateDryRun,
          dry_run_commit: simulateDryRun
        })
      });
      if (!response.ok) {
        const payload = await response.json().catch(() => ({ error: "PR simulation failed." })) as { error?: string };
        throw new Error(payload.error || "PR simulation failed.");
      }
      const payload = (await response.json()) as BootstrapSimulatePRResponse;
      setSimulateResult(payload);
      if (!payload.validation.valid) {
        setError("PR simulation completed with validation errors.");
      } else {
        setError("");
      }
    } catch {
      setError("Could not simulate PR preview.");
    } finally {
      setSimulateSaving(false);
    }
  }, [currentStep, patchSession, projectId, simulateDryRun]);

  const runSCMValidation = useCallback(async (nextValues: WizardValues): Promise<SCMValidationResult | null> => {
    setScmValidationInFlight(true);
    setError("");
    try {
      const payload: Record<string, any> = {
        provider: nextValues.scmProvider,
        appRepoUrl: nextValues.repositoryUrl,
        gitopsRepoUrl: nextValues.gitopsRepoUrl,
        defaultBranch: nextValues.defaultBranch,
        authMethod: nextValues.authMethod
      };
      const secretField = branchByScmMethod[nextValues.authMethod];
      payload[secretField] = nextValues[secretField as keyof WizardValues];

      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/validate-scm`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
      });
      const raw = await response.text();
      let parsed: SCMValidationResult | null = null;
      try {
        parsed = raw.trim() ? (JSON.parse(raw) as SCMValidationResult) : null;
      } catch {
        parsed = null;
      }

      if (!response.ok) {
        setScmValidation(null);
        setLastScmValidationFingerprint("");
        const errorPayload = parsed as unknown as { error?: string } | null;
        setError(errorPayload && typeof errorPayload.error === "string" ? errorPayload.error : `SCM validation failed: ${response.status}`);
        return null;
      }
      if (!parsed) {
        setError("Unable to read validation response.");
        return null;
      }
      setScmValidation(parsed);
      setLastScmValidationFingerprint(currentScmFingerprint);
      if (parsed.branches.length > 0 && parsed.branches.includes(nextValues.defaultBranch) === false) {
        setValues((previous) => ({ ...previous, defaultBranch: parsed!.branches[0] }));
      }
      if (!parsed.valid) {
        if (parsed.errors.length > 0) {
          setError(parsed.errors.map((item) => item.message).join(" "));
        } else {
          setError("SCM validation failed. Check repositories and access credentials.");
        }
      } else {
        setError("");
      }
      return parsed;
    } catch {
      setScmValidation(null);
      setLastScmValidationFingerprint("");
      setError("Could not validate repositories right now. Please retry.");
      return null;
    } finally {
      setScmValidationInFlight(false);
    }
  }, [currentScmFingerprint, projectId]);

  const refreshManifestTemplates = useCallback(async () => {
    if (!projectId) return;
    try {
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/manifest-templates`);
      if (!response.ok) return;
      const payload = await response.json() as { manifestTemplates?: ManifestTemplateFile[] };
      const items = Array.isArray(payload.manifestTemplates) ? payload.manifestTemplates : [];
      setManifestTemplates(items);
      setEditedTemplateYAML({});
      setActiveTemplatePath((current) => {
        if (current && items.some((item) => templatePath(item) === current)) {
          return current;
        }
        return items.length > 0 ? templatePath(items[0]) : "";
      });
    } catch {
      // keep last known templates on network errors
    }
  }, [projectId, templatePath]);

  const refreshAgentStatus = useCallback(async () => {
    if (!projectId) {
      return null;
    }
    try {
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/agent-status`);
      if (!response.ok) {
        throw new Error(`GET failed: ${response.status}`);
      }
      const status = (await response.json()) as AgentStatusResponse;
      setAgentStatus(status);
      if (status.selectedNamespaces && status.selectedNamespaces.length > 0) {
        setValues((current) => ({ ...current, selectedBaseNamespaces: status.selectedNamespaces ?? [] }));
      }
      const sessionResponse = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session`);
      if (sessionResponse.ok) {
        const session = (await sessionResponse.json()) as BootstrapSession;
        setDiscoveredResources(extractDiscoveredResourcesFromSessionData(session.data));
        setServiceGraph(extractServiceGraphFromSessionData(session.data));
        setDiscoveredServiceEnvs(extractServiceEnvsFromSessionData(session.data));
        const templates = extractManifestTemplatesFromSessionData(session.data);
        setManifestTemplates(templates);
        setEditedTemplateYAML({});
        setActiveTemplatePath(templates.length > 0 ? templatePath(templates[0]) : "");
      }
      await refreshManifestTemplates();
      return status;
    } catch {
      return null;
    }
  }, [extractDiscoveredResourcesFromSessionData, extractManifestTemplatesFromSessionData, extractServiceGraphFromSessionData, extractServiceEnvsFromSessionData, projectId, refreshManifestTemplates, templatePath]);

  const refreshRunnerStatus = useCallback(async () => {
    if (!projectId) {
      return null;
    }
    try {
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/runner-status`);
      if (!response.ok) {
        return null;
      }
      const status = (await response.json()) as RunnerStatusResponse;
      setRunnerStatus(status);
      return status;
    } catch {
      return null;
    }
  }, [projectId]);

  const checkRunnerHealth = useCallback(async () => {
    try {
      const response = await fetch("/api/v1/runners/health");
      if (!response.ok) {
        throw new Error(`GET failed: ${response.status}`);
      }
      const payload = (await response.json()) as RunnerHealthResponse;
      setRunnerHealth(payload);
    } catch {
      setError("Could not fetch runner health status.");
    }
  }, []);

  const generateRunnerDeploymentInstructions = useCallback(async () => {
    if (!projectId) {
      return;
    }
    setRunnerBusy(true);
    setError("");
    try {
      const payload = {
        deploymentMode: runnerForm.mode,
        clusterId: runnerForm.clusterId.trim() || "default",
        runnerNamespace: runnerForm.runnerNamespace.trim() || "envpilot-system",
        releaseName: runnerForm.releaseName.trim() || `envpilot-runner-${projectId}`,
        gitOpsPath: runnerForm.gitOpsPath.trim()
      };
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/runner-deployment-instructions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
      });
      if (!response.ok) {
        const raw = await response.text();
        throw new Error(raw.trim() || `POST failed: ${response.status}`);
      }
      const deployed = (await response.json()) as RunnerDeploymentInstructionsResponse;
      setRunnerDeploymentInstructions(deployed);
      await refreshRunnerStatus();
    } catch {
      setError("Could not generate runner deployment instructions.");
    } finally {
      setRunnerBusy(false);
    }
  }, [projectId, refreshRunnerStatus, runnerForm]);

  const generateAgentInstallCommand = useCallback(async () => {
    if (!projectId) {
      return;
    }
    setAgentBusy(true);
    setError("");
    try {
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/agent-token`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({})
      });
      if (!response.ok) {
        throw new Error(`POST failed: ${response.status}`);
      }
      const payload = (await response.json()) as AgentInstallResponse;
      setAgentInstall(payload);
      await refreshAgentStatus();
    } catch {
      setError("Could not generate agent install command.");
    } finally {
      setAgentBusy(false);
    }
  }, [projectId, refreshAgentStatus]);

  const startResourceScan = useCallback(async () => {
    if (!projectId) {
      return;
    }
    setAgentBusy(true);
    setError("");
    try {
      const nextStep = Math.max(currentStep, 3);
      await patchSession(nextStep, values);
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session/resource-scan/start`, {
        method: "POST"
      });
      if (!response.ok) {
        throw new Error(`POST failed: ${response.status}`);
      }
      await refreshAgentStatus();
    } catch {
      setError("Could not start resource scan.");
    } finally {
      setAgentBusy(false);
    }
  }, [currentStep, patchSession, projectId, refreshAgentStatus, values]);

  const toggleNamespace = useCallback((namespace: string) => {
    setValues((current) => {
      const selected = new Set(current.selectedBaseNamespaces);
      if (selected.has(namespace)) {
        selected.delete(namespace);
      } else {
        selected.add(namespace);
      }
      const merged = { ...current, selectedBaseNamespaces: Array.from(selected).sort() };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, persistLocally]);

  const loadSession = useCallback(async () => {
    setLoading(true);
    setError("");
    setSimulateResult(null);
    const loadAndRestore = async () => {
      try {
        const raw = window.localStorage.getItem(storageKey(projectId));
        if (!raw) {
          return;
        }
        const parsed = JSON.parse(raw);
        if (parsed?.projectId !== projectId) {
          return;
        }
        if (typeof parsed.currentStep === "number" && Number.isFinite(parsed.currentStep)) {
          setCurrentStep(parsed.currentStep);
        }
        if (parsed.values && typeof parsed.values === "object") {
          setValues((current) => ({
            ...current,
            ...normalizeStoredValues(parsed.values)
          }));
        }
      } catch {
        // localStorage data can be stale or unavailable
      }
    };

    const hydrate = (restored: Partial<WizardValues>, restoreStep: number) => {
      setValues((current) => {
        const merged = { ...current, ...restored };
        persistLocally(merged, restoreStep);
        return merged;
      });
    };

    await loadAndRestore();

    try {
      const sessionResponse = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session`);
      if (sessionResponse.status === 404) {
        const createResponse = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session`, {
          method: "POST",
          headers: { "Content-Type": "application/json" }
        });
        if (!createResponse.ok) {
          throw new Error(`POST failed: ${createResponse.status}`);
        }
        const created = (await createResponse.json()) as BootstrapSession;
        setSessionID(created.id);
        setCurrentStep(created.current_step);
        hydrate(normalizeStoredValues(created.data), created.current_step);
        await refreshRunnerStatus();
        setLoading(false);
        return;
      }
      if (!sessionResponse.ok) {
        throw new Error(`GET failed: ${sessionResponse.status}`);
      }
      const session = (await sessionResponse.json()) as BootstrapSession;
      setSessionID(session.id);
      setCurrentStep(session.current_step);
      const restored = normalizeStoredValues(session.data);
      setDiscoveredResources(extractDiscoveredResourcesFromSessionData(session.data));
      setServiceGraph(extractServiceGraphFromSessionData(session.data));
      setDiscoveredServiceEnvs(extractServiceEnvsFromSessionData(session.data));
      const templates = extractManifestTemplatesFromSessionData(session.data);
      setManifestTemplates(templates);
      setEditedTemplateYAML({});
      setActiveTemplatePath(templates.length > 0 ? templatePath(templates[0]) : "");
      hydrate(restored, session.current_step);
      await refreshManifestTemplates();
      await refreshRunnerStatus();
    } catch {
      setError("Could not load bootstrap session from API, using local draft if available.");
    } finally {
      setLoading(false);
    }
  }, [extractDiscoveredResourcesFromSessionData, extractManifestTemplatesFromSessionData, extractServiceGraphFromSessionData, extractServiceEnvsFromSessionData, normalizeStoredValues, persistLocally, projectId, refreshManifestTemplates, refreshRunnerStatus, templatePath]);

  const updateResourceReview = useCallback((resource: DiscoveredResource, patch: Partial<ResourceReviewItem>) => {
    setValues((current) => {
      const key = resourceKey(resource);
      const existing = getResourceReviewItem(resource, current);
      const next: ResourceReviewItem = {
        include: typeof patch.include === "boolean" ? patch.include : existing.include,
        strategy: patch.strategy ?? existing.strategy
      };
      const merged = {
        ...current,
        resourceReview: {
          ...current.resourceReview,
          [key]: next
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getResourceReviewItem, persistLocally, resourceKey]);

  const updateConfigMapStrategy = useCallback((resource: DiscoveredResource, strategy: ConfigMapStrategy) => {
    setValues((current) => {
      const key = resourceKey(resource);
      const existing = getConfigMapConfig(resource, current);
      const merged = {
        ...current,
        configMapStrategies: {
          ...current.configMapStrategies,
          [key]: { ...existing, strategy }
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getConfigMapConfig, persistLocally, resourceKey]);

  const updateConfigMapKeySelection = useCallback((resource: DiscoveredResource, keyName: string, selected: boolean) => {
    setValues((current) => {
      const mapKey = resourceKey(resource);
      const existing = getConfigMapConfig(resource, current);
      const merged = {
        ...current,
        configMapStrategies: {
          ...current.configMapStrategies,
          [mapKey]: {
            ...existing,
            keys: {
              ...existing.keys,
              [keyName]: {
                ...(existing.keys[keyName] ?? { value: "{{ .Branch }}" }),
                selected
              }
            }
          }
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getConfigMapConfig, persistLocally, resourceKey]);

  const updateConfigMapTemplatedValue = useCallback((resource: DiscoveredResource, keyName: string, value: string) => {
    setValues((current) => {
      const mapKey = resourceKey(resource);
      const existing = getConfigMapConfig(resource, current);
      const merged = {
        ...current,
        configMapStrategies: {
          ...current.configMapStrategies,
          [mapKey]: {
            ...existing,
            keys: {
              ...existing.keys,
              [keyName]: {
                ...(existing.keys[keyName] ?? { selected: true }),
                selected: true,
                value
              }
            }
          }
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getConfigMapConfig, persistLocally, resourceKey]);

  const updateServiceClassification = useCallback((serviceID: string, classification: ServiceClassification) => {
    setValues((current) => {
      const merged = {
        ...current,
        serviceClassifications: {
          ...current.serviceClassifications,
          [serviceID]: classification
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, persistLocally]);

  const updateEnvVar = useCallback((serviceId: string, containerName: string, varId: string, patch: Partial<EditableServiceEnvVar>) => {
    setValues((current) => {
      const service = sortedDiscoveredServiceEnvs.find((item) => item.serviceId === serviceId);
      if (!service) return current;
      const containers = editorForService(service, current).map((container) => {
        if (container.container !== containerName) return container;
        return {
          ...container,
          variables: container.variables.map((item) => item.id === varId ? { ...item, ...patch } : item)
        };
      });
      const merged = {
        ...current,
        envEditor: {
          ...current.envEditor,
          [serviceId]: containers
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, editorForService, persistLocally, sortedDiscoveredServiceEnvs]);

  const deleteEnvVar = useCallback((serviceId: string, containerName: string, varId: string) => {
    setValues((current) => {
      const service = sortedDiscoveredServiceEnvs.find((item) => item.serviceId === serviceId);
      if (!service) return current;
      const containers = editorForService(service, current).map((container) => {
        if (container.container !== containerName) return container;
        return {
          ...container,
          variables: container.variables.filter((item) => item.id !== varId)
        };
      });
      const merged = {
        ...current,
        envEditor: {
          ...current.envEditor,
          [serviceId]: containers
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, editorForService, persistLocally, sortedDiscoveredServiceEnvs]);

  const addEnvVar = useCallback((serviceId: string, containerName: string) => {
    setValues((current) => {
      const service = sortedDiscoveredServiceEnvs.find((item) => item.serviceId === serviceId);
      if (!service) return current;
      const now = Date.now().toString(36);
      const containers = editorForService(service, current).map((container) => {
        if (container.container !== containerName) return container;
        return {
          ...container,
          variables: [
            ...container.variables,
            {
              id: `${containerName}:new:${now}`,
              name: "",
              type: "static" as const,
              value: "",
              required: false
            }
          ]
        };
      });
      const merged = {
        ...current,
        envEditor: {
          ...current.envEditor,
          [serviceId]: containers
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, editorForService, persistLocally, sortedDiscoveredServiceEnvs]);

  const updateSecretStrategyOption = useCallback((secret: DiscoveredSecretRef, strategy: SecretStrategy | "") => {
    setValues((current) => {
      const existing = getSecretStrategy(secret, current);
      const next: EditableSecretStrategy = {
        ...existing,
        strategy,
      };
      if (strategy !== "manual input") {
        next.manualValue = "";
      }
      const merged = {
        ...current,
        secretStrategies: {
          ...current.secretStrategies,
          [secret.id]: next
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getSecretStrategy, persistLocally]);

  const updateSecretBackend = useCallback((secret: DiscoveredSecretRef, backend: string) => {
    setValues((current) => {
      const existing = getSecretStrategy(secret, current);
      const merged = {
        ...current,
        secretStrategies: {
          ...current.secretStrategies,
          [secret.id]: {
            ...existing,
            backend
          }
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getSecretStrategy, persistLocally]);

  const updateSecretReference = useCallback((secret: DiscoveredSecretRef, reference: string) => {
    setValues((current) => {
      const existing = getSecretStrategy(secret, current);
      const merged = {
        ...current,
        secretStrategies: {
          ...current.secretStrategies,
          [secret.id]: {
            ...existing,
            reference
          }
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getSecretStrategy, persistLocally]);

  const updateSecretManualValue = useCallback((secret: DiscoveredSecretRef, manualValue: string) => {
    setValues((current) => {
      const existing = getSecretStrategy(secret, current);
      const merged = {
        ...current,
        secretStrategies: {
          ...current.secretStrategies,
          [secret.id]: {
            ...existing,
            manualValue,
            manualValueStored: existing.manualValueStored && manualValue.trim() === ""
          }
        }
      };
      persistLocally(merged, currentStep);
      return merged;
    });
  }, [currentStep, getSecretStrategy, persistLocally]);

  useEffect(() => {
    void loadSession();
  }, [loadSession]);

  useEffect(() => {
    let active = true;
    const poll = async () => {
      if (!active) {
        return;
      }
      await refreshAgentStatus();
      await refreshRunnerStatus();
    };
    void poll();
    const timer = window.setInterval(() => {
      void poll();
    }, 5000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [refreshAgentStatus, refreshRunnerStatus]);

  const goNext = async () => {
    if (!isStepComplete(currentStep)) {
      return;
    }
    if (currentStep >= steps.length - 1) {
      await compileSession(values);
      return;
    }
    const nextStep = Math.min(currentStep + 1, steps.length - 1);

    if (currentStep === 0) {
      const validation = await runSCMValidation(values);
      if (!validation || !validation.valid) {
        return;
      }
      await patchSession(nextStep, values);
      const next = clearSensitiveInputs(values);
      setValues(next);
      setCurrentStep(nextStep);
      return;
    }

    if (currentStep === 3) {
      await startResourceScan();
    }

    setCurrentStep(nextStep);
    void patchSession(nextStep, values);
  };

  const goBack = () => {
    const nextStep = Math.max(currentStep - 1, 0);
    setCurrentStep(nextStep);
    void patchSession(nextStep, values);
  };

  const goToStep = (nextStep: number) => {
    if (!canNavigateToStep(nextStep)) {
      return;
    }
    setCurrentStep(nextStep);
    void patchSession(nextStep, values);
  };

  const updateValue = (next: Partial<WizardValues>) => {
    setValues((current) => {
      const merged = { ...current, ...next };
      persistLocally(merged, currentStep);
      return merged;
    });
    setSimulateResult(null);
    if (currentStep === 0) {
      setScmValidation(null);
      setLastScmValidationFingerprint("");
    }
  };

  const updateActiveTemplateYAML = useCallback((nextYAML: string) => {
    if (!effectiveActiveTemplatePath) return;
    setEditedTemplateYAML((current) => ({
      ...current,
      [effectiveActiveTemplatePath]: nextYAML
    }));
  }, [effectiveActiveTemplatePath]);

  const saveEditedTemplates = useCallback(async () => {
    if (!projectId || manifestTemplates.length === 0) return;
    const payloadTemplates = manifestTemplates.map((item) => {
      const path = templatePath(item);
      return {
        ...item,
        yaml: editedTemplateYAML[path] ?? item.yaml
      };
    });
    try {
      setTemplateSaving(true);
      setError("");
      const response = await fetch(`/api/projects/${encodeURIComponent(projectId)}/bootstrap-session`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          current_step: currentStep,
          status: wizardStatusByStep[currentStep] ?? "compiled",
          step_data: {
            manifestTemplates: payloadTemplates
          }
        })
      });
      if (!response.ok) {
        throw new Error(`PATCH failed: ${response.status}`);
      }
      const session = (await response.json()) as BootstrapSession;
      const persisted = extractManifestTemplatesFromSessionData(session.data);
      setManifestTemplates(persisted);
      setEditedTemplateYAML({});
      setActiveTemplatePath((current) => {
        if (current && persisted.some((item) => templatePath(item) === current)) {
          return current;
        }
        return persisted.length > 0 ? templatePath(persisted[0]) : "";
      });
    } catch {
      setError("Could not save template changes.");
    } finally {
      setTemplateSaving(false);
    }
  }, [currentStep, editedTemplateYAML, extractManifestTemplatesFromSessionData, manifestTemplates, projectId, templatePath]);

  const validateNow = async () => {
    await runSCMValidation(values);
  };

  if (loading) {
    return (
      <Card>
        <CardHeader>
          <h2>Bootstrap setup</h2>
        </CardHeader>
        <div>Loading setup draft…</div>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <h2>Bootstrap setup</h2>
        {sessionID ? <span className="muted">Session: {sessionID}</span> : null}
      </CardHeader>

      <div className="wizard-stepper" role="list" aria-label="Setup steps">
        {steps.map((step, index) => {
          const status = getStepStatus(index);
          const canNavigate = canNavigateToStep(index);
          const disabledHint = getStepDisabledHint(index);
          const describedById = `bootstrap-step-hint-${index}`;
          return (
            <button
              key={step.id}
              className={`wizard-step wizard-step-${status}`}
              aria-selected={currentStep === index}
              aria-disabled={!canNavigate}
              aria-label={`Step ${index + 1}: ${getStepStatusLabel(status, index)}.`}
              aria-describedby={!canNavigate ? describedById : undefined}
              disabled={!canNavigate}
              title={canNavigate ? step.description : disabledHint}
              type="button"
              onClick={() => goToStep(index)}
            >
              <span className="wizard-step-icon">{getStepIcon(status)}</span>
              <strong>{index + 1}</strong>
              <span>{step.label}</span>
              <small>
                {status === "current"
                  ? "In progress"
                  : status === "completed"
                    ? "Completed"
                    : status === "ready"
                      ? "Ready"
                      : "Locked"}
              </small>
              {!canNavigate ? (
                <span id={describedById} className="sr-only">
                  {disabledHint}
                </span>
              ) : null}
            </button>
          );
        })}
      </div>

      <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">
        Step {currentStep + 1} of {steps.length}. {getStepStatusLabel(getStepStatus(currentStep), currentStep)}.
      </div>

      <div className="wizard-progress" role="progressbar" aria-valuemin={1} aria-valuemax={steps.length} aria-valuenow={currentStep + 1}>
        <div className="wizard-progress-track">
          <div
            className="wizard-progress-fill"
            style={{ width: `${steps.length > 1 ? ((currentStep / (steps.length - 1)) * 100).toFixed(0) : 100}%` }}
          />
        </div>
        <div className="wizard-progress-text">
          Step {currentStep + 1} of {steps.length}
        </div>
      </div>

      {error ? <Toast tone="danger">{error}</Toast> : null}
      {currentStepMessage ? <Toast tone="warning">{currentStepMessage}</Toast> : null}
      {navigationBlockers ? <Toast tone="warning">Why can&apos;t I continue: {navigationBlockers}</Toast> : null}

      <div className="project-settings-form">
        {currentStep === 0 ? (
          <>
            <label>
              SCM provider
              <select
                value={values.scmProvider}
                onChange={(event) => {
                  const nextProvider = event.target.value === "gitlab" ? "gitlab" : "github";
                  updateValue({ scmProvider: nextProvider });
                }}
              >
                <option value="github">GitHub</option>
                <option value="gitlab">GitLab</option>
              </select>
            </label>
            <label>
              App repository URL
              <input
                value={values.repositoryUrl}
                onChange={(event) => {
                  updateValue({ repositoryUrl: event.target.value });
                }}
              />
            </label>
            <label>
              GitOps repository URL
              <input
                value={values.gitopsRepoUrl}
                onChange={(event) => {
                  updateValue({ gitopsRepoUrl: event.target.value });
                }}
              />
            </label>
            <label>
              Default branch
              {scmValidation?.branches && scmValidation.branches.length > 0 ? (
                <select
                  value={values.defaultBranch}
                  onChange={(event) => {
                    updateValue({ defaultBranch: event.target.value });
                  }}
                >
                  {scmValidation.branches
                    .filter((branch) => branch.trim())
                    .map((branch) => (
                      <option key={branch} value={branch}>
                        {branch}
                      </option>
                    ))}
                </select>
              ) : (
                <input
                  value={values.defaultBranch}
                  onChange={(event) => {
                    updateValue({ defaultBranch: event.target.value });
                  }}
                />
              )}
            </label>
            <label>
              Auth method
              <select
                value={values.authMethod}
                onChange={(event) => {
                  updateValue({ authMethod: event.target.value as AuthMethod });
                }}
              >
                {authMethods.map((authMethod) => (
                  <option key={authMethod} value={authMethod}>
                    {authMethod}
                  </option>
                ))}
              </select>
            </label>
            {values.authMethod === "OAuth" ? (
              <label>
                OAuth token
                <input
                  type="password"
                  value={values.oauthToken}
                  onChange={(event) => {
                    updateValue({ oauthToken: event.target.value });
                  }}
                />
              </label>
            ) : null}
            {values.authMethod === "App token" ? (
              <label>
                App token
                <input
                  type="password"
                  value={values.appToken}
                  onChange={(event) => {
                    updateValue({ appToken: event.target.value });
                  }}
                />
              </label>
            ) : null}
            {values.authMethod === "Deploy token" ? (
              <label>
                Deploy token
                <input
                  type="password"
                  value={values.deployToken}
                  onChange={(event) => {
                    updateValue({ deployToken: event.target.value });
                  }}
                />
              </label>
            ) : null}
            {values.authMethod === "SSH key" ? (
              <label>
                SSH private key
                <input
                  type="password"
                  value={values.sshPrivateKey}
                  onChange={(event) => {
                    updateValue({ sshPrivateKey: event.target.value });
                  }}
                />
              </label>
            ) : null}

            <div className="wizard-actions">
              <Button disabled={scmValidationInFlight} variant="secondary" type="button" onClick={validateNow}>
                {scmValidationInFlight ? "Validating…" : "Validate repositories"}
              </Button>
              {hasValidSCMConnection ? <span className="muted">Repositories validated</span> : null}
            </div>

            {scmValidation ? (
              <div className="validation-result">
                <div>Validation: {scmValidation.valid ? "ok" : "issues"}</div>
                <div>App repo read: {scmValidation.appRepositoryReadable ? "yes" : "no"}</div>
                <div>GitOps writable: {scmValidation.gitopsRepositoryWritable ? "yes" : "no"}</div>
                {scmValidation.errors.length > 0 ? (
                  <div>
                    <strong>Errors</strong>
                    <ul>
                      {scmValidation.errors.map((item) => (
                        <li key={`${item.field}:${item.code}`}>{item.message}</li>
                      ))}
                    </ul>
                  </div>
                ) : null}
                {scmValidation.warnings.length > 0 ? (
                  <div>
                    <strong>Warnings</strong>
                    <ul>
                      {scmValidation.warnings.map((item) => (
                        <li key={`${item.field}:${item.code}`}>{item.message}</li>
                      ))}
                    </ul>
                  </div>
                ) : null}
                {scmValidation.branches.length > 0 ? <div>Available branches: {scmValidation.branches.join(", ")}</div> : null}
              </div>
            ) : null}
          </>
        ) : currentStep === 1 ? (
          <>
            <label>
              Deployment backend
              <select
                aria-label="Deployment backend"
                value={values.deploymentBackend}
                onChange={(event) => {
                  updateValue({ deploymentBackend: event.target.value as DeploymentBackend });
                }}
              >
                <option value="helm_direct">Helm Direct</option>
                <option value="fluxcd">FluxCD GitOps</option>
              </select>
              <div style={{ color: "#4f4f4f", fontSize: "0.85rem", marginTop: "0.5rem" }}>
                <strong>Tradeoff:</strong>
                <ul style={{ margin: "0.35rem 0 0 1rem", padding: 0 }}>
                  <li>
                    Helm Direct is fast to start and does not require Flux installed.
                  </li>
                  <li>
                    FluxCD GitOps enables fully declarative GitOps flow and is recommended if you want every environment change represented as a Git commit.
                  </li>
                </ul>
              </div>
            </label>
            {isFluxBackend ? (
              <div className="wizard-review">
                <strong>FluxCD GitOps settings</strong>
                <label>
                  GitOps output path
                  <input
                    value={values.gitOpsOutputPath}
                    onChange={(event) => {
                      updateValue({ gitOpsOutputPath: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Flux namespace
                  <input
                    value={values.fluxNamespace}
                    onChange={(event) => {
                      updateValue({ fluxNamespace: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Flux git repository ref
                  <input
                    value={values.fluxGitRepositoryRef}
                    onChange={(event) => {
                      updateValue({ fluxGitRepositoryRef: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Flux kustomization ref
                  <input
                    value={values.fluxKustomizationRef}
                    onChange={(event) => {
                      updateValue({ fluxKustomizationRef: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Commit mode
                  <select
                    value={values.gitOpsCommitMode}
                    onChange={(event) => {
                      updateValue({ gitOpsCommitMode: event.target.value as GitOpsCommitMode });
                    }}
                  >
                    <option value="direct">direct</option>
                    <option value="pull request">pull request</option>
                  </select>
                </label>
              </div>
            ) : (
              <div className="wizard-review">
                <strong>Helm Direct settings</strong>
                <label>
                  Helm chart path / reference
                  <input
                    value={values.helmDirectChartRef}
                    onChange={(event) => {
                      updateValue({ helmDirectChartRef: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Release name pattern
                  <input
                    value={values.helmDirectReleaseNamePattern}
                    onChange={(event) => {
                      updateValue({ helmDirectReleaseNamePattern: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Namespace pattern
                  <input
                    value={values.helmDirectNamespacePattern}
                    onChange={(event) => {
                      updateValue({ helmDirectNamespacePattern: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Values override strategy
                  <select
                    value={values.helmDirectValuesOverrideStrategy}
                    onChange={(event) => {
                      updateValue({ helmDirectValuesOverrideStrategy: event.target.value });
                    }}
                  >
                    {helmValuesOverrideStrategies.map((strategy) => (
                      <option key={strategy} value={strategy}>
                        {strategy}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Image tag value path
                  <input
                    value={values.helmDirectImageTagValuePath}
                    onChange={(event) => {
                      updateValue({ helmDirectImageTagValuePath: event.target.value });
                    }}
                  />
                </label>
                <label>
                  Helm timeout (seconds)
                  <input
                    min={1}
                    type="number"
                    value={values.helmDirectTimeout}
                    onChange={(event) => {
                      updateValue({ helmDirectTimeout: Number(event.target.value) });
                    }}
                  />
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={values.helmDirectWait}
                    onChange={(event) => {
                      updateValue({ helmDirectWait: event.target.checked });
                    }}
                  />
                  Wait for completion
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={values.helmDirectCreateNamespace}
                    onChange={(event) => {
                      updateValue({ helmDirectCreateNamespace: event.target.checked });
                    }}
                  />
                  Create namespace
                </label>
                <div className="muted" style={{ marginTop: "0.5rem" }}>
                  Preview: release <strong>{previewReleaseName}</strong> · namespace <strong>{previewNamespace}</strong>
                </div>
              </div>
            )}
            <label>
              Default mode
              <select
                aria-label="Default mode"
                value={values.defaultMode}
                onChange={(event) => {
                  const nextMode = event.target.value === "Full" ? "Full" : "Hybrid";
                  updateValue({ defaultMode: nextMode });
                }}
              >
                <option value="Full">Full</option>
                <option value="Hybrid">Hybrid</option>
              </select>
            </label>
            <label>
              Default TTL (hours)
              <input
                min={1}
                type="number"
                value={values.defaultTTLHours}
                onChange={(event) => {
                  updateValue({ defaultTTLHours: Number(event.target.value) });
                }}
              />
            </label>
            <label>
              CPU request
              <input
                placeholder="250m"
                value={values.cpuRequest}
                onChange={(event) => {
                  updateValue({ cpuRequest: event.target.value });
                }}
              />
            </label>
            <label>
              CPU limit
              <input
                placeholder="1000m"
                value={values.cpuLimit}
                onChange={(event) => {
                  updateValue({ cpuLimit: event.target.value });
                }}
              />
            </label>
            <label>
              Memory request
              <input
                placeholder="256Mi"
                value={values.memoryRequest}
                onChange={(event) => {
                  updateValue({ memoryRequest: event.target.value });
                }}
              />
            </label>
            <label>
              Memory limit
              <input
                placeholder="1Gi"
                value={values.memoryLimit}
                onChange={(event) => {
                  updateValue({ memoryLimit: event.target.value });
                }}
              />
            </label>
            <label>
              Storage quota
              <input
                placeholder="10Gi"
                value={values.storageQuota}
                onChange={(event) => {
                  updateValue({ storageQuota: event.target.value });
                }}
              />
            </label>
            <label>
              Max active environments
              <input
                min={1}
                type="number"
                value={values.maxActiveEnvironments}
                onChange={(event) => {
                  updateValue({ maxActiveEnvironments: Number(event.target.value) });
                }}
              />
            </label>
            {resourcePolicyValidationMessage(values) !== "" ? (
              <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{resourcePolicyValidationMessage(values)}</div>
            ) : null}
            <div className="wizard-review">
              <strong>Network policy</strong>
              <label style={{ display: "flex", alignItems: "center", gap: "0.5rem" }}>
                <input
                  type="checkbox"
                  checked={values.networkFeatureToBase}
                  onChange={(event) => {
                    updateValue({ networkFeatureToBase: event.target.checked });
                  }}
                />
                Feature namespace can access base namespace services
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.5rem" }}>
                <input
                  type="checkbox"
                  checked={values.networkBaseToFeature}
                  onChange={(event) => {
                    updateValue({ networkBaseToFeature: event.target.checked });
                  }}
                />
                Base namespace can access feature namespace services
              </label>
              <label>
                Egress policy
                <select
                  value={values.networkEgressMode}
                  onChange={(event) => {
                    updateValue({ networkEgressMode: event.target.value as NetworkEgressMode });
                  }}
                >
                  {networkEgressModes.map((mode) => (
                    <option key={mode} value={mode}>{mode}</option>
                  ))}
                </select>
              </label>
              {networkPolicyWarning !== "" ? (
                <div style={{ color: "#8a5200", fontSize: "0.85rem" }}>{networkPolicyWarning}</div>
              ) : null}
            </div>
            <div className="wizard-review">
              <strong>Cleanup safety</strong>
              <label>
                Protected namespaces
                <input
                  placeholder="default,kube-system,flux-system"
                  value={values.cleanupProtectedNamespaces}
                  onChange={(event) => {
                    updateValue({ cleanupProtectedNamespaces: event.target.value });
                  }}
                />
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: "0.5rem" }}>
                <input
                  type="checkbox"
                  checked={values.cleanupDeleteEnvPlaneLabelsOnly}
                  onChange={(event) => {
                    updateValue({ cleanupDeleteEnvPlaneLabelsOnly: event.target.checked });
                  }}
                />
                Delete only resources with EnvPlane labels
              </label>
              <label>
                Finalizer strategy
                <select
                  value={values.cleanupFinalizerStrategy}
                  onChange={(event) => {
                    updateValue({ cleanupFinalizerStrategy: event.target.value as CleanupFinalizerStrategy });
                  }}
                >
                  {cleanupFinalizerStrategies.map((strategy) => (
                    <option key={strategy} value={strategy}>{strategy}</option>
                  ))}
                </select>
              </label>
              {cleanupSafetyValidationMessage(values) !== "" ? (
                <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{cleanupSafetyValidationMessage(values)}</div>
              ) : null}
            </div>
            <label>
              Base preview domain
              <input
                placeholder="preview.company.com"
                value={values.previewDomain}
                onChange={(event) => {
                  updateValue({ previewDomain: event.target.value });
                }}
              />
              {domainError ? <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{domainError}</div> : null}
            </label>
            <label>
              Host pattern template
              <input
                placeholder="pr-{{ .PRNumber }}-{{ .Service }}.preview.company.com"
                value={values.hostPatternTemplate}
                onChange={(event) => {
                  updateValue({ hostPatternTemplate: event.target.value });
                }}
              />
              {hostTemplateError ? <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{hostTemplateError}</div> : null}
              <div className="muted">Supported placeholders: <code>{`{{ .PRNumber }}`}</code>, <code>{`{{ .Service }}`}</code></div>
            </label>
            {isFluxBackend ? (
              <>
                <label>
                  GitOps output path
                  <input
                    placeholder="environments/{{ .PRNumber }}"
                    value={values.gitOpsOutputPath}
                    onChange={(event) => {
                      updateValue({ gitOpsOutputPath: event.target.value });
                    }}
                  />
                  <div style={{ color: "#b42318", fontSize: "0.85rem" }}>
                    {gitOpsConfigValidation.outputPath || null}
                  </div>
                </label>
                <label>
                  Flux namespace
                  <input
                    value={values.fluxNamespace}
                    onChange={(event) => {
                      updateValue({ fluxNamespace: event.target.value });
                    }}
                  />
                  <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{gitOpsConfigValidation.namespace}</div>
                </label>
                <label>
                  Flux Kustomization reference
                  <input
                    value={values.fluxKustomizationRef}
                    onChange={(event) => {
                      updateValue({ fluxKustomizationRef: event.target.value });
                    }}
                  />
                  <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{gitOpsConfigValidation.kustomizationRef}</div>
                </label>
                <label>
                  Flux GitRepository reference
                  <input
                    value={values.fluxGitRepositoryRef}
                    onChange={(event) => {
                      updateValue({ fluxGitRepositoryRef: event.target.value });
                    }}
                  />
                  <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{gitOpsConfigValidation.gitRepositoryRef}</div>
                </label>
                <label>
                  Commit mode
                  <select
                    value={values.gitOpsCommitMode}
                    onChange={(event) => {
                      updateValue({ gitOpsCommitMode: event.target.value as GitOpsCommitMode });
                    }}
                  >
                    <option value="direct">direct</option>
                    <option value="pull request">pull request</option>
                  </select>
                  <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{gitOpsConfigValidation.commitMode}</div>
                </label>
                {scmValidation ? (
                  <div>
                    <strong>GitOps writable:</strong> {scmValidation.gitopsRepositoryWritable ? "yes" : "no"}
                  </div>
                ) : null}
              </>
            ) : null}
            <div>
              <strong>Sample URL:</strong> {samplePreviewURL || "n/a"}
            </div>
            {isFluxBackend ? null : (
              <div>
                <strong>Deployment mode:</strong> Helm Direct
              </div>
            )}
          </>
        ) : currentStep === 2 ? (
          <div className="wizard-review">
            <div className="wizard-actions">
              <Button disabled={agentBusy} variant="secondary" type="button" onClick={() => void generateAgentInstallCommand()}>
                {agentBusy ? "Generating..." : "Generate install command"}
              </Button>
              <Button disabled={agentBusy} variant="secondary" type="button" onClick={() => void refreshAgentStatus()}>
                Refresh status
              </Button>
            </div>
            {agentInstall?.helmCommand ? (
              <>
                {agentInstall.bootstrapSecretCommand ? (
                  <label>
                    Bootstrap secret command
                    {agentInstall.bootstrapSecretCommandSensitive ? (
                      <span className="muted"> Sensitive: displayed once. Do not paste into logs or support tickets.</span>
                    ) : null}
                    <textarea readOnly rows={4} value={agentInstall.bootstrapSecretCommand} />
                  </label>
                ) : null}
                <label>
                  Helm install command
                  <textarea readOnly rows={5} value={agentInstall.helmCommand} />
                </label>
              </>
            ) : null}
            <div>
              <strong>Connection status:</strong> {agentStatus?.status ?? "waiting"}
            </div>
            <div>
              <strong>Cluster:</strong> {agentStatus?.clusterId ?? agentInstall?.clusterId ?? "n/a"}
            </div>
            <div>
              <strong>Agent:</strong> {agentStatus?.agentId ?? "n/a"}
            </div>
            {agentStatus?.tokenExpiresAt ? (
              <div>
                <strong>Token expires:</strong> {agentStatus.tokenExpiresAt}
              </div>
            ) : null}
            {agentStatus?.error ? (
              <div>
                <strong>Error:</strong> {agentStatus.error}
              </div>
            ) : null}
            {detectedIngressClasses.length === 0 ? (
              <div style={{ color: "#8a5200" }}>
                <strong>Warning:</strong> no ingress controller detected. Enter ingress class manually.
              </div>
            ) : (
              <div>
                <strong>Detected ingress classes:</strong> {detectedIngressClasses.join(", ")}
              </div>
            )}
            <label>
              Ingress class
              <select
                value={values.selectedIngressClass}
                onChange={(event) => {
                  updateValue({ selectedIngressClass: event.target.value });
                }}
              >
                <option value="">Select ingress class</option>
                {detectedIngressClasses.map((ingressClass) => (
                  <option key={ingressClass} value={ingressClass}>{ingressClass}</option>
                ))}
                <option value="__manual__">Manual input</option>
              </select>
            </label>
            {values.selectedIngressClass === "__manual__" ? (
              <label>
                Manual ingress class
                <input
                  placeholder="nginx"
                  value={values.manualIngressClass}
                  onChange={(event) => {
                    updateValue({ manualIngressClass: event.target.value });
                  }}
                />
              </label>
            ) : null}
          </div>
        ) : currentStep === 3 ? (
          <div className="wizard-review">
            <p className="muted">Select one or more base namespaces, then start resource scan.</p>
            <div>
              {(agentStatus?.capabilityReport?.namespaces ?? []).length > 0 ? (
                (agentStatus?.capabilityReport?.namespaces ?? []).map((namespace) => (
                  <label key={namespace}>
                    <input
                      checked={values.selectedBaseNamespaces.includes(namespace)}
                      type="checkbox"
                      onChange={() => {
                        toggleNamespace(namespace);
                      }}
                    />
                    {namespace}
                  </label>
                ))
              ) : (
                <div className="muted">No namespaces discovered yet. Wait for agent connection/capability report.</div>
              )}
            </div>
            <div className="wizard-actions">
              <Button disabled={agentBusy || values.selectedBaseNamespaces.length === 0} variant="secondary" type="button" onClick={() => void startResourceScan()}>
                {agentBusy ? "Starting..." : "Start resource scan"}
              </Button>
              <Button disabled={agentBusy} variant="secondary" type="button" onClick={() => void refreshAgentStatus()}>
                Refresh scan status
              </Button>
            </div>
            <div>
              <strong>Scan status:</strong> {agentStatus?.resourceScanStatus ?? "idle"}
            </div>
            {typeof agentStatus?.resourceCount === "number" ? (
              <div>
                <strong>Discovered resources:</strong> {agentStatus.resourceCount}
              </div>
            ) : null}
            {agentStatus?.capabilityReport?.permissionWarnings && agentStatus.capabilityReport.permissionWarnings.length > 0 ? (
              <div>
                <strong>Permission warnings</strong>
                <ul>
                  {agentStatus.capabilityReport.permissionWarnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>
        ) : currentStep === 4 ? (
          <div className="wizard-review">
            <p className="muted">Review discovered resources. Choose include/exclude and strategy per resource.</p>
            {discoveredResources.length === 0 ? (
              <div className="muted">No discovered resources yet. Start scan on previous step and refresh.</div>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Kind</th>
                    <th>Name</th>
                    <th>Namespace</th>
                    <th>Include</th>
                    <th>Strategy</th>
                  </tr>
                </thead>
                <tbody>
                  {discoveredResources.map((resource) => {
                    const key = resourceKey(resource);
                    const item = getResourceReviewItem(resource, values);
                    const invalidReason = invalidResourceItems.get(key) ?? "";
                    return (
                      <tr key={key} style={invalidReason ? { background: "rgba(255, 0, 0, 0.08)" } : undefined}>
                        <td>{resource.kind}</td>
                        <td>{resource.name}</td>
                        <td>{resource.namespace}</td>
                        <td>
                          <input
                            checked={item.include}
                            type="checkbox"
                            onChange={(event) => {
                              const nextInclude = event.target.checked;
                              updateResourceReview(resource, {
                                include: nextInclude,
                                strategy: nextInclude ? item.strategy : "ignore"
                              });
                            }}
                          />
                        </td>
                        <td>
                          <select
                            value={item.strategy}
                            onChange={(event) => {
                              updateResourceReview(resource, {
                                strategy: event.target.value as ResourceStrategy
                              });
                            }}
                          >
                            {resourceStrategies.map((strategy) => (
                              <option key={strategy} value={strategy}>
                                {strategy}
                              </option>
                            ))}
                          </select>
                          {resource.sourceMapping?.status === "unresolved" ? (
                            <div className="muted">Source: unresolved</div>
                          ) : resource.sourceMapping?.status === "resolved" ? (
                            <div className="muted">
                              Source: {resource.sourceMapping.kind ?? "resolved"} {resource.sourceMapping.namespace ? `${resource.sourceMapping.namespace}/` : ""}{resource.sourceMapping.name ?? ""}
                            </div>
                          ) : null}
                          {invalidReason ? (
                            <div style={{ color: "#b42318", fontSize: "0.85rem", marginTop: "0.25rem" }}>{invalidReason}</div>
                          ) : null}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>
        ) : currentStep === 5 ? (
          <div className="wizard-review">
            <p className="muted">Configure ConfigMap strategy and keys.</p>
            {discoveredConfigMaps.length === 0 ? (
              <div className="muted">No ConfigMaps discovered.</div>
            ) : (
              discoveredConfigMaps.map((resource) => {
                const mapKey = resourceKey(resource);
                const cfg = resolveConfigMapConfig(resource, values.configMapStrategies[mapKey]);
                const keys = resource.configMapKeys ?? [];
                return (
                  <div key={mapKey} style={{ marginBottom: "1rem", borderTop: "1px solid #ddd", paddingTop: "0.75rem" }}>
                    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
                      <strong>{resource.name}</strong>
                      <select
                        value={cfg.strategy}
                        onChange={(event) => {
                          updateConfigMapStrategy(resource, event.target.value as ConfigMapStrategy);
                        }}
                      >
                        {configMapStrategies.map((strategy) => (
                          <option key={strategy} value={strategy}>{strategy}</option>
                        ))}
                      </select>
                    </div>
                    <div className="muted">{resource.namespace}</div>
                    {keys.length === 0 ? (
                      <div className="muted">No keys discovered for this ConfigMap.</div>
                    ) : (
                      <table style={{ marginTop: "0.5rem" }}>
                        <thead>
                          <tr>
                            <th>Select</th>
                            <th>Key</th>
                            <th>Templated value</th>
                          </tr>
                        </thead>
                        <tbody>
                          {keys.map((keyName) => {
                            const keyCfg = cfg.keys[keyName] ?? { selected: true, value: "{{ .Branch }}" };
                            const keyError = cfg.strategy === "template" && keyCfg.selected
                              ? validateDynamicTemplate(keyCfg.value)
                              : "";
                            return (
                              <tr key={`${mapKey}:${keyName}`}>
                                <td>
                                  <input
                                    checked={keyCfg.selected}
                                    type="checkbox"
                                    onChange={(event) => {
                                      updateConfigMapKeySelection(resource, keyName, event.target.checked);
                                    }}
                                  />
                                </td>
                                <td>{keyName}</td>
                                <td>
                                  <input
                                    disabled={cfg.strategy !== "template" || !keyCfg.selected}
                                    placeholder="{{ .Branch }}"
                                    value={cfg.strategy === "template" ? keyCfg.value : ""}
                                    onChange={(event) => {
                                      updateConfigMapTemplatedValue(resource, keyName, event.target.value);
                                    }}
                                  />
                                  {keyError ? <div style={{ color: "#b42318", fontSize: "0.85rem" }}>{keyError}</div> : null}
                                </td>
                              </tr>
                            );
                          })}
                        </tbody>
                      </table>
                    )}
                  </div>
                );
              })
            )}
            {configMapValidation.errors.length > 0 ? (
              <div style={{ color: "#b42318", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Blocking errors</strong>
                <ul>
                  {configMapValidation.errors.map((errorItem) => (
                    <li key={errorItem}>{errorItem}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            <div className="muted">Allowed template tokens: {allowedDynamicTemplates.join(", ")}</div>
          </div>
        ) : currentStep === 6 ? (
          <div className="wizard-review">
            <p className="muted">Classify discovered services for preview environment behavior.</p>
            {sortedServices.length === 0 ? (
              <div className="muted">No services found in discovered graph yet.</div>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Group</th>
                    <th>Service</th>
                    <th>Namespace</th>
                    <th>Classification</th>
                  </tr>
                </thead>
                <tbody>
                  {sortedServices.map((service) => {
                    const isAppService = appServiceIDs.has(service.id);
                    const classification = getServiceClassification(service.id, values);
                    const serviceErrors = serviceClassificationValidation.errors.filter((issue) => issue.serviceId === service.id);
                    const serviceWarnings = serviceClassificationValidation.warnings.filter((issue) => issue.serviceId === service.id);
                    return (
                      <tr key={service.id} style={serviceErrors.length > 0 ? { background: "rgba(255, 0, 0, 0.08)" } : undefined}>
                        <td>{isAppService ? "App service" : "Dependency"}</td>
                        <td>{service.name}</td>
                        <td>{service.namespace}</td>
                        <td>
                          <select
                            value={classification}
                            onChange={(event) => {
                              updateServiceClassification(service.id, event.target.value as ServiceClassification);
                            }}
                          >
                            {serviceClassifications.map((item) => (
                              <option key={item} value={item}>
                                {item}
                              </option>
                            ))}
                          </select>
                          {serviceErrors.map((issue) => (
                            <div key={issue.code} style={{ color: "#b42318", fontSize: "0.85rem", marginTop: "0.25rem" }}>{issue.message}</div>
                          ))}
                          {serviceWarnings.map((issue) => (
                            <div key={issue.code} style={{ color: "#8a5200", fontSize: "0.85rem", marginTop: "0.25rem" }}>{issue.message}</div>
                          ))}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
            <div style={{ marginTop: "1rem", borderTop: "1px solid #ddd", paddingTop: "0.75rem" }}>
              <label>
                Routing mode
                <select
                  value={values.routingMode}
                  onChange={(event) => {
                    updateValue({ routingMode: event.target.value as RoutingMode });
                  }}
                >
                  {routingModes.map((mode) => (
                    <option key={mode} value={mode}>{mode}</option>
                  ))}
                </select>
              </label>
              <div className="muted">Ingress class: {effectiveIngressClass || "n/a"}</div>
              {routingPreviewRows.length === 0 ? (
                <div className="muted">No routing preview available until services are discovered and classified.</div>
              ) : (
                <table style={{ marginTop: "0.5rem" }}>
                  <thead>
                    <tr>
                      <th>Source</th>
                      <th>Target namespace</th>
                      <th>Target service</th>
                      <th>Classification</th>
                    </tr>
                  </thead>
                  <tbody>
                    {routingPreviewRows.map((row) => (
                      <tr key={`${row.source}:${row.targetNamespace}:${row.targetService}`}>
                        <td>{row.source}</td>
                        <td>{row.targetNamespace}</td>
                        <td>{row.targetService}</td>
                        <td>{row.classification}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {values.routingMode === "hybrid fallback" ? (
                <div className="muted">Fallback routes target base/shared dependency services in {values.selectedBaseNamespaces[0] ?? "dev-base"}.</div>
              ) : null}
            </div>
            {serviceClassificationValidation.errors.length > 0 ? (
              <div style={{ color: "#b42318", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Blocking errors</strong>
                <ul>
                  {serviceClassificationValidation.errors.map((issue) => (
                    <li key={`${issue.serviceId}:${issue.code}`}>{issue.message}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            {routingValidation.errors.length > 0 ? (
              <div style={{ color: "#b42318", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Routing errors</strong>
                <ul>
                  {routingValidation.errors.map((issue) => (
                    <li key={issue}>{issue}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            {serviceClassificationValidation.warnings.length > 0 ? (
              <div style={{ color: "#8a5200", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Warnings</strong>
                <ul>
                  {serviceClassificationValidation.warnings.map((issue) => (
                    <li key={`${issue.serviceId}:${issue.code}`}>{issue.message}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            {routingValidation.warnings.length > 0 ? (
              <div style={{ color: "#8a5200", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Routing warnings</strong>
                <ul>
                  {routingValidation.warnings.map((issue) => (
                    <li key={issue}>{issue}</li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>
        ) : currentStep === 7 ? (
          <div className="wizard-review">
            <p className="muted">Manage environment variables per service and container.</p>
            {sortedDiscoveredServiceEnvs.length === 0 ? (
              <div className="muted">No discovered service variables available yet.</div>
            ) : (
              sortedDiscoveredServiceEnvs.map((service) => {
                const isAppService = appServiceIDs.has(service.serviceId);
                const containers = editorForService(service, values);
                return (
                  <div key={service.serviceId} style={{ marginBottom: "1rem", borderTop: "1px solid #ddd", paddingTop: "0.75rem" }}>
                    <div>
                      <strong>{service.serviceName}</strong> ({service.namespace}) - {isAppService ? "App service" : "Dependency"}
                    </div>
                    {containers.map((container) => (
                      <div key={`${service.serviceId}:${container.container}`} style={{ marginTop: "0.75rem", paddingLeft: "0.5rem" }}>
                        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
                          <strong>Container: {container.container}</strong>
                          <Button variant="secondary" type="button" onClick={() => addEnvVar(service.serviceId, container.container)}>
                            Add variable
                          </Button>
                        </div>
                        <table style={{ marginTop: "0.5rem" }}>
                          <thead>
                            <tr>
                              <th>Name</th>
                              <th>Type</th>
                              <th>Value</th>
                              <th>Actions</th>
                            </tr>
                          </thead>
                          <tbody>
                            {container.variables.map((variable) => {
                              const serviceName = service.serviceName;
                              const validationErrors = envTemplateValidation.errors.filter((error) =>
                                error.includes(`${serviceName}/${container.container}/${variable.name || ""}`) ||
                                (variable.name === "" && error.includes(`${serviceName}/${container.container}:`))
                              );
                              return (
                                <tr key={variable.id}>
                                  <td>
                                    <input
                                      value={variable.name}
                                      onChange={(event) => {
                                        updateEnvVar(service.serviceId, container.container, variable.id, { name: event.target.value });
                                      }}
                                    />
                                  </td>
                                  <td>
                                    <select
                                      value={variable.type}
                                      onChange={(event) => {
                                        updateEnvVar(service.serviceId, container.container, variable.id, { type: event.target.value as EnvVarType });
                                      }}
                                    >
                                      {envVarTypes.map((type) => (
                                        <option key={type} value={type}>{type}</option>
                                      ))}
                                    </select>
                                  </td>
                                  <td>
                                    <input
                                      type={variable.type === "secret" ? "password" : "text"}
                                      value={variable.type === "secret" && variable.value ? "********" : variable.value}
                                      placeholder={variable.type === "dynamic" ? "{{ .PRNumber }}" : ""}
                                      onChange={(event) => {
                                        updateEnvVar(service.serviceId, container.container, variable.id, { value: event.target.value });
                                      }}
                                    />
                                    {validationErrors.map((validationError) => (
                                      <div key={validationError} style={{ color: "#b42318", fontSize: "0.85rem" }}>{validationError}</div>
                                    ))}
                                  </td>
                                  <td>
                                    <Button variant="secondary" type="button" onClick={() => deleteEnvVar(service.serviceId, container.container, variable.id)}>
                                      Delete
                                    </Button>
                                  </td>
                                </tr>
                              );
                            })}
                          </tbody>
                        </table>
                      </div>
                    ))}
                  </div>
                );
              })
            )}
            {envTemplateValidation.errors.length > 0 ? (
              <div style={{ color: "#b42318", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Blocking errors</strong>
                <ul>
                  {envTemplateValidation.errors.map((errorItem) => (
                    <li key={errorItem}>{errorItem}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            {envTemplateValidation.warnings.length > 0 ? (
              <div style={{ color: "#8a5200", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Warnings</strong>
                <ul>
                  {envTemplateValidation.warnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            <div className="muted">Allowed dynamic templates: {allowedDynamicTemplates.join(", ")}</div>
          </div>
        ) : currentStep === 8 ? (
          <div className="wizard-review">
            <p className="muted">Define how each discovered secret should be handled for preview environments.</p>
            {discoveredSecrets.length === 0 ? (
              <div className="muted">No secrets discovered from workloads yet.</div>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Secret</th>
                    <th>Source</th>
                    <th>Required</th>
                    <th>Strategy</th>
                    <th>Backend</th>
                    <th>Reference / Value</th>
                  </tr>
                </thead>
                <tbody>
                  {discoveredSecrets.map((secret) => {
                    const cfg = getSecretStrategy(secret, values);
                    const showBackend = cfg.strategy === "external secret";
                    const showReference = cfg.strategy === "reference existing secret" || cfg.strategy === "external secret";
                    const showManual = cfg.strategy === "manual input";
                    const rowErrors = secretStrategyValidation.errors.filter((item) => item.includes(`${secret.serviceName}/${secret.container}/${secret.variable}`));
                    return (
                      <tr key={secret.id}>
                        <td>{secret.namespace}/{secret.secretName}</td>
                        <td>{secret.serviceName}/{secret.container}/{secret.variable}</td>
                        <td>{secret.required ? "yes" : "no"}</td>
                        <td>
                          <select
                            value={cfg.strategy}
                            onChange={(event) => {
                              updateSecretStrategyOption(secret, event.target.value as SecretStrategy | "");
                            }}
                          >
                            <option value="">Select</option>
                            {secretStrategies.map((strategy) => (
                              <option key={strategy} value={strategy}>{strategy}</option>
                            ))}
                          </select>
                        </td>
                        <td>
                          {showBackend ? (
                            <select
                              value={cfg.backend}
                              onChange={(event) => {
                                updateSecretBackend(secret, event.target.value);
                              }}
                            >
                              {secretBackends.map((backend) => (
                                <option key={backend} value={backend}>{backend}</option>
                              ))}
                            </select>
                          ) : (
                            <span className="muted">n/a</span>
                          )}
                        </td>
                        <td>
                          {showReference ? (
                            <input
                              placeholder={cfg.strategy === "external secret" ? "secret/backend/path#field" : "namespace/secret-name"}
                              value={cfg.reference}
                              onChange={(event) => {
                                updateSecretReference(secret, event.target.value);
                              }}
                            />
                          ) : showManual ? (
                            <>
                              <input
                                type="password"
                                placeholder={cfg.manualValueStored ? "Stored (masked)" : "Enter secret value"}
                                value={cfg.manualValue}
                                onChange={(event) => {
                                  updateSecretManualValue(secret, event.target.value);
                                }}
                              />
                              {cfg.manualValueStored ? <div className="muted">Stored value exists and remains masked.</div> : null}
                            </>
                          ) : (
                            <span className="muted">n/a</span>
                          )}
                          {rowErrors.map((issue) => (
                            <div key={issue} style={{ color: "#b42318", fontSize: "0.85rem" }}>{issue}</div>
                          ))}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
            {secretStrategyValidation.errors.length > 0 ? (
              <div style={{ color: "#b42318", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Blocking errors</strong>
                <ul>
                  {secretStrategyValidation.errors.map((errorItem) => (
                    <li key={errorItem}>{errorItem}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            {secretStrategyValidation.warnings.length > 0 ? (
              <div style={{ color: "#8a5200", fontSize: "0.9rem", marginTop: "0.5rem" }}>
                <strong>Warnings</strong>
                <ul>
                  {secretStrategyValidation.warnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>
        ) : currentStep === 9 ? (
          <div className="wizard-review">
            <p className="muted">Review generated templates, edit YAML, compare against original resource, then save changes.</p>
            {sortedTemplatePaths.length === 0 ? (
              <div className="muted">No generated templates yet. Complete resource scan and resource review first.</div>
            ) : (
              <div className="template-editor">
                <aside className="template-tree" aria-label="Generated template files">
                  {sortedTemplatePaths.map((path) => {
                    const item = templatesByPath.get(path);
                    if (!item) return null;
                    const edited = editedTemplateYAML[path];
                    const isChanged = edited !== undefined && edited !== item.yaml;
                    return (
                      <button
                        key={path}
                        type="button"
                        className={`template-tree-item ${path === effectiveActiveTemplatePath ? "template-tree-item-active" : ""}`}
                        onClick={() => setActiveTemplatePath(path)}
                      >
                        <span>{path}</span>
                        {isChanged ? <small>edited</small> : null}
                      </button>
                    );
                  })}
                </aside>
                <section className="template-editor-pane">
                  {activeTemplate ? (
                    <>
                      <div className="template-editor-header">
                        <strong>{effectiveActiveTemplatePath}</strong>
                        <span className="muted">{activeTemplate.kind} {activeTemplate.namespace ? `in ${activeTemplate.namespace}` : ""}</span>
                      </div>
                      <div className="template-editor-grid">
                        <div>
                          <div className="muted">Template YAML (editable)</div>
                          <textarea
                            className="template-yaml-input"
                            rows={20}
                            value={activeTemplateEditedYAML}
                            onChange={(event) => updateActiveTemplateYAML(event.target.value)}
                          />
                          <div className="muted">
                            Variables: {`{{ .PRNumber }}`}, {`{{ .CommitSHA }}`}, {`{{ .Service }}`}
                          </div>
                          <div className="template-variable-preview">
                            {activeTemplateEditedYAML.split(/\r?\n/).map((line, index) => (
                              <div key={`${effectiveActiveTemplatePath}:line:${index}`}>
                                {line.split(/(\{\{\s*\.[^}]+\s*\}\})/g).map((part, partIndex) => (
                                  /^\{\{\s*\.[^}]+\s*\}\}$/.test(part)
                                    ? <mark key={`${index}:${partIndex}`}>{part}</mark>
                                    : <span key={`${index}:${partIndex}`}>{part}</span>
                                ))}
                              </div>
                            ))}
                          </div>
                        </div>
                        <div>
                          <div className="muted">Diff vs original resource</div>
                          <pre className="template-diff" aria-live="polite">
                            {activeTemplateDiff.map((row, index) => (
                              <div
                                key={`${effectiveActiveTemplatePath}:diff:${index}`}
                                className={row.type === "add" ? "diff-add" : row.type === "remove" ? "diff-remove" : "diff-same"}
                              >
                                <span className="diff-prefix">{row.type === "add" ? "+" : row.type === "remove" ? "-" : " "}</span>
                                <span>{row.text}</span>
                              </div>
                            ))}
                          </pre>
                        </div>
                      </div>
                      <div className="wizard-actions">
                        <Button
                          disabled={!hasTemplateChanges || templateSaving}
                          variant="primary"
                          type="button"
                          onClick={() => void saveEditedTemplates()}
                        >
                          {templateSaving ? "Saving..." : "Save templates"}
                        </Button>
                      </div>
                    </>
                  ) : (
                    <div className="muted">Select a template file to edit.</div>
                  )}
                </section>
              </div>
            )}
          </div>
        ) : (
          <div className="wizard-review">
            <p className="muted">Final review before compile. Resolve blocking issues first.</p>
            <div><strong>SCM:</strong> {values.scmProvider} | app {values.repositoryUrl || "n/a"} | gitops {values.gitopsRepoUrl || "n/a"} | branch {values.defaultBranch || "n/a"} | auth {values.authMethod}</div>
            <div><strong>Deployment:</strong> {values.deploymentBackend === "fluxcd" ? "FluxCD GitOps" : "Helm Direct"}</div>
            <div><strong>Cluster:</strong> status {agentStatus?.status ?? "waiting"} | cluster {agentStatus?.clusterId || "n/a"} | ingress {effectiveIngressClass || "n/a"} | scan {agentStatus?.resourceScanStatus ?? "idle"}</div>
            <div><strong>Namespaces:</strong> {values.selectedBaseNamespaces.join(", ") || "none"}</div>
            <div><strong>Services:</strong> discovered {sortedServices.length} | classifications {Object.keys(values.serviceClassifications).length} | routing mode {values.routingMode} | routes {routingPreviewRows.length}</div>
            <div><strong>Env vars:</strong> service groups {sortedDiscoveredServiceEnvs.length} | validation errors {envTemplateValidation.errors.length}</div>
            <div><strong>Secrets strategy:</strong> discovered secrets {discoveredSecrets.length} | strategies {Object.keys(values.secretStrategies).length} | blocking {secretStrategyValidation.errors.length}</div>
            <div><strong>Domain:</strong> {values.previewDomain} | host pattern {values.hostPatternTemplate} | sample {samplePreviewURL || "n/a"}</div>
            {values.deploymentBackend === "fluxcd" ? (
              <div><strong>GitOps:</strong> path {values.gitOpsOutputPath}, ns {values.fluxNamespace}, kustomization {values.fluxKustomizationRef}, gitRepository {values.fluxGitRepositoryRef}, commit {values.gitOpsCommitMode}</div>
            ) : (
              <div>
                <strong>Helm Direct:</strong>
                <div>chart {values.helmDirectChartRef}</div>
                <div>release {values.helmDirectReleaseNamePattern}</div>
                <div>namespace {values.helmDirectNamespacePattern}</div>
                <div>strategy {values.helmDirectValuesOverrideStrategy}, image tag {values.helmDirectImageTagValuePath}</div>
                <div>wait {values.helmDirectWait ? "yes" : "no"}, timeout {values.helmDirectTimeout}s, create namespace {values.helmDirectCreateNamespace ? "yes" : "no"}</div>
              </div>
            )}
            <div><strong>Templates:</strong> generated {manifestTemplates.length} | edited {Object.keys(editedTemplateYAML).length}</div>
            <div><strong>Policies:</strong> TTL {values.defaultTTLHours}h, cpu {values.cpuRequest}/{values.cpuLimit}, memory {values.memoryRequest}/{values.memoryLimit}, storage {values.storageQuota}, max envs {values.maxActiveEnvironments}</div>
            <div><strong>Network policy:</strong> feature to base {values.networkFeatureToBase ? "yes" : "no"}, base to feature {values.networkBaseToFeature ? "yes" : "no"}, egress {values.networkEgressMode}</div>
            <div><strong>Cleanup safety:</strong> protected {values.cleanupProtectedNamespaces}, labels only {values.cleanupDeleteEnvPlaneLabelsOnly ? "yes" : "no"}, finalizer {values.cleanupFinalizerStrategy}</div>
            {reviewBlockingErrors.length > 0 ? (
              <div style={{ color: "#b42318", fontSize: "0.9rem", marginTop: "0.75rem" }}>
                <strong>Blocking errors</strong>
                <ul>
                  {reviewBlockingErrors.map((issue) => (
                    <li key={issue}>{issue}</li>
                  ))}
                </ul>
              </div>
            ) : (
              <div style={{ color: "#067647", fontSize: "0.9rem", marginTop: "0.75rem" }}>
                <strong>No blocking errors.</strong>
              </div>
            )}
            {reviewWarnings.length > 0 ? (
              <div style={{ color: "#8a5200", fontSize: "0.9rem", marginTop: "0.75rem" }}>
                <strong>Warnings</strong>
                <ul>
                  {reviewWarnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            <div className="wizard-review" style={{ marginTop: "1rem" }}>
              <strong>Runner deployment instructions</strong>
              <label>
                Deployment mode
                <select
                  value={runnerForm.mode}
                  onChange={(event) => {
                    setRunnerForm((current) => ({
                      ...current,
                      mode: event.target.value as RunnerDeploymentMode
                    }));
                  }}
                >
                  <option value="helm">helm</option>
                  <option value="gitops">gitops</option>
                </select>
              </label>
              <label>
                Cluster ID
                <input
                  value={runnerForm.clusterId}
                  onChange={(event) => {
                    setRunnerForm((current) => ({
                      ...current,
                      clusterId: event.target.value
                    }));
                  }}
                />
              </label>
              <label>
                Runner namespace
                <input
                  value={runnerForm.runnerNamespace}
                  onChange={(event) => {
                    setRunnerForm((current) => ({
                      ...current,
                      runnerNamespace: event.target.value
                    }));
                  }}
                />
              </label>
              {runnerForm.mode === "helm" ? (
                <label>
                  Release name
                  <input
                    value={runnerForm.releaseName}
                    onChange={(event) => {
                      setRunnerForm((current) => ({
                        ...current,
                        releaseName: event.target.value
                      }));
                    }}
                  />
                </label>
              ) : null}
              {runnerForm.mode === "gitops" ? (
                <label>
                  GitOps manifest path
                  <input
                    value={runnerForm.gitOpsPath}
                    onChange={(event) => {
                      setRunnerForm((current) => ({
                        ...current,
                        gitOpsPath: event.target.value
                      }));
                    }}
                  />
                </label>
              ) : null}
              <div className="wizard-actions">
                <Button
                  disabled={runnerBusy}
                  variant="primary"
                  type="button"
                  onClick={() => void generateRunnerDeploymentInstructions()}
                >
                  {runnerBusy ? "Generating..." : "Generate deployment instructions"}
                </Button>
                <Button
                  disabled={runnerBusy}
                  variant="secondary"
                  type="button"
                  onClick={() => void refreshRunnerStatus()}
                >
                  Refresh runner status
                </Button>
                <Button
                  disabled={runnerBusy}
                  variant="secondary"
                  type="button"
                  onClick={() => void checkRunnerHealth()}
                >
                  Check health
                </Button>
              </div>
              {runnerDeploymentInstructions ? (
                  <div style={{ marginTop: "0.75rem" }}>
                  <div><strong>Latest instruction mode:</strong> {runnerDeploymentInstructions.deploymentMode}</div>
                  <div><strong>Project config URL:</strong> {runnerDeploymentInstructions.projectConfigUrl}</div>
                  <div><strong>Token expires:</strong> {runnerDeploymentInstructions.expiresAt}</div>
                  {runnerDeploymentInstructions.bootstrapSecretCommand ? (
                    <label>
                      Bootstrap secret command
                      {runnerDeploymentInstructions.bootstrapSecretCommandSensitive ? (
                        <span className="form-help"> Sensitive one-time command. Do not paste it into logs or support tickets.</span>
                      ) : null}
                      <textarea readOnly rows={4} value={runnerDeploymentInstructions.bootstrapSecretCommand} />
                    </label>
                  ) : null}
                  {runnerDeploymentInstructions.helmCommand ? (
                    <label>
                      Helm command
                      <textarea readOnly rows={4} value={runnerDeploymentInstructions.helmCommand} />
                    </label>
                  ) : null}
                  {runnerDeploymentInstructions.gitOpsManifest ? (
                    <>
                      <div><strong>GitOps path:</strong> {runnerDeploymentInstructions.gitOpsPath || runnerForm.gitOpsPath}</div>
                      <label>
                        GitOps manifest
                        <textarea readOnly rows={8} value={runnerDeploymentInstructions.gitOpsManifest} />
                      </label>
                    </>
                  ) : null}
                </div>
              ) : null}
              <div><strong>Runner status:</strong> {runnerStatus?.status ?? "waiting"}</div>
              <div><strong>Runner mode:</strong> {runnerStatus?.deploymentMode ?? runnerDeploymentInstructions?.deploymentMode ?? "n/a"}</div>
              <div><strong>Runner cluster:</strong> {runnerStatus?.clusterId ?? runnerDeploymentInstructions?.clusterId ?? "n/a"}</div>
              <div><strong>Runner namespace:</strong> {runnerStatus?.runnerNamespace ?? runnerDeploymentInstructions?.runnerNamespace ?? "n/a"}</div>
              {runnerStatus?.runnerId ? <div><strong>Runner ID:</strong> {runnerStatus.runnerId}</div> : null}
              {runnerStatus?.projectConfigUrl ? <div><strong>Current config endpoint:</strong> {runnerStatus.projectConfigUrl}</div> : null}
              {runnerStatus?.error ? <div style={{ color: "#b42318" }}><strong>Runner registration error:</strong> {runnerStatus.error}</div> : null}
              {runnerStatus?.lastSeenAt ? <div><strong>Last seen:</strong> {runnerStatus.lastSeenAt}</div> : null}
              {runnerHealth ? (
                <div style={{ marginTop: "0.5rem" }}>
                  <strong>Healthcheck:</strong> {runnerHealth.status} ({runnerHealth.component || "runner"}) {runnerHealth.at ? `at ${runnerHealth.at}` : ""}
                </div>
              ) : null}
            </div>
            <div className="wizard-review" style={{ marginTop: "1rem" }}>
              <strong>Test PR simulation</strong>
              <label style={{ display: "flex", alignItems: "center", gap: "0.5rem" }}>
                <input
                  type="checkbox"
                  checked={simulateDryRun}
                  onChange={(event) => {
                    setSimulateDryRun(event.target.checked);
                    setSimulateResult(null);
                  }}
                  disabled={simulateSaving}
                />
                Simulate with dry-run commit
              </label>
              <div style={{ display: "flex", gap: "0.5rem", marginTop: "0.5rem" }}>
                <Button
                  variant="secondary"
                  type="button"
                  disabled={simulateSaving}
                  onClick={() => void simulatePR(values)}
                >
                  {simulateSaving ? "Simulating…" : "Simulate PR"}
                </Button>
              </div>
              {simulateResult !== null ? (
                <div style={{ marginTop: "0.75rem" }}>
                  <div><strong>Validation:</strong> {simulateResult.validation.valid ? "valid" : "invalid"}</div>
                  {simulateResult.validation.issues.length === 0 ? null : (
                    <ul style={{ marginTop: "0.5rem", color: "#b42318" }}>
                      {simulateResult.validation.issues.slice(0, 10).map((issue) => (
                        <li key={`${issue.file}:${issue.line ?? 0}:${issue.column ?? 0}:${issue.code}`}>
                          {issue.file}:{issue.line || 1}:{issue.column || 0} {issue.code} — {issue.message}
                        </li>
                      ))}
                    </ul>
                  )}
                  <div style={{ marginTop: "0.5rem" }}>
                    Generated templates: {simulateResult.manifestTemplates.length}
                  </div>
                  {simulateResult.manifestTemplates.length > 0 ? (
                    <ul style={{ marginTop: "0.5rem" }}>
                      {simulateResult.manifestTemplates.map((template) => {
                        const path = templatePath(template);
                        return <li key={`${template.namespace}-${template.kind}-${template.name}`}>{path}</li>;
                      })}
                    </ul>
                  ) : null}
                  {simulateResult.dryRun ? (
                    <div style={{ marginTop: "0.5rem" }}>
                      <div>Dry-run status: {simulateResult.dryRun.status}</div>
                      <div>Dry-run path: {simulateResult.dryRun.commitPath}</div>
                      <div>Files: {simulateResult.dryRun.fileCount}</div>
                      <div>Dry-run message: {simulateResult.dryRun.message}</div>
                    </div>
                  ) : null}
                </div>
              ) : null}
            </div>
          </div>
        )}
      </div>

      <div className="wizard-actions">
        <Button disabled={currentStep === 0} variant="secondary" type="button" onClick={goBack}>
          Back
        </Button>
        <Button disabled={!canProceedFromCurrentStep || saving || scmValidationInFlight || agentBusy || runnerBusy || templateSaving || compileSaving} variant="primary" type="button" onClick={() => void goNext()}>
          {currentStep < steps.length - 1 ? "Next" : compileSaving ? "Compiling…" : "Compile"}
        </Button>
      </div>

      {saving ? <Toast tone="info">Saving progress...</Toast> : null}
      {currentStep === 0 && hasValidSCMConnection === false ? (
        <Toast tone="warning">You need to validate SCM credentials before moving to the next step.</Toast>
      ) : null}
    </Card>
  );
}
