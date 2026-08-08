package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/envpilot/runner/internal/domain"
)

var ErrProductNotFound = errors.New("product template not found")

type ProductStore interface {
	List() ([]domain.ProductTemplate, error)
	Get(name string) (domain.ProductTemplate, error)
	Save(product domain.ProductTemplate) error
	Delete(name string) error
}

type JSONProductStore struct {
	path string
	mu   sync.RWMutex
	data map[string]domain.ProductTemplate
}

func NewJSONProductStore(path string, defaults []domain.ProductTemplate) (*JSONProductStore, error) {
	store := &JSONProductStore{
		path: path,
		data: map[string]domain.ProductTemplate{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	if len(store.data) == 0 {
		for _, product := range defaults {
			name := normalizeProductName(product.Name)
			if name == "" {
				continue
			}
			product.Name = name
			store.data[name] = product
		}
		if len(store.data) > 0 {
			if err := store.persistLocked(); err != nil {
				return nil, err
			}
		}
	}
	return store, nil
}

func (s *JSONProductStore) List() ([]domain.ProductTemplate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]domain.ProductTemplate, 0, len(s.data))
	for _, item := range s.data {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})
	return items, nil
}

func (s *JSONProductStore) Get(name string) (domain.ProductTemplate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	product, ok := s.data[normalizeProductName(name)]
	if !ok {
		return domain.ProductTemplate{}, ErrProductNotFound
	}
	return product, nil
}

func (s *JSONProductStore) Save(product domain.ProductTemplate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name := normalizeProductName(product.Name)
	if name == "" {
		return ErrProductNotFound
	}
	product.Name = name
	s.data[name] = product
	return s.persistLocked()
}

func (s *JSONProductStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = normalizeProductName(name)
	if _, ok := s.data[name]; !ok {
		return ErrProductNotFound
	}
	delete(s.data, name)
	return s.persistLocked()
}

func (s *JSONProductStore) load() error {
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
	raw := map[string]domain.ProductTemplate{}
	if err := json.Unmarshal(content, &raw); err != nil {
		return err
	}
	for key, product := range raw {
		name := normalizeProductName(product.Name)
		if name == "" {
			name = normalizeProductName(key)
		}
		if name == "" {
			continue
		}
		product.Name = name
		s.data[name] = product
	}
	return nil
}

func (s *JSONProductStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, content, 0o600)
}

func normalizeProductName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
