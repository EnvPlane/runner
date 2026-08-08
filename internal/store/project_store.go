package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/envpilot/contracts/domain"
)

var ErrProjectNotFound = errors.New("project not found")

type ProjectStore interface {
	List() ([]domain.Project, error)
	Get(id string) (domain.Project, error)
	Save(project domain.Project) error
}

type JSONProjectStore struct {
	path string
	mu   sync.RWMutex
	data map[string]domain.Project
}

func NewJSONProjectStore(path string, defaults []domain.Project) (*JSONProjectStore, error) {
	store := &JSONProjectStore{
		path: path,
		data: map[string]domain.Project{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	if len(store.data) == 0 {
		for _, project := range defaults {
			store.data[project.ID] = project
		}
		if len(defaults) > 0 {
			if err := store.persistLocked(); err != nil {
				return nil, err
			}
		}
	}
	return store, nil
}

func (s *JSONProjectStore) List() ([]domain.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]domain.Project, 0, len(s.data))
	for _, item := range s.data {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func (s *JSONProjectStore) Get(id string) (domain.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.data[id]
	if !ok {
		return domain.Project{}, ErrProjectNotFound
	}
	return item, nil
}

func (s *JSONProjectStore) Save(project domain.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data[project.ID] = project
	return s.persistLocked()
}

func (s *JSONProjectStore) load() error {
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
	return json.Unmarshal(content, &s.data)
}

func (s *JSONProjectStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, content, 0o600)
}
