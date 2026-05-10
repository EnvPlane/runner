package domain

import (
	"strings"
	"time"
)

type EnvironmentStatus string

const (
	StatusCreating            EnvironmentStatus = "creating"
	StatusReady               EnvironmentStatus = "ready"
	StatusFailed              EnvironmentStatus = "failed"
	StatusDeleteRequested     EnvironmentStatus = "delete_requested"
	StatusGitOpsDeletePending EnvironmentStatus = "gitops_delete_pending"
	StatusDeleteFailed        EnvironmentStatus = "delete_failed"
	StatusTerminating         EnvironmentStatus = "terminating"
	StatusTerminated          EnvironmentStatus = "terminated"
)

type EnvironmentMode string

const (
	ModeFull   EnvironmentMode = "full"
	ModeHybrid EnvironmentMode = "hybrid"
)

type Environment struct {
	ID                        string            `json:"id"`
	Project                   string            `json:"project"`
	Product                   string            `json:"product"`
	ClusterID                 string            `json:"clusterId,omitempty"`
	Namespace                 string            `json:"namespace"`
	Mode                      EnvironmentMode   `json:"mode"`
	Status                    EnvironmentStatus `json:"status"`
	Domain                    string            `json:"domain"`
	URL                       string            `json:"url"`
	Source                    SCMSource         `json:"source"`
	Base                      BaseEnvironment   `json:"base,omitempty"`
	GitOps                    GitOpsTarget      `json:"gitops"`
	Charts                    ChartVersions     `json:"charts"`
	Infrastructure            Infrastructure    `json:"infrastructure"`
	Services                  []ServiceOverride `json:"services"`
	Overrides                 map[string]string `json:"overrides,omitempty"`
	Pinned                    bool              `json:"pinned"`
	PinnedUntil               *time.Time        `json:"pinnedUntil,omitempty"`
	Idle                      bool              `json:"idle,omitempty"`
	TTLHours                  int               `json:"ttlHours"`
	CostEstimateDay           string            `json:"costEstimateDay,omitempty"`
	IdleSince                 *time.Time        `json:"idleSince,omitempty"`
	LastActivityAt            *time.Time        `json:"lastActivityAt,omitempty"`
	ExpiresAt                 *time.Time        `json:"expiresAt,omitempty"`
	CreatedAt                 time.Time         `json:"createdAt"`
	UpdatedAt                 time.Time         `json:"updatedAt"`
	ManifestPath              string            `json:"manifestPath"`
	NamespaceManifestPath     string            `json:"namespaceManifestPath,omitempty"`
	KustomizationManifestPath string            `json:"kustomizationManifestPath,omitempty"`
	Events                    []KubernetesEvent `json:"events,omitempty"`
	FluxStatus                *FluxStatus       `json:"fluxStatus,omitempty"`
	LastError                 string            `json:"lastError,omitempty"`
}

type EnvironmentRecord struct {
	ID        string            `json:"id" db:"id"`
	ProjectID string            `json:"project_id" db:"project_id"`
	PRID      string            `json:"pr_id" db:"pr_id"`
	Branch    string            `json:"branch" db:"branch"`
	CommitSHA string            `json:"commit_sha" db:"commit_sha"`
	Status    EnvironmentStatus `json:"status" db:"status"`
	Type      EnvironmentMode   `json:"type" db:"type"`
	TTL       int               `json:"ttl" db:"ttl"`
	CreatedAt time.Time         `json:"created_at" db:"created_at"`
	UpdatedAt time.Time         `json:"updated_at" db:"updated_at"`
	Payload   Environment       `json:"payload" db:"-"`
}

func NewEnvironmentRecord(environment Environment) EnvironmentRecord {
	return EnvironmentRecord{
		ID:        environment.ID,
		ProjectID: environment.Project,
		PRID:      environment.Source.PullRequestID,
		Branch:    environment.Source.Branch,
		CommitSHA: environment.Source.Commit,
		Status:    environment.Status,
		Type:      environment.Mode,
		TTL:       environment.TTLHours,
		CreatedAt: environment.CreatedAt,
		UpdatedAt: environment.UpdatedAt,
		Payload:   environment,
	}
}

