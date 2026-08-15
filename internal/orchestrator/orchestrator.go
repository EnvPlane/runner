package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/envpilot/contracts/domain"
	"github.com/envpilot/runner/internal/gitops"
	"github.com/envpilot/runner/internal/store"
	"gopkg.in/yaml.v3"
)

type DeploymentBackendType string

const (
	DeploymentBackendHelmDirect     DeploymentBackendType = "helm_direct"
	DeploymentBackendFluxCD         DeploymentBackendType = "fluxcd"
	DeploymentBackendGitOpsManifest DeploymentBackendType = "gitops_manifest"
)

type Manifest struct {
	Path    string
	Kind    string
	Content []byte
}

type DeploymentBackend interface {
	Render(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) ([]Manifest, error)
	Apply(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) error
	Delete(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) error
	Status(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) (domain.EnvironmentStatus, error)
}

type deploymentBackendWithWriter interface {
	ApplyWithWriter(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig, writer gitops.Writer) (gitops.CommitResult, error)
	DeleteWithWriter(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig, writer gitops.Writer) (gitops.CommitResult, error)
}

type HelmUpgradeOptions struct {
	ReleaseName     string
	ChartRef        string
	ChartVersion    string
	Namespace       string
	ValuesFile      string
	CreateNamespace bool
	Wait            bool
	Timeout         int
}

type HelmExecutor interface {
	UpgradeInstall(ctx context.Context, options HelmUpgradeOptions) error
	Uninstall(ctx context.Context, options HelmUninstallOptions) error
	DeleteNamespace(ctx context.Context, namespace string) error
	Status(ctx context.Context, options HelmStatusOptions) (HelmStatus, error)
	IsNamespaceManaged(ctx context.Context, namespace, projectID, environmentID string) (bool, error)
	Readiness(ctx context.Context, options HelmReadinessOptions) (bool, error)
}

type HelmStatusOptions struct {
	ReleaseName string
	Namespace   string
}

type HelmStatus struct {
	Found      bool
	Status     string
	Chart      string
	AppVersion string
	Namespace  string
}

type kubernetesNamespace struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

type HelmReadinessOptions struct {
	Release   string
	Namespace string
}

