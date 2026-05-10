package domain

import (
	"strings"
	"time"
)

type ClusterCapabilityReport struct {
	KubernetesVersion  string   `json:"kubernetesVersion,omitempty"`
	Namespaces         []string `json:"namespaces,omitempty"`
	IngressControllers []string `json:"ingressControllers,omitempty"`
	FluxCRDs           []string `json:"fluxCRDs,omitempty"`
	CertManagerCRDs    []string `json:"certManagerCRDs,omitempty"`
	ExternalDNSPresent bool     `json:"externalDNSPresent,omitempty"`
	StorageClasses     []string `json:"storageClasses,omitempty"`
	PermissionWarnings []string `json:"permissionWarnings,omitempty"`
	CapabilityFlags    []string `json:"capabilityFlags,omitempty"`
}

type ResourceSnapshot struct {
	Kind            string                   `json:"kind"`
	Namespace       string                   `json:"namespace"`
	Name            string                   `json:"name"`
	Labels          map[string]string        `json:"labels,omitempty"`
	Annotations     map[string]string        `json:"annotations,omitempty"`
	Manifest        map[string]any           `json:"manifest,omitempty"`
	OwnerReferences []ResourceOwnerReference `json:"ownerReferences,omitempty"`
	Selector        map[string]string        `json:"selector,omitempty"`
	PodLabels       map[string]string        `json:"podLabels,omitempty"`
	EnvVars         []ResourceEnvVar         `json:"envVars,omitempty"`
	EnvFrom         []ResourceEnvFromRef     `json:"envFrom,omitempty"`
	Containers      []ResourceContainerEnv   `json:"containers,omitempty"`
	ConfigMapKeys   []string                 `json:"configMapKeys,omitempty"`
	IngressRules    []ResourceIngressRule    `json:"ingressRules,omitempty"`
	SourceMapping   *ResourceSourceMapping   `json:"sourceMapping,omitempty"`
}

type ResourceOwnerReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

type ResourceEnvVar struct {
	Name           string `json:"name"`
	Value          string `json:"value,omitempty"`
	ValueFrom      string `json:"valueFrom,omitempty"`
	ValueFromKind  string `json:"valueFromKind,omitempty"`
	ValueFromName  string `json:"valueFromName,omitempty"`
	ValueFromKey   string `json:"valueFromKey,omitempty"`
	ValueFromField string `json:"valueFromField,omitempty"`
	ValueFromPath  string `json:"valueFromPath,omitempty"`
	SourceType     string `json:"sourceType,omitempty"`
}

type ResourceContainerEnv struct {
	Name    string               `json:"name"`
	EnvVars []ResourceEnvVar     `json:"envVars,omitempty"`
	EnvFrom []ResourceEnvFromRef `json:"envFrom,omitempty"`
}

type ResourceEnvFromRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	SourceType string `json:"sourceType,omitempty"`
}

type ResourceIngressRule struct {
	Host        string `json:"host,omitempty"`
	Path        string `json:"path,omitempty"`
	ServiceName string `json:"serviceName"`
	ServicePort string `json:"servicePort,omitempty"`
}

type ResourceSourceMapping struct {
	Status                 string `json:"status"`
	Kind                   string `json:"kind,omitempty"`
	Namespace              string `json:"namespace,omitempty"`
	Name                   string `json:"name,omitempty"`
	GitRepositoryNamespace string `json:"gitRepositoryNamespace,omitempty"`
	GitRepositoryName      string `json:"gitRepositoryName,omitempty"`
	Reason                 string `json:"reason,omitempty"`
}

type ServiceGraph struct {
	Nodes []ServiceGraphNode `json:"nodes"`
	Edges []ServiceGraphEdge `json:"edges"`
}

type ServiceEnvironmentVariables struct {
	Services []ServiceEnvironmentGroup `json:"services"`
}

type ServiceEnvironmentGroup struct {
	ServiceID   string                   `json:"serviceId"`
	ServiceName string                   `json:"serviceName"`
	Namespace   string                   `json:"namespace"`
	Containers  []ServiceContainerEnvSet `json:"containers"`
}

type ServiceContainerEnvSet struct {
	Container string               `json:"container"`
	Vars      []ResourceEnvVar     `json:"vars,omitempty"`
	EnvFrom   []ResourceEnvFromRef `json:"envFrom,omitempty"`
}

