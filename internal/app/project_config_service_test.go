package app

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"envpilot/internal/domain"
	"envpilot/internal/store"
)

func TestProjectConfigServiceSavesEncryptedSensitiveValuesAndMasksPublicResponse(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	sessionStore, err := store.NewJSONBootstrapSessionStore(filepath.Join(tmp, "bootstrap-sessions.json"))
	if err != nil {
		t.Fatalf("new bootstrap session store: %v", err)
	}
	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(tmp, "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}

	encryptor := MustNewAESGCMCredentialEncryptor("unit-test-encryption-key", "unit")
	sessionSvc := NewBootstrapSessionServiceWithEncryptor(sessionStore, encryptor)
	if _, err := sessionSvc.Create("checkout", "alice"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	const token = "super-secret-app-token"
	const manualSecret = "super-secret-db-password"
	if _, err := sessionSvc.Update("checkout", BootstrapSessionUpdate{
		Status: strPtr("compiled"),
		StepData: map[string]any{
			"scmProvider":   "github",
			"repositoryUrl": "https://github.com/acme/checkout",
			"gitopsRepoUrl": "https://github.com/acme/gitops",
			"appToken":      token,
			"secretStrategies": map[string]any{
				"dev/db-password": map[string]any{
					"strategy":    "manual input",
					"required":    true,
					"serviceId":   "Service/dev/orders",
					"container":   "orders-api",
					"variable":    "DB_PASSWORD",
					"manualValue": manualSecret,
				},
			},
		},
	}); err != nil {
		t.Fatalf("update session: %v", err)
	}

	rawSession, err := sessionSvc.GetStored("checkout")
	if err != nil {
		t.Fatalf("get stored session: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)
	publicConfig, err := configSvc.SaveFromBootstrapSession(domain.Project{
		ID:                 "checkout",
		Name:               "Checkout",
		ProductID:          "generic",
		AppRepositoryID:    "github.com/acme/checkout",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}, rawSession, "alice")
	if err != nil {
		t.Fatalf("save project config: %v", err)
	}

	publicPayload, err := json.Marshal(publicConfig)
	if err != nil {
		t.Fatalf("marshal public config: %v", err)
	}
	if bytes.Contains(publicPayload, []byte(token)) || bytes.Contains(publicPayload, []byte(manualSecret)) {
		t.Fatalf("public config leaked plaintext secret: %s", string(publicPayload))
	}
	if bytes.Contains(publicPayload, []byte("ciphertext")) {
		t.Fatalf("public config leaked encrypted envelope: %s", string(publicPayload))
	}
	if publicConfig.Version != 1 {
		t.Fatalf("expected version 1, got %d", publicConfig.Version)
	}

	rawConfig, err := configStore.Latest("checkout")
	if err != nil {
		t.Fatalf("get raw project config: %v", err)
	}
	rawPayload, err := json.Marshal(rawConfig)
	if err != nil {
		t.Fatalf("marshal raw project config: %v", err)
	}
	if bytes.Contains(rawPayload, []byte(token)) || bytes.Contains(rawPayload, []byte(manualSecret)) {
		t.Fatalf("raw project config leaked plaintext secret: %s", string(rawPayload))
	}

	credentials, ok := toMap(rawConfig.Sensitive["scmCredentials"])
	if !ok {
		t.Fatalf("expected scm credential sensitive map, got %#v", rawConfig.Sensitive)
	}
	encryptedToken, ok := encryptedCredentialFromValue(credentials["appToken"])
	if !ok {
		t.Fatalf("expected encrypted app token, got %#v", credentials["appToken"])
	}
	tokenPlaintext, err := encryptor.DecryptCredential(context.Background(), encryptedToken)
	if err != nil {
		t.Fatalf("decrypt app token: %v", err)
	}
	if string(tokenPlaintext) != token {
		t.Fatalf("decrypted token = %q", string(tokenPlaintext))
	}

	secrets, ok := toMap(rawConfig.Sensitive["manualSecrets"])
	if !ok {
		t.Fatalf("expected manual secrets sensitive map, got %#v", rawConfig.Sensitive)
	}
	encryptedSecret, ok := encryptedCredentialFromValue(secrets["dev/db-password"])
	if !ok {
		t.Fatalf("expected encrypted manual secret, got %#v", secrets["dev/db-password"])
	}
	secretPlaintext, err := encryptor.DecryptCredential(context.Background(), encryptedSecret)
	if err != nil {
		t.Fatalf("decrypt manual secret: %v", err)
	}
	if string(secretPlaintext) != manualSecret {
		t.Fatalf("decrypted manual secret = %q", string(secretPlaintext))
	}
}

func TestProjectConfigServiceCreatesNewVersionOnEachSave(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)
	project := domain.Project{
		ID:                 "checkout",
		Name:               "Checkout",
		ProductID:          "generic",
		AppRepositoryID:    "github.com/acme/checkout",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}
	session := domain.BootstrapSession{
		ID:        "checkout-bs-1",
		ProjectID: "checkout",
		Data: map[string]any{
			"repositoryUrl": "https://github.com/acme/checkout",
		},
	}
	first, err := configSvc.SaveFromBootstrapSession(project, session, "alice")
	if err != nil {
		t.Fatalf("save first config: %v", err)
	}
	second, err := configSvc.SaveFromBootstrapSession(project, session, "alice")
	if err != nil {
		t.Fatalf("save second config: %v", err)
	}
	if first.Version != 1 || second.Version != 2 {
		t.Fatalf("expected versions 1 and 2, got %d and %d", first.Version, second.Version)
	}
}

func TestProjectConfigServiceDefaultsDeploymentConfigToHelmDirect(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)

	project := domain.Project{
		ID:         "checkout",
		Name:       "Checkout",
		ProductID:  "generic",
		GitOpsRepo: domain.RepositoryRef{URL: "https://github.com/acme/gitops"},
	}
	session := domain.BootstrapSession{
		ID:        "checkout-bs-1",
		ProjectID: "checkout",
		Data:      map[string]any{"repositoryUrl": "https://github.com/acme/checkout"},
	}

	config, err := configSvc.SaveFromBootstrapSession(project, session, "alice")
	if err != nil {
		t.Fatalf("save config: %v", err)
	}

	deployment, ok := toMap(config.Config["deployment"])
	if !ok {
		t.Fatalf("expected deployment config: %#v", config.Config)
	}
	if got := strings.TrimSpace(strings.ToLower(asStringValue(deployment["backend"]))); got != domain.DeploymentBackendHelmDirect {
		t.Fatalf("expected deployment backend %q got %q", domain.DeploymentBackendHelmDirect, got)
	}
	helmDirect, ok := toMap(deployment["helmDirect"])
	if !ok {
		t.Fatalf("expected helmDirect config: %#v", deployment)
	}
	if strings.TrimSpace(asStringValue(helmDirect["namespaceMode"])) == "" {
		t.Fatalf("expected non-empty namespaceMode: %#v", helmDirect)
	}
	if strings.TrimSpace(asStringValue(helmDirect["releaseNamePattern"])) == "" {
		t.Fatalf("expected non-empty releaseNamePattern: %#v", helmDirect)
	}
	if value, ok := asIntValue(helmDirect["timeout"]); !ok || value <= 0 {
		t.Fatalf("expected positive timeout: %#v", helmDirect)
	}
}

func TestProjectConfigServiceRejectsInvalidDeploymentBackend(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)

	project := domain.Project{
		ID:         "checkout",
		Name:       "Checkout",
		ProductID:  "generic",
		GitOpsRepo: domain.RepositoryRef{URL: "https://github.com/acme/gitops"},
	}
	session := domain.BootstrapSession{
		ID:        "checkout-bs-1",
		ProjectID: "checkout",
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "argocd",
			},
		},
	}

	if _, err := configSvc.SaveFromBootstrapSession(project, session, "alice"); err == nil {
		t.Fatalf("expected unsupported backend validation error")
	}
}

