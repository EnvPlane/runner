import * as assert from "node:assert/strict";
import { test } from "node:test";
import {
  inferEnvVarType,
  inferEnvVarValue,
  isRequiredDynamicVar,
  validateDynamicTemplate
} from "./envEditorValidation";

test("infers env var type from source metadata", () => {
  assert.equal(inferEnvVarType({ name: "TOKEN", valueFrom: "valueFrom", valueFromKind: "secretKeyRef" }), "secret");
  assert.equal(inferEnvVarType({ name: "POD_IP", valueFrom: "valueFrom", valueFromKind: "fieldRef" }), "dynamic");
  assert.equal(inferEnvVarType({ name: "COMMIT", value: "{{ .CommitSHA }}" }), "dynamic");
  assert.equal(inferEnvVarType({ name: "STATIC", value: "abc" }), "static");
});

test("builds inferred display value for references", () => {
  assert.equal(inferEnvVarValue({ name: "DB_PASSWORD", valueFromKind: "secretKeyRef", valueFromName: "db", valueFromKey: "password" }), "db:password");
  assert.equal(inferEnvVarValue({ name: "CFG", valueFromKind: "configMapKeyRef", valueFromName: "app", valueFromKey: "version" }), "app:version");
  assert.equal(inferEnvVarValue({ name: "POD_IP", valueFromKind: "fieldRef", valueFromField: "status.podIP" }), "status.podIP");
});

test("required dynamic vars are detected by name", () => {
  assert.equal(isRequiredDynamicVar("PR_NUMBER"), true);
  assert.equal(isRequiredDynamicVar("branch"), true);
  assert.equal(isRequiredDynamicVar("COMMIT_SHA"), true);
  assert.equal(isRequiredDynamicVar("OPTIONAL_VAR"), false);
});

test("dynamic template validation accepts known tokens and rejects invalid syntax", () => {
  assert.equal(validateDynamicTemplate("{{ .PRNumber }}"), "");
  assert.equal(validateDynamicTemplate("release-{{ .Branch }}"), "");
  assert.equal(validateDynamicTemplate("{{ .Unknown }}").includes("not allowed"), true);
  assert.equal(validateDynamicTemplate("{{ PRNumber }}"), "template syntax is invalid.");
  assert.equal(validateDynamicTemplate("no-template").includes("must include template token"), true);
});
