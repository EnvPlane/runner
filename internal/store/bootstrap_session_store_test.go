package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/envpilot/contracts/domain"
)

func TestJSONBootstrapSessionStorePersistsSession(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	store, err := NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new bootstrap session store: %v", err)
	}

	createdAt := time.Now().UTC()
	session := domain.BootstrapSession{
		ID:          "orders-bs-1",
		ProjectID:   "orders",
		CurrentStep: 1,
		Status:      domain.BootstrapSessionStatusScanning,
		CreatedBy:   "alice",
		Data: map[string]any{
			"repository": "https://github.com/acme/orders",
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := store.Save(session); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}

	got, err := reloaded.GetByProject("orders")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "orders-bs-1" {
		t.Fatalf("id = %q", got.ID)
	}
	if got.ProjectID != session.ProjectID {
		t.Fatalf("project_id = %q", got.ProjectID)
	}
	if got.Status != session.Status {
		t.Fatalf("status = %q", got.Status)
	}
	if len(got.Data) != 1 || got.Data["repository"].(string) != "https://github.com/acme/orders" {
		t.Fatalf("step data = %#v", got.Data)
	}
}

func TestJSONBootstrapSessionStoreClaimBootstrapTokenIsAtomic(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bootstrap-sessions.json")
	store, err := NewJSONBootstrapSessionStore(path)
	if err != nil {
		t.Fatalf("new bootstrap session store: %v", err)
	}
	now := time.Now().UTC()
	token := "agent-registration-token"
	tokenHash := testTokenHash(token)
	if err := store.Save(domain.BootstrapSession{
		ID:          "orders-bs-claim",
		ProjectID:   "orders",
		CurrentStep: 3,
		Status:      domain.BootstrapSessionStatusDraft,
		CreatedBy:   "alice",
		CreatedAt:   now,
		UpdatedAt:   now,
		Data: map[string]any{
			"agentRegistrationTokenProject":   "orders",
			"agentRegistrationTokenHash":      tokenHash,
			"agentRegistrationTokenExpiresAt": now.Add(time.Hour).Format(time.RFC3339Nano),
			"agentClusterId":                  "dev-us",
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	claimRequest := BootstrapTokenClaimRequest{
		ProjectID:       "orders",
		TokenProjectKey: "agentRegistrationTokenProject",
		TokenHashKey:    "agentRegistrationTokenHash",
		TokenHash:       tokenHash,
		TokenUsedAtKey:  "agentRegistrationTokenUsedAt",
		TokenExpiresKey: "agentRegistrationTokenExpiresAt",
		Identity: map[string]string{
			"agentClusterId": "dev-us",
		},
		StepData: map[string]any{
			"agentRegistrationTokenUsedAt": now.Format(time.RFC3339Nano),
			"agentAuthTokenHash":           testTokenHash("agent-auth-token"),
			"agentId":                      "agent-1",
		},
		Now: now,
	}

	var wg sync.WaitGroup
	errs := make([]error, 16)
	for idx := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.ClaimBootstrapToken(claimRequest)
		}(idx)
	}
	wg.Wait()

	successes := 0
	alreadyUsed := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrBootstrapTokenAlreadyUsed):
			alreadyUsed++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one successful claim, got %d errors=%v", successes, errs)
	}
	if alreadyUsed != len(errs)-1 {
		t.Fatalf("expected remaining claims to be already-used, got %d", alreadyUsed)
	}
	claimed, err := store.GetByProject("orders")
	if err != nil {
		t.Fatalf("get claimed session: %v", err)
	}
	if claimed.Data["agentRegistrationTokenUsedAt"] == "" {
		t.Fatalf("claim did not persist usedAt: %#v", claimed.Data)
	}
	if claimed.Data["agentAuthTokenHash"] != testTokenHash("agent-auth-token") {
		t.Fatalf("claim did not persist auth token hash: %#v", claimed.Data)
	}

	if _, err := store.ClaimBootstrapToken(claimRequest); !errors.Is(err, ErrBootstrapTokenAlreadyUsed) {
		t.Fatalf("second claim after success err=%v want already used", err)
	}
}

func testTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
