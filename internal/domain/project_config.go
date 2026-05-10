package domain

import "time"

type ProjectConfig struct {
	ID            string         `json:"id" db:"id"`
	ProjectID     string         `json:"project_id" db:"project_id"`
	Version       int            `json:"version" db:"version"`
	Config        map[string]any `json:"config,omitempty" db:"config"`
	Sensitive     map[string]any `json:"sensitive,omitempty" db:"sensitive"`
	SensitiveRefs map[string]any `json:"sensitive_refs,omitempty"`
	CreatedBy     string         `json:"created_by" db:"created_by"`
	CreatedAt     time.Time      `json:"created_at" db:"created_at"`
}
