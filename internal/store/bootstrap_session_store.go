package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/envpilot/contracts/domain"
)

type JSONBootstrapSessionStore struct {
	path string
	mu   sync.RWMutex
	data map[string]domain.BootstrapSession
}

func NewJSONBootstrapSessionStore(path string) (*JSONBootstrapSessionStore, error) {
	store := &JSONBootstrapSessionStore{
		path: path,
		data: map[string]domain.BootstrapSession{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *JSONBootstrapSessionStore) GetByProject(projectID string) (domain.BootstrapSession, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.BootstrapSession{}, ErrBootstrapSessionNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]domain.BootstrapSession, 0, len(s.data))
	for _, session := range s.data {
		if strings.EqualFold(strings.TrimSpace(session.ProjectID), projectID) {
			items = append(items, session)
		}
	}
	if len(items) == 0 {
		return domain.BootstrapSession{}, ErrBootstrapSessionNotFound
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].UpdatedAt.IsZero() && !items[j].UpdatedAt.IsZero() {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		if !items[i].UpdatedAt.IsZero() {
			return true
		}
		if !items[j].UpdatedAt.IsZero() {
			return false
		}
		if items[i].Status == items[j].Status {
			return items[i].CurrentStep > items[j].CurrentStep
		}
		return strings.Compare(string(items[i].ID), string(items[j].ID)) > 0
	})
	return items[0], nil
}

func (s *JSONBootstrapSessionStore) Save(session domain.BootstrapSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	session.ProjectID = strings.TrimSpace(session.ProjectID)
	session.ID = strings.TrimSpace(session.ID)
	session.CreatedBy = strings.TrimSpace(session.CreatedBy)

	if session.ID == "" {
		return errors.New("bootstrap session id is required")
	}
	if session.ProjectID == "" {
		return errors.New("project id is required")
	}
	if session.Data == nil {
		session.Data = map[string]any{}
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = time.Now().UTC()
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = session.UpdatedAt
	}
	s.data[session.ID] = session
	return s.persistLocked()
}

func (s *JSONBootstrapSessionStore) ClaimBootstrapToken(request BootstrapTokenClaimRequest) (domain.BootstrapSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, err := s.getByProjectLocked(request.ProjectID)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	updated, err := ApplyBootstrapTokenClaim(session, request)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	previous := s.data[updated.ID]
	s.data[updated.ID] = updated
	if err := s.persistLocked(); err != nil {
		s.data[updated.ID] = previous
		return domain.BootstrapSession{}, err
	}
	return updated, nil
}

func (s *JSONBootstrapSessionStore) getByProjectLocked(projectID string) (domain.BootstrapSession, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.BootstrapSession{}, ErrBootstrapSessionNotFound
	}
	items := make([]domain.BootstrapSession, 0, len(s.data))
	for _, session := range s.data {
		if strings.EqualFold(strings.TrimSpace(session.ProjectID), projectID) {
			items = append(items, session)
		}
	}
	if len(items) == 0 {
		return domain.BootstrapSession{}, ErrBootstrapSessionNotFound
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].UpdatedAt.IsZero() && !items[j].UpdatedAt.IsZero() {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		if !items[i].UpdatedAt.IsZero() {
			return true
		}
		if !items[j].UpdatedAt.IsZero() {
			return false
		}
		if items[i].Status == items[j].Status {
			return items[i].CurrentStep > items[j].CurrentStep
		}
		return strings.Compare(string(items[i].ID), string(items[j].ID)) > 0
	})
	return items[0], nil
}

func (s *JSONBootstrapSessionStore) load() error {
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

	var sessions map[string]domain.BootstrapSession
	if err := json.Unmarshal(content, &sessions); err != nil {
		return err
	}
	if sessions == nil {
		if s.data == nil {
			s.data = map[string]domain.BootstrapSession{}
		}
		return nil
	}
	if s.data == nil {
		s.data = map[string]domain.BootstrapSession{}
	}
	for id, session := range sessions {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if strings.TrimSpace(session.ID) == "" {
			session.ID = id
		}
		if session.Data == nil {
			session.Data = map[string]any{}
		}
		s.data[session.ID] = session
	}
	return nil
}

func (s *JSONBootstrapSessionStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, content, 0o600)
}
