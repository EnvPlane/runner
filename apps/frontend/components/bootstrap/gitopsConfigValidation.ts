export type GitOpsCommitMode = "direct" | "pull request";

type ValidationResult = string;

type ValidationMessage = {
  commitMode: ValidationResult;
  outputPath: ValidationResult;
  namespace: ValidationResult;
  kustomizationRef: ValidationResult;
  gitRepositoryRef: ValidationResult;
};

const kubernetesNamePattern = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

const allowedCommitModes: GitOpsCommitMode[] = ["direct", "pull request"];

export function validateGitOpsCommitMode(mode: string): ValidationResult {
  const trimmed = String(mode ?? "").trim();
  if (trimmed === "") {
    return "Commit mode is required.";
  }
  if (!allowedCommitModes.includes(trimmed as GitOpsCommitMode)) {
    return "Commit mode must be direct or pull request.";
  }
  return "";
}

export function validateGitOpsOutputPath(path: string): ValidationResult {
  const trimmed = String(path ?? "").trim();
  if (trimmed === "") {
    return "GitOps output path is required.";
  }
  if (trimmed.startsWith("/")) {
    return "GitOps output path must be relative.";
  }
  if (trimmed.endsWith("/")) {
    return "GitOps output path must not end with a slash.";
  }
  if (trimmed.includes("//")) {
    return "GitOps output path contains empty segments.";
  }
  if (trimmed.includes("..")) {
    return "GitOps output path must not contain path traversal markers.";
  }

  const normalized = trimmed.replace(/\{\{\s*\.[A-Za-z0-9]+\s*\}\}/g, "segment");
  const segments = normalized.split("/");
  if (segments.length === 0) {
    return "GitOps output path is required.";
  }
  for (const segment of segments) {
    if (segment.trim() === "") {
      return "GitOps output path contains empty segment.";
    }
    if (segment.length > 63) {
      return "Each path segment must be at most 63 characters.";
    }
    if (!/^[A-Za-z0-9._-]+$/.test(segment)) {
      return "GitOps output path must contain only letters, digits, '.', '-' and '_'.";
    }
  }
  return "";
}

function validateNameSegment(segment: string, fieldLabel: string): string {
  const trimmed = segment.trim();
  if (trimmed === "") {
    return `${fieldLabel} is required.`;
  }
  if (trimmed.length > 253) {
    return `${fieldLabel} is too long.`;
  }
  if (trimmed.length > 63) {
    return `${fieldLabel} must be at most 63 characters.`;
  }
  if (!kubernetesNamePattern.test(trimmed)) {
    return `${fieldLabel} must contain lowercase letters, digits, and dashes only and cannot start or end with a dash.`;
  }
  return "";
}

export function validateGitOpsReferenceLabel(value: string, fieldLabel: string): ValidationResult {
  const trimmed = String(value ?? "").trim();
  if (trimmed === "") {
    return `${fieldLabel} is required.`;
  }

  const parts = trimmed.split("/");
  if (parts.length > 2) {
    return `${fieldLabel} must be a name or namespace/name.`;
  }

  const firstError = validateNameSegment(parts[0], `${fieldLabel} namespace/name`);
  if (parts.length === 1) {
    return firstError;
  }
  if (firstError) {
    return firstError;
  }
  return validateNameSegment(parts[1], `${fieldLabel} namespace/name`);
}

export function validateGitOpsConfiguration(payload: {
  gitOpsOutputPath: string;
  fluxNamespace: string;
  fluxGitRepositoryRef: string;
  fluxKustomizationRef: string;
  gitOpsCommitMode: string;
}): ValidationMessage {
  return {
    outputPath: validateGitOpsOutputPath(payload.gitOpsOutputPath),
    namespace: validateNameSegment(payload.fluxNamespace, "Flux namespace"),
    gitRepositoryRef: validateGitOpsReferenceLabel(payload.fluxGitRepositoryRef, "Flux GitRepository reference"),
    kustomizationRef: validateGitOpsReferenceLabel(payload.fluxKustomizationRef, "Flux Kustomization reference"),
    commitMode: validateGitOpsCommitMode(payload.gitOpsCommitMode)
  };
}
