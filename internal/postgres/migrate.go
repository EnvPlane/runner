package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Migrator struct {
	db  *sql.DB
	dir string
}

type MigrationStep struct {
	Version     string   `json:"version"`
	File        string   `json:"file"`
	Description string   `json:"description"`
	Requires    []string `json:"requires,omitempty"`
	Checksum    string   `json:"checksum,omitempty"`
}

type MigrationManifest struct {
	Version    string          `json:"version"`
	Migrations []MigrationStep `json:"migrations"`
}

type migrationRecord struct {
	File        string
	Description string
	Checksum    string
}

const defaultManifestFile = "migrations.json"
const migrationStateTable = "schema_migrations"

func Open(databaseURL string) (*sql.DB, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, fmt.Errorf("database url is required")
	}
	return sql.Open("pgx", databaseURL)
}

func NewMigrator(db *sql.DB) Migrator {
	return Migrator{db: db, dir: "migrations/postgres"}
}

func NewMigratorWithDir(db *sql.DB, dir string) Migrator {
	if strings.TrimSpace(dir) == "" {
		dir = "migrations/postgres"
	}
	return Migrator{db: db, dir: dir}
}

func (m Migrator) Apply(ctx context.Context) error {
	if m.db == nil {
		return fmt.Errorf("postgres db is nil")
	}
	if err := m.ensureMigrationState(ctx); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	steps, err := m.Plan()
	if err != nil {
		return fmt.Errorf("build migration plan: %w", err)
	}
	for _, step := range steps {
		version := strings.TrimSpace(step.Version)
		path := strings.TrimSpace(step.File)
		if version == "" {
			version = strings.TrimSuffix(path, filepath.Ext(path))
		}
		if path == "" {
			return fmt.Errorf("migration %s has missing file", version)
		}
		applied, record, err := m.applied(ctx, version)
		if err != nil {
			return err
		}
		if applied {
			if err := validateAppliedChecksum(version, step.Checksum, record.Checksum); err != nil {
				return err
			}
			continue
		}

		content, err := os.ReadFile(filepath.Join(m.dir, path))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		if strings.TrimSpace(step.Checksum) == "" {
			step.Checksum = computeChecksum(content)
		}
		step.Version = version
		step.File = path
		if err := m.applyOne(ctx, step, string(content)); err != nil {
			return err
		}
	}
	return nil
}

func (m Migrator) ensureMigrationState(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + quoteIdentifier(migrationStateTable) + ` (
			version text PRIMARY KEY,
			file text,
			description text,
			checksum text,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`,
		`ALTER TABLE ` + quoteIdentifier(migrationStateTable) + ` ADD COLUMN IF NOT EXISTS file text`,
		`ALTER TABLE ` + quoteIdentifier(migrationStateTable) + ` ADD COLUMN IF NOT EXISTS description text`,
		`ALTER TABLE ` + quoteIdentifier(migrationStateTable) + ` ADD COLUMN IF NOT EXISTS checksum text`,
	}
	for _, statement := range statements {
		if _, err := m.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func computeChecksum(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:])
}

func validateAppliedChecksum(version, expected, current string) error {
	expected = strings.TrimSpace(expected)
	current = strings.TrimSpace(current)
	if expected == "" || current == "" {
		return nil
	}
	if expected != current {
		return fmt.Errorf("migration %s checksum mismatch: applied=%s current=%s", version, current, expected)
	}
	return nil
}

