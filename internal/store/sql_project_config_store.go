package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"envpilot/internal/domain"
)

type SQLProjectConfigStore struct {
	db *sql.DB
}

func NewSQLProjectConfigStore(db *sql.DB) (*SQLProjectConfigStore, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	return &SQLProjectConfigStore{db: db}, nil
}

func (s *SQLProjectConfigStore) Latest(projectID string) (domain.ProjectConfig, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.ProjectConfig{}, ErrProjectConfigNotFound
	}
	row := s.db.QueryRow(`
SELECT id, project_id, version, config, sensitive, created_by, created_at
FROM project_config_versions
WHERE project_id = $1
ORDER BY version DESC, created_at DESC
LIMIT 1`, projectID)
	return scanProjectConfig(row)
}

func (s *SQLProjectConfigStore) Save(config domain.ProjectConfig) error {
	config.ID = strings.TrimSpace(config.ID)
	config.ProjectID = strings.TrimSpace(config.ProjectID)
	if config.ID == "" {
		return errors.New("project config id is required")
	}
	if config.ProjectID == "" {
		return errors.New("project id is required")
	}
	if config.Version <= 0 {
		return errors.New("project config version is required")
	}
	if config.Config == nil {
		config.Config = map[string]any{}
	}
	if config.Sensitive == nil {
		config.Sensitive = map[string]any{}
	}
	if config.CreatedAt.IsZero() {
		config.CreatedAt = time.Now().UTC()
	}
	configPayload, err := json.Marshal(config.Config)
	if err != nil {
		return err
	}
	sensitivePayload, err := json.Marshal(config.Sensitive)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
INSERT INTO project_config_versions (id, project_id, version, config, sensitive, created_by, created_at)
VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7)
ON CONFLICT (project_id, version) DO UPDATE SET
	id = EXCLUDED.id,
	config = EXCLUDED.config,
	sensitive = EXCLUDED.sensitive,
	created_by = EXCLUDED.created_by,
	created_at = EXCLUDED.created_at`,
		config.ID,
		config.ProjectID,
		config.Version,
		string(configPayload),
		string(sensitivePayload),
		config.CreatedBy,
		config.CreatedAt)
	return err
}

func scanProjectConfig(scanner interface{ Scan(dest ...any) error }) (domain.ProjectConfig, error) {
	var (
		config           domain.ProjectConfig
		configPayload    []byte
		sensitivePayload []byte
	)
	err := scanner.Scan(
		&config.ID,
		&config.ProjectID,
		&config.Version,
		&configPayload,
		&sensitivePayload,
		&config.CreatedBy,
		&config.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.ProjectConfig{}, ErrProjectConfigNotFound
		}
		return domain.ProjectConfig{}, err
	}
	if len(configPayload) > 0 {
		if err := json.Unmarshal(configPayload, &config.Config); err != nil {
			return domain.ProjectConfig{}, err
		}
	}
	if len(sensitivePayload) > 0 {
		if err := json.Unmarshal(sensitivePayload, &config.Sensitive); err != nil {
			return domain.ProjectConfig{}, err
		}
	}
	if config.Config == nil {
		config.Config = map[string]any{}
	}
	if config.Sensitive == nil {
		config.Sensitive = map[string]any{}
	}
	return config, nil
}
