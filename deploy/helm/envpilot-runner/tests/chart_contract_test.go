package tests

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRunnerChartDefinesHelmDeployContract(t *testing.T) {
	requiredFiles := []string{
		"../Chart.yaml",
		"../values.yaml",
		"../templates/deployment.yaml",
		"../templates/auth-pvc.yaml",
		"../templates/serviceaccount.yaml",
		"../templates/rbac.yaml",
		"../templates/secret.yaml",
	}
	for _, path := range requiredFiles {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("required chart file %s is missing: %v", path, err)
		}
	}

	deployment, err := os.ReadFile("../templates/deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment template: %v", err)
	}
	deploymentText := string(deployment)
	for _, expected := range []string{
		"kind: Deployment",
		"imagePullSecrets",
		"ENVPILOT_CONTROL_PLANE_URL",
		"ENVPILOT_PROJECT_CONFIG_URL",
		"ENVPILOT_PROJECT_CONFIG_TOKEN",
		"ENVPILOT_RUNNER_REGISTRATION_TOKEN",
		"ENVPILOT_RUNNER_AUTH_TOKEN",
		"ENVPILOT_RUNNER_AUTH_TOKEN_FILE",
		"name: HOME",
		"name: runner-work",
		"name: wait-control-plane",
		"/health",
	} {
		if !strings.Contains(deploymentText, expected) {
			t.Fatalf("deployment template does not contain %q", expected)
		}
	}
	rendered := renderRunnerChart(t)
	if !strings.Contains(rendered, "- runner") {
		t.Fatalf("rendered chart must start runner subcommand:\n%s", rendered)
	}

	rbac, err := os.ReadFile("../templates/rbac.yaml")
	if err != nil {
		t.Fatalf("read rbac template: %v", err)
	}
	rbacText := string(rbac)
	for _, expected := range []string{
		"discovery-reader",
		"feature-env-writer",
		"secret-manager",
		"rbac.discovery.scope",
		"rbac.featureEnvWriter.mode",
		"releaseNamespace",
		"preconfiguredNamespaces",
		"generatedFeatureNamespaces",
		"rbac.featureEnvWriter.namespaces",
		"rbac.featureEnvWriter.generatedNamespaces",
		"rbac.featureEnvWriter.allowNetworkPolicies",
		"rbac.featureEnvWriter.allowFluxResources",
		"rbac.secretManager.enabled",
	} {
		if !strings.Contains(rbacText, expected) {
			t.Fatalf("rbac template does not contain %q", expected)
		}
	}
}

func TestRunnerChartUsesPersistentImage(t *testing.T) {
	values, err := os.ReadFile("../values.yaml")
	if err != nil {
		t.Fatalf("read values: %v", err)
	}
	valuesText := string(values)
	for _, expected := range []string{
		"repository: ghcr.io/envpilot/runner",
		`tag: "0.1.1"`,
	} {
		if !strings.Contains(valuesText, expected) {
			t.Fatalf("values.yaml does not contain %q", expected)
		}
	}
	if strings.Contains(valuesText, "ttl"+".sh") {
		t.Fatalf("values.yaml must not reference temporary image registries")
	}
}

