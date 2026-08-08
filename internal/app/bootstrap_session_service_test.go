package app

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/envpilot/runner/internal/domain"
	"github.com/envpilot/runner/internal/store"
)

func TestBootstrapSessionUpdateRejectsInvalidStatus(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	sessionStore, err := store.NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	svc := NewBootstrapSessionService(sessionStore)
	if _, err := svc.Create("checkout", "alice"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := svc.Update("checkout", BootstrapSessionUpdate{Status: strPtr("invalid")}); err == nil {
		t.Fatalf("expected status validation error")
	}
}

func TestBootstrapSessionEncryptsCredentialsBeforeSaveAndMasksOnLoad(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	sessionStore, err := store.NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	encryptor := MustNewAESGCMCredentialEncryptor("unit-test-encryption-key", "unit")
	svc := NewBootstrapSessionServiceWithEncryptor(sessionStore, encryptor)
	if _, err := svc.Create("checkout", "alice"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	const secret = "super-secret-oauth-token"
	updated, err := svc.Update("checkout", BootstrapSessionUpdate{
		CurrentStep: ptrInt(1),
		Status:      strPtr("scanning"),
		StepData: map[string]any{
			"repositoryUrl": "https://github.com/acme/checkout",
			"oauthToken":    secret,
		},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	publicPayload, err := json.Marshal(updated)
	if err != nil {
		t.Fatalf("marshal public session: %v", err)
	}
	if bytes.Contains(publicPayload, []byte(secret)) {
		t.Fatalf("public session leaked plaintext secret: %s", string(publicPayload))
	}
	if marker, ok := updated.Data["oauthToken"].(map[string]any); !ok || marker["stored"] != true || marker["masked"] != true {
		t.Fatalf("expected masked credential marker, got %#v", updated.Data["oauthToken"])
	}

	raw, err := sessionStore.GetByProject("checkout")
	if err != nil {
		t.Fatalf("get raw session: %v", err)
	}
	rawPayload, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal raw session: %v", err)
	}
	if bytes.Contains(rawPayload, []byte(secret)) {
		t.Fatalf("raw session leaked plaintext secret: %s", string(rawPayload))
	}
	encrypted, ok := encryptedCredentialFromValue(raw.Data["oauthToken"])
	if !ok {
		t.Fatalf("expected encrypted credential envelope, got %#v", raw.Data["oauthToken"])
	}
	plaintext, err := encryptor.DecryptCredential(context.Background(), encrypted)
	if err != nil {
		t.Fatalf("decrypt credential: %v", err)
	}
	if string(plaintext) != secret {
		t.Fatalf("decrypted credential = %q", string(plaintext))
	}

	loaded, err := svc.Get("checkout")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	loadedPayload, err := json.Marshal(loaded)
	if err != nil {
		t.Fatalf("marshal loaded session: %v", err)
	}
	if bytes.Contains(loadedPayload, []byte(secret)) {
		t.Fatalf("loaded session leaked plaintext secret: %s", string(loadedPayload))
	}
}

func TestBootstrapSessionUpdateMergesStepData(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	sessionStore, err := store.NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	svc := NewBootstrapSessionService(sessionStore)
	created, err := svc.Create("checkout", "alice")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	created, err = svc.Update("checkout", BootstrapSessionUpdate{
		CurrentStep: ptrInt(1),
		Status:      strPtr("scanning"),
		StepData: map[string]any{
			"repository": "https://github.com/acme/checkout",
		},
	})
	if err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if created.CurrentStep != 1 || created.Status != domain.BootstrapSessionStatusScanning {
		t.Fatalf("unexpected session update: %#v", created)
	}
	if created.Data["repository"] != "https://github.com/acme/checkout" {
		t.Fatalf("unexpected step data: %#v", created.Data)
	}
}

func TestBootstrapSessionPersistsGitOpsConfigurationFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	sessionStore, err := store.NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	svc := NewBootstrapSessionService(sessionStore)
	if _, err := svc.Create("checkout", "alice"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	const gitOpsOutputPath = "environments/{{ .PRNumber }}/{{ .Service }}"
	const fluxNamespace = "flux-system"
	const gitRepoRef = "ns/envpilot-gitops"
	const kustomizationRef = "ns/envpilot-prs"
	const commitMode = "pull request"

	updated, err := svc.Update("checkout", BootstrapSessionUpdate{
		StepData: map[string]any{
			"gitOpsOutputPath":     gitOpsOutputPath,
			"fluxNamespace":        fluxNamespace,
			"fluxGitRepositoryRef": gitRepoRef,
			"fluxKustomizationRef": kustomizationRef,
			"gitOpsCommitMode":     commitMode,
		},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	if updated.Data["gitOpsOutputPath"] != gitOpsOutputPath {
		t.Fatalf("unexpected gitOpsOutputPath: %#v", updated.Data["gitOpsOutputPath"])
	}
	if updated.Data["fluxNamespace"] != fluxNamespace {
		t.Fatalf("unexpected fluxNamespace: %#v", updated.Data["fluxNamespace"])
	}
	if updated.Data["fluxGitRepositoryRef"] != gitRepoRef {
		t.Fatalf("unexpected fluxGitRepositoryRef: %#v", updated.Data["fluxGitRepositoryRef"])
	}
	if updated.Data["fluxKustomizationRef"] != kustomizationRef {
		t.Fatalf("unexpected fluxKustomizationRef: %#v", updated.Data["fluxKustomizationRef"])
	}
	if updated.Data["gitOpsCommitMode"] != commitMode {
		t.Fatalf("unexpected gitOpsCommitMode: %#v", updated.Data["gitOpsCommitMode"])
	}
}

func TestBootstrapSessionSecretStrategiesEncryptAndMaskManualValues(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	sessionStore, err := store.NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	encryptor := MustNewAESGCMCredentialEncryptor("unit-test-encryption-key", "unit")
	svc := NewBootstrapSessionServiceWithEncryptor(sessionStore, encryptor)
	if _, err := svc.Create("checkout", "alice"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	const secret = "super-secret-db-password"
	updated, err := svc.Update("checkout", BootstrapSessionUpdate{
		StepData: map[string]any{
			"secretStrategies": map[string]any{
				"dev/db-password": map[string]any{
					"strategy":    "manual input",
					"required":    true,
					"serviceId":   "Service/dev/orders",
					"container":   "orders-api",
					"variable":    "DB_PASSWORD",
					"manualValue": secret,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	publicPayload, err := json.Marshal(updated)
	if err != nil {
		t.Fatalf("marshal public session: %v", err)
	}
	if bytes.Contains(publicPayload, []byte(secret)) {
		t.Fatalf("public session leaked plaintext secret: %s", string(publicPayload))
	}

	publicStrategies, ok := updated.Data["secretStrategies"].(map[string]any)
	if !ok {
		t.Fatalf("expected public secret strategies map, got %#v", updated.Data["secretStrategies"])
	}
	item, ok := publicStrategies["dev/db-password"].(map[string]any)
	if !ok {
		t.Fatalf("expected public secret strategy item, got %#v", publicStrategies["dev/db-password"])
	}
	if item["manualValueMasked"] != true || item["manualValueStored"] != true {
		t.Fatalf("expected masked public secret markers, got %#v", item)
	}

	raw, err := sessionStore.GetByProject("checkout")
	if err != nil {
		t.Fatalf("get raw session: %v", err)
	}
	rawPayload, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal raw session: %v", err)
	}
	if bytes.Contains(rawPayload, []byte(secret)) {
		t.Fatalf("raw session leaked plaintext secret: %s", string(rawPayload))
	}
	rawStrategies, ok := raw.Data["secretStrategies"].(map[string]any)
	if !ok {
		t.Fatalf("expected raw secret strategies map, got %#v", raw.Data["secretStrategies"])
	}
	rawItem, ok := rawStrategies["dev/db-password"].(map[string]any)
	if !ok {
		t.Fatalf("expected raw secret strategy item, got %#v", rawStrategies["dev/db-password"])
	}
	encrypted, ok := encryptedCredentialFromValue(rawItem["manualValueEncrypted"])
	if !ok {
		t.Fatalf("expected encrypted manual value envelope, got %#v", rawItem["manualValueEncrypted"])
	}
	plaintext, err := encryptor.DecryptCredential(context.Background(), encrypted)
	if err != nil {
		t.Fatalf("decrypt secret strategy manual value: %v", err)
	}
	if string(plaintext) != secret {
		t.Fatalf("decrypted manual value = %q", string(plaintext))
	}
}

func TestBootstrapSessionSecretStrategiesRejectRequiredUnresolved(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	sessionStore, err := store.NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	svc := NewBootstrapSessionService(sessionStore)
	if _, err := svc.Create("checkout", "alice"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	_, err = svc.Update("checkout", BootstrapSessionUpdate{
		StepData: map[string]any{
			"secretStrategies": map[string]any{
				"dev/db-password": map[string]any{
					"required": true,
					"strategy": "",
				},
			},
		},
	})
	if err == nil {
		t.Fatalf("expected validation error for unresolved required secret")
	}
}

func ptrInt(value int) *int {
	return &value
}

func strPtr(value string) *string {
	return &value
}
