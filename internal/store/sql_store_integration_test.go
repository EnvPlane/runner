//go:build integration
// +build integration

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/envplane/contracts/domain"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestSQLStoreCRUD(t *testing.T) {
	db, closeDB := setupSQLStoreIntegrationDB(t)
	defer closeDB()
	if _, err := db.Exec(`TRUNCATE TABLE environments`); err != nil {
		t.Fatalf("truncate environments: %v", err)
	}

	store := NewSQLStore(db)
	createdAt := time.Now().UTC().Truncate(time.Second)
	updatedAt := createdAt
	base := domain.Environment{
		ID:        "sql-001",
		Project:   "cms",
		Product:   "bethunder",
		Namespace: "sql-001-cms",
		Mode:      domain.ModeHybrid,
		Status:    domain.StatusCreating,
		Source: domain.SCMSource{
			PullRequestID: "1701",
			Branch:        "feature/sql-001",
			Commit:        "abc123",
		},
		TTLHours:  72,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if err := store.Save(base); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.Get("sql-001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Namespace != "sql-001-cms" {
		t.Fatalf("namespace = %q", got.Namespace)
	}
	if got.TTLHours != 72 {
		t.Fatalf("ttl = %d", got.TTLHours)
	}
	record, err := store.GetRecord("sql-001")
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if record.ProjectID != "cms" {
		t.Fatalf("project_id = %q", record.ProjectID)
	}
	if record.PRID != "1701" {
		t.Fatalf("pr_id = %q", record.PRID)
	}
	if record.Branch != "feature/sql-001" {
		t.Fatalf("branch = %q", record.Branch)
	}
	if record.CommitSHA != "abc123" {
		t.Fatalf("commit = %q", record.CommitSHA)
	}
	if record.Status != domain.StatusCreating {
		t.Fatalf("status = %q", record.Status)
	}
	if record.Type != domain.ModeHybrid {
		t.Fatalf("type = %q", record.Type)
	}
	if record.TTL != 72 {
		t.Fatalf("ttl = %d", record.TTL)
	}

	if err := store.Delete("sql-001"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get("sql-001"); err == nil {
		t.Fatal("expected deleted environment")
	}
}

func TestSQLStoreListSortsByCreatedAtDesc(t *testing.T) {
	db, closeDB := setupSQLStoreIntegrationDB(t)
	defer closeDB()
	if _, err := db.Exec(`TRUNCATE TABLE environments`); err != nil {
		t.Fatalf("truncate environments: %v", err)
	}

	store := NewSQLStore(db)
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.Save(domain.Environment{
		ID:        "sql-old",
		Project:   "cms",
		Status:    domain.StatusCreating,
		Mode:      domain.ModeFull,
		CreatedAt: now.Add(-2 * time.Hour),
		UpdatedAt: now.Add(-2 * time.Hour),
		Source:    domain.SCMSource{PullRequestID: "1001", Branch: "feature/old"},
	}); err != nil {
		t.Fatalf("save old env: %v", err)
	}
	if err := store.Save(domain.Environment{
		ID:        "sql-new",
		Project:   "cms",
		Status:    domain.StatusCreating,
		Mode:      domain.ModeFull,
		CreatedAt: now,
		UpdatedAt: now,
		Source:    domain.SCMSource{PullRequestID: "1002", Branch: "feature/new"},
	}); err != nil {
		t.Fatalf("save new env: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].ID != "sql-new" {
		t.Fatalf("expected newest first, got %q", items[0].ID)
	}
	if items[1].ID != "sql-old" {
		t.Fatalf("expected oldest second, got %q", items[1].ID)
	}
}

func TestSQLProjectStoreCRUD(t *testing.T) {
	db, closeDB := setupSQLStoreIntegrationDB(t)
	defer closeDB()
	if _, err := db.Exec(`TRUNCATE TABLE projects`); err != nil {
		t.Fatalf("truncate projects: %v", err)
	}

	projectStore, err := NewSQLProjectStore(db, nil)
	if err != nil {
		t.Fatalf("new project store: %v", err)
	}
	project := domain.Project{
		ID:                 "sql-project-1",
		Name:               "SQL Project",
		ProductID:          "cms",
		CreatedAt:          time.Now().UTC().Truncate(time.Second),
		UpdatedAt:          time.Now().UTC().Truncate(time.Second),
		AppRepositoryID:    "app-repo",
		GitOpsRepositoryID: "gitops-repo",
		GitRepo: domain.RepositoryRef{
			Provider:      "github",
			URL:           "https://github.example.com/example/sql",
			DefaultBranch: "main",
		},
	}
	if err := projectStore.Save(project); err != nil {
		t.Fatalf("save project: %v", err)
	}

	got, err := projectStore.Get("sql-project-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.ID != project.ID {
		t.Fatalf("project id = %q", got.ID)
	}
	if got.AppRepositoryID != project.AppRepositoryID {
		t.Fatalf("app_repository_id = %q", got.AppRepositoryID)
	}
	items, err := projectStore.List()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 project, got %d", len(items))
	}
}

func TestSQLProductStoreCRUD(t *testing.T) {
	db, closeDB := setupSQLStoreIntegrationDB(t)
	defer closeDB()
	if _, err := db.Exec(`TRUNCATE TABLE products`); err != nil {
		t.Fatalf("truncate products: %v", err)
	}

	productStore, err := NewSQLProductStore(db, nil)
	if err != nil {
		t.Fatalf("new product store: %v", err)
	}
	product := domain.ProductTemplate{
		Name:             "Sql-Product-1",
		NamespaceSuffix:  "sql",
		ManifestSourceID: "manual-source",
		BasePath:         "/sql/base/path",
		DefaultDomain:    "sql.example",
	}
	if err := productStore.Save(product); err != nil {
		t.Fatalf("save product: %v", err)
	}

	got, err := productStore.Get("sql-product-1")
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	if got.Name != "sql-product-1" {
		t.Fatalf("product name = %q", got.Name)
	}
	if got.ManifestSourceID != product.ManifestSourceID {
		t.Fatalf("manifest source = %q", got.ManifestSourceID)
	}
	items, err := productStore.List()
	if err != nil {
		t.Fatalf("list products: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 product, got %d", len(items))
	}

	if err := productStore.Delete("sql-product-1"); err != nil {
		t.Fatalf("delete product: %v", err)
	}
	if _, err := productStore.Get("sql-product-1"); err == nil {
		t.Fatal("expected deleted product")
	}
}

func TestSQLSettingsStoreCRUD(t *testing.T) {
	db, closeDB := setupSQLStoreIntegrationDB(t)
	defer closeDB()
	if _, err := db.Exec(`TRUNCATE TABLE control_plane_settings`); err != nil {
		t.Fatalf("truncate settings: %v", err)
	}

	settingsStore, err := NewSQLSettingsStore(db, domain.ControlPlaneSettings{
		SchemaVersion: "v-test",
		Runtime: domain.RuntimeSettings{
			DefaultTTLHours: 42,
		},
	})
	if err != nil {
		t.Fatalf("new settings store: %v", err)
	}
	got, err := settingsStore.Get()
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if got.SchemaVersion != "v-test" {
		t.Fatalf("schema version = %q", got.SchemaVersion)
	}

	next := got
	next.Runtime.DefaultTTLHours = 24
	if err := settingsStore.Save(next); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	got, err = settingsStore.Get()
	if err != nil {
		t.Fatalf("get settings after save: %v", err)
	}
	if got.Runtime.DefaultTTLHours != 24 {
		t.Fatalf("default ttl = %d", got.Runtime.DefaultTTLHours)
	}
}

func TestSQLBootstrapSessionStoreClaimBootstrapTokenIsAtomic(t *testing.T) {
	db, closeDB := setupSQLStoreIntegrationDB(t)
	defer closeDB()
	if _, err := db.Exec(`TRUNCATE TABLE bootstrap_sessions`); err != nil {
		t.Fatalf("truncate bootstrap_sessions: %v", err)
	}

	store, err := NewSQLBootstrapSessionStore(db)
	if err != nil {
		t.Fatalf("new bootstrap session store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	tokenHash := sqlTestTokenHash("agent-registration-token")
	if err := store.Save(domain.BootstrapSession{
		ID:          "sql-bootstrap-claim",
		ProjectID:   "sql-bootstrap",
		CurrentStep: 3,
		Status:      domain.BootstrapSessionStatusDraft,
		CreatedBy:   "alice",
		CreatedAt:   now,
		UpdatedAt:   now,
		Data: map[string]any{
			"agentRegistrationTokenProject":   "sql-bootstrap",
			"agentRegistrationTokenHash":      tokenHash,
			"agentRegistrationTokenExpiresAt": now.Add(time.Hour).Format(time.RFC3339Nano),
			"agentClusterId":                  "dev-us",
		},
	}); err != nil {
		t.Fatalf("save bootstrap session: %v", err)
	}

	claimRequest := BootstrapTokenClaimRequest{
		ProjectID:       "sql-bootstrap",
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
			"agentAuthTokenHash":           sqlTestTokenHash("agent-auth-token"),
			"agentId":                      "agent-1",
		},
		Now: now,
	}

	var wg sync.WaitGroup
	errs := make([]error, 12)
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
		t.Fatalf("expected exactly one successful SQL claim, got %d errors=%v", successes, errs)
	}
	if alreadyUsed != len(errs)-1 {
		t.Fatalf("expected remaining SQL claims to be already-used, got %d", alreadyUsed)
	}
	claimed, err := store.GetByProject("sql-bootstrap")
	if err != nil {
		t.Fatalf("get claimed session: %v", err)
	}
	if strings.TrimSpace(claimed.Data["agentRegistrationTokenUsedAt"].(string)) == "" {
		t.Fatalf("claim did not persist usedAt: %#v", claimed.Data)
	}
	if claimed.Data["agentAuthTokenHash"] != sqlTestTokenHash("agent-auth-token") {
		t.Fatalf("claim did not persist auth token hash: %#v", claimed.Data)
	}
}

func TestSQLBootstrapSessionStoreClaimBootstrapTokenRollsBackOnUpdateError(t *testing.T) {
	db, closeDB := setupSQLStoreIntegrationDB(t)
	defer closeDB()
	if _, err := db.Exec(`TRUNCATE TABLE bootstrap_sessions`); err != nil {
		t.Fatalf("truncate bootstrap_sessions: %v", err)
	}

	store, err := NewSQLBootstrapSessionStore(db)
	if err != nil {
		t.Fatalf("new bootstrap session store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	tokenHash := sqlTestTokenHash("agent-registration-token-rollback")
	if err := store.Save(domain.BootstrapSession{
		ID:          "sql-bootstrap-claim-rollback",
		ProjectID:   "sql-bootstrap-rollback",
		CurrentStep: 3,
		Status:      domain.BootstrapSessionStatusDraft,
		CreatedBy:   "alice",
		CreatedAt:   now,
		UpdatedAt:   now,
		Data: map[string]any{
			"agentRegistrationTokenProject":   "sql-bootstrap-rollback",
			"agentRegistrationTokenHash":      tokenHash,
			"agentRegistrationTokenExpiresAt": now.Add(time.Hour).Format(time.RFC3339Nano),
			"agentClusterId":                  "dev-us",
		},
	}); err != nil {
		t.Fatalf("save bootstrap session: %v", err)
	}

	if _, err := db.Exec(`
CREATE OR REPLACE FUNCTION envplane_test_fail_bootstrap_claim()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	IF NEW.id = 'sql-bootstrap-claim-rollback' THEN
		RAISE EXCEPTION 'forced bootstrap claim rollback';
	END IF;
	RETURN NEW;
END;
$$`); err != nil {
		t.Fatalf("create rollback test function: %v", err)
	}
	defer db.Exec(`DROP FUNCTION IF EXISTS envplane_test_fail_bootstrap_claim()`)
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS envplane_test_fail_bootstrap_claim ON bootstrap_sessions`); err != nil {
		t.Fatalf("drop stale rollback test trigger: %v", err)
	}
	if _, err := db.Exec(`
CREATE TRIGGER envplane_test_fail_bootstrap_claim
BEFORE UPDATE ON bootstrap_sessions
FOR EACH ROW
EXECUTE FUNCTION envplane_test_fail_bootstrap_claim()`); err != nil {
		t.Fatalf("create rollback test trigger: %v", err)
	}
	defer db.Exec(`DROP TRIGGER IF EXISTS envplane_test_fail_bootstrap_claim ON bootstrap_sessions`)

	_, err = store.ClaimBootstrapToken(BootstrapTokenClaimRequest{
		ProjectID:       "sql-bootstrap-rollback",
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
			"agentAuthTokenHash":           sqlTestTokenHash("agent-auth-token-rollback"),
			"agentId":                      "agent-rollback",
		},
		Now: now,
	})
	if err == nil {
		t.Fatal("expected forced claim update error")
	}

	claimed, err := store.GetByProject("sql-bootstrap-rollback")
	if err != nil {
		t.Fatalf("get session after failed claim: %v", err)
	}
	if usedAt, ok := claimed.Data["agentRegistrationTokenUsedAt"].(string); ok && strings.TrimSpace(usedAt) != "" {
		t.Fatalf("failed claim partially persisted usedAt: %#v", claimed.Data)
	}
	if authHash, ok := claimed.Data["agentAuthTokenHash"].(string); ok && strings.TrimSpace(authHash) != "" {
		t.Fatalf("failed claim partially persisted auth token hash: %#v", claimed.Data)
	}
}

func sqlTestTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func setupSQLStoreIntegrationDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("ENVPLANE_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("ENVPLANE_TEST_DATABASE_URL is not set; skipping SQL store integration test")
	}
	if os.Getenv("ENVPLANE_TEST_DATABASE_SCHEMA_READY") != "1" {
		t.Skip("database schema must be provisioned by control-plane; set ENVPLANE_TEST_DATABASE_SCHEMA_READY=1")
	}
	migrationsDir := strings.TrimSpace(os.Getenv("ENVPLANE_MIGRATIONS_DIR"))
	if migrationsDir == "" {
		t.Fatal("ENVPLANE_MIGRATIONS_DIR must point to the control-plane migration artifact")
	}
	if _, err := os.Stat(strings.TrimSuffix(migrationsDir, "/") + "/migrations.json"); err != nil {
		t.Fatalf("control-plane migration artifact is unavailable: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping database: %v", err)
	}
	cancel()

	cleanup := func() {
		_ = db.Close()
	}
	return db, cleanup
}