func (r EnvironmentRecord) Environment() Environment {
	environment := r.Payload
	if environment.ID == "" {
		environment.ID = r.ID
	}
	if environment.Project == "" {
		environment.Project = r.ProjectID
	}
	if environment.Source.PullRequestID == "" {
		environment.Source.PullRequestID = r.PRID
	}
	if environment.Source.Branch == "" {
		environment.Source.Branch = r.Branch
	}
	if environment.Source.Commit == "" {
		environment.Source.Commit = r.CommitSHA
	}
	if environment.Status == "" {
		environment.Status = r.Status
	}
	if environment.Mode == "" {
		environment.Mode = r.Type
	}
	if environment.TTLHours == 0 {
		environment.TTLHours = r.TTL
	}
	if environment.CreatedAt.IsZero() {
		environment.CreatedAt = r.CreatedAt
	}
	if environment.UpdatedAt.IsZero() {
		environment.UpdatedAt = r.UpdatedAt
	}
	return environment
}

type SCMSource struct {
	Provider      string `json:"provider"`
	Repository    string `json:"repository"`
	PullRequestID string `json:"pullRequestId"`
	Branch        string `json:"branch"`
	Commit        string `json:"commit"`
	Author        string `json:"author"`
	URL           string `json:"url"`
}

type GitOpsTarget struct {
	Path            string `json:"path"`
	Renderer        string `json:"renderer,omitempty"`
	ValuesPath      string `json:"valuesPath,omitempty"`
	RawManifestPath string `json:"rawManifestPath,omitempty"`
	SourceRefName   string `json:"sourceRefName"`
	TargetNamespace string `json:"targetNamespace,omitempty"`
	HealthCheckName string `json:"healthCheckName"`
	RepositoryID    string `json:"repositoryId,omitempty"`
	BaseBranch      string `json:"baseBranch,omitempty"`
	Branch          string `json:"branch,omitempty"`
	BranchStrategy  string `json:"branchStrategy,omitempty"`
	PullRequestURL  string `json:"pullRequestUrl,omitempty"`
}

type BaseEnvironment struct {
	EnvironmentID string           `json:"environmentId"`
	Namespace     string           `json:"namespace"`
	Domain        string           `json:"domain,omitempty"`
	Services      []BaseServiceRef `json:"services,omitempty"`
}

type ChartVersions struct {
	App   string `json:"app"`
	Infra string `json:"infra"`
	Nginx string `json:"nginx"`
}

type Infrastructure struct {
	MySQL     bool   `json:"mysql"`
	Postgres  bool   `json:"postgres"`
	RabbitMQ  bool   `json:"rabbitmq"`
	Redis     bool   `json:"redis"`
	Memcached bool   `json:"memcached"`
	MongoDB   bool   `json:"mongodb"`
	Zone      string `json:"zone"`
	Capacity  string `json:"capacity"`
}

type ServiceOverride struct {
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	Replace bool   `json:"replace,omitempty"`
}

type CreateEnvironmentRequest struct {
	ID             string            `json:"id"`
	Project        string            `json:"project"`
	Product        string            `json:"product"`
	ClusterID      string            `json:"clusterId,omitempty"`
	Namespace      string            `json:"namespace"`
	Mode           EnvironmentMode   `json:"mode"`
	Domain         string            `json:"domain"`
	Source         SCMSource         `json:"source"`
	Base           BaseEnvironment   `json:"base,omitempty"`
	Charts         ChartVersions     `json:"charts"`
	Infrastructure Infrastructure    `json:"infrastructure"`
	Services       []ServiceOverride `json:"services"`
	Overrides      map[string]string `json:"overrides,omitempty"`
	TTLHours       int               `json:"ttlHours"`
	Pinned         bool              `json:"pinned"`
}