type ServiceGraphNode struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type ServiceGraphEdge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Type       string  `json:"type"`
	Reason     string  `json:"reason,omitempty"`
	Confidence float64 `json:"confidence"`
}

type AgentRegistrationTokenRequest struct {
	ClusterID      string `json:"clusterId,omitempty"`
	ClusterIDSnake string `json:"cluster_id,omitempty"`
	AgentNamespace string `json:"agentNamespace,omitempty"`
	ReleaseName    string `json:"releaseName,omitempty"`
}

type AgentRegistrationTokenResponse struct {
	ProjectID                       string    `json:"projectId"`
	ClusterID                       string    `json:"clusterId"`
	AgentNamespace                  string    `json:"agentNamespace"`
	ReleaseName                     string    `json:"releaseName"`
	RegistrationToken               string    `json:"registrationToken"`
	ExpiresAt                       time.Time `json:"expiresAt"`
	HelmCommand                     string    `json:"helmCommand"`
	BootstrapSecretCommand          string    `json:"bootstrapSecretCommand,omitempty"`
	BootstrapSecretCommandSensitive bool      `json:"bootstrapSecretCommandSensitive,omitempty"`
	Status                          string    `json:"status"`
}

type BootstrapAgentStatusResponse struct {
	Status             string                   `json:"status"`
	ClusterID          string                   `json:"clusterId,omitempty"`
	AgentID            string                   `json:"agentId,omitempty"`
	LastSeenAt         *time.Time               `json:"lastSeenAt,omitempty"`
	TokenExpiresAt     *time.Time               `json:"tokenExpiresAt,omitempty"`
	TokenIssuedAt      *time.Time               `json:"tokenIssuedAt,omitempty"`
	CapabilityReport   *ClusterCapabilityReport `json:"capabilityReport,omitempty"`
	SelectedNamespaces []string                 `json:"selectedNamespaces,omitempty"`
	ResourceScanStatus string                   `json:"resourceScanStatus,omitempty"`
	ResourceCount      int                      `json:"resourceCount,omitempty"`
	Error              string                   `json:"error,omitempty"`
}

type AgentResourceScanRequest struct {
	ProjectID          string                      `json:"projectId"`
	ProjectIDSnake     string                      `json:"project_id,omitempty"`
	ClusterID          string                      `json:"clusterId"`
	ClusterIDSnake     string                      `json:"cluster_id,omitempty"`
	AgentID            string                      `json:"agentId"`
	ResourceSnapshots  []ResourceSnapshot          `json:"resourceSnapshots"`
	ServiceGraph       ServiceGraph                `json:"serviceGraph,omitempty"`
	ServiceEnvs        ServiceEnvironmentVariables `json:"serviceEnvs,omitempty"`
	PermissionWarnings []string                    `json:"permissionWarnings,omitempty"`
	ObservedAt         time.Time                   `json:"observedAt,omitempty"`
}

type AgentResourceScanTaskResponse struct {
	ProjectID  string    `json:"projectId"`
	ClusterID  string    `json:"clusterId"`
	AgentID    string    `json:"agentId"`
	Namespaces []string  `json:"namespaces"`
	ObservedAt time.Time `json:"observedAt"`
}

type RunnerDeploymentMode string

const (
	RunnerDeploymentModeHelm   RunnerDeploymentMode = "helm"
	RunnerDeploymentModeGitOps RunnerDeploymentMode = "gitops"
)

type RunnerDeploymentInstructionsRequest struct {
	ProjectID       string `json:"projectId,omitempty"`
	ClusterID       string `json:"clusterId"`
	ClusterIDSnake  string `json:"cluster_id,omitempty"`
	DeploymentMode  string `json:"deploymentMode"`
	RunnerNamespace string `json:"runnerNamespace"`
	ReleaseName     string `json:"releaseName"`
	GitOpsPath      string `json:"gitOpsPath,omitempty"`
	GitOpsPathSnake string `json:"git_ops_path,omitempty"`
}