func TestProjectConfigServiceValidatesFluxcdDeploymentRequirements(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)

	project := domain.Project{
		ID:         "checkout",
		Name:       "Checkout",
		ProductID:  "generic",
		GitOpsRepo: domain.RepositoryRef{URL: "https://github.com/acme/gitops"},
	}

	_, err = configSvc.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "checkout-bs-1",
		ProjectID: "checkout",
		Data: map[string]any{
			"deployment":           map[string]any{"backend": "fluxcd"},
			"gitOpsOutputPath":     "environments/{{ .PRNumber }}/{{ .Service }}",
			"fluxNamespace":        "flux-system",
			"fluxKustomizationRef": "ns/envpilot-prs",
			"gitOpsCommitMode":     "pull request",
		},
	}, "alice")
	if err != nil {
		t.Fatalf("save valid fluxcd config: %v", err)
	}

	rawConfig, err := configStore.Latest("checkout")
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	deployment, ok := toMap(rawConfig.Config["deployment"])
	if !ok || strings.TrimSpace(asStringValue(deployment["backend"])) != domain.DeploymentBackendFluxCD {
		t.Fatalf("expected fluxcd deployment config: %#v", rawConfig.Config)
	}
	fluxcd, ok := toMap(deployment["fluxcd"])
	if !ok {
		t.Fatalf("expected fluxcd block: %#v", deployment)
	}
	if asStringValue(fluxcd["gitopsRepo"]) != project.GitOpsRepo.URL {
		t.Fatalf("expected gitopsRepo=%q got %q", project.GitOpsRepo, asStringValue(fluxcd["gitopsRepo"]))
	}
	if asStringValue(fluxcd["gitopsPath"]) == "" {
		t.Fatalf("expected gitopsPath: %#v", fluxcd)
	}

	_, err = configSvc.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "checkout-bs-2",
		ProjectID: "checkout",
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "fluxcd",
				"fluxcd": map[string]any{
					"gitopsRepo": project.GitOpsRepo,
					"gitopsPath": "environments/{{ .PRNumber }}/{{ .Service }}",
				},
			},
		},
	}, "alice")
	if err == nil {
		t.Fatalf("expected fluxcd validation error for incomplete config")
	}
}