type RenderPreview struct {
	Environment Environment       `json:"environment"`
	Values      map[string]string `json:"values"`
	ValuesYAML  string            `json:"valuesYaml"`
	Manifests   []RenderOutput    `json:"manifests"`
}

type RenderOutput struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type UpdateEnvironmentStatusRequest struct {
	Status    EnvironmentStatus `json:"status"`
	Message   string            `json:"message"`
	ClusterID string            `json:"clusterId,omitempty"`
}

type KubernetesEvent struct {
	UID          string    `json:"uid"`
	Namespace    string    `json:"namespace"`
	Type         string    `json:"type"`
	Reason       string    `json:"reason"`
	Message      string    `json:"message"`
	InvolvedKind string    `json:"involvedKind"`
	InvolvedName string    `json:"involvedName"`
	Count        int32     `json:"count"`
	FirstSeen    time.Time `json:"firstSeen"`
	LastSeen     time.Time `json:"lastSeen"`
}

type IngestEnvironmentEventsRequest struct {
	ClusterID string            `json:"clusterId,omitempty"`
	Events    []KubernetesEvent `json:"events"`
}

type FluxStatus struct {
	Status         EnvironmentStatus    `json:"status"`
	Message        string               `json:"message"`
	Kustomizations []FluxResourceStatus `json:"kustomizations"`
	HelmReleases   []FluxResourceStatus `json:"helmReleases"`
	UpdatedAt      time.Time            `json:"updatedAt"`
}

type FluxResourceStatus struct {
	Kind                   string    `json:"kind"`
	Name                   string    `json:"name"`
	Namespace              string    `json:"namespace"`
	Ready                  bool      `json:"ready"`
	Failed                 bool      `json:"failed"`
	Reason                 string    `json:"reason"`
	Message                string    `json:"message"`
	ObservedGeneration     int64     `json:"observedGeneration"`
	LastAppliedRevision    string    `json:"lastAppliedRevision,omitempty"`
	LastAttemptedRevision  string    `json:"lastAttemptedRevision,omitempty"`
	LastHandledReconcileAt string    `json:"lastHandledReconcileAt,omitempty"`
	LastTransitionTime     time.Time `json:"lastTransitionTime"`
}

type IngestFluxStatusRequest struct {
	ClusterID  string     `json:"clusterId,omitempty"`
	FluxStatus FluxStatus `json:"fluxStatus"`
}

func (e Environment) ManifestFilename() string {
	switch rendererKindName(e.GitOps.Renderer) {
	case "helm":
		return e.GitOpsDirectory() + "/helm-release.yaml"
	case "raw":
		return e.GitOpsDirectory() + "/raw-manifests.yaml"
	case "kustomize-overlay":
		return e.GitOpsDirectory() + "/overlay/kustomization.yaml"
	}
	return e.GitOpsDirectory() + "/flux-kustomization.yaml"
}

func (e Environment) NamespaceManifestFilename() string {
	return e.GitOpsDirectory() + "/namespace.yaml"
}

func (e Environment) PathKustomizationFilename() string {
	return e.GitOpsDirectory() + "/kustomization.yaml"
}

func (e Environment) GitOpsDirectory() string {
	project := gitOpsPathSegment(e.Project)
	if project == "" {
		project = "default"
	}
	return "feature-envs/" + project + "/" + gitOpsPathSegment(e.ID)
}

func rendererKindName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "helm", "helm-release":
		return "helm"
	case "raw", "raw-manifests", "raw-manifest":
		return "raw"
	case "kustomize", "kustomize-overlay", "overlay":
		return "kustomize-overlay"
	default:
		return "flux-kustomization"
	}
}

func gitOpsPathSegment(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return ""
	}
	var builder strings.Builder
	lastDash := false
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			if c == '-' {
				if lastDash {
					continue
				}
				lastDash = true
			} else {
				lastDash = false
			}
			builder.WriteRune(c)
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}
