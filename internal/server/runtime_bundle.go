package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/envpilot/contracts/domain"
	"github.com/envpilot/runner/internal/app"
	"github.com/envpilot/runner/internal/bootstrap"
)

const runtimeBundleFixedTimestamp = "2026-05-03T00:00:00Z"

var runtimeBundleRunnerAssets = map[string]string{
	"runner/runner-helm-values.yaml": "deploy/deploy/helm/envpilot-runner/values.yaml",
	"runner/deployment.yaml":         "deploy/deploy/helm/envpilot-runner/templates/deployment.yaml",
	"runner/rbac.yaml":               "deploy/deploy/helm/envpilot-runner/templates/rbac.yaml",
	"runner/serviceaccount.yaml":     "deploy/deploy/helm/envpilot-runner/templates/serviceaccount.yaml",
	"runner/NOTES.txt":               "deploy/deploy/helm/envpilot-runner/templates/NOTES.txt",
}

type projectRuntimeBundleFile struct {
	path    string
	content []byte
}

func (s *Server) getProjectRuntimeBundle(w http.ResponseWriter, r *http.Request) {
	project, err := s.projects.GetProject(r.PathValue("id"))
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.canAccessProject(r, project) {
		writeError(w, http.StatusForbidden, fmt.Errorf("project access denied"))
		return
	}
	if s.projectConfigs == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("project config storage is not configured"))
		return
	}
	config, err := s.projectConfigs.LatestWithSensitive(project.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}

	templates := runtimeBundleManifestTemplates(config)
	if len(templates) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("compiled manifest templates are not available. run compile first"))
		return
	}

	files := make([]projectRuntimeBundleFile, 0, 1+len(templates)+len(runtimeBundleRunnerAssets))
	runnerTemplate, err := runtimeBundleRunnerTemplateFiles()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	files = append(files, runnerTemplate...)

	for _, item := range templates {
		if strings.TrimSpace(item.YAML) == "" {
			continue
		}
		files = append(files, projectRuntimeBundleFile{
			path:    runtimeBundleTemplatePath(item),
			content: []byte(strings.TrimSuffix(item.YAML, "\n") + "\n"),
		})
	}

	configFile, err := runtimeBundleConfigFile(config)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	files = append(files, projectRuntimeBundleFile{
		path:    "project-config.enc.yaml",
		content: []byte(bootstrap.MarshalDeterministicYAML(configFile)),
	})

	sort.Slice(files, func(i, j int) bool {
		return files[i].path < files[j].path
	})

	var zipBuffer bytes.Buffer
	zipWriter := zip.NewWriter(&zipBuffer)
	fixedTimestamp, _ := time.Parse(time.RFC3339, runtimeBundleFixedTimestamp)
	for _, item := range files {
		header := &zip.FileHeader{
			Name:     item.path,
			Method:   zip.Deflate,
			Modified: fixedTimestamp,
		}
		header.SetMode(0o644)
		entry, err := zipWriter.CreateHeader(header)
		if err != nil {
			_ = zipWriter.Close()
			writeMappedError(w, err)
			return
		}
		if _, err := entry.Write(item.content); err != nil {
			_ = zipWriter.Close()
			writeMappedError(w, err)
			return
		}
	}
	if err := zipWriter.Close(); err != nil {
		writeMappedError(w, err)
		return
	}

	filename := fmt.Sprintf("%s-runtime-bundle.zip", normalizeSettingsID(project.ID))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, &zipBuffer)
}

