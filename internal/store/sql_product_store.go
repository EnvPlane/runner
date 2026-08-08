package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/envpilot/runner/internal/domain"
)

type SQLProductStore struct {
	db *sql.DB
}

func NewSQLProductStore(db *sql.DB, defaults []domain.ProductTemplate) (*SQLProductStore, error) {
	store := &SQLProductStore{db: db}

	empty, err := store.isEmpty()
	if err != nil {
		return nil, err
	}
	if empty {
		for _, product := range defaults {
			product.Name = normalizeProductName(product.Name)
			if product.Name == "" {
				continue
			}
			if strings.TrimSpace(product.ManifestSourceID) == "" && strings.TrimSpace(product.BasePath) == "" {
				product.BasePath = "common/apps/" + product.Name
			}
			if err := store.Save(product); err != nil {
				return nil, err
			}
		}
	}

	return store, nil
}

func (s *SQLProductStore) List() ([]domain.ProductTemplate, error) {
	rows, err := s.db.Query(`
SELECT name, manifest_source_id, payload, updated_at
FROM products
ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]domain.ProductTemplate, 0)
	for rows.Next() {
		product, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, product)
	}
	return items, rows.Err()
}

func (s *SQLProductStore) Get(name string) (domain.ProductTemplate, error) {
	product, err := s.scanByQuery(`
SELECT name, manifest_source_id, payload, updated_at
FROM products
WHERE name = $1`, normalizeProductName(name))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ProductTemplate{}, ErrProductNotFound
		}
		return domain.ProductTemplate{}, err
	}
	return product, nil
}

func (s *SQLProductStore) Save(product domain.ProductTemplate) error {
	product.Name = normalizeProductName(product.Name)
	if product.Name == "" {
		return errors.New("product name is required")
	}
	if product.ManifestSourceID == "" && strings.TrimSpace(product.BasePath) == "" {
		return errors.New("manifestSourceId or basePath is required")
	}

	payload, err := json.Marshal(product)
	if err != nil {
		return err
	}

	updatedAt := time.Now().UTC()
	_, err = s.db.Exec(`
INSERT INTO products (name, manifest_source_id, payload, updated_at)
VALUES ($1, $2, $3::jsonb, $4)
ON CONFLICT (name) DO UPDATE SET
	manifest_source_id = EXCLUDED.manifest_source_id,
	payload = EXCLUDED.payload,
	updated_at = EXCLUDED.updated_at`,
		product.Name,
		product.ManifestSourceID,
		string(payload),
		updatedAt,
	)
	return err
}

func (s *SQLProductStore) Delete(name string) error {
	_, err := s.db.Exec(`DELETE FROM products WHERE name = $1`, normalizeProductName(name))
	return err
}

func (s *SQLProductStore) isEmpty() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM products LIMIT 1)`).Scan(&exists)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

func (s *SQLProductStore) scanByQuery(query string, args ...any) (domain.ProductTemplate, error) {
	row := s.db.QueryRow(query, args...)
	return scanProduct(row)
}

func scanProduct(scanner rowScanner) (domain.ProductTemplate, error) {
	var (
		product     domain.ProductTemplate
		name        string
		manifestRef string
		payload     []byte
	)
	var updatedAt time.Time
	err := scanner.Scan(&name, &manifestRef, &payload, &updatedAt)
	if err != nil {
		return domain.ProductTemplate{}, err
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &product); err != nil {
			return domain.ProductTemplate{}, err
		}
	}
	if product.Name == "" {
		product.Name = name
	}
	if product.ManifestSourceID == "" {
		product.ManifestSourceID = manifestRef
	}
	return product, nil
}