type helmStatusOutput struct {
	Info struct {
		Status        string `json:"status"`
		Notes         string `json:"notes"`
		FirstDeployed string `json:"first_deployed"`
		LastDeployed  string `json:"last_deployed"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name       string `json:"name"`
			AppVersion string `json:"app_version"`
		} `json:"metadata"`
	} `json:"chart"`
	Namespace string `json:"namespace"`
}

type CLIHelmExecutor struct {
	runCommand func(context.Context, string, ...string) ([]byte, error)
}

func NewCLIHelmExecutor() *CLIHelmExecutor {
	return &CLIHelmExecutor{
		runCommand: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			return cmd.CombinedOutput()
		},
	}
}

func (e *CLIHelmExecutor) UpgradeInstall(ctx context.Context, options HelmUpgradeOptions) error {
	if strings.TrimSpace(options.ChartRef) != "" && !domain.IsSafeHelmChartRef(options.ChartRef) {
		return fmt.Errorf("invalid helm chart reference")
	}
	args := []string{
		"upgrade",
		"--install",
		"--",
		options.ReleaseName,
		options.ChartRef,
		"--namespace",
		options.Namespace,
	}
	if options.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	if strings.TrimSpace(options.ValuesFile) != "" {
		args = append(args, "-f", options.ValuesFile)
	}
	if strings.TrimSpace(options.ChartVersion) != "" && !isDirectHelmChartArchive(options.ChartRef) {
		args = append(args, "--version", strings.TrimSpace(options.ChartVersion))
	}
	if options.Wait {
		args = append(args, "--wait")
	}
	if options.Timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(options.Timeout)+"s")
	}
	output, err := e.runCommand(ctx, "helm", args...)
	if err == nil {
		return nil
	}
	return fmt.Errorf("helm apply failed for release %q in namespace %q: %s", options.ReleaseName, options.Namespace, helmOutputMessage(output, err))
}

type HelmUninstallOptions struct {
	ReleaseName string
	Namespace   string
}

func (e *CLIHelmExecutor) Uninstall(ctx context.Context, options HelmUninstallOptions) error {
	args := []string{
		"uninstall",
		options.ReleaseName,
		"--namespace",
		options.Namespace,
	}
	output, err := e.runCommand(ctx, "helm", args...)
	if err == nil {
		return nil
	}
	if isHelmReleaseMissing(err, output) {
		return nil
	}
	return fmt.Errorf("helm delete failed for release %q in namespace %q: %s", options.ReleaseName, options.Namespace, helmOutputMessage(output, err))
}

func (e *CLIHelmExecutor) DeleteNamespace(ctx context.Context, namespace string) error {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil
	}
	output, err := e.runCommand(ctx, "kubectl", "delete", "namespace", namespace, "--ignore-not-found=true")
	if err != nil {
		return fmt.Errorf("helm namespace delete failed for namespace %q: %s", namespace, helmOutputMessage(output, err))
	}
	return nil
}

func (e *CLIHelmExecutor) Status(ctx context.Context, options HelmStatusOptions) (HelmStatus, error) {
	args := []string{
		"status",
		options.ReleaseName,
		"--namespace",
		options.Namespace,
		"-o",
		"json",
	}
	output, err := e.runCommand(ctx, "helm", args...)
	if err != nil {
		if isHelmReleaseNotFoundInOutput(output, err) {
			return HelmStatus{Found: false}, nil
		}
		return HelmStatus{}, fmt.Errorf("helm status failed for release %q in namespace %q: %s", options.ReleaseName, options.Namespace, helmOutputMessage(output, err))
	}
	var parsed helmStatusOutput
	if err := json.Unmarshal(output, &parsed); err != nil {
		return HelmStatus{}, fmt.Errorf("helm status output parse failed for release %q in namespace %q: %w", options.ReleaseName, options.Namespace, err)
	}
	return HelmStatus{
		Found:      true,
		Status:     strings.TrimSpace(parsed.Info.Status),
		Chart:      strings.TrimSpace(parsed.Chart.Metadata.Name),
		AppVersion: strings.TrimSpace(parsed.Chart.Metadata.AppVersion),
		Namespace:  strings.TrimSpace(parsed.Namespace),
	}, nil
}

func (e *CLIHelmExecutor) IsNamespaceManaged(ctx context.Context, namespace, projectID, environmentID string) (bool, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return false, nil
	}
	output, err := e.runCommand(ctx, "kubectl", "get", "namespace", namespace, "-o", "json")
	if err != nil {
		if isKubectlNoResources(output, err) || isKubectlNotFound(err, output) {
			return false, nil
		}
		return false, fmt.Errorf("helm namespace ownership check failed for namespace %q: %s", namespace, helmOutputMessage(output, err))
	}
	var ns kubernetesNamespace
	if err := json.Unmarshal(output, &ns); err != nil {
		return false, fmt.Errorf("helm namespace ownership parse failed for namespace %q: %w", namespace, err)
	}
	labels := ns.Metadata.Labels
	if labels == nil {
		return false, nil
	}
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	managedOK := strings.TrimSpace(labels["envpilot.io/managed"]) == "true"
	projectOK := true
	environmentOK := true
	if projectID != "" {
		projectOK = strings.TrimSpace(labels["envpilot.io/project-id"]) == projectID
	}
	if environmentID != "" {
		environmentOK = strings.TrimSpace(labels["envpilot.io/environment-id"]) == environmentID
	}
	return managedOK && projectOK && environmentOK, nil
}

type kubernetesPodList struct {
	Items []struct {
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func (e *CLIHelmExecutor) Readiness(ctx context.Context, options HelmReadinessOptions) (bool, error) {
	namespace := strings.TrimSpace(options.Namespace)
	if namespace == "" {
		return true, nil
	}
	output, err := e.runCommand(ctx, "kubectl", "get", "pods", "--namespace", namespace, "-l", "release="+strings.TrimSpace(options.Release), "-o", "json")
	if err != nil {
		if isKubectlNoResources(output, err) {
			return false, nil
		}
		if isKubectlNotFound(err, output) {
			return true, nil
		}
		return false, fmt.Errorf("helm workload readiness check failed for namespace %q release %q: %s", namespace, options.Release, helmOutputMessage(output, err))
	}
	var podList kubernetesPodList
	if err := json.Unmarshal(output, &podList); err != nil {
		return false, fmt.Errorf("helm workload readiness parse failed for namespace %q release %q: %w", namespace, options.Release, err)
	}
	if len(podList.Items) == 0 {
		return false, nil
	}
	for _, pod := range podList.Items {
		phase := strings.ToLower(strings.TrimSpace(pod.Status.Phase))
		if phase == "" {
			return false, nil
		}
		if phase != "running" && phase != "succeeded" {
			return false, nil
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && strings.ToLower(strings.TrimSpace(condition.Status)) != "true" {
				return false, nil
			}
		}
	}
	return true, nil
}

func helmOutputMessage(output []byte, err error) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out: " + text
	}
	return text
}

type manifestBackend struct {
	renderer gitops.Renderer
}

type HelmDirectBackend struct {
	manifestBackend
	helmExecutor HelmExecutor
}

func NewHelmDirectBackend(renderer gitops.Renderer) *HelmDirectBackend {
	return NewHelmDirectBackendWithExecutor(renderer, NewCLIHelmExecutor())
}

func NewHelmDirectBackendWithExecutor(renderer gitops.Renderer, helmExecutor HelmExecutor) *HelmDirectBackend {
	if helmExecutor == nil {
		helmExecutor = NewCLIHelmExecutor()
	}
	return &HelmDirectBackend{
		manifestBackend: manifestBackend{renderer: renderer},
		helmExecutor:    helmExecutor,
	}
}

func (b *HelmDirectBackend) Delete(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) error {
	config := resolveHelmDirectConfig(projectConfig)
	releaseName, err := b.renderReleaseName(environment, projectConfig)
	if err != nil {
		return err
	}
	namespace := b.targetNamespace(environment, config)
	if err := b.helmExecutor.Uninstall(ctx, HelmUninstallOptions{
		ReleaseName: releaseName,
		Namespace:   namespace,
	}); err != nil {
		if !isHelmReleaseMissing(err, nil) {
			return err
		}
	}
	if !b.shouldDeleteNamespace(environment, config, namespace) {
		return nil
	}
	managed, err := b.helmExecutor.IsNamespaceManaged(ctx, namespace, strings.TrimSpace(environment.Project), strings.TrimSpace(environment.ID))
	if err != nil {
		return err
	}
	if !managed {
		return nil
	}
	return b.helmExecutor.DeleteNamespace(ctx, namespace)
}

func (b *HelmDirectBackend) Status(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) (domain.EnvironmentStatus, error) {
	_ = projectConfig
	config := resolveHelmDirectConfig(projectConfig)
	releaseName, err := b.renderReleaseName(environment, projectConfig)
	if err != nil {
		return "", err
	}
	namespace := b.targetNamespace(environment, config)
	releaseStatus, err := b.helmExecutor.Status(ctx, HelmStatusOptions{
		ReleaseName: releaseName,
		Namespace:   namespace,
	})
	if err != nil {
		return "", err
	}
	if !releaseStatus.Found {
		if environment.Status == domain.StatusTerminated || environment.Status == domain.StatusDeleteFailed || environment.Status == domain.StatusTerminating || environment.Status == domain.StatusGitOpsDeletePending || environment.Status == domain.StatusDeleteRequested {
			return domain.StatusTerminated, nil
		}
		return domain.EnvironmentStatus("not_found"), nil
	}
	switch strings.ToLower(strings.TrimSpace(releaseStatus.Status)) {
	case "deployed":
		return b.checkReleaseReady(ctx, namespace, releaseStatus, environment, config)
	case "pending-install", "pending-upgrade", "pending":
		return domain.StatusCreating, nil
	case "failed", "superseded", "uninstalled", "uninstalling", "unknown", "degraded":
		return domain.StatusFailed, nil
	default:
		return domain.StatusCreating, nil
	}
}

func (b *HelmDirectBackend) checkReleaseReady(ctx context.Context, namespace string, status HelmStatus, environment domain.Environment, config helmDirectConfig) (domain.EnvironmentStatus, error) {
	_ = status
	if !config.wait {
		return domain.StatusReady, nil
	}
	releaseName, err := b.renderReleaseName(environment, domain.ProjectConfig{
		Config: map[string]any{
			"deployment": map[string]any{
				"backend": "helm_direct",
				"helmDirect": map[string]any{
					"wait": config.wait,
				},
			},
		},
	})
	if err != nil {
		return "", err
	}
	ready, err := b.helmExecutor.Readiness(ctx, HelmReadinessOptions{
		Release:   releaseName,
		Namespace: namespace,
	})
	if err != nil {
		return "", err
	}
	if !ready {
		return domain.StatusCreating, nil
	}
	return domain.StatusReady, nil
}

type helmDirectValue struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type helmDirectRelease struct {
	Name         string            `yaml:"name"`
	Namespace    string            `yaml:"namespace"`
	ChartPath    string            `yaml:"chartPath"`
	ChartRef     string            `yaml:"chartRef"`
	ChartVersion string            `yaml:"chartVersion,omitempty"`
	Timeout      int               `yaml:"timeout"`
	Wait         bool              `yaml:"wait"`
	Labels       []helmDirectValue `yaml:"labels"`
	Annotations  []helmDirectValue `yaml:"annotations"`
}

type helmDirectValues struct {
	ImageTags []helmDirectValue `yaml:"imageTags"`
}

type helmDirectNamespace struct {
	Name        string            `yaml:"name"`
	Labels      []helmDirectValue `yaml:"labels"`
	Annotations []helmDirectValue `yaml:"annotations"`
}

type helmDirectMetadata struct {
	Labels      []helmDirectValue `yaml:"labels"`
	Annotations []helmDirectValue `yaml:"annotations"`
}

type helmDirectManifest struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   helmDirectMetadata `yaml:"metadata"`
	Spec       struct {
		Release    helmDirectRelease   `yaml:"release"`
		Namespace  helmDirectNamespace `yaml:"namespace"`
		Identity   []helmDirectValue   `yaml:"identity"`
		Values     helmDirectValues    `yaml:"values"`
		ValuesFile string              `yaml:"valuesFile,omitempty"`
		ValuesObj  []helmDirectValue   `yaml:"valuesObject,omitempty"`
	} `yaml:"spec"`
}

func (b *HelmDirectBackend) Render(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) ([]Manifest, error) {
	_ = b
	_ = ctx
	releaseMetadata, err := b.renderHelmDirectManifest(environment, projectConfig)
	if err != nil {
		return nil, err
	}
	return []Manifest{
		{
			Path:    environment.ManifestFilename(),
			Kind:    "HelmDirect",
			Content: releaseMetadata,
		},
	}, nil
}

func (b *HelmDirectBackend) Apply(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) error {
	config := resolveHelmDirectConfig(projectConfig)
	releaseName, err := b.renderReleaseName(environment, projectConfig)
	if err != nil {
		return err
	}
	namespace := b.targetNamespace(environment, config)
	valuesFile, cleanup, err := b.helmValuesFile(environment)
	if err != nil {
		return err
	}
	defer func() {
		_ = cleanup()
	}()
	return b.helmExecutor.UpgradeInstall(ctx, HelmUpgradeOptions{
		ReleaseName:     releaseName,
		ChartRef:        b.chartRef(environment, config),
		ChartVersion:    helmDirectChartVersion(config.chartRef, config.chartVersion),
		Namespace:       namespace,
		ValuesFile:      valuesFile,
		CreateNamespace: config.createNamespace,
		Wait:            config.wait,
		Timeout:         config.timeout,
	})
}

// DeploymentTarget resolves the exact release and namespace that Helm will
// use. The runner reports these facts back to the control plane before apply,
// including when Helm subsequently fails.
func (b *HelmDirectBackend) DeploymentTarget(environment domain.Environment, projectConfig domain.ProjectConfig) (string, string, error) {
	config := resolveHelmDirectConfig(projectConfig)
	releaseName, err := b.renderReleaseName(environment, projectConfig)
	if err != nil {
		return "", "", err
	}
	return releaseName, b.targetNamespace(environment, config), nil
}

func (b *HelmDirectBackend) helmValuesFile(environment domain.Environment) (string, func() error, error) {
	valuesFile := strings.TrimSpace(environment.GitOps.ValuesPath)
	if valuesFile != "" {
		return valuesFile, func() error { return nil }, nil
	}
	values := b.renderHelmImageValues(environment)
	if len(values) == 0 {
		return "", func() error { return nil }, nil
	}
	valuesObject := make(map[string]string, len(values))
	for _, item := range values {
		valuesObject[item.Name] = item.Value
	}
	f, err := os.CreateTemp("", "envpilot-helm-direct-values-*.yaml")
	if err != nil {
		return "", nil, err
	}
	payload := gitops.ValuesYAML(valuesObject)
	if _, err := f.WriteString(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() error {
		return os.Remove(f.Name())
	}, nil
}

func (b *HelmDirectBackend) renderHelmDirectManifest(environment domain.Environment, projectConfig domain.ProjectConfig) ([]byte, error) {
	environmentID := strings.TrimSpace(environment.ID)
	if environmentID == "" {
		return nil, fmt.Errorf("environment id is required")
	}
	projectID := strings.TrimSpace(environment.Project)
	if projectID == "" {
		return nil, fmt.Errorf("project is required")
	}

	releaseName, err := b.renderReleaseName(environment, projectConfig)
	if err != nil {
		return nil, err
	}
	config := resolveHelmDirectConfig(projectConfig)
	releaseNamespace := b.targetNamespace(environment, config)
	labels := b.renderManagedLabels(projectID, environmentID)
	valuesFile := strings.TrimSpace(environment.GitOps.ValuesPath)

	payload := helmDirectManifest{
		APIVersion: "v1alpha1",
		Kind:       "HelmDirectDeployment",
		Metadata: helmDirectMetadata{
			Labels:      labels,
			Annotations: labels,
		},
		Spec: struct {
			Release    helmDirectRelease   `yaml:"release"`
			Namespace  helmDirectNamespace `yaml:"namespace"`
			Identity   []helmDirectValue   `yaml:"identity"`
			Values     helmDirectValues    `yaml:"values"`
			ValuesFile string              `yaml:"valuesFile,omitempty"`
			ValuesObj  []helmDirectValue   `yaml:"valuesObject,omitempty"`
		}{
			Release: helmDirectRelease{
				Name:         releaseName,
				Namespace:    releaseNamespace,
				ChartPath:    b.chartPath(environment),
				ChartRef:     b.chartRef(environment, config),
				ChartVersion: config.chartVersion,
				Timeout:      config.timeout,
				Wait:         config.wait,
				Labels:       labels,
				Annotations: func() []helmDirectValue {
					c := make([]helmDirectValue, len(labels))
					copy(c, labels)
					return c
				}(),
			},
			Namespace: helmDirectNamespace{
				Name: releaseNamespace,
				Labels: func() []helmDirectValue {
					c := make([]helmDirectValue, len(labels))
					copy(c, labels)
					return c
				}(),
				Annotations: func() []helmDirectValue {
					c := make([]helmDirectValue, len(labels))
					copy(c, labels)
					return c
				}(),
			},
			Identity: b.renderIdentityValues(environment),
			Values: helmDirectValues{
				ImageTags: b.renderHelmImageValues(environment),
			},
			ValuesFile: valuesFile,
		},
	}
	if valuesFile == "" {
		payload.Spec.ValuesObj = payload.Spec.Values.ImageTags
		payload.Spec.Values.ImageTags = nil
	}

	data, err := yamlMarshalCanonical(payload)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(data, []byte("\n")), nil
}

func (b *HelmDirectBackend) renderManagedLabels(projectID, environmentID string) []helmDirectValue {
	return []helmDirectValue{
		{Name: "envpilot.io/project-id", Value: projectID},
		{Name: "envpilot.io/environment-id", Value: environmentID},
		{Name: "envpilot.io/managed", Value: "true"},
	}
}

func (b *HelmDirectBackend) chartPath(environment domain.Environment) string {
	return strings.TrimSpace(environment.GitOps.Path)
}

func (b *HelmDirectBackend) chartRef(environment domain.Environment, config helmDirectConfig) string {
	if ref := strings.TrimSpace(config.chartRef); ref != "" {
		return ref
	}
	ref := strings.TrimSpace(environment.Charts.App)
	if ref != "" {
		return ref
	}
	if strings.TrimSpace(environment.GitOps.Path) != "" {
		return strings.TrimSpace(environment.GitOps.Path)
	}
	return "latest"
}

func (b *HelmDirectBackend) targetNamespace(environment domain.Environment, config helmDirectConfig) string {
	namespace := strings.TrimSpace(environment.Namespace)
	switch strings.ToLower(strings.TrimSpace(config.namespaceMode)) {
	case "shared":
		if rendered := strings.TrimSpace(b.renderTemplatePattern(config.namespacePattern, environment)); rendered != "" {
			return rendered
		}
		if namespace != "" {
			return namespace
		}
		return strings.TrimSpace(environment.Project)
	default:
		if rendered := strings.TrimSpace(b.renderTemplatePattern(config.namespacePattern, environment)); rendered != "" {
			return rendered
		}
		if namespace != "" {
			return namespace
		}
		return strings.TrimSpace(environment.ID)
	}
}

func (b *HelmDirectBackend) renderReleaseName(environment domain.Environment, projectConfig domain.ProjectConfig) (string, error) {
	config := resolveHelmDirectConfig(projectConfig)
	t, err := template.New("release").Parse(strings.TrimSpace(config.releaseNamePattern))
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := t.Execute(&output, b.helmDirectTemplateData(environment)); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

func (b *HelmDirectBackend) renderTemplatePattern(pattern string, environment domain.Environment) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	t, err := template.New("pattern").Parse(pattern)
	if err != nil {
		return ""
	}
	var output bytes.Buffer
	if err := t.Execute(&output, b.helmDirectTemplateData(environment)); err != nil {
		return ""
	}
	return strings.TrimSpace(output.String())
}

func (b *HelmDirectBackend) helmDirectTemplateData(environment domain.Environment) map[string]any {
	projectID := strings.TrimSpace(environment.Project)
	environmentID := strings.TrimSpace(environment.ID)
	prNumber := strings.TrimSpace(environment.Source.PullRequestID)
	branch := strings.TrimSpace(environment.Source.Branch)
	commit := strings.TrimSpace(environment.Source.Commit)
	return map[string]any{
		"ProjectID":     projectID,
		"EnvironmentID": environmentID,
		"PRNumber":      prNumber,
		"MRNumber":      prNumber,
		"Branch":        branch,
		"CommitSHA":     commit,
		"project": map[string]string{
			"id": projectID,
		},
		"environment": map[string]string{
			"id":        environmentID,
			"name":      environmentID,
			"projectId": projectID,
		},
		"source": map[string]string{
			"pr":     prNumber,
			"mr":     prNumber,
			"branch": branch,
			"commit": commit,
		},
	}
}

func (b *HelmDirectBackend) shouldDeleteNamespace(environment domain.Environment, config helmDirectConfig, namespace string) bool {
	if strings.EqualFold(strings.TrimSpace(config.namespaceMode), "shared") {
		return false
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return false
	}
	targetNamespace := strings.TrimSpace(environment.Namespace)
	if targetNamespace != "" && namespace != targetNamespace {
		return false
	}
	return true
}

func (b *HelmDirectBackend) renderIdentityValues(environment domain.Environment) []helmDirectValue {
	values := []helmDirectValue{
		{Name: "projectId", Value: strings.TrimSpace(environment.Project)},
		{Name: "environmentId", Value: strings.TrimSpace(environment.ID)},
		{Name: "prNumber", Value: strings.TrimSpace(environment.Source.PullRequestID)},
		{Name: "branch", Value: strings.TrimSpace(environment.Source.Branch)},
		{Name: "commitSHA", Value: strings.TrimSpace(environment.Source.Commit)},
	}
	sort.Slice(values, func(i, j int) bool {
		return values[i].Name < values[j].Name
	})
	return values
}

func (b *HelmDirectBackend) renderHelmImageValues(environment domain.Environment) []helmDirectValue {
	valuesByName := map[string]string{}
	for _, service := range environment.Services {
		key := normalizeHelmServiceTag(strings.TrimSpace(service.Name))
		if key == "" {
			continue
		}
		tag := strings.TrimSpace(service.Tag)
		if tag == "" {
			tag = "latest"
		}
		valuesByName[key] = tag
	}
	for _, service := range environment.Base.Services {
		key := normalizeHelmServiceTag(strings.TrimSpace(service.Name))
		if key == "" {
			continue
		}
		valuesByName[key] = "latest"
	}
	for key, value := range environment.Overrides {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		valuesByName[trimmedKey] = strings.TrimSpace(value)
	}
	out := make([]helmDirectValue, 0, len(valuesByName))
	for key, value := range valuesByName {
		out = append(out, helmDirectValue{Name: key, Value: value})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

type helmDirectConfig struct {
	namespaceMode      string
	namespacePattern   string
	releaseNamePattern string
	chartRef           string
	chartVersion       string
	timeout            int
	wait               bool
	createNamespace    bool
}

func resolveHelmDirectConfig(projectConfig domain.ProjectConfig) helmDirectConfig {
	config := helmDirectConfig{
		namespaceMode:      "dedicated",
		releaseNamePattern: "{{ .project.id }}-{{ .environment.name }}",
		timeout:            300,
		wait:               true,
		createNamespace:    true,
	}
	rawDeployment, ok := projectConfig.Config["deployment"]
	if !ok {
		return config
	}
	rawDeploymentMap, ok := rawDeployment.(map[string]any)
	if !ok {
		return config
	}
	rawHelmDirect, ok := rawDeploymentMap["helmDirect"]
	if !ok {
		return config
	}
	helmDirectConfigValue, ok := rawHelmDirect.(map[string]any)
	if !ok {
		return config
	}
	if value, ok := helmDirectConfigValue["namespaceMode"]; ok {
		if text := strings.TrimSpace(asString(value)); text != "" {
			config.namespaceMode = text
		}
	}
	if value := asString(helmDirectConfigValue["namespacePattern"]); strings.TrimSpace(value) != "" {
		config.namespacePattern = strings.TrimSpace(value)
	}
	if value := asString(helmDirectConfigValue["releaseNamePattern"]); strings.TrimSpace(value) != "" {
		config.releaseNamePattern = strings.TrimSpace(value)
	}
	config.chartRef = strings.TrimSpace(asString(helmDirectConfigValue["chartRef"]))
	config.chartVersion = strings.TrimSpace(asString(helmDirectConfigValue["chartVersion"]))
	if value, ok := helmDirectConfigValue["timeout"]; ok {
		if timeout, ok := asInt(value); ok && timeout > 0 {
			config.timeout = timeout
		}
	}
	if value, ok := helmDirectConfigValue["wait"]; ok {
		config.wait = asBool(value)
	}
	if value, ok := helmDirectConfigValue["createNamespace"]; ok {
		config.createNamespace = asBool(value)
	}
	return config
}

// Helm accepts --version for chart repositories and OCI references, but a
// direct .tgz archive already identifies its exact chart artifact and rejects
// that flag. Keep the configured version in the contract while omitting it at
// execution time for this narrow exception.
func helmDirectChartVersion(chartRef, chartVersion string) string {
	chartVersion = strings.TrimSpace(chartVersion)
	if chartVersion == "" || isDirectHelmChartArchive(chartRef) {
		return ""
	}
	return chartVersion
}

func isDirectHelmChartArchive(chartRef string) bool {
	parsed, err := url.Parse(strings.TrimSpace(chartRef))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".tgz")
}

func normalizeHelmServiceTag(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return ""
	case "nginx":
		return "nginxTag"
	case "php", "php-fpm":
		return "phpTag"
	case "nuxt":
		return "nuxtTag"
	case "api", "cms-api":
		return "cmsApiTag"
	case "backend", "cms-backend":
		return "cmsBackendTag"
	case "frontend", "cms-frontend":
		return "cmsFrontendTag"
	case "migration", "cms-migration":
		return "cmsMigrationTag"
	case "rest", "cms-rest":
		return "cmsRestTag"
	case "notifications", "api-notifications":
		return "apiNotificationTag"
	case "heimdall":
		return "apiHeimdallTag"
	case "cursus":
		return "apiCursusTag"
	case "iris":
		return "apiIrisTag"
	case "betting":
		return "bettingTag"
	case "websockets":
		return "websocketsTag"
	default:
		return serviceTagKeyLike(name)
	}
}

func serviceTagKeyLike(name string) string {
	parts := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(name)), func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	if len(parts) == 0 {
		return "serviceTag"
	}
	var output strings.Builder
	output.WriteString(parts[0])
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		output.WriteString(strings.ToUpper(part[:1]))
		if len(part) > 1 {
			output.WriteString(part[1:])
		}
	}
	return output.String() + "Tag"
}

func asInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case string:
		if v == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func asBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.TrimSpace(strings.ToLower(v)) {
		case "true", "1", "on", "yes":
			return true
		case "false", "0", "off", "no":
			return false
		default:
			return false
		}
	default:
		return false
	}
}

func isHelmReleaseMissing(err error, output []byte) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(string(output)))
	if text == "" && errors.Is(err, exec.ErrNotFound) {
		return false
	}
	messages := []string{
		"release: not found",
		"uninstall: release not found",
		"has no deployed releases",
		"not found",
		"release name not provided",
	}
	for _, message := range messages {
		if strings.Contains(text, message) {
			return true
		}
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return true
	}
	return false
}

func isKubectlNoResources(output []byte, err error) bool {
	text := strings.ToLower(strings.TrimSpace(string(output)))
	if strings.Contains(text, "no resources found") {
		return true
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no resources found") {
		return true
	}
	return false
}

func isKubectlNotFound(err error, output []byte) bool {
	text := strings.ToLower(strings.TrimSpace(string(output)))
	if strings.Contains(text, "not found") {
		return true
	}
	if strings.Contains(text, "error: unavailable") {
		return true
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return true
	}
	return false
}

func isHelmReleaseNotFoundInOutput(output []byte, err error) bool {
	if len(output) == 0 {
		return isHelmReleaseMissing(err, output)
	}
	return isHelmReleaseMissing(err, output)
}

func yamlMarshalCanonical(value interface{}) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("...\n")), nil
}

type FluxBackend struct{ manifestBackend }

func NewFluxBackend(renderer gitops.Renderer) *FluxBackend {
	return &FluxBackend{manifestBackend: manifestBackend{renderer: renderer}}
}

func (b *FluxBackend) Render(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) ([]Manifest, error) {
	return b.manifestBackend.Render(ctx, environment, projectConfig)
}

func (b *FluxBackend) Apply(context.Context, domain.Environment, domain.ProjectConfig) error {
	return nil
}

func (b *FluxBackend) ApplyWithWriter(ctx context.Context, environment domain.Environment, _ domain.ProjectConfig, writer gitops.Writer) (gitops.CommitResult, error) {
	_ = b
	if writer == nil {
		return gitops.CommitResult{}, fmt.Errorf("gitops writer is required for flux backend apply")
	}
	return writer.Commit(ctx, "envpilot: create "+environment.ID)
}

func (b *FluxBackend) Delete(context.Context, domain.Environment, domain.ProjectConfig) error {
	_ = b
	return nil
}

func (b *FluxBackend) DeleteWithWriter(ctx context.Context, environment domain.Environment, _ domain.ProjectConfig, writer gitops.Writer) (gitops.CommitResult, error) {
	_ = b
	if writer == nil {
		return gitops.CommitResult{}, fmt.Errorf("gitops writer is required for flux backend delete")
	}
	if err := writer.RemovePath(ctx, environment.GitOpsDirectory(), "envpilot: delete manifests "+environment.ID); err != nil {
		return gitops.CommitResult{}, err
	}
	return writer.Commit(ctx, "envpilot: delete "+environment.ID)
}

func (b *FluxBackend) Status(_ context.Context, environment domain.Environment, _ domain.ProjectConfig) (domain.EnvironmentStatus, error) {
	_ = b
	if environment.FluxStatus == nil {
		return environment.Status, nil
	}
	if environment.FluxStatus.Status != "" {
		return environment.FluxStatus.Status, nil
	}
	if len(environment.FluxStatus.HelmReleases) == 0 && len(environment.FluxStatus.Kustomizations) == 0 {
		return environment.Status, nil
	}
	for _, res := range environment.FluxStatus.HelmReleases {
		if res.Failed {
			return domain.StatusFailed, nil
		}
		if !res.Ready {
			return domain.StatusCreating, nil
		}
	}
	for _, res := range environment.FluxStatus.Kustomizations {
		if res.Failed {
			return domain.StatusFailed, nil
		}
		if !res.Ready {
			return domain.StatusCreating, nil
		}
	}
	return domain.StatusReady, nil
}

type GitOpsManifestBackend struct{ manifestBackend }

func NewGitOpsManifestBackend(renderer gitops.Renderer) *GitOpsManifestBackend {
	return &GitOpsManifestBackend{manifestBackend: manifestBackend{renderer: renderer}}
}

func (b *manifestBackend) Render(_ context.Context, environment domain.Environment, _ domain.ProjectConfig) ([]Manifest, error) {
	manifests, err := b.renderer.RenderManifestSet(environment)
	if err != nil {
		return nil, err
	}
	adapted := make([]Manifest, len(manifests))
	for index, manifest := range manifests {
		adapted[index] = Manifest{
			Path:    manifest.Path,
			Kind:    manifest.Kind,
			Content: manifest.Content,
		}
	}
	return adapted, nil
}

func (b *manifestBackend) Apply(context.Context, domain.Environment, domain.ProjectConfig) error {
	return nil
}

func (b *manifestBackend) Delete(context.Context, domain.Environment, domain.ProjectConfig) error {
	return nil
}

func (b *manifestBackend) Status(_ context.Context, environment domain.Environment, _ domain.ProjectConfig) (domain.EnvironmentStatus, error) {
	return environment.Status, nil
}

type EnvironmentOrchestrator struct {
	store           store.EnvironmentStore
	backend         DeploymentBackend
	backendResolver func(projectConfig domain.ProjectConfig) (DeploymentBackend, error)
	writer          gitops.Writer
	now             func() time.Time
}

func New(store store.EnvironmentStore, renderer gitops.Renderer, writer gitops.Writer) *EnvironmentOrchestrator {
	return NewWithBackendResolver(store, func(projectConfig domain.ProjectConfig) (DeploymentBackend, error) {
		return ResolveDeploymentBackendFromProjectConfig(projectConfig, renderer)
	}, writer)
}

func ResolveDeploymentBackendType(raw string) DeploymentBackendType {
	return NormalizeDeploymentBackendType(raw)
}

func NormalizeDeploymentBackendType(raw string) DeploymentBackendType {
	switch DeploymentBackendType(strings.ToLower(strings.TrimSpace(raw))) {
	case DeploymentBackendHelmDirect, DeploymentBackendFluxCD, DeploymentBackendGitOpsManifest:
		return DeploymentBackendType(strings.ToLower(strings.TrimSpace(raw)))
	case DeploymentBackendType("helm-direct"):
		return DeploymentBackendHelmDirect
	case DeploymentBackendType("flux"):
		return DeploymentBackendFluxCD
	case DeploymentBackendType("flux_cd"):
		return DeploymentBackendFluxCD
	default:
		return DeploymentBackendType(strings.ToLower(strings.TrimSpace(raw)))
	}
}

func NewDeploymentBackend(backendType DeploymentBackendType, renderer gitops.Renderer) DeploymentBackend {
	backend, err := ResolveDeploymentBackend(backendType, renderer)
	if err != nil {
		return NewGitOpsManifestBackend(renderer)
	}
	return backend
}

func ResolveDeploymentBackend(backendType DeploymentBackendType, renderer gitops.Renderer) (DeploymentBackend, error) {
	backendType = NormalizeDeploymentBackendType(string(backendType))
	switch backendType {
	case "", DeploymentBackendHelmDirect:
		return NewHelmDirectBackend(renderer), nil
	case DeploymentBackendFluxCD:
		return NewFluxBackend(renderer), nil
	case DeploymentBackendGitOpsManifest:
		return NewGitOpsManifestBackend(renderer), nil
	default:
		available := []string{
			string(DeploymentBackendHelmDirect),
			string(DeploymentBackendFluxCD),
		}
		sort.Strings(available)
		return nil, fmt.Errorf("unsupported deployment backend %q, supported: %s", backendType, strings.Join(available, ", "))
	}
}

func ResolveDeploymentBackendFromProjectConfig(projectConfig domain.ProjectConfig, renderer gitops.Renderer) (DeploymentBackend, error) {
	rawDeployment, ok := projectConfig.Config["deployment"]
	if !ok {
		return ResolveDeploymentBackend(DeploymentBackendType(domain.InferDeploymentBackend("", map[string]any{}, projectConfig.Config)), renderer)
	}
	rawDeploymentMap, ok := rawDeployment.(map[string]any)
	if !ok {
		return ResolveDeploymentBackend(DeploymentBackendType(domain.InferDeploymentBackend("", map[string]any{}, projectConfig.Config)), renderer)
	}
	return ResolveDeploymentBackend(DeploymentBackendType(domain.InferDeploymentBackend(rawDeploymentMap["backend"], rawDeploymentMap, projectConfig.Config)), renderer)
}

func asString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func NewWithBackend(store store.EnvironmentStore, backend DeploymentBackend, writer gitops.Writer) *EnvironmentOrchestrator {
	o := NewWithBackendResolver(store, nil, writer)
	o.backend = backend
	return o
}

func NewWithBackendResolver(store store.EnvironmentStore, backendResolver func(projectConfig domain.ProjectConfig) (DeploymentBackend, error), writer gitops.Writer) *EnvironmentOrchestrator {
	return &EnvironmentOrchestrator{
		store:           store,
		backendResolver: backendResolver,
		writer:          writer,
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func (o *EnvironmentOrchestrator) backendForProjectConfig(projectConfig domain.ProjectConfig) (DeploymentBackend, error) {
	if o.backendResolver != nil {
		return o.backendResolver(projectConfig)
	}
	if o.backend == nil {
		return nil, fmt.Errorf("deployment backend is not configured")
	}
	return o.backend, nil
}

func (o *EnvironmentOrchestrator) Create(ctx context.Context, environment domain.Environment) (domain.Environment, error) {
	return o.CreateWithWriterAndProjectConfig(ctx, environment, o.writer, domain.ProjectConfig{})
}

func (o *EnvironmentOrchestrator) CreateWithProjectConfig(ctx context.Context, environment domain.Environment, projectConfig domain.ProjectConfig) (domain.Environment, error) {
	return o.CreateWithWriterAndProjectConfig(ctx, environment, o.writer, projectConfig)
}

func (o *EnvironmentOrchestrator) CreateWithWriter(ctx context.Context, environment domain.Environment, writer gitops.Writer) (domain.Environment, error) {
	return o.CreateWithWriterAndProjectConfig(ctx, environment, writer, domain.ProjectConfig{})
}

func (o *EnvironmentOrchestrator) CreateWithWriterAndProjectConfig(ctx context.Context, environment domain.Environment, writer gitops.Writer, projectConfig domain.ProjectConfig) (domain.Environment, error) {
	if writer == nil {
		writer = o.writer
	}
	backend, err := o.backendForProjectConfig(projectConfig)
	if err != nil {
		environment.Status = domain.StatusFailed
		environment.LastError = err.Error()
		environment.UpdatedAt = o.now()
		_ = o.store.Save(environment)
		return environment, err
	}
	manifests, err := backend.Render(ctx, environment, projectConfig)
	if err != nil {
		environment.Status = domain.StatusFailed
		environment.LastError = err.Error()
		environment.UpdatedAt = o.now()
		_ = o.store.Save(environment)
		return environment, err
	}

	environment.Status = domain.StatusCreating
	environment.LastError = ""
	environment.UpdatedAt = o.now()
	for _, manifest := range manifests {
		path, err := writer.WriteManifest(ctx, manifest.Path, manifest.Content, "envpilot: create "+environment.ID+" "+manifest.Kind)
		if err != nil {
			environment.Status = domain.StatusFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		switch manifest.Path {
		case environment.ManifestFilename():
			environment.ManifestPath = path
		case environment.NamespaceManifestFilename():
			environment.NamespaceManifestPath = path
		case environment.PathKustomizationFilename():
			environment.KustomizationManifestPath = path
		}
	}
	if backendWithWriter, ok := backend.(deploymentBackendWithWriter); ok {
		commit, err := backendWithWriter.ApplyWithWriter(ctx, environment, projectConfig, writer)
		if err != nil {
			environment.Status = domain.StatusFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		if commit.PullRequestURL != "" {
			environment.GitOps.PullRequestURL = commit.PullRequestURL
		}
	} else {
		if err := backend.Apply(ctx, environment, projectConfig); err != nil {
			environment.Status = domain.StatusFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		commit, err := writer.Commit(ctx, "envpilot: create "+environment.ID)
		if err != nil {
			environment.Status = domain.StatusFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		if commit.PullRequestURL != "" {
			environment.GitOps.PullRequestURL = commit.PullRequestURL
		}
	}
	if err := o.store.Save(environment); err != nil {
		return domain.Environment{}, err
	}
	return environment, nil
}

func (o *EnvironmentOrchestrator) Delete(ctx context.Context, id string) (domain.Environment, error) {
	return o.DeleteWithWriterAndProjectConfig(ctx, id, o.writer, domain.ProjectConfig{})
}

func (o *EnvironmentOrchestrator) DeleteWithProjectConfig(ctx context.Context, id string, projectConfig domain.ProjectConfig) (domain.Environment, error) {
	return o.DeleteWithWriterAndProjectConfig(ctx, id, o.writer, projectConfig)
}

func (o *EnvironmentOrchestrator) DeleteWithWriter(ctx context.Context, id string, writer gitops.Writer) (domain.Environment, error) {
	return o.DeleteWithWriterAndProjectConfig(ctx, id, writer, domain.ProjectConfig{})
}

func (o *EnvironmentOrchestrator) DeleteWithWriterAndProjectConfig(ctx context.Context, id string, writer gitops.Writer, projectConfig domain.ProjectConfig) (domain.Environment, error) {
	if writer == nil {
		writer = o.writer
	}
	environment, err := o.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	if environment.Status == domain.StatusTerminated {
		return environment, nil
	}
	environment.Status = domain.StatusDeleteRequested
	environment.LastError = ""
	environment.UpdatedAt = o.now()
	_ = o.store.Save(environment)
	environment.Status = domain.StatusGitOpsDeletePending
	environment.UpdatedAt = o.now()
	_ = o.store.Save(environment)
	backend, err := o.backendForProjectConfig(projectConfig)
	if err != nil {
		environment.Status = domain.StatusDeleteFailed
		environment.LastError = err.Error()
		environment.UpdatedAt = o.now()
		_ = o.store.Save(environment)
		return environment, err
	}
	if backendWithWriter, ok := backend.(deploymentBackendWithWriter); ok {
		commit, err := backendWithWriter.DeleteWithWriter(ctx, environment, projectConfig, writer)
		if err != nil {
			environment.Status = domain.StatusDeleteFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		if commit.PullRequestURL != "" {
			environment.GitOps.PullRequestURL = commit.PullRequestURL
		}
	} else {
		if err := backend.Delete(ctx, environment, projectConfig); err != nil {
			environment.Status = domain.StatusDeleteFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		if err := writer.RemovePath(ctx, environment.GitOpsDirectory(), "envpilot: delete manifests "+environment.ID); err != nil {
			environment.Status = domain.StatusDeleteFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		commit, err := writer.Commit(ctx, "envpilot: delete "+environment.ID)
		if err != nil {
			environment.Status = domain.StatusDeleteFailed
			environment.LastError = err.Error()
			environment.UpdatedAt = o.now()
			_ = o.store.Save(environment)
			return environment, err
		}
		if commit.PullRequestURL != "" {
			environment.GitOps.PullRequestURL = commit.PullRequestURL
		}
	}

	environment.Status = domain.StatusTerminated
	environment.LastError = ""
	environment.ManifestPath = ""
	environment.NamespaceManifestPath = ""
	environment.KustomizationManifestPath = ""
	environment.UpdatedAt = o.now()
	if err := o.store.Save(environment); err != nil {
		return domain.Environment{}, err
	}
	return environment, nil
}

func (o *EnvironmentOrchestrator) Status(ctx context.Context, id string, projectConfig domain.ProjectConfig) (domain.Environment, error) {
	environment, err := o.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	backend, err := o.backendForProjectConfig(projectConfig)
	if err != nil {
		return domain.Environment{}, err
	}
	status, err := backend.Status(ctx, environment, projectConfig)
	if err != nil {
		return domain.Environment{}, err
	}
	environment.Status = status
	environment.LastError = ""
	environment.UpdatedAt = o.now()
	if err := o.store.Save(environment); err != nil {
		return domain.Environment{}, err
	}
	return environment, nil
}

func (o *EnvironmentOrchestrator) UpdateStatus(id string, status domain.EnvironmentStatus, message string) (domain.Environment, error) {
	environment, err := o.store.Get(id)
	if err != nil {
		return domain.Environment{}, err
	}
	environment.Status = status
	environment.LastError = ""
	if status == domain.StatusFailed {
		environment.LastError = message
	}
	environment.UpdatedAt = o.now()
	if err := o.store.Save(environment); err != nil {
		return domain.Environment{}, err
	}
	return environment, nil
}
