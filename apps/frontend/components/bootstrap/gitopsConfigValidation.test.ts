import * as assert from "node:assert/strict";
import { test } from "node:test";
import { validateGitOpsConfiguration } from "./gitopsConfigValidation";

test("valid GitOps settings pass validation", () => {
  const result = validateGitOpsConfiguration({
    gitOpsOutputPath: "environments/{{ .PRNumber }}/{{ .Service }}",
    fluxNamespace: "flux-system",
    fluxGitRepositoryRef: "envpilot-gitops",
    fluxKustomizationRef: "envpilot-prs",
    gitOpsCommitMode: "direct"
  });

  assert.equal(result.outputPath, "");
  assert.equal(result.namespace, "");
  assert.equal(result.gitRepositoryRef, "");
  assert.equal(result.kustomizationRef, "");
  assert.equal(result.commitMode, "");
});

test("GitOps output path rejects absolute and empty values", () => {
  const empty = validateGitOpsConfiguration({
    gitOpsOutputPath: "",
    fluxNamespace: "flux-system",
    fluxGitRepositoryRef: "envpilot-gitops",
    fluxKustomizationRef: "envpilot-prs",
    gitOpsCommitMode: "direct"
  });
  const absolute = validateGitOpsConfiguration({
    gitOpsOutputPath: "/environments/app",
    fluxNamespace: "flux-system",
    fluxGitRepositoryRef: "envpilot-gitops",
    fluxKustomizationRef: "envpilot-prs",
    gitOpsCommitMode: "direct"
  });

  assert.equal(empty.outputPath.includes("required"), true);
  assert.equal(absolute.outputPath.includes("relative"), true);
});

test("Flux references and namespace are validated", () => {
  const badNamespace = validateGitOpsConfiguration({
    gitOpsOutputPath: "environments/{{ .PRNumber }}",
    fluxNamespace: "Flux_System",
    fluxGitRepositoryRef: "envpilot-gitops",
    fluxKustomizationRef: "envpilot-prs",
    gitOpsCommitMode: "direct"
  });
  const badRefs = validateGitOpsConfiguration({
    gitOpsOutputPath: "environments/{{ .PRNumber }}",
    fluxNamespace: "flux-system",
    fluxGitRepositoryRef: "bad reference",
    fluxKustomizationRef: "ns/envpilot/prs",
    gitOpsCommitMode: "direct"
  });

  assert.equal(badNamespace.namespace.includes("lowercase"), true);
  assert.equal(badRefs.gitRepositoryRef !== "", true);
  assert.equal(badRefs.kustomizationRef !== "", true);
});

test("commit mode supports only supported values", () => {
  const invalid = validateGitOpsConfiguration({
    gitOpsOutputPath: "environments/{{ .PRNumber }}",
    fluxNamespace: "flux-system",
    fluxGitRepositoryRef: "envpilot-gitops",
    fluxKustomizationRef: "envpilot-prs",
    gitOpsCommitMode: "auto"
  });
  const valid = validateGitOpsConfiguration({
    gitOpsOutputPath: "environments/{{ .PRNumber }}",
    fluxNamespace: "flux-system",
    fluxGitRepositoryRef: "envpilot-gitops",
    fluxKustomizationRef: "envpilot-prs",
    gitOpsCommitMode: "pull request"
  });

  assert.equal(invalid.commitMode.includes("direct or pull request"), true);
  assert.equal(valid.commitMode, "");
});
