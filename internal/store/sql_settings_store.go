package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"envpilot/internal/domain"
)

type SQLSettingsStore struct {
	db *sql.DB
}

func NewSQLSettingsStore(db *sql.DB, defaults domain.ControlPlaneSettings) (*SQLSettingsStore, error) {
	store := &SQLSettingsStore{db: db}
	_, err := store.Get()
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if defaults.SchemaVersion == "" {
			defaults.SchemaVersion = "v1"
		}
		if defaults.UpdatedAt.IsZero() {
			defaults.UpdatedAt = time.Now().UTC()
		}
		if err := store.Save(defaults); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *SQLSettingsStore) Get() (domain.ControlPlaneSettings, error) {
	row := s.db.QueryRow(`
SELECT id, payload, updated_at
FROM control_plane_settings
WHERE id = $1`, "default")

	var id string
	var payload []byte
	var updatedAt time.Time
	var settings domain.ControlPlaneSettings
	if err := row.Scan(&id, &payload, &updatedAt); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	_ = id
	if err := json.Unmarshal(payload, &settings); err != nil {
		return domain.ControlPlaneSettings{}, err
	}
	settings.UpdatedAt = updatedAt
	return settings, nil
}

func (s *SQLSettingsStore) Save(settings domain.ControlPlaneSettings) error {
	payload, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if settings.UpdatedAt.IsZero() {
		settings.UpdatedAt = time.Now().UTC()
	}

	_, err = s.db.Exec(`
INSERT INTO control_plane_settings (id, payload, updated_at)
VALUES ('default', $1::jsonb, $2)
ON CONFLICT (id) DO UPDATE SET
	payload = EXCLUDED.payload,
	updated_at = EXCLUDED.updated_at`,
		string(payload),
		settings.UpdatedAt)
	return err
}
