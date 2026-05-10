package postgres

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresInitialMigrationDefinesFoundationTables(t *testing.T) {
	content, err := os.ReadFile("../../migrations/postgres/001_initial.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(content)
	for _, expected := range []string{
		"CREATE TABLE IF NOT EXISTS environments",
		"CREATE TABLE IF NOT EXISTS projects",
		"CREATE TABLE IF NOT EXISTS products",
		"CREATE TABLE IF NOT EXISTS control_plane_settings",
		"CREATE TABLE IF NOT EXISTS jobs",
		"CREATE TABLE IF NOT EXISTS schema_migrations",
		"payload jsonb NOT NULL",
	} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("migration does not contain %q", expected)
		}
	}
}

func TestNewMigratorWithDirDefaultsMigrationPath(t *testing.T) {
	migrator := NewMigratorWithDir(nil, "")
	if migrator.dir != "migrations/postgres" {
		t.Fatalf("migration dir = %q", migrator.dir)
	}
}

func TestPlanUsesManifestForOrderAndMetadata(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "001_a.sql"), []byte("a;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "002_b.sql"), []byte("b;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	manifest := MigrationManifest{
		Version: "1",
		Migrations: []MigrationStep{
			{Version: "002_b", File: "002_b.sql", Description: "second"},
			{Version: "001_a", File: "001_a.sql", Description: "first"},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultManifestFile), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	steps, err := migrator.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %#v", steps)
	}
	if steps[0].Version != "002_b" || steps[1].Version != "001_a" {
		t.Fatalf("unexpected plan order: %#v", []string{steps[0].Version, steps[1].Version})
	}
}

func TestPlanComputesChecksumForManifestDefinedMigrations(t *testing.T) {
	dir := t.TempDir()
	migrationContent := []byte("SELECT 1;")

	if err := os.WriteFile(filepath.Join(dir, "001_a.sql"), migrationContent, 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	manifest := MigrationManifest{
		Version: "1",
		Migrations: []MigrationStep{
			{Version: "001_a", File: "001_a.sql"},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal migration manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultManifestFile), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	steps, err := migrator.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %#v", steps)
	}
	if steps[0].Checksum != computeChecksum(migrationContent) {
		t.Fatalf("checksum mismatch: expected %s, got %s", computeChecksum(migrationContent), steps[0].Checksum)
	}
}

func TestPlanRejectsManifestChecksumMismatch(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "001_a.sql"), []byte("SELECT 1;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	manifest := MigrationManifest{
		Version: "1",
		Migrations: []MigrationStep{
			{Version: "001_a", File: "001_a.sql", Checksum: "deadbeef"},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal migration manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultManifestFile), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	if _, err := migrator.Plan(); err == nil {
		t.Fatalf("expected checksum mismatch")
	}
}

func TestPlanRespectsManifestDependenciesAndStableOrdering(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "001_base.sql"), []byte("base;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "002_feature.sql"), []byte("feature;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "003_optional.sql"), []byte("optional;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	manifest := MigrationManifest{
		Version: "1",
		Migrations: []MigrationStep{
			{Version: "002_feature", File: "002_feature.sql", Requires: []string{"001_base"}},
			{Version: "001_base", File: "001_base.sql"},
			{Version: "003_optional", File: "003_optional.sql"},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal migration manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultManifestFile), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	steps, err := migrator.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %#v", steps)
	}
	if steps[0].Version != "001_base" || steps[1].Version != "002_feature" || steps[2].Version != "003_optional" {
		t.Fatalf("unexpected plan order: %#v", []string{steps[0].Version, steps[1].Version, steps[2].Version})
	}
}

func TestPlanRejectsMissingDependency(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "001_base.sql"), []byte("base;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	manifest := MigrationManifest{
		Version: "1",
		Migrations: []MigrationStep{
			{Version: "001_base", File: "001_base.sql", Requires: []string{"not_exists"}},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal migration manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultManifestFile), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	if _, err := migrator.Plan(); err == nil {
		t.Fatalf("expected missing dependency error")
	}
}

func TestPlanRejectsDependencyCycle(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "001_a.sql"), []byte("a;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "002_b.sql"), []byte("b;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	manifest := MigrationManifest{
		Version: "1",
		Migrations: []MigrationStep{
			{Version: "001_a", File: "001_a.sql", Requires: []string{"002_b"}},
			{Version: "002_b", File: "002_b.sql", Requires: []string{"001_a"}},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal migration manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultManifestFile), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	if _, err := migrator.Plan(); err == nil {
		t.Fatalf("expected cycle detection error")
	}
}

func TestPlanFallsBackToLexicographicOrderWhenManifestMissing(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "002_after.sql"), []byte("after;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "001_before.sql"), []byte("before;"), 0o600); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	steps, err := migrator.Plan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("len steps = %d", len(steps))
	}
	if steps[0].Version != "001_before" || steps[1].Version != "002_after" {
		t.Fatalf("unexpected fallback order: %#v", []string{steps[0].Version, steps[1].Version})
	}
}

func TestPlanRequiresValidManifestEntries(t *testing.T) {
	dir := t.TempDir()

	manifest := MigrationManifest{
		Version: "1",
		Migrations: []MigrationStep{
			{Version: "bad", File: "missing.sql"},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, defaultManifestFile), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	migrator := NewMigratorWithDir(nil, dir)
	_, err = migrator.Plan()
	if err == nil {
		t.Fatalf("expected plan validation error for invalid version")
	}
}

func TestOpenRejectsEmptyDatabaseURL(t *testing.T) {
	if _, err := Open(" "); err == nil {
		t.Fatal("expected empty database url error")
	}
}
