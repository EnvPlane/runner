package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/envpilot/contracts/domain"
)

type SettingsStore interface {
	Get() (domain.ControlPlaneSettings, error)
	Save(settings domain.ControlPlaneSettings) error
}

type JSONSettingsStore struct {
	path string
	mu   sync.RWMutex
	data domain.ControlPlaneSettings
}

func NewJSONSettingsStore(path string, defaults domain.ControlPlaneSettings) (*JSONSettingsStore, error) {
	store := &JSONSettingsStore{path: path, data: defaults}
	if err := store.load(); err != nil {
		return nil, err
	}
	if store.data.SchemaVersion == "" {
		store.data = defaults
		if err := store.persistLocked(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *JSONSettingsStore) Get() (domain.ControlPlaneSettings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data, nil
}

func (s *JSONSettingsStore) Save(settings domain.ControlPlaneSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = settings
	return s.persistLocked()
}

func (s *JSONSettingsStore) load() error {
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

func (s *JSONSettingsStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, content, 0o600)
}