func TestProjectConfigServiceMigratesLegacyFluxcdSessionFields(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)

	project := domain.Project{
		ID:        "legacy-flux",
		Name:      "Legacy",
		ProductID: "generic",
	}

	_, err = configSvc.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "legacy-flux-bs-1",
		ProjectID: "legacy-flux",
		Data: map[string]any{
			"deployment":           map[string]any{"backend": "fluxcd"},
			"gitOpsRepoUrl":        "https://github.com/acme/gitops",
			"gitOpsOutputPath":     "environments/{{ .PRNumber }}/{{ .Service }}",
			"fluxNamespace":        "flux-system",
			"fluxKustomizationRef": "ns/envpilot-prs",
			"gitOpsCommitMode":     "push",
		},
	}, "alice")
	if err != nil {
		t.Fatalf("save valid legacy fluxcd config: %v", err)
	}

	rawConfig, err := configStore.Latest("legacy-flux")
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	deployment, ok := toMap(rawConfig.Config["deployment"])
	if !ok || strings.TrimSpace(asStringValue(deployment["backend"])) != domain.DeploymentBackendFluxCD {
		t.Fatalf("expected legacy migration to fluxcd: %#v", rawConfig.Config)
	}
	fluxcd, ok := toMap(deployment["fluxcd"])
	if !ok {
		t.Fatalf("expected fluxcd block after migration: %#v", deployment)
	}
	if got := asStringValue(fluxcd["gitopsRepo"]); got != "https://github.com/acme/gitops" {
		t.Fatalf("expected migrated gitopsRepo=%q got %q", "https://github.com/acme/gitops", got)
	}
	if asStringValue(fluxcd["kustomizationName"]) == "" {
		t.Fatalf("expected migrated kustomizationName from legacy field")
	}
}

func TestProjectConfigServiceInfersFluxcdBackendFromLegacyFluxFields(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)

	project := domain.Project{
		ID:        "legacy-flux-inferred",
		Name:      "Legacy Flux Inferred",
		ProductID: "generic",
	}

	_, err = configSvc.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "legacy-flux-inferred-bs-1",
		ProjectID: "legacy-flux-inferred",
		Data: map[string]any{
			"gitOpsRepoUrl":        "https://github.com/acme/gitops",
			"gitOpsOutputPath":     "environments/{{ .PRNumber }}/{{ .Service }}",
			"fluxNamespace":        "flux-system",
			"fluxKustomizationRef": "ns/envpilot-prs",
			"gitOpsCommitMode":     "push",
		},
	}, "alice")
	if err != nil {
		t.Fatalf("save valid inferred fluxcd config: %v", err)
	}

	rawConfig, err := configStore.Latest("legacy-flux-inferred")
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	deployment, ok := toMap(rawConfig.Config["deployment"])
	if !ok || strings.TrimSpace(asStringValue(deployment["backend"])) != domain.DeploymentBackendFluxCD {
		t.Fatalf("expected inferred fluxcd backend: %#v", rawConfig.Config)
	}
	fluxcd, ok := toMap(deployment["fluxcd"])
	if !ok {
		t.Fatalf("expected fluxcd block after inference: %#v", deployment)
	}
	if got := asStringValue(fluxcd["gitopsRepo"]); got != "https://github.com/acme/gitops" {
		t.Fatalf("expected inferred gitopsRepo=%q got %q", "https://github.com/acme/gitops", got)
	}
	if asStringValue(fluxcd["gitopsPath"]) == "" {
		t.Fatalf("expected inferred gitopsPath: %#v", fluxcd)
	}
	if asStringValue(fluxcd["fluxNamespace"]) != "flux-system" {
		t.Fatalf("expected inferred fluxNamespace=flux-system, got %q", asStringValue(fluxcd["fluxNamespace"]))
	}
}