func runtimeBundleConfigFile(config domain.ProjectConfig) (map[string]any, error) {
	encryptedSensitive, err := runtimeBundleEncryptedSensitive(config.Sensitive)
	if err != nil {
		return nil, err
	}
	configFile := map[string]any{
		"projectId": config.ProjectID,
		"version":   config.Version,
		"config":    config.Config,
		"createdAt": config.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if len(encryptedSensitive) > 0 {
		configFile["encryptedSensitive"] = encryptedSensitive
	}
	if createdBy := strings.TrimSpace(config.CreatedBy); createdBy != "" {
		configFile["createdBy"] = createdBy
	}
	return configFile, nil
}

func runtimeBundleEncryptedSensitive(input map[string]any) (map[string]any, error) {
	if len(input) == 0 {
		return nil, nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		safeValue, err := runtimeBundleEncryptedSensitiveValue(value)
		if err != nil {
			return nil, fmt.Errorf("sensitive payload %q is not encrypted or externally referenced: %w", key, err)
		}
		output[key] = safeValue
	}
	return output, nil
}

func runtimeBundleEncryptedSensitiveValue(value any) (any, error) {
	switch typed := value.(type) {
	case app.EncryptedCredential:
		return map[string]any{
			"type":       typed.Type,
			"version":    typed.Version,
			"algorithm":  typed.Algorithm,
			"key_id":     typed.KeyID,
			"nonce":      typed.Nonce,
			"ciphertext": typed.Ciphertext,
		}, nil
	case map[string]any:
		if runtimeBundleIsEncryptedEnvelope(typed) || runtimeBundleIsExternalSecretReference(typed) {
			return typed, nil
		}
		output := make(map[string]any, len(typed))
		for key, item := range typed {
			safeItem, err := runtimeBundleEncryptedSensitiveValue(item)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			output[key] = safeItem
		}
		return output, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected %T", value)
	}
}

func runtimeBundleIsEncryptedEnvelope(value map[string]any) bool {
	return strings.TrimSpace(asString(value["type"])) == "envpilot.credential.v1" &&
		strings.TrimSpace(asString(value["algorithm"])) == "AES-256-GCM" &&
		strings.TrimSpace(asString(value["nonce"])) != "" &&
		strings.TrimSpace(asString(value["ciphertext"])) != ""
}

func runtimeBundleIsExternalSecretReference(value map[string]any) bool {
	kind := strings.TrimSpace(asString(value["kind"]))
	provider := strings.TrimSpace(asString(value["provider"]))
	ref := strings.TrimSpace(firstNonEmpty(asString(value["ref"]), asString(value["reference"]), asString(value["secretRef"])))
	return strings.EqualFold(kind, "ExternalSecret") && provider != "" && ref != ""
}

func runtimeBundleManifestTemplates(config domain.ProjectConfig) []bootstrap.ManifestTemplate {
	rawSessionData, ok := asStringAnyMap(config.Config["bootstrapSessionData"])
	if !ok {
		return nil
	}
	rawTemplates, _ := rawSessionData[bootstrapManifestTemplatesKey]
	templates := asBootstrapManifestTemplates(rawTemplates)
	return templates
}

func runtimeBundleTemplatePath(item bootstrap.ManifestTemplate) string {
	namespace := strings.TrimSpace(item.Namespace)
	if namespace == "" {
		namespace = "cluster"
	}
	kind := strings.TrimSpace(strings.ToLower(item.Kind))
	if kind == "" {
		kind = "resource"
	}
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = "resource"
	}
	namespace = sanitizePathComponent(namespace)
	kind = sanitizePathComponent(kind)
	name = sanitizePathComponent(name)
	return path.Join("templates", namespace, kind+"-"+name+".yaml")
}

func sanitizePathComponent(value string) string {
	return strings.NewReplacer("..", "", "/", "-", "\\", "-", ":", "-", "*", "-", "?", "-", "\"", "-", "<", "-", ">", "-", "|", "-", " ", "-").Replace(strings.TrimSpace(value))
}

func runtimeBundleRunnerTemplateFiles() ([]projectRuntimeBundleFile, error) {
	files := make([]projectRuntimeBundleFile, 0, len(runtimeBundleRunnerAssets))
	for arcPath, fsPath := range runtimeBundleRunnerAssets {
		content, err := readRuntimeBundleRunnerTemplate(fsPath)
		if err != nil {
			return nil, err
		}
		files = append(files, projectRuntimeBundleFile{
			path:    arcPath,
			content: content,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].path < files[j].path
	})
	return files, nil
}

func readRuntimeBundleRunnerTemplate(pathName string) ([]byte, error) {
	candidates := []string{
		filepath.Clean(pathName),
		filepath.Clean(filepath.Join("..", pathName)),
		filepath.Clean(filepath.Join("..", "..", pathName)),
		filepath.Clean(filepath.Join("..", "..", "..", pathName)),
	}
	for _, candidate := range candidates {
		content, err := os.ReadFile(candidate)
		if err == nil {
			return content, nil
		}
	}
	return nil, fmt.Errorf("runtime bundle runner template not found in any candidate: %s", strings.Join(candidates, ", "))
}