type RunnerDeploymentInstructionsResponse struct {
	ProjectID                       string               `json:"projectId"`
	ClusterID                       string               `json:"clusterId"`
	DeploymentMode                  RunnerDeploymentMode `json:"deploymentMode"`
	RunnerNamespace                 string               `json:"runnerNamespace"`
	ReleaseName                     string               `json:"releaseName"`
	RegistrationToken               string               `json:"registrationToken"`
	ProjectConfigToken              string               `json:"projectConfigToken,omitempty"`
	ProjectConfigURL                string               `json:"projectConfigUrl"`
	ExpiresAt                       time.Time            `json:"expiresAt"`
	HelmCommand                     string               `json:"helmCommand,omitempty"`
	BootstrapSecretCommand          string               `json:"bootstrapSecretCommand,omitempty"`
	BootstrapSecretCommandSensitive bool                 `json:"bootstrapSecretCommandSensitive,omitempty"`
	GitOpsPath                      string               `json:"gitOpsPath,omitempty"`
	GitOpsManifest                  string               `json:"gitOpsManifest,omitempty"`
	Status                          string               `json:"status"`
}

// Backward-compatible aliases. Prefer RunnerDeploymentInstructionsRequest/Response.
type RunnerDeploymentRequest = RunnerDeploymentInstructionsRequest
type RunnerDeploymentResponse = RunnerDeploymentInstructionsResponse

type RunnerStatusResponse struct {
	Status           string     `json:"status"`
	DeploymentMode   string     `json:"deploymentMode"`
	ClusterID        string     `json:"clusterId,omitempty"`
	RunnerID         string     `json:"runnerId,omitempty"`
	RunnerNamespace  string     `json:"runnerNamespace,omitempty"`
	LastSeenAt       *time.Time `json:"lastSeenAt,omitempty"`
	TokenExpiresAt   *time.Time `json:"tokenExpiresAt,omitempty"`
	TokenIssuedAt    *time.Time `json:"tokenIssuedAt,omitempty"`
	Error            string     `json:"error,omitempty"`
	ProjectConfigURL string     `json:"projectConfigUrl,omitempty"`
}

type RunnerRegistrationRequest struct {
	ProjectID         string    `json:"projectId,omitempty"`
	ClusterID         string    `json:"clusterId"`
	RunnerID          string    `json:"runnerId"`
	DeploymentMode    string    `json:"deploymentMode,omitempty"`
	RunnerNamespace   string    `json:"runnerNamespace"`
	RegistrationToken string    `json:"registrationToken,omitempty"`
	RunnerVersion     string    `json:"runnerVersion,omitempty"`
	ObservedAt        time.Time `json:"observedAt,omitempty"`
}

type RunnerRegistrationResponse struct {
	Status          string `json:"status"`
	Registered      string `json:"registered"`
	ProjectID       string `json:"projectId"`
	RunnerID        string `json:"runnerId"`
	RunnerAuthToken string `json:"runnerAuthToken"`
}

type RunnerHeartbeatRequest struct {
	ProjectID       string    `json:"projectId,omitempty"`
	ClusterID       string    `json:"clusterId"`
	RunnerID        string    `json:"runnerId"`
	DeploymentMode  string    `json:"deploymentMode,omitempty"`
	RunnerNamespace string    `json:"runnerNamespace"`
	RunnerAuthToken string    `json:"runnerAuthToken,omitempty"`
	Status          string    `json:"status,omitempty"`
	Error           string    `json:"error,omitempty"`
	ObservedAt      time.Time `json:"observedAt,omitempty"`
}

type RunnerHeartbeatStatus string

const (
	RunnerHeartbeatStatusWaiting   RunnerHeartbeatStatus = "waiting"
	RunnerHeartbeatStatusConnected RunnerHeartbeatStatus = "connected"
	RunnerHeartbeatStatusOnline    RunnerHeartbeatStatus = "online"
	RunnerHeartbeatStatusFailed    RunnerHeartbeatStatus = "failed"
)

func ParseRunnerHeartbeatStatus(raw string) (RunnerHeartbeatStatus, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		return RunnerHeartbeatStatusConnected, true
	}
	switch RunnerHeartbeatStatus(normalized) {
	case RunnerHeartbeatStatusWaiting, RunnerHeartbeatStatusConnected, RunnerHeartbeatStatusOnline, RunnerHeartbeatStatusFailed:
		return RunnerHeartbeatStatus(normalized), true
	default:
		return "", false
	}
}