func TestProjectConfigServiceSavesHelmDirectValues(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)

	project := domain.Project{
		ID:                 "checkout-helm-direct",
		Name:               "Checkout",
		ProductID:          "generic",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}

	_, err = configSvc.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "checkout-helm-direct-bs-1",
		ProjectID: "checkout-helm-direct",
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"chartRef":               "deploy/helm/checkout",
					"namespacePattern":       "envpilot-pr-{{ .PRNumber }}",
					"releaseNamePattern":     "{{ .project.id }}-{{ .environment.name }}",
					"timeout":                420,
					"wait":                   true,
					"createNamespace":        true,
					"valuesOverrideStrategy": "merge",
					"imageTagValuePath":      "imageTag",
				},
			},
		},
	}, "alice")
	if err != nil {
		t.Fatalf("save config: %v", err)
	}

	rawConfig, err := configStore.Latest("checkout-helm-direct")
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	deployment, ok := toMap(rawConfig.Config["deployment"])
	if !ok {
		t.Fatalf("expected deployment in project config: %#v", rawConfig.Config)
	}
	if strings.TrimSpace(asStringValue(deployment["backend"])) != domain.DeploymentBackendHelmDirect {
		t.Fatalf("expected backend %q, got %#v", domain.DeploymentBackendHelmDirect, deployment)
	}
	helmDirect, ok := toMap(deployment["helmDirect"])
	if !ok {
		t.Fatalf("expected helmDirect block: %#v", deployment)
	}
	if strings.TrimSpace(asStringValue(helmDirect["chartRef"])) != "deploy/helm/checkout" {
		t.Fatalf("expected chartRef, got %#v", helmDirect)
	}
	if strings.TrimSpace(asStringValue(helmDirect["namespacePattern"])) != "envpilot-pr-{{ .PRNumber }}" {
		t.Fatalf("expected namespacePattern, got %#v", helmDirect)
	}
	if strings.TrimSpace(asStringValue(helmDirect["valuesOverrideStrategy"])) != "merge" {
		t.Fatalf("expected valuesOverrideStrategy, got %#v", helmDirect)
	}
	if strings.TrimSpace(asStringValue(helmDirect["imageTagValuePath"])) != "imageTag" {
		t.Fatalf("expected imageTagValuePath, got %#v", helmDirect)
	}
	if createNamespace, ok := helmDirect["createNamespace"].(bool); !ok || !createNamespace {
		t.Fatalf("expected createNamespace=true, got %#v", helmDirect["createNamespace"])
	}
	if timeout, ok := helmDirect["timeout"].(int); !ok || timeout != 420 {
		t.Fatalf("expected timeout=420, got %#v", helmDirect["timeout"])
	}
}

func TestProjectConfigServiceHelmDirectRequiresReleaseNamePatternAndNamespacePattern(t *testing.T) {
	t.Parallel()

	configStore, err := store.NewJSONProjectConfigStore(filepath.Join(t.TempDir(), "project-configs.json"))
	if err != nil {
		t.Fatalf("new project config store: %v", err)
	}
	configSvc := NewProjectConfigService(configStore)

	project := domain.Project{
		ID:                 "checkout-missing",
		Name:               "Checkout",
		ProductID:          "generic",
		GitOpsRepositoryID: "github.com/acme/gitops",
	}

	_, err = configSvc.SaveFromBootstrapSession(project, domain.BootstrapSession{
		ID:        "checkout-missing-bs-1",
		ProjectID: "checkout-missing",
		Data: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"chartRef":           "deploy/helm/checkout",
					"namespacePattern":   "   ",
					"releaseNamePattern": "   ",
				},
			},
		},
	}, "alice")
	if err == nil {
		t.Fatalf("expected helm_direct validation error for missing required fields")
	}
	if !strings.Contains(err.Error(), "releaseNamePattern") {
		t.Fatalf("expected releaseNamePattern validation error: %v", err)
	}
}
