package domain

import "time"

type BootstrapSessionStatus string

const (
	BootstrapSessionStatusDraft    BootstrapSessionStatus = "draft"
	BootstrapSessionStatusScanning BootstrapSessionStatus = "scanning"
	BootstrapSessionStatusReviewed BootstrapSessionStatus = "reviewed"
	BootstrapSessionStatusCompiled BootstrapSessionStatus = "compiled"
	BootstrapSessionStatusDeployed BootstrapSessionStatus = "deployed"
)

type BootstrapSession struct {
	ID          string               `json:"id" db:"id"`
	ProjectID   string               `json:"project_id" db:"project_id"`
	CurrentStep int                  `json:"current_step" db:"current_step"`
	Status      BootstrapSessionStatus `json:"status" db:"status"`
	CreatedBy   string               `json:"created_by" db:"created_by"`
	Data        map[string]any       `json:"data,omitempty" db:"data"`
	CreatedAt   time.Time            `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at" db:"updated_at"`
}
