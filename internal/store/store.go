package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/envpilot/contracts/domain"
)

var ErrNotFound = errors.New("environment not found")
var ErrBootstrapSessionNotFound = errors.New("bootstrap session not found")
var ErrProjectConfigNotFound = errors.New("project config not found")
var ErrBootstrapTokenAlreadyUsed = errors.New("bootstrap token already used")
var ErrBootstrapTokenInvalid = errors.New("invalid bootstrap token")
var ErrBootstrapTokenExpired = errors.New("bootstrap token expired")
var ErrBootstrapIdentityMismatch = errors.New("bootstrap identity mismatch")

type EnvironmentStore interface {
	List() ([]domain.Environment, error)
	ListRecords() ([]domain.EnvironmentRecord, error)
	Get(id string) (domain.Environment, error)
	GetRecord(id string) (domain.EnvironmentRecord, error)
	Save(environment domain.Environment) error
	Delete(id string) error
}

type BootstrapSessionStore interface {
	GetByProject(projectID string) (domain.BootstrapSession, error)
	Save(session domain.BootstrapSession) error
	ClaimBootstrapToken(request BootstrapTokenClaimRequest) (domain.BootstrapSession, error)
}

type BootstrapTokenClaimRequest struct {
	ProjectID       string
	TokenProjectKey string
	TokenHashKey    string
	TokenHash       string
	TokenUsedAtKey  string
	TokenExpiresKey string
	Identity        map[string]string
	StepData        map[string]any
	Now             time.Time
}

type ProjectConfigStore interface {
	Latest(projectID string) (domain.ProjectConfig, error)
	Save(config domain.ProjectConfig) error
}

type JSONStore struct {
	path string
	mu   sync.RWMutex
	data map[string]domain.EnvironmentRecord
}

func NewJSONStore(path string) (*JSONStore, error) {
	store := &JSONStore{
		path: path,
		data: map[string]domain.EnvironmentRecord{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *JSONStore) List() ([]domain.Environment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]domain.Environment, 0, len(s.data))
	for _, item := range s.data {
		items = append(items, item.Environment())
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return items, nil
}

func (s *JSONStore) ListRecords() ([]domain.EnvironmentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]domain.EnvironmentRecord, 0, len(s.data))
	for _, item := range s.data {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return items, nil
}

func (s *JSONStore) Get(id string) (domain.Environment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.data[id]
	if !ok {
		return domain.Environment{}, ErrNotFound
	}
	return item.Environment(), nil
}

func (s *JSONStore) GetRecord(id string) (domain.EnvironmentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.data[id]
	if !ok {
		return domain.EnvironmentRecord{}, ErrNotFound
	}
	return item, nil
}

func (s *JSONStore) Save(environment domain.Environment) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data[environment.ID] = domain.NewEnvironmentRecord(environment)
	return s.persistLocked()
}

func (s *JSONStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.data[id]; !ok {
		return ErrNotFound
	}
	delete(s.data, id)
	return s.persistLocked()
}

func (s *JSONStore) load() error {
	content, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return nil
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(content, &raw); err != nil {
		return err
	}
	for id, item := range raw {
		if bytes.Contains(item, []byte(`"project_id"`)) || bytes.Contains(item, []byte(`"payload"`)) {
			var record domain.EnvironmentRecord
			if err := json.Unmarshal(item, &record); err != nil {
				return err
			}
			if record.ID == "" {
				record.ID = id
			}
			if record.Payload.ID == "" {
				record.Payload = record.Environment()
			}
			s.data[record.ID] = record
			continue
		}

		var environment domain.Environment
		if err := json.Unmarshal(item, &environment); err != nil {
			return err
		}
		if environment.ID == "" {
			environment.ID = id
		}
		s.data[environment.ID] = domain.NewEnvironmentRecord(environment)
	}
	return nil
}

func (s *JSONStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, content, 0o600)
}
