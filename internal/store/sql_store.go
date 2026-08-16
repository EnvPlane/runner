package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/envpilot/contracts/domain"
)

type SQLStore struct {
	db *sql.DB
}

func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) List() ([]domain.Environment, error) {
	rows, err := s.db.Query(`
SELECT id, project_id, pr_id, branch, commit_sha, status, type, ttl, payload, created_at, updated_at
FROM environments
ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.Environment
	for rows.Next() {
		record, err := scanEnvironmentRecord(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, record.Environment())
	}
	return items, rows.Err()
}

func (s *SQLStore) ListRecords() ([]EnvironmentRecord, error) {
	rows, err := s.db.Query(`
SELECT id, project_id, pr_id, branch, commit_sha, status, type, ttl, payload, created_at, updated_at
FROM environments
ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []EnvironmentRecord
	for rows.Next() {
		record, err := scanEnvironmentRecord(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, record)
	}
	return items, rows.Err()
}

func (s *SQLStore) Get(id string) (domain.Environment, error) {
	record, err := s.get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	return record.Environment(), nil
}

func (s *SQLStore) GetRecord(id string) (EnvironmentRecord, error) {
	return s.get(id)
}

func (s *SQLStore) Save(environment domain.Environment) error {
	if strings.TrimSpace(environment.ID) == "" {
		return errors.New("environment id is required")
	}

	record := NewEnvironmentRecord(environment)
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
		environment.CreatedAt = record.CreatedAt
		record.Payload = environment
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
		environment.UpdatedAt = record.UpdatedAt
		record.Payload = environment
	}

	record.Payload = environment
	payload, err := json.Marshal(record.Payload)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
INSERT INTO environments (id, project_id, pr_id, branch, commit_sha, status, type, ttl, payload, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11)
ON CONFLICT (id) DO UPDATE SET
	project_id = EXCLUDED.project_id,
	pr_id = EXCLUDED.pr_id,
	branch = EXCLUDED.branch,
	commit_sha = EXCLUDED.commit_sha,
	status = EXCLUDED.status,
	type = EXCLUDED.type,
	ttl = EXCLUDED.ttl,
	payload = EXCLUDED.payload,
	updated_at = EXCLUDED.updated_at`,
		record.ID, record.ProjectID, record.PRID, record.Branch, record.CommitSHA,
		record.Status, record.Type, record.TTL, string(payload), record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return err
	}
	return nil
}

func (s *SQLStore) Delete(id string) error {
	result, err := s.db.Exec(`DELETE FROM environments WHERE id = $1`, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) get(id string) (EnvironmentRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return EnvironmentRecord{}, ErrNotFound
	}

	record, err := s.scanByQuery(`
SELECT id, project_id, pr_id, branch, commit_sha, status, type, ttl, payload, created_at, updated_at
FROM environments
WHERE id = $1`, id)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	return record, nil
}

func (s *SQLStore) scanByQuery(query string, args ...any) (EnvironmentRecord, error) {
	row := s.db.QueryRow(query, args...)
	return scanEnvironmentRecord(row)
}

func scanEnvironmentRecord(scanner rowScanner) (EnvironmentRecord, error) {
	var (
		record    EnvironmentRecord
		payload   []byte
		createdAt time.Time
		updatedAt time.Time
	)
	err := scanner.Scan(&record.ID, &record.ProjectID, &record.PRID, &record.Branch, &record.CommitSHA,
		&record.Status, &record.Type, &record.TTL, &payload, &createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return EnvironmentRecord{}, ErrNotFound
		}
		return EnvironmentRecord{}, err
	}
	record.CreatedAt = createdAt
	record.UpdatedAt = updatedAt
	if len(payload) > 0 {
		var environment domain.Environment
		if err := json.Unmarshal(payload, &environment); err != nil {
			return EnvironmentRecord{}, err
		}
		record.Payload = environment
	}
	record.Payload = envFallbackRecord(record.Payload, record.ID, record.ProjectID, record.PRID, record.Branch, record.CommitSHA, record.Status, record.Type, record.TTL, record.CreatedAt, record.UpdatedAt)
	return record, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func envFallbackRecord(payload domain.Environment, id, projectID, prID, branch, commitSHA string, status domain.EnvironmentStatus, mode domain.EnvironmentMode, ttl int, createdAt, updatedAt time.Time) domain.Environment {
	if payload.ID == "" {
		payload.ID = id
	}
	if payload.Project == "" {
		payload.Project = projectID
	}
	if payload.Source.PullRequestID == "" {
		payload.Source.PullRequestID = prID
	}
	if payload.Source.Branch == "" {
		payload.Source.Branch = branch
	}
	if payload.Source.Commit == "" {
		payload.Source.Commit = commitSHA
	}
	if strings.TrimSpace(string(payload.Status)) == "" {
		payload.Status = status
	}
	if payload.Mode == "" {
		payload.Mode = mode
	}
	if payload.TTLHours == 0 {
		payload.TTLHours = ttl
	}
	if payload.CreatedAt.IsZero() {
		payload.CreatedAt = createdAt
	}
	if payload.UpdatedAt.IsZero() {
		payload.UpdatedAt = updatedAt
	}
	return payload
}