func (m Migrator) Plan() ([]MigrationStep, error) {
	manifestPath := filepath.Join(m.dir, defaultManifestFile)
	manifestContent, err := os.ReadFile(manifestPath)
	if err == nil {
		var manifest MigrationManifest
		if err := json.Unmarshal(manifestContent, &manifest); err != nil {
			return nil, fmt.Errorf("read migration manifest %s: %w", manifestPath, err)
		}
		steps, err := validateMigrationManifest(manifest)
		if err != nil {
			return nil, err
		}
		return populateMigrationChecksums(m.dir, steps)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read migration manifest %s: %w", manifestPath, err)
	}

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, fmt.Errorf("read postgres migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || strings.EqualFold(entry.Name(), defaultManifestFile) {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	steps := make([]MigrationStep, 0, len(names))
	for _, name := range names {
		steps = append(steps, MigrationStep{
			Version: strings.TrimSuffix(name, filepath.Ext(name)),
			File:    name,
		})
	}
	return populateMigrationChecksums(m.dir, steps)
}

func validateMigrationManifest(manifest MigrationManifest) ([]MigrationStep, error) {
	if manifest.Version != "" && strings.TrimSpace(manifest.Version) != "1" {
		return nil, fmt.Errorf("unsupported migration manifest version: %s", manifest.Version)
	}
	if len(manifest.Migrations) == 0 {
		return nil, fmt.Errorf("migration manifest is empty")
	}
	seenVersions := map[string]struct{}{}
	seenFiles := map[string]struct{}{}
	stepsByVersion := map[string]MigrationStep{}
	positionByVersion := map[string]int{}
	steps := make([]MigrationStep, 0, len(manifest.Migrations))
	for _, step := range manifest.Migrations {
		version := strings.TrimSpace(step.Version)
		file := strings.TrimSpace(step.File)
		if version == "" {
			return nil, fmt.Errorf("manifest migration entry requires version")
		}
		if file == "" {
			return nil, fmt.Errorf("manifest migration entry %s requires file", version)
		}
		if !strings.HasSuffix(file, ".sql") {
			return nil, fmt.Errorf("migration %s has invalid file extension: %s", version, file)
		}
		if _, err := strconv.Atoi(strings.SplitN(version, "_", 2)[0]); err != nil {
			return nil, fmt.Errorf("migration %q has invalid version format", version)
		}
		if _, ok := seenVersions[version]; ok {
			return nil, fmt.Errorf("duplicate migration version %s", version)
		}
		if _, ok := seenFiles[file]; ok {
			return nil, fmt.Errorf("duplicate migration file %s", file)
		}
		requires := make([]string, 0, len(step.Requires))
		seenRequirements := map[string]struct{}{}
		for _, requirement := range step.Requires {
			requirement = strings.TrimSpace(requirement)
			if requirement == "" {
				continue
			}
			if _, ok := seenRequirements[requirement]; ok {
				continue
			}
			seenRequirements[requirement] = struct{}{}
			requires = append(requires, requirement)
		}
		step.Version = version
		step.File = file
		step.Requires = requires
		step.Checksum = strings.TrimSpace(step.Checksum)
		step.Description = strings.TrimSpace(step.Description)
		positionByVersion[version] = len(steps)
		steps = append(steps, step)
		stepsByVersion[version] = step
		seenVersions[version] = struct{}{}
		seenFiles[file] = struct{}{}
	}
	ordered, err := orderMigrationsByDependencies(steps, positionByVersion)
	if err != nil {
		return nil, err
	}
	for _, step := range ordered {
		for _, requirement := range step.Requires {
			if _, ok := stepsByVersion[requirement]; !ok {
				return nil, fmt.Errorf("migration %s requires missing dependency %s", step.Version, requirement)
			}
		}
	}
	return ordered, nil
}

func populateMigrationChecksums(dir string, steps []MigrationStep) ([]MigrationStep, error) {
	for i, step := range steps {
		step.Version = strings.TrimSpace(step.Version)
		step.File = strings.TrimSpace(step.File)
		if step.File == "" {
			return nil, fmt.Errorf("migration %s has missing file", step.Version)
		}
		content, err := os.ReadFile(filepath.Join(dir, step.File))
		if err != nil {
			return nil, fmt.Errorf("migration file missing: %s", step.File)
		}
		checksum := computeChecksum(content)
		if step.Checksum != "" && step.Checksum != checksum {
			return nil, fmt.Errorf("migration checksum mismatch for %s", step.Version)
		}
		step.Checksum = checksum
		steps[i] = step
	}
	return steps, nil
}

func orderMigrationsByDependencies(steps []MigrationStep, positionByVersion map[string]int) ([]MigrationStep, error) {
	if len(steps) == 0 {
		return steps, nil
	}
	ordered := make([]MigrationStep, 0, len(steps))
	applied := map[string]struct{}{}
	for len(ordered) < len(steps) {
		chosenIndex := -1
		for i, step := range steps {
			if _, done := applied[step.Version]; done {
				continue
			}
			canApply := true
			for _, requirement := range step.Requires {
				if _, ready := applied[requirement]; !ready {
					canApply = false
					break
				}
			}
			if !canApply {
				continue
			}
			if chosenIndex == -1 || positionByVersion[steps[chosenIndex].Version] > positionByVersion[step.Version] {
				chosenIndex = i
			}
		}
		if chosenIndex < 0 {
			return nil, fmt.Errorf("migration dependency cycle detected")
		}
		ordered = append(ordered, steps[chosenIndex])
		applied[steps[chosenIndex].Version] = struct{}{}
	}
	return ordered, nil
}

func (m Migrator) applied(ctx context.Context, version string) (bool, migrationRecord, error) {
	var record migrationRecord
	var file sql.NullString
	var description sql.NullString
	var checksum sql.NullString

	row := m.db.QueryRowContext(ctx, `SELECT file, description, checksum FROM schema_migrations WHERE version = $1`, version)
	if err := row.Scan(&file, &description, &checksum); err != nil {
		if err == sql.ErrNoRows {
			return false, migrationRecord{}, nil
		}
		return false, migrationRecord{}, fmt.Errorf("check migration %s: %w", version, err)
	}
	if file.Valid {
		record.File = file.String
	}
	if description.Valid {
		record.Description = description.String
	}
	if checksum.Valid {
		record.Checksum = checksum.String
	}
	return true, record, nil
}

func (m Migrator) applyOne(ctx context.Context, step MigrationStep, statement string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", step.Version, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("apply migration %s: %w", step.Version, err)
	}
	if step.Checksum == "" {
		step.Checksum = computeChecksum([]byte(statement))
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_migrations (version, file, description, checksum) VALUES ($1, $2, $3, $4)`,
		step.Version, step.File, step.Description, step.Checksum,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", step.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", step.Version, err)
	}
	return nil
}
