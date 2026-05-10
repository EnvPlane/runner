package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"envpilot/internal/domain"
	"envpilot/internal/store"
)

type BootstrapSessionService struct {
	store     store.BootstrapSessionStore
	encryptor CredentialEncryptor
	now       func() time.Time
}

type BootstrapSessionUpdate struct {
	Status        *string        `json:"status,omitempty"`
	CurrentStep   *int           `json:"current_step,omitempty"`
	StepData      map[string]any `json:"stepData,omitempty"`
	StepDataSnake map[string]any `json:"step_data,omitempty"`
}

const (
	bootstrapSecretStrategiesField      = "secretStrategies"
	bootstrapSecretStrategiesFieldSnake = "secret_strategies"
	secretManualValuePlaceholder        = "********"
)

func NewBootstrapSessionService(store store.BootstrapSessionStore) *BootstrapSessionService {
	return NewBootstrapSessionServiceWithEncryptor(store, MustNewAESGCMCredentialEncryptor("envpilot-local-development-key", "local"))
}

func NewBootstrapSessionServiceWithEncryptor(store store.BootstrapSessionStore, encryptor CredentialEncryptor) *BootstrapSessionService {
	if encryptor == nil {
		encryptor = MustNewAESGCMCredentialEncryptor("envpilot-local-development-key", "local")
	}
	return &BootstrapSessionService{
		store:     store,
		encryptor: encryptor,
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func (s *BootstrapSessionService) Get(projectID string) (domain.BootstrapSession, error) {
	session, err := s.store.GetByProject(projectID)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	return s.publicSession(session), nil
}

func (s *BootstrapSessionService) GetStored(projectID string) (domain.BootstrapSession, error) {
	return s.store.GetByProject(projectID)
}

func (s *BootstrapSessionService) Create(projectID, createdBy string) (domain.BootstrapSession, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.BootstrapSession{}, ValidationError{Message: "project id is required"}
	}
	existing, err := s.store.GetByProject(projectID)
	if err == nil {
		if strings.TrimSpace(createdBy) != "" {
			existing.CreatedBy = strings.TrimSpace(createdBy)
			existing.UpdatedAt = s.now()
			_ = s.store.Save(existing)
		}
		return s.publicSession(existing), nil
	}
	if !errors.Is(err, store.ErrBootstrapSessionNotFound) {
		return domain.BootstrapSession{}, err
	}
	timestamp := s.now()
	session := domain.BootstrapSession{
		ID:          newBootstrapSessionID(projectID, timestamp),
		ProjectID:   projectID,
		CurrentStep: 0,
		Status:      domain.BootstrapSessionStatusDraft,
		CreatedBy:   strings.TrimSpace(createdBy),
		Data:        map[string]any{},
		CreatedAt:   timestamp,
		UpdatedAt:   timestamp,
	}
	if err := s.store.Save(session); err != nil {
		return domain.BootstrapSession{}, err
	}
	return s.publicSession(session), nil
}

func (s *BootstrapSessionService) Update(projectID string, request BootstrapSessionUpdate) (domain.BootstrapSession, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return domain.BootstrapSession{}, ValidationError{Message: "project id is required"}
	}
	session, err := s.store.GetByProject(projectID)
	if err != nil {
		return domain.BootstrapSession{}, err
	}

	if request.Status != nil {
		status := domain.BootstrapSessionStatus(strings.ToLower(strings.TrimSpace(*request.Status)))
		if err := validateBootstrapSessionStatus(status); err != nil {
			return domain.BootstrapSession{}, err
		}
		session.Status = status
	}

	if request.CurrentStep != nil {
		if *request.CurrentStep < 0 {
			return domain.BootstrapSession{}, ValidationError{Message: "current step must be non-negative"}
		}
		session.CurrentStep = *request.CurrentStep
	}

	stepData := bootstrapSessionStepData(request)
	if stepData != nil {
		if session.Data == nil {
			session.Data = map[string]any{}
		}
		for key, value := range stepData {
			normalized := strings.TrimSpace(key)
			if normalized == "" {
				continue
			}
			if isBootstrapSecretStrategiesField(normalized) {
				existing := session.Data[bootstrapSecretStrategiesField]
				if existing == nil {
					existing = session.Data[bootstrapSecretStrategiesFieldSnake]
				}
				normalizedStrategies, err := s.normalizeBootstrapSecretStrategies(value, existing)
				if err != nil {
					return domain.BootstrapSession{}, err
				}
				session.Data[bootstrapSecretStrategiesField] = normalizedStrategies
				delete(session.Data, bootstrapSecretStrategiesFieldSnake)
				continue
			}
			if isBootstrapCredentialField(normalized) {
				credential, ok := value.(string)
				if !ok || strings.TrimSpace(credential) == "" {
					continue
				}
				encrypted, err := s.encryptCredentialValue(strings.TrimSpace(credential))
				if err != nil {
					return domain.BootstrapSession{}, err
				}
				session.Data[normalized] = encrypted
				continue
			}
			session.Data[normalized] = value
		}
	}
	session.UpdatedAt = s.now()
	if err := s.store.Save(session); err != nil {
		return domain.BootstrapSession{}, err
	}
	return s.publicSession(session), nil
}

