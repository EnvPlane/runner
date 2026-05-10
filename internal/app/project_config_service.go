package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"envpilot/internal/domain"
	"envpilot/internal/store"
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

	configData, sensitiveData := buildProjectConfigData(project, session)
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

func buildProjectConfigData(project domain.Project, session domain.BootstrapSession) (map[string]any, map[string]any) {
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

	config := map[string]any{
		"schemaVersion":        "v1",
		"project":              projectConfigProjectSummary(project),
		"bootstrapSessionId":   session.ID,
		"bootstrapSessionData": sessionData,
	}
	return config, sensitive
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