func TestRunnerChartDocumentsAuthPersistenceSecret(t *testing.T) {
	values, err := os.ReadFile("../values.yaml")
	if err != nil {
		t.Fatalf("read values: %v", err)
	}
	valuesText := string(values)
	for _, expected := range []string{
		"authPersistence:",
		"runner-auth-token",
		"existingSecret:",
		"runnerAuthTokenKey:",
		"runnerAuthToken",
		"one-time bootstrap token",
		"createClaim:",
		"claimName:",
	} {
		if !strings.Contains(valuesText, expected) {
			t.Fatalf("values.yaml does not document auth persistence field %q", expected)
		}
	}

	rendered := renderRunnerChart(
		t,
		"--set", "controlPlane.authPersistence.existingSecret=envpilot-runner-auth",
	)
	if !strings.Contains(rendered, "ENVPILOT_RUNNER_AUTH_TOKEN") {
		t.Fatalf("rendered chart missing persisted runner auth token env:\n%s", rendered)
	}
	if !strings.Contains(rendered, "name: \"envpilot-runner-auth\"") {
		t.Fatalf("rendered chart missing persisted auth secret reference:\n%s", rendered)
	}
	if !strings.Contains(rendered, "key: \"runner-auth-token\"") {
		t.Fatalf("rendered chart missing persisted auth secret key:\n%s", rendered)
	}
	if !strings.Contains(rendered, "ENVPILOT_RUNNER_AUTH_TOKEN_FILE") {
		t.Fatalf("rendered chart missing persisted runner auth token file:\n%s", rendered)
	}
	if !strings.Contains(rendered, "kind: PersistentVolumeClaim") {
		t.Fatalf("rendered chart missing default auth persistence PVC:\n%s", rendered)
	}
	if !strings.Contains(rendered, "claimName: \"envpilot-runner-chart-auth\"") {
		t.Fatalf("rendered chart missing auth PVC claim reference:\n%s", rendered)
	}
}

func TestRunnerChartDefaultRBACIsLeastPrivilege(t *testing.T) {
	rendered := renderRunnerChart(t)
	docs := renderedDocs(rendered)
	if !strings.Contains(rendered, "kind: ClusterRole") {
		t.Fatalf("default chart should render read-only discovery ClusterRole")
	}
	if !strings.Contains(rendered, "discovery-reader") {
		t.Fatalf("default chart should render discovery-reader RBAC")
	}
	if !strings.Contains(rendered, "feature-env-writer") {
		t.Fatalf("default chart should render feature-env-writer RBAC")
	}
	if strings.Contains(rendered, "secret-manager") {
		t.Fatalf("secret-manager RBAC must be disabled by default")
	}

	for _, doc := range docs {
		if !docIsKind(doc, "ClusterRole") {
			continue
		}
		for _, forbiddenVerb := range []string{`"create"`, `"update"`, `"patch"`, `"delete"`} {
			if strings.Contains(doc, forbiddenVerb) {
				t.Fatalf("default ClusterRole must not include write verb %s:\n%s", forbiddenVerb, doc)
			}
		}
		if docHasAnyResource(doc, "secrets") && docHasAnyVerb(doc, `"create"`, `"update"`, `"patch"`, `"delete"`) {
			t.Fatalf("default chart must not grant cluster-wide secret writes:\n%s", doc)
		}
		if docHasAnyResource(doc, "deployments", "statefulsets", "replicasets", "daemonsets") &&
			docHasAnyVerb(doc, `"delete"`) {
			t.Fatalf("default chart must not grant cluster-wide workload delete:\n%s", doc)
		}
	}

	writerDoc := findDoc(docs, "kind: Role", "feature-env-writer")
	if writerDoc == "" {
		t.Fatalf("feature-env-writer Role not found")
	}
	// Helm's default storage driver persists release records in Secrets. This
	// remains namespace-scoped in the feature-environment writer Role; it must
	// never be moved into the discovery ClusterRole.
	if !docHasAnyResource(writerDoc, "secrets") {
		t.Fatalf("feature-env-writer must manage Helm release Secrets:\n%s", writerDoc)
	}
	if docHasAnyResource(writerDoc, "networkpolicies", "helmreleases", "kustomizations", "gitrepositories") {
		t.Fatalf("feature-env-writer optional capabilities must be disabled by default:\n%s", writerDoc)
	}
}

