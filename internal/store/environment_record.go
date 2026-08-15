package store

import (
	"time"

	"github.com/envpilot/contracts/domain"
)

// EnvironmentRecord is the runner persistence projection and is intentionally local.
type EnvironmentRecord struct {
	TenantID  string                   `json:"tenant_id,omitempty"`
	ID        string                   `json:"id"`
	ProjectID string                   `json:"project_id"`
	PRID      string                   `json:"pr_id"`
	Branch    string                   `json:"branch"`
	CommitSHA string                   `json:"commit_sha"`
	Status    domain.EnvironmentStatus `json:"status"`
	Type      domain.EnvironmentMode   `json:"type"`
	TTL       int                      `json:"ttl"`
	CreatedAt time.Time                `json:"created_at"`
	UpdatedAt time.Time                `json:"updated_at"`
	Payload   domain.Environment       `json:"payload"`
}

func NewEnvironmentRecord(environment domain.Environment) EnvironmentRecord {
	return EnvironmentRecord{TenantID: environment.TenantID, ID: environment.ID, ProjectID: environment.Project, PRID: environment.Source.PullRequestID, Branch: environment.Source.Branch, CommitSHA: environment.Source.Commit, Status: environment.Status, Type: environment.Mode, TTL: environment.TTLHours, CreatedAt: environment.CreatedAt, UpdatedAt: environment.UpdatedAt, Payload: environment}
}

func (r EnvironmentRecord) Environment() domain.Environment {
	e := r.Payload
	if e.ID == "" {
		e.ID = r.ID
	}
	if e.TenantID == "" {
		e.TenantID = r.TenantID
	}
	if e.Project == "" {
		e.Project = r.ProjectID
	}
	if e.Source.PullRequestID == "" {
		e.Source.PullRequestID = r.PRID
	}
	if e.Source.Branch == "" {
		e.Source.Branch = r.Branch
	}
	if e.Source.Commit == "" {
		e.Source.Commit = r.CommitSHA
	}
	if e.Status == "" {
		e.Status = r.Status
	}
	if e.Mode == "" {
		e.Mode = r.Type
	}
	if e.TTLHours == 0 {
		e.TTLHours = r.TTL
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = r.CreatedAt
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = r.UpdatedAt
	}
	return e
}