func (s *BootstrapSessionService) ClaimBootstrapToken(request store.BootstrapTokenClaimRequest) (domain.BootstrapSession, error) {
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	if request.ProjectID == "" {
		return domain.BootstrapSession{}, ValidationError{Message: "project id is required"}
	}
	if request.Now.IsZero() {
		request.Now = s.now()
	}
	session, err := s.store.ClaimBootstrapToken(request)
	if err != nil {
		return domain.BootstrapSession{}, err
	}
	return s.publicSession(session), nil
}

func BootstrapSessionCredentialFields(request BootstrapSessionUpdate) []string {
	stepData := bootstrapSessionStepData(request)
	fields := []string{}
	for key, value := range stepData {
		normalized := strings.TrimSpace(key)
		if !isBootstrapCredentialField(normalized) {
			continue
		}
		credential, ok := value.(string)
		if ok && strings.TrimSpace(credential) != "" {
			fields = append(fields, normalized)
		}
	}
	return fields
}

func BootstrapSessionSecretStrategyFields(request BootstrapSessionUpdate) []string {
	stepData := bootstrapSessionStepData(request)
	if stepData == nil {
		return nil
	}
	var raw any
	if value, ok := stepData[bootstrapSecretStrategiesField]; ok {
		raw = value
	} else if value, ok := stepData[bootstrapSecretStrategiesFieldSnake]; ok {
		raw = value
	} else {
		return nil
	}
	items, ok := toMap(raw)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(items))
	for key := range items {
		id := strings.TrimSpace(key)
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *BootstrapSessionService) encryptCredentialValue(value string) (map[string]any, error) {
	encrypted, err := s.encryptor.EncryptCredential(context.Background(), []byte(value))
	if err != nil {
		return nil, err
	}
	return encryptedCredentialToMap(encrypted)
}

func (s *BootstrapSessionService) publicSession(session domain.BootstrapSession) domain.BootstrapSession {
	if session.Data == nil {
		session.Data = map[string]any{}
		return session
	}
	publicData := make(map[string]any, len(session.Data))
	for key, value := range session.Data {
		if isBootstrapSecretStrategiesField(key) {
			publicData[bootstrapSecretStrategiesField] = sanitizePublicSecretStrategies(value)
			continue
		}
		if isBootstrapCredentialField(key) && isEncryptedCredentialValue(value) {
			publicData[key] = map[string]any{
				"stored": true,
				"masked": true,
			}
			continue
		}
		publicData[key] = value
	}
	session.Data = publicData
	return session
}

