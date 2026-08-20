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

	"github.com/envplane/contracts/domain"
)

type JSONProjectConfigStore struct {
	path string
	mu   sync.RWMutex
	data map[string]domain.ProjectConfig
}

func NewJSONProjectConfigStore(path string) (*JSONProjectConfigStore, error) {
	store := &JSONProjectConfigStore{
		path: path,
		data: map[string]domain.ProjectConfig{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *JSONProjectConfigStore) Latest(projectID string) (domain.ProjectConfig, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.ProjectConfig{}, ErrProjectConfigNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]domain.ProjectConfig, 0)
	for _, item := range s.data {
		if item.ProjectID == projectID {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return domain.ProjectConfig{}, ErrProjectConfigNotFound
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Version == items[j].Version {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].Version > items[j].Version
	})
	return items[0], nil
}

func (s *JSONProjectConfigStore) Save(config domain.ProjectConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.data[config.ID] = config
	return s.persistLocked()
}

func (s *JSONProjectConfigStore) load() error {
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
	if err := json.Unmarshal(content, &s.data); err != nil {
		return err
	}
	if s.data == nil {
		s.data = map[string]domain.ProjectConfig{}
	}
	return nil
}

func (s *JSONProjectConfigStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, content, 0o600)
}
