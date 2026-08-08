package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/envpilot/runner/internal/domain"
)

type SQLProjectStore struct {
	db *sql.DB
}

func NewSQLProjectStore(db *sql.DB, defaults []domain.Project) (*SQLProjectStore, error) {
	store := &SQLProjectStore{db: db}

	existing, err := store.isEmpty()
	if err != nil {
		return nil, err
	}
	if existing {
		for _, project := range defaults {
			if err := store.Save(project); err != nil {
				return nil, err
			}
		}
	}

	if store == nil {
		return nil, nil
	}
	return store, nil
}

func (s *SQLProjectStore) List() ([]domain.Project, error) {
	rows, err := s.db.Query(`
SELECT id, product_id, app_repository_id, gitops_repository_id, cluster_id, payload, created_at, updated_at
FROM projects
ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]domain.Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, project)
	}
	return items, rows.Err()
}

func (s *SQLProjectStore) Get(id string) (domain.Project, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.Project{}, ErrProjectNotFound
	}

	project, err := s.scanByQuery(`
SELECT id, product_id, app_repository_id, gitops_repository_id, cluster_id, payload, created_at, updated_at
FROM projects
WHERE id = $1`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Project{}, ErrProjectNotFound
		}
		return domain.Project{}, err
	}
	return project, nil
}

func (s *SQLProjectStore) Save(project domain.Project) error {
	project.ID = strings.TrimSpace(project.ID)
	if project.ID == "" {
		return errors.New("project id is required")
	}

	if project.CreatedAt.IsZero() {
		project.CreatedAt = time.Now().UTC()
	}
	if project.UpdatedAt.IsZero() {
		project.UpdatedAt = time.Now().UTC()
	}

	payload, err := json.Marshal(project)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
INSERT INTO projects (id, product_id, app_repository_id, gitops_repository_id, cluster_id, payload, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)
ON CONFLICT (id) DO UPDATE SET
	product_id = EXCLUDED.product_id,
	app_repository_id = EXCLUDED.app_repository_id,
	gitops_repository_id = EXCLUDED.gitops_repository_id,
	cluster_id = EXCLUDED.cluster_id,
	payload = EXCLUDED.payload,
	updated_at = EXCLUDED.updated_at`,
		project.ID,
		project.ProductID,
		project.AppRepositoryID,
		project.GitOpsRepositoryID,
		project.ClusterID,
		string(payload),
		project.CreatedAt,
		project.UpdatedAt)
	return err
}

func (s *SQLProjectStore) isEmpty() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects LIMIT 1)`).Scan(&exists)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

func (s *SQLProjectStore) scanByQuery(query string, args ...any) (domain.Project, error) {
	row := s.db.QueryRow(query, args...)
	return scanProject(row)
}

func scanProject(scanner rowScanner) (domain.Project, error) {
	var (
		project    domain.Project
		id         string
		payload    []byte
		productID  string
		appRepoID  string
		gitOpsRepo string
		clusterID  string
		createdAt  time.Time
		updatedAt  time.Time
	)
	err := scanner.Scan(&id, &productID, &appRepoID, &gitOpsRepo, &clusterID, &payload, &createdAt, &updatedAt)
	if err != nil {
		return domain.Project{}, err
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &project); err != nil {
			return domain.Project{}, err
		}
	}
	if project.ID == "" {
		project.ID = id
	}
	if project.ProductID == "" {
		project.ProductID = productID
	}
	if project.AppRepositoryID == "" {
		project.AppRepositoryID = appRepoID
	}
	if project.GitOpsRepositoryID == "" {
		project.GitOpsRepositoryID = gitOpsRepo
	}
	if project.ClusterID == "" {
		project.ClusterID = clusterID
	}
	if project.CreatedAt.IsZero() {
		project.CreatedAt = createdAt
	}
	if project.UpdatedAt.IsZero() {
		project.UpdatedAt = updatedAt
	}
	return project, nil
}
