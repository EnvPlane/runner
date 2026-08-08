//go:build integration
// +build integration

package jobs

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/envpilot/runner/internal/domain"
	"github.com/envpilot/runner/internal/postgres"
)

func TestSQLJobStoreRecovery(t *testing.T) {
	db, closeDB := setupSQLManagerIntegrationDB(t)
	defer closeDB()

	if _, err := db.Exec(`TRUNCATE TABLE jobs`); err != nil {
		t.Fatalf("truncate jobs: %v", err)
	}

	jobStore := NewSQLStore(db)
	createdAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	queued := Job{
		ID:            "job-001001",
		Type:          TypeCreateEnvironment,
		Status:        StatusQueued,
		EnvironmentID: "pr-001001",
		Request:       domain.CreateEnvironmentRequest{ID: "pr-001001"},
		MaxAttempts:   3,
		CreatedAt:     createdAt,
	}
	if err := jobStore.Save(queued); err != nil {
		t.Fatalf("save queued job: %v", err)
	}
	running := Job{
		ID:            "job-001002",
		Type:          TypeDeleteEnvironment,
		Status:        StatusRunning,
		EnvironmentID: "pr-001002",
		Request:       domain.CreateEnvironmentRequest{ID: "pr-001002"},
		MaxAttempts:   3,
		CreatedAt:     createdAt,
		StartedAt:     &createdAt,
	}
	if err := jobStore.Save(running); err != nil {
		t.Fatalf("save running job: %v", err)
	}

	manager := NewManager(&noopExecutor{}, WithStore(jobStore))
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if manager.QueueDepth() != 2 {
		t.Fatalf("queue depth = %d", manager.QueueDepth())
	}

	recovered, ok := manager.Get("job-001002")
	if !ok {
		t.Fatal("expected recovered running job")
	}
	if recovered.Status != StatusQueued || recovered.StartedAt != nil {
		t.Fatalf("unexpected recovered job: %+v", recovered)
	}
}

func TestSQLJobStorePersistsRetryState(t *testing.T) {
	db, closeDB := setupSQLManagerIntegrationDB(t)
	defer closeDB()

	if _, err := db.Exec(`TRUNCATE TABLE jobs`); err != nil {
		t.Fatalf("truncate jobs: %v", err)
	}

	jobStore := NewSQLStore(db)
	if err := jobStore.Save(Job{
		ID:            "job-001003",
		Type:          TypeCreateEnvironment,
		Status:        StatusQueued,
		EnvironmentID: "pr-001003",
		Request:       domain.CreateEnvironmentRequest{ID: "pr-001003"},
		MaxAttempts:   2,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	executor := &flakyExecutor{failuresRemaining: 1}
	manager := NewManager(executor, WithStore(jobStore), WithRetryDelay(0))
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}

	processed, err := manager.ProcessNext(context.Background())
	if !processed {
		t.Fatal("expected processing attempt")
	}
	if err == nil {
		t.Fatal("expected first attempt failure")
	}
	first, ok := manager.Get("job-001003")
	if !ok {
		t.Fatal("expected persisted job after first attempt")
	}
	if first.Status != StatusQueued || first.Attempts != 1 || first.NextRunAt == nil || first.Error == "" {
		t.Fatalf("unexpected first attempt state: %+v", first)
	}

	processed, err = manager.ProcessNext(context.Background())
	if !processed {
		t.Fatal("expected second attempt")
	}
	if err != nil {
		t.Fatalf("expected second attempt success, got %v", err)
	}
	second, ok := manager.Get("job-001003")
	if !ok {
		t.Fatal("expected persisted job after second attempt")
	}
	if second.Status != StatusSucceeded || second.Attempts != 2 {
		t.Fatalf("unexpected second attempt state: %+v", second)
	}
}

func setupSQLManagerIntegrationDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("ENVPILOT_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("ENVPILOT_TEST_DATABASE_URL is not set; skipping SQL jobs integration test")
	}

	db, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping database: %v", err)
	}
	migrator := postgres.NewMigratorWithDir(db, filepath.Join("..", "..", "migrations", "postgres"))
	if err := migrator.Apply(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	cancel()

	cleanup := func() {
		_ = db.Close()
	}
	return db, cleanup
}

type noopExecutor struct{}

func (n *noopExecutor) CreateEnvironment(_ context.Context, req domain.CreateEnvironmentRequest) (domain.Environment, error) {
	return domain.Environment{ID: req.ID, Status: domain.StatusCreating}, nil
}
func (n *noopExecutor) DeleteEnvironment(_ context.Context, id string, _ bool) (domain.Environment, error) {
	return domain.Environment{ID: id, Status: domain.StatusTerminated}, nil
}
func (n *noopExecutor) GetEnvironment(_ string) (domain.Environment, error) {
	return domain.Environment{}, nil
}