func TestRunnerChartPreconfiguredNamespaceWriterRendersPerNamespace(t *testing.T) {
	rendered := renderRunnerChart(
		t,
		"--set", "rbac.featureEnvWriter.mode=preconfiguredNamespaces",
		"--set", "rbac.featureEnvWriter.namespaces[0]=envpilot-pr-123",
		"--set", "rbac.featureEnvWriter.namespaces[1]=envpilot-pr-124",
	)
	docs := renderedDocs(rendered)
	for _, namespace := range []string{"envpilot-pr-123", "envpilot-pr-124"} {
		if role := findResourceDoc(docs, "Role", "envpilot-runner-chart-feature-env-writer", namespace); role == "" {
			t.Fatalf("feature-env-writer Role not found in namespace %s:\n%s", namespace, rendered)
		}
		binding := findResourceDoc(docs, "RoleBinding", "envpilot-runner-chart-feature-env-writer", namespace)
		if binding == "" {
			t.Fatalf("feature-env-writer RoleBinding not found in namespace %s:\n%s", namespace, rendered)
		}
		if !strings.Contains(binding, "namespace: envpilot-system") {
			t.Fatalf("feature namespace writer RoleBinding must bind the release service account:\n%s", binding)
		}
	}
	if role := findResourceDoc(docs, "Role", "envpilot-runner-chart-feature-env-writer", "envpilot-system"); role != "" {
		t.Fatalf("preconfigured namespace mode must not also render release-namespace writer Role:\n%s", role)
	}
}

func TestRunnerChartGeneratedFeatureNamespaceWriterRendersPerNamespace(t *testing.T) {
	rendered := renderRunnerChart(
		t,
		"--set", "rbac.featureEnvWriter.mode=generatedFeatureNamespaces",
		"--set", "rbac.featureEnvWriter.generatedNamespaces[0]=envpilot-pr-201",
	)
	docs := renderedDocs(rendered)
	if role := findResourceDoc(docs, "Role", "envpilot-runner-chart-feature-env-writer", "envpilot-pr-201"); role == "" {
		t.Fatalf("generated feature namespace writer Role not found:\n%s", rendered)
	}
	if binding := findResourceDoc(docs, "RoleBinding", "envpilot-runner-chart-feature-env-writer", "envpilot-pr-201"); binding == "" {
		t.Fatalf("generated feature namespace writer RoleBinding not found:\n%s", rendered)
	}
}

func TestRunnerChartNamespaceScopedDiscoveryUsesRoleBinding(t *testing.T) {
	rendered := renderRunnerChart(t, "--set", "rbac.discovery.scope=namespace")
	if strings.Contains(rendered, "kind: ClusterRoleBinding") {
		t.Fatalf("namespace-scoped discovery must not render ClusterRoleBinding:\n%s", rendered)
	}
	if strings.Contains(rendered, "kind: ClusterRole") {
		t.Fatalf("namespace-scoped discovery must not render ClusterRole:\n%s", rendered)
	}
	discoveryRole := findDoc(renderedDocs(rendered), "kind: Role", "discovery-reader")
	if discoveryRole == "" {
		t.Fatalf("namespace-scoped discovery Role not found:\n%s", rendered)
	}
	if !strings.Contains(rendered, "kind: RoleBinding") {
		t.Fatalf("namespace-scoped discovery RoleBinding not found:\n%s", rendered)
	}
}

func TestRunnerChartCapabilityFlagsControlOptionalPermissions(t *testing.T) {
	rendered := renderRunnerChart(
		t,
		"--set", "rbac.featureEnvWriter.allowNetworkPolicies=true",
		"--set", "rbac.featureEnvWriter.allowFluxResources=true",
		"--set", "rbac.secretManager.enabled=true",
	)
	docs := renderedDocs(rendered)
	writerDoc := findDoc(docs, "kind: Role", "feature-env-writer")
	if writerDoc == "" {
		t.Fatalf("feature-env-writer Role not found")
	}
	for _, expectedResource := range []string{
		"networkpolicies",
		"helmreleases",
		"kustomizations",
		"gitrepositories",
	} {
		if !docHasAnyResource(writerDoc, expectedResource) {
			t.Fatalf("enabled feature-env-writer capability missing %s:\n%s", expectedResource, writerDoc)
		}
	}

	secretManagerDoc := findDoc(docs, "kind: Role", "secret-manager")
	if secretManagerDoc == "" {
		t.Fatalf("secret-manager Role not found when enabled")
	}
	if !docHasAnyResource(secretManagerDoc, "secrets") {
		t.Fatalf("secret-manager must manage secrets when enabled:\n%s", secretManagerDoc)
	}
	for _, doc := range docs {
		if docIsKind(doc, "ClusterRole") && docHasAnyResource(doc, "secrets") &&
			docHasAnyVerb(doc, `"create"`, `"update"`, `"patch"`, `"delete"`) {
			t.Fatalf("secret-manager must remain namespace-scoped, found cluster-wide secret write:\n%s", doc)
		}
	}
}

