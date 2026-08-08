package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/envpilot/contracts/domain"
)

var ErrNotFound = errors.New("job not found")

type Store interface {
	List() ([]Job, error)
	Get(id string) (Job, error)
	Save(job Job) error
}

type MemoryStore struct {
	mu   sync.RWMutex
	jobs map[string]Job
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: map[string]Job{}}
}

func (s *MemoryStore) List() ([]Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		items = append(items, job)
	}
	sortJobs(items)
	return items, nil
}

func (s *MemoryStore) Get(id string) (Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return job, nil
}

func (s *MemoryStore) Save(job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
	return nil
}

type SQLStore struct {
	db *sql.DB
}

func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) List() ([]Job, error) {
	rows, err := s.db.Query(`
SELECT id, type, status, environment_id, event, request, result, error, attempts, max_attempts, next_run_at, created_at, started_at, completed_at
FROM jobs
ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, job)
	}
	return items, rows.Err()
}

func (s *SQLStore) Get(id string) (Job, error) {
	row := s.db.QueryRow(`
SELECT id, type, status, environment_id, event, request, result, error, attempts, max_attempts, next_run_at, created_at, started_at, completed_at
FROM jobs
WHERE id = $1`, id)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

func (s *SQLStore) Save(job Job) error {
	eventJSON, err := json.Marshal(job.Event)
	if err != nil {
		return err
	}
	requestJSON, err := json.Marshal(job.Request)
	if err != nil {
		return err
	}
	var result any
	if job.Result != nil {
		content, err := json.Marshal(job.Result)
		if err != nil {
			return err
		}
		result = string(content)
	}
	_, err = s.db.Exec(`
INSERT INTO jobs (id, type, status, environment_id, event, request, result, error, attempts, max_attempts, next_run_at, created_at, started_at, completed_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7::jsonb, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (id) DO UPDATE SET
	type = EXCLUDED.type,
	status = EXCLUDED.status,
	environment_id = EXCLUDED.environment_id,
	event = EXCLUDED.event,
	request = EXCLUDED.request,
	result = EXCLUDED.result,
	error = EXCLUDED.error,
	attempts = EXCLUDED.attempts,
	max_attempts = EXCLUDED.max_attempts,
	next_run_at = EXCLUDED.next_run_at,
	created_at = EXCLUDED.created_at,
	started_at = EXCLUDED.started_at,
	completed_at = EXCLUDED.completed_at`,
		job.ID, job.Type, job.Status, job.EnvironmentID, string(eventJSON), string(requestJSON), result, job.Error, job.Attempts, job.MaxAttempts, job.NextRunAt, job.CreatedAt, job.StartedAt, job.CompletedAt)
	return err
}

type jobScanner interface {
	Scan(dest ...any) error
}

func scanJob(scanner jobScanner) (Job, error) {
	var job Job
	var eventJSON []byte
	var requestJSON []byte
	var resultJSON sql.NullString
	var nextRunAt sql.NullTime
	var startedAt sql.NullTime
	var completedAt sql.NullTime
	err := scanner.Scan(&job.ID, &job.Type, &job.Status, &job.EnvironmentID, &eventJSON, &requestJSON, &resultJSON, &job.Error, &job.Attempts, &job.MaxAttempts, &nextRunAt, &job.CreatedAt, &startedAt, &completedAt)
	if err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal(eventJSON, &job.Event); err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal(requestJSON, &job.Request); err != nil {
		return Job{}, err
	}
	if resultJSON.Valid && strings.TrimSpace(resultJSON.String) != "" {
		var result domain.Environment
		if err := json.Unmarshal([]byte(resultJSON.String), &result); err != nil {
			return Job{}, err
		}
		job.Result = &result
	}
	if nextRunAt.Valid {
		job.NextRunAt = &nextRunAt.Time
	}
	if startedAt.Valid {
		job.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.Time
	}
	return job, nil
}

func sortJobs(items []Job) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
}

func maxNumericJobID(items []Job) int64 {
	var maxID int64
	for _, item := range items {
		raw := strings.TrimPrefix(item.ID, "job-")
		value, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && value > maxID {
			maxID = value
		}
	}
	return maxID
}

func isRecoverableStatus(status Status) bool {
	return status == StatusQueued || status == StatusRunning
}

func recoverableJob(job Job, now time.Time) Job {
	if job.Status == StatusRunning {
		job.Status = StatusQueued
		job.Error = "job recovered after worker restart"
		job.StartedAt = nil
		if job.NextRunAt == nil {
			job.NextRunAt = &now
		}
	}
	return job
}
