package app

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/envpilot/runner/internal/domain"
	"github.com/envpilot/runner/internal/store"
)

type ProjectConfigService struct {
	store store.ProjectConfigStore
	now   func() time.Time
}

func NewProjectConfigService(configStore store.ProjectConfigStore) *ProjectConfigService {
	return &ProjectConfigService{
		store: configStore,
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func (s *ProjectConfigService) Latest(projectID string) (domain.ProjectConfig, error) {
	config, err := s.store.Latest(projectID)
	if err != nil {
		return domain.ProjectConfig{}, err
	}
	return publicProjectConfig(config), nil
}

func (s *ProjectConfigService) LatestWithSensitive(projectID string) (domain.ProjectConfig, error) {
	return s.store.Latest(projectID)
}

func (s *ProjectConfigService) SaveFromBootstrapSession(project domain.Project, session domain.BootstrapSession, createdBy string) (domain.ProjectConfig, error) {
	project.ID = strings.TrimSpace(project.ID)
	if project.ID == "" {
		return domain.ProjectConfig{}, ValidationError{Message: "project id is required"}
	}
	if session.ProjectID != "" && session.ProjectID != project.ID {
		return domain.ProjectConfig{}, ValidationError{Message: "bootstrap session project mismatch"}
	}

	nextVersion := 1
	latest, err := s.store.Latest(project.ID)
	if err == nil {
		nextVersion = latest.Version + 1
	} else if !errors.Is(err, store.ErrProjectConfigNotFound) {
		return domain.ProjectConfig{}, err
	}

	configData, sensitiveData, err := buildProjectConfigData(project, session)
	if err != nil {
		return domain.ProjectConfig{}, err
	}
	createdAt := s.now()
	config := domain.ProjectConfig{
		ID:        fmt.Sprintf("%s-config-v%d", project.ID, nextVersion),
		ProjectID: project.ID,
		Version:   nextVersion,
		Config:    configData,
		Sensitive: sensitiveData,
		CreatedBy: strings.TrimSpace(createdBy),
		CreatedAt: createdAt,
	}
	if err := s.store.Save(config); err != nil {
		return domain.ProjectConfig{}, err
	}
	return publicProjectConfig(config), nil
}

func buildProjectConfigData(project domain.Project, session domain.BootstrapSession) (map[string]any, map[string]any, error) {
	sessionData := cloneMap(session.Data)
	sensitive := map[string]any{}

	credentials := map[string]any{}
	for key, value := range sessionData {
		if isBootstrapCredentialField(key) && isEncryptedCredentialValue(value) {
			credentials[key] = value
			sessionData[key] = map[string]any{
				"stored": true,
				"masked": true,
			}
		}
	}
	if len(credentials) > 0 {
		sensitive["scmCredentials"] = credentials
	}

	if rawStrategies, ok := sessionData[bootstrapSecretStrategiesField]; ok {
		publicStrategies, encryptedSecrets := splitProjectSecretStrategies(rawStrategies)
		sessionData[bootstrapSecretStrategiesField] = publicStrategies
		if len(encryptedSecrets) > 0 {
			sensitive["manualSecrets"] = encryptedSecrets
		}
	}
	if rawStrategies, ok := sessionData[bootstrapSecretStrategiesFieldSnake]; ok {
		publicStrategies, encryptedSecrets := splitProjectSecretStrategies(rawStrategies)
		sessionData[bootstrapSecretStrategiesField] = publicStrategies
		delete(sessionData, bootstrapSecretStrategiesFieldSnake)
		if len(encryptedSecrets) > 0 {
			sensitive["manualSecrets"] = encryptedSecrets
		}
	}

	deploymentConfig, err := buildProjectDeploymentConfig(project, sessionData)
	if err != nil {
		return nil, nil, err
	}

	config := map[string]any{
		"schemaVersion":        "v1",
		"project":              projectConfigProjectSummary(project),
		"bootstrapSessionId":   session.ID,
		"bootstrapSessionData": sessionData,
		"deployment":           deploymentConfigToMap(deploymentConfig),
	}
	return config, sensitive, nil
}

func buildProjectDeploymentConfig(project domain.Project, sessionData map[string]any) (domain.ProjectDeploymentConfig, error) {
	rawDeployment, _ := toMap(sessionData["deployment"])
	if rawDeployment == nil {
		rawDeployment = map[string]any{}
	}
	backend := domain.InferDeploymentBackend(rawDeployment["backend"], rawDeployment, sessionData)

	config := domain.ProjectDeploymentConfig{
		Backend: backend,
	}

	switch backend {
	case domain.DeploymentBackendHelmDirect:
		helmDirect, err := normalizeHelmDirectConfig(rawDeployment["helmDirect"], sessionData)
		if err != nil {
			return domain.ProjectDeploymentConfig{}, err
		}
		config.HelmDirect = helmDirect
	case domain.DeploymentBackendFluxCD:
		fluxCD, err := normalizeFluxCDConfig(rawDeployment["fluxcd"], sessionData, project)
		if err != nil {
			return domain.ProjectDeploymentConfig{}, err
		}
		config.FluxCD = fluxCD
	}

	if err := validateProjectDeploymentConfig(config); err != nil {
		return domain.ProjectDeploymentConfig{}, err
	}
	return config, nil
}

func validateProjectDeploymentConfig(config domain.ProjectDeploymentConfig) error {
	switch config.Backend {
	case domain.DeploymentBackendHelmDirect:
		return nil
	case domain.DeploymentBackendFluxCD:
		if config.FluxCD == nil {
			return ValidationError{Message: "deployment.fluxcd is required for fluxcd backend"}
		}
		if strings.TrimSpace(config.FluxCD.GitopsRepo) == "" {
			return ValidationError{Message: "deployment.fluxcd.gitopsRepo is required for fluxcd backend"}
		}
		if strings.TrimSpace(config.FluxCD.GitopsPath) == "" {
			return ValidationError{Message: "deployment.fluxcd.gitopsPath is required for fluxcd backend"}
		}
		if strings.TrimSpace(config.FluxCD.FluxNamespace) == "" {
			return ValidationError{Message: "deployment.fluxcd.fluxNamespace is required for fluxcd backend"}
		}
		if strings.TrimSpace(config.FluxCD.KustomizationName) == "" {
			return ValidationError{Message: "deployment.fluxcd.kustomizationName is required for fluxcd backend"}
		}
		if strings.TrimSpace(config.FluxCD.CommitMode) == "" {
			return ValidationError{Message: "deployment.fluxcd.commitMode is required for fluxcd backend"}
		}
		return nil
	default:
		return ValidationError{Message: "unsupported deployment.backend"}
	}
}

func normalizeHelmDirectConfig(raw any, sessionData map[string]any) (*domain.ProjectHelmDirectConfig, error) {
	config := &domain.ProjectHelmDirectConfig{
		NamespaceMode:          "dedicated",
		NamespacePattern:       "envpilot-pr-{{ .PRNumber }}",
		ReleaseNamePattern:     "{{ .project.id }}-{{ .environment.name }}",
		ChartRef:               "deploy/helm/envpilot",
		Timeout:                300,
		Wait:                   true,
		CreateNamespace:        true,
		ValuesOverrideStrategy: "merge",
		ImageTagValuePath:      "imageTag",
	}

	item, ok := toMap(raw)
	if !ok {
		item = map[string]any{}
	}
	if value, ok := item["namespaceMode"]; ok {
		valueString := strings.TrimSpace(asStringValue(value))
		if valueString == "" {
			return nil, ValidationError{Message: "deployment.helmDirect.namespaceMode is required for helm_direct backend"}
		}
		config.NamespaceMode = valueString
	}
	if value, ok := item["releaseNamePattern"]; ok {
		valueString := strings.TrimSpace(asStringValue(value))
		if valueString == "" {
			return nil, ValidationError{Message: "deployment.helmDirect.releaseNamePattern is required for helm_direct backend"}
		}
		config.ReleaseNamePattern = valueString
	}
	if value, ok := item["namespacePattern"]; ok {
		valueString := strings.TrimSpace(asStringValue(value))
		if valueString == "" {
			return nil, ValidationError{Message: "deployment.helmDirect.namespacePattern is required for helm_direct backend"}
		}
		config.NamespacePattern = valueString
	}
	if value, ok := item["chartRef"]; ok {
		valueString := strings.TrimSpace(asStringValue(value))
		if valueString == "" {
			return nil, ValidationError{Message: "deployment.helmDirect.chartRef is required for helm_direct backend"}
		}
		config.ChartRef = valueString
	}
	if value, ok := item["valuesOverrideStrategy"]; ok {
		valueString := strings.TrimSpace(asStringValue(value))
		if valueString == "" {
			return nil, ValidationError{Message: "deployment.helmDirect.valuesOverrideStrategy is required for helm_direct backend"}
		}
		config.ValuesOverrideStrategy = valueString
	}
	if value, ok := item["imageTagValuePath"]; ok {
		valueString := strings.TrimSpace(asStringValue(value))
		if valueString == "" {
			return nil, ValidationError{Message: "deployment.helmDirect.imageTagValuePath is required for helm_direct backend"}
		}
		config.ImageTagValuePath = valueString
	}
	if value, ok := item["timeout"]; ok {
		if timeout, ok := asIntValue(value); ok && timeout > 0 {
			config.Timeout = timeout
		} else {
			return nil, ValidationError{Message: "deployment.helmDirect.timeout must be positive"}
		}
	}
	if value, ok := item["wait"]; ok {
		config.Wait = asBoolValue(value)
	}
	if value, ok := item["createNamespace"]; ok {
		config.CreateNamespace = asBoolValue(value)
	}
	if value := asStringValue(sessionData["helmReleaseNamespaceMode"]); strings.TrimSpace(value) != "" && strings.TrimSpace(config.NamespaceMode) == "" {
		config.NamespaceMode = strings.TrimSpace(value)
	}
	if strings.TrimSpace(config.NamespaceMode) == "" {
		return nil, ValidationError{Message: "deployment.helmDirect.namespaceMode is required for helm_direct backend"}
	}
	if strings.TrimSpace(config.ReleaseNamePattern) == "" {
		return nil, ValidationError{Message: "deployment.helmDirect.releaseNamePattern is required for helm_direct backend"}
	}
	if strings.TrimSpace(config.NamespacePattern) == "" {
		return nil, ValidationError{Message: "deployment.helmDirect.namespacePattern is required for helm_direct backend"}
	}
	if strings.TrimSpace(config.ChartRef) == "" {
		return nil, ValidationError{Message: "deployment.helmDirect.chartRef is required for helm_direct backend"}
	}
	if strings.TrimSpace(config.ValuesOverrideStrategy) == "" {
		return nil, ValidationError{Message: "deployment.helmDirect.valuesOverrideStrategy is required for helm_direct backend"}
	}
	if strings.TrimSpace(config.ImageTagValuePath) == "" {
		return nil, ValidationError{Message: "deployment.helmDirect.imageTagValuePath is required for helm_direct backend"}
	}
	if config.Timeout <= 0 {
		return nil, ValidationError{Message: "deployment.helmDirect.timeout must be positive"}
	}
	return config, nil
}

func normalizeFluxCDConfig(raw any, sessionData map[string]any, project domain.Project) (*domain.ProjectFluxCDConfig, error) {
	item, ok := toMap(raw)
	if !ok {
		item = map[string]any{}
	}
	gitopsRepo := firstNonEmptyString(
		asStringValue(item["gitopsRepo"]),
		asStringValue(sessionData["gitopsRepo"]),
		asStringValue(sessionData["gitOpsRepo"]),
		asStringValue(sessionData["gitopsRepoUrl"]),
		asStringValue(sessionData["gitOpsRepoUrl"]),
		project.GitOpsRepo.URL,
		project.GitOpsRepositoryID,
	)
	gitopsPath := firstNonEmptyString(asStringValue(item["gitopsPath"]), asStringValue(sessionData["gitOpsOutputPath"]), asStringValue(sessionData["gitopsPath"]))
	fluxNamespace := firstNonEmptyString(asStringValue(item["fluxNamespace"]), asStringValue(sessionData["fluxNamespace"]))
	kustomizationName := firstNonEmptyString(
		asStringValue(item["kustomizationName"]),
		asStringValue(sessionData["kustomizationName"]),
		asStringValue(sessionData["fluxKustomizationRef"]),
	)
	commitMode := firstNonEmptyString(asStringValue(item["commitMode"]), asStringValue(sessionData["gitOpsCommitMode"]))

	return &domain.ProjectFluxCDConfig{
		GitopsRepo:        gitopsRepo,
		GitopsPath:        gitopsPath,
		FluxNamespace:     fluxNamespace,
		KustomizationName: kustomizationName,
		CommitMode:        commitMode,
	}, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func asIntValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case uint:
		return int(typed), true
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		return int(typed), true
	case uint64:
		return int(typed), true
	case float64:
		return int(typed), true
	case float32:
		return int(typed), true
	case string:
		timeoutValue := strings.TrimSpace(typed)
		if timeoutValue == "" {
			return 0, false
		}
		i, err := strconv.Atoi(timeoutValue)
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

func deploymentConfigToMap(config domain.ProjectDeploymentConfig) map[string]any {
	result := map[string]any{
		"backend": string(config.Backend),
	}
	if config.HelmDirect != nil {
		result["helmDirect"] = map[string]any{
			"namespaceMode":          config.HelmDirect.NamespaceMode,
			"namespacePattern":       config.HelmDirect.NamespacePattern,
			"releaseNamePattern":     config.HelmDirect.ReleaseNamePattern,
			"chartRef":               config.HelmDirect.ChartRef,
			"timeout":                config.HelmDirect.Timeout,
			"wait":                   config.HelmDirect.Wait,
			"createNamespace":        config.HelmDirect.CreateNamespace,
			"valuesOverrideStrategy": config.HelmDirect.ValuesOverrideStrategy,
			"imageTagValuePath":      config.HelmDirect.ImageTagValuePath,
		}
	}
	if config.FluxCD != nil {
		result["fluxcd"] = map[string]any{
			"gitopsRepo":        config.FluxCD.GitopsRepo,
			"gitopsPath":        config.FluxCD.GitopsPath,
			"fluxNamespace":     config.FluxCD.FluxNamespace,
			"kustomizationName": config.FluxCD.KustomizationName,
			"commitMode":        config.FluxCD.CommitMode,
		}
	}
	return result
}

func projectConfigProjectSummary(project domain.Project) map[string]any {
	return map[string]any{
		"id":                    project.ID,
		"name":                  project.Name,
		"productId":             project.ProductID,
		"appRepositoryId":       project.AppRepositoryID,
		"gitOpsRepositoryId":    project.GitOpsRepositoryID,
		"clusterId":             project.ClusterID,
		"gitRepo":               project.GitRepo,
		"gitOpsRepo":            project.GitOpsRepo,
		"baseEnvConfig":         project.BaseEnvConfig,
		"costPolicy":            project.CostPolicy,
		"webhookBranchFilters":  project.WebhookBranchFilters,
		"webhookLabels":         project.WebhookLabels,
		"webhookAllowDraftPRs":  project.WebhookAllowDraftPRs,
		"githubInstallationIds": project.GitHubInstallationIDs,
		"gitlabProjectIds":      project.GitLabProjectIDs,
	}
}

func splitProjectSecretStrategies(raw any) (map[string]any, map[string]any) {
	items, ok := toMap(raw)
	if !ok {
		return map[string]any{}, map[string]any{}
	}
	publicItems := make(map[string]any, len(items))
	encryptedSecrets := map[string]any{}
	for id, rawItem := range items {
		item, ok := toMap(rawItem)
		if !ok {
			continue
		}
		publicItem := map[string]any{}
		for key, value := range item {
			if key == "manualValueEncrypted" {
				if isEncryptedCredentialValue(value) {
					encryptedSecrets[id] = value
					publicItem["manualValueStored"] = true
					publicItem["manualValueMasked"] = true
					publicItem["manualValue"] = ""
				}
				continue
			}
			publicItem[key] = value
		}
		publicItems[id] = publicItem
	}
	return publicItems, encryptedSecrets
}

func publicProjectConfig(config domain.ProjectConfig) domain.ProjectConfig {
	config.SensitiveRefs = projectConfigSensitiveRefs(config.Sensitive)
	config.Sensitive = nil
	return config
}

func projectConfigSensitiveRefs(sensitive map[string]any) map[string]any {
	if len(sensitive) == 0 {
		return nil
	}
	refs := map[string]any{}
	if credentials, ok := toMap(sensitive["scmCredentials"]); ok {
		fields := make([]string, 0, len(credentials))
		for field := range credentials {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		refs["scmCredentials"] = map[string]any{
			"stored": true,
			"fields": fields,
		}
	}
	if secrets, ok := toMap(sensitive["manualSecrets"]); ok {
		ids := make([]string, 0, len(secrets))
		for id := range secrets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		refs["manualSecrets"] = map[string]any{
			"stored": true,
			"ids":    ids,
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	clone := make(map[string]any, len(input))
	for key, value := range input {
		clone[key] = value
	}
	return clone
}
