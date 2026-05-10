package app

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
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