func encryptedCredentialToMap(encrypted EncryptedCredential) (map[string]any, error) {
	payload, err := json.Marshal(encrypted)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func encryptedCredentialFromValue(value any) (EncryptedCredential, bool) {
	payload, err := json.Marshal(value)
	if err != nil {
		return EncryptedCredential{}, false
	}
	var encrypted EncryptedCredential
	if err := json.Unmarshal(payload, &encrypted); err != nil {
		return EncryptedCredential{}, false
	}
	if encrypted.Type != encryptedCredentialEnvelopeType || encrypted.Ciphertext == "" || encrypted.Nonce == "" {
		return EncryptedCredential{}, false
	}
	return encrypted, true
}

func isEncryptedCredentialValue(value any) bool {
	_, ok := encryptedCredentialFromValue(value)
	return ok
}

func isBootstrapCredentialField(field string) bool {
	switch strings.TrimSpace(field) {
	case "oauthToken", "oauth_token",
		"appToken", "app_token",
		"deployToken", "deploy_token",
		"sshPrivateKey", "ssh_private_key":
		return true
	default:
		return false
	}
}

func isBootstrapSecretStrategiesField(field string) bool {
	switch strings.TrimSpace(field) {
	case bootstrapSecretStrategiesField, bootstrapSecretStrategiesFieldSnake:
		return true
	default:
		return false
	}
}

func bootstrapSessionStepData(request BootstrapSessionUpdate) map[string]any {
	if request.StepData != nil {
		return request.StepData
	}
	return request.StepDataSnake
}

func (s *BootstrapSessionService) normalizeBootstrapSecretStrategies(raw any, existing any) (map[string]any, error) {
	items, ok := toMap(raw)
	if !ok {
		return nil, ValidationError{Message: "secret strategies must be an object"}
	}
	existingItems, _ := toMap(existing)
	normalized := make(map[string]any, len(items))
	for rawID, rawItem := range items {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		itemMap, ok := toMap(rawItem)
		if !ok {
			return nil, ValidationError{Message: fmt.Sprintf("secret strategy %q must be an object", id)}
		}
		strategy := strings.TrimSpace(asStringValue(itemMap["strategy"]))
		required := asBoolValue(itemMap["required"])
		if required && strategy == "" {
			return nil, ValidationError{Message: fmt.Sprintf("required secret %q must have a strategy", id)}
		}
		if strategy != "" {
			switch strategy {
			case "reference existing secret", "external secret", "encrypted clone", "manual input":
			default:
				return nil, ValidationError{Message: fmt.Sprintf("secret %q has unsupported strategy %q", id, strategy)}
			}
		}

		existingEntry, _ := toMap(existingItems[id])
		entry := map[string]any{
			"strategy": strategy,
			"required": required,
		}
		for _, field := range []string{"backend", "reference", "secretName", "namespace", "service", "serviceId", "container", "variable", "source"} {
			value := strings.TrimSpace(asStringValue(itemMap[field]))
			if value == "" && existingEntry != nil {
				value = strings.TrimSpace(asStringValue(existingEntry[field]))
			}
			if value != "" {
				entry[field] = value
			}
		}

		manualValue := strings.TrimSpace(asStringValue(itemMap["manualValue"]))
		if manualValue == "" {
			manualValue = strings.TrimSpace(asStringValue(itemMap["manual_value"]))
		}
		if strategy == "manual input" {
			switch {
			case manualValue != "" && manualValue != secretManualValuePlaceholder:
				encrypted, err := s.encryptCredentialValue(manualValue)
				if err != nil {
					return nil, err
				}
				entry["manualValueEncrypted"] = encrypted
				entry["manualValueStored"] = true
			case existingEntry != nil && isEncryptedCredentialValue(existingEntry["manualValueEncrypted"]):
				entry["manualValueEncrypted"] = existingEntry["manualValueEncrypted"]
				entry["manualValueStored"] = true
			default:
				if required {
					return nil, ValidationError{Message: fmt.Sprintf("required secret %q with manual input strategy must include a value", id)}
				}
			}
		}
		if strategy == "external secret" && strings.TrimSpace(asStringValue(entry["backend"])) == "" {
			entry["backend"] = "vault"
		}
		if strategy == "reference existing secret" && strings.TrimSpace(asStringValue(entry["reference"])) == "" {
			value := strings.TrimSpace(asStringValue(entry["secretName"]))
			if value != "" {
				entry["reference"] = value
			}
		}
		normalized[id] = entry
	}
	return normalized, nil
}

func sanitizePublicSecretStrategies(raw any) map[string]any {
	items, ok := toMap(raw)
	if !ok {
		return map[string]any{}
	}
	publicItems := make(map[string]any, len(items))
	for id, rawItem := range items {
		item, ok := toMap(rawItem)
		if !ok {
			continue
		}
		publicItem := map[string]any{}
		for key, value := range item {
			if key == "manualValueEncrypted" {
				if isEncryptedCredentialValue(value) {
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
	return publicItems
}

func toMap(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	if data, ok := value.(map[string]any); ok {
		return data, true
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, false
	}
	return data, true
}

func asStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func asBoolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized == "true" || normalized == "1" || normalized == "yes"
	case float64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return false
	}
}

func validateBootstrapSessionStatus(status domain.BootstrapSessionStatus) error {
	switch status {
	case domain.BootstrapSessionStatusDraft,
		domain.BootstrapSessionStatusScanning,
		domain.BootstrapSessionStatusReviewed,
		domain.BootstrapSessionStatusCompiled,
		domain.BootstrapSessionStatusDeployed:
		return nil
	default:
		return ValidationError{Message: fmt.Sprintf("invalid bootstrap session status: %q", status)}
	}
}

func newBootstrapSessionID(projectID string, now time.Time) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = "project"
	}
	nowUnix := now.UnixNano()
	if nowUnix <= 0 {
		nowUnix = time.Now().UTC().UnixNano()
	}
	return projectID + "-bs-" + fmt.Sprintf("%d", nowUnix)
}
