package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"envpilot/internal/domain"
)

type SQLBootstrapSessionStore struct {
	db *sql.DB
}

func NewSQLBootstrapSessionStore(db *sql.DB) (*SQLBootstrapSessionStore, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	return &SQLBootstrapSessionStore{db: db}, nil
}

func (s *SQLBootstrapSessionStore) GetByProject(projectID string) (domain.BootstrapSession, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.BootstrapSession{}, ErrBootstrapSessionNotFound
	}
	row := s.db.QueryRow(`
SELECT id, project_id, current_step, status, created_by, data, created_at, updated_at
FROM bootstrap_sessions
WHERE project_id = $1
ORDER BY updated_at DESC
LIMIT 1`, projectID)
	return s.scanSession(row)
}

func (s *SQLBootstrapSessionStore) Save(session domain.BootstrapSession) error {
	session.ID = strings.TrimSpace(session.ID)
	session.ProjectID = strings.TrimSpace(session.ProjectID)
	session.CreatedBy = strings.TrimSpace(session.CreatedBy)

	if session.ID == "" {
		return errors.New("bootstrap session id is required")
	}
	if session.ProjectID == "" {
		return errors.New("project id is required")
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = time.Now().UTC()
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = session.UpdatedAt
	}

	if session.Data == nil {
		session.Data = map[string]any{}
	}
	payload, err := json.Marshal(session.Data)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
INSERT INTO bootstrap_sessions (id, project_id, current_step, status, created_by, data, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
ON CONFLICT (id) DO UPDATE SET
	project_id = EXCLUDED.project_id,
	current_step = EXCLUDED.current_step,
	status = EXCLUDED.status,
	created_by = EXCLUDED.created_by,
	data = EXCLUDED.data,
	updated_at = EXCLUDED.updated_at`,
		session.ID,
		session.ProjectID,
		session.CurrentStep,
		string(session.Status),
		session.CreatedBy,
		string(payload),
		session.CreatedAt,
		session.UpdatedAt)
	return err
}

func (s *SQLBootstrapSessionStore) ClaimBootstrapToken(request BootstrapTokenClaimRequest) (domain.BootstrapSession, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	defer tx.Rollback()

	row := tx.QueryRow(`
SELECT id, project_id, current_step, status, created_by, data, created_at, updated_at
FROM bootstrap_sessions
WHERE project_id = $1
ORDER BY updated_at DESC
LIMIT 1
FOR UPDATE`, strings.TrimSpace(request.ProjectID))
	session, err := s.scanSession(row)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	updated, err := ApplyBootstrapTokenClaim(session, request)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	payload, err := json.Marshal(updated.Data)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	if _, err := tx.Exec(`
UPDATE bootstrap_sessions
SET data = $2::jsonb, updated_at = $3
WHERE id = $1`,
		updated.ID,
		string(payload),
		updated.UpdatedAt); err != nil {
		return domain.BootstrapSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.BootstrapSession{}, err
	}
	return updated, nil
}

func (s *SQLBootstrapSessionStore) scanByQuery(query string, args ...any) (domain.BootstrapSession, error) {
	row := s.db.QueryRow(query, args...)
	return s.scanSession(row)
}

func (s *SQLBootstrapSessionStore) scanSession(scanner interface{ Scan(dest ...any) error }) (domain.BootstrapSession, error) {
	var (
		session domain.BootstrapSession
		payload []byte
	)
	err := scanner.Scan(
		&session.ID,
		&session.ProjectID,
		&session.CurrentStep,
		&session.Status,
		&session.CreatedBy,
		&payload,
		&session.CreatedAt,
		&session.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.BootstrapSession{}, ErrBootstrapSessionNotFound
		}
		return domain.BootstrapSession{}, err
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &session.Data); err != nil {
			return domain.BootstrapSession{}, err
		}
	}
	if session.Data == nil {
		session.Data = map[string]any{}
	}
	return session, nil
}