func TestRunnerChartRejectsPlaintextBootstrapTokensWithoutExplicitOverride(t *testing.T) {
	commandArgs := []string{
		"template", "envpilot-runner", "..",
		"--namespace", "envpilot-system",
		"--set", "controlPlane.token=raw-runner-token",
		"--set", "controlPlane.configToken=raw-config-token",
	}
	cmd := exec.Command("helm", commandArgs...)
	cmd.Dir = "."
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helm template should fail when plaintext tokens are set without override:\n%s", string(output))
	}
	if !strings.Contains(string(output), "allowUnsafePlaintextTokens=true") {
		t.Fatalf("expected unsafe plaintext token guidance, got:\n%s", string(output))
	}
}

func TestRunnerChartAllowsPlaintextBootstrapTokensOnlyWithExplicitOverride(t *testing.T) {
	rendered := renderRunnerChart(
		t,
		"--set", "controlPlane.token=raw-runner-token",
		"--set", "controlPlane.configToken=raw-config-token",
		"--set", "controlPlane.allowUnsafePlaintextTokens=true",
	)
	if !strings.Contains(rendered, "raw-runner-token") || !strings.Contains(rendered, "raw-config-token") {
		t.Fatalf("explicit unsafe override should render tokens for local testing:\n%s", rendered)
	}
}

func renderRunnerChart(t *testing.T, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"template", "envpilot-runner", "..", "--namespace", "envpilot-system"}, args...)
	cmd := exec.Command("helm", commandArgs...)
	cmd.Dir = "."
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, string(output))
	}
	return string(output)
}

func renderedDocs(rendered string) []string {
	rawDocs := strings.Split(rendered, "\n---")
	docs := make([]string, 0, len(rawDocs))
	for _, raw := range rawDocs {
		doc := strings.TrimSpace(raw)
		if doc != "" {
			docs = append(docs, doc)
		}
	}
	return docs
}

func findDoc(docs []string, needles ...string) string {
	for _, doc := range docs {
		matches := true
		for _, needle := range needles {
			if !strings.Contains(doc, needle) {
				matches = false
				break
			}
		}
		if matches {
			return doc
		}
	}
	return ""
}

func findResourceDoc(docs []string, kind string, name string, namespace string) string {
	for _, doc := range docs {
		if !docIsKind(doc, kind) {
			continue
		}
		if !strings.Contains(doc, "\n  name: "+name+"\n") && !strings.Contains(doc, "\n  name: "+name+"\r\n") {
			continue
		}
		if namespace != "" && !strings.Contains(doc, "\n  namespace: "+namespace+"\n") && !strings.Contains(doc, "\n  namespace: "+namespace+"\r\n") {
			continue
		}
		return doc
	}
	return ""
}

func docIsKind(doc string, kind string) bool {
	return strings.Contains(doc, "\nkind: "+kind+"\n") || strings.HasPrefix(doc, "kind: "+kind+"\n")
}

func docHasAnyResource(doc string, resources ...string) bool {
	for _, resource := range resources {
		if strings.Contains(doc, "- "+resource) {
			return true
		}
	}
	return false
}

func docHasAnyVerb(doc string, verbs ...string) bool {
	for _, verb := range verbs {
		if strings.Contains(doc, verb) {
			return true
		}
	}
	return false
}
