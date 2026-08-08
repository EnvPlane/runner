package store

import (
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/envpilot/runner/internal/domain"
)

func ApplyBootstrapTokenClaim(session domain.BootstrapSession, request BootstrapTokenClaimRequest) (domain.BootstrapSession, error) {
	projectID := strings.TrimSpace(request.ProjectID)
	if projectID == "" || strings.TrimSpace(request.TokenHash) == "" {
		return domain.BootstrapSession{}, ErrBootstrapTokenInvalid
	}
	if !strings.EqualFold(strings.TrimSpace(session.ProjectID), projectID) {
		return domain.BootstrapSession{}, ErrBootstrapSessionNotFound
	}
	if session.Data == nil {
		session.Data = map[string]any{}
	}
	if request.TokenProjectKey != "" && strings.TrimSpace(asStoreString(session.Data[request.TokenProjectKey])) != projectID {
		return domain.BootstrapSession{}, fmt.Errorf("%w: token project binding", ErrBootstrapTokenInvalid)
	}
	storedHash := strings.TrimSpace(asStoreString(session.Data[request.TokenHashKey]))
	if storedHash == "" || subtle.ConstantTimeCompare([]byte(storedHash), []byte(strings.TrimSpace(request.TokenHash))) != 1 {
		return domain.BootstrapSession{}, ErrBootstrapTokenInvalid
	}
	if strings.TrimSpace(asStoreString(session.Data[request.TokenUsedAtKey])) != "" {
		return domain.BootstrapSession{}, ErrBootstrapTokenAlreadyUsed
	}
	expiresAt := strings.TrimSpace(asStoreString(session.Data[request.TokenExpiresKey]))
	if expiresAt == "" {
		return domain.BootstrapSession{}, ErrBootstrapTokenExpired
	}
	parsedExpiresAt, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || time.Now().UTC().After(parsedExpiresAt) {
		return domain.BootstrapSession{}, ErrBootstrapTokenExpired
	}
	for key, expected := range request.Identity {
		key = strings.TrimSpace(key)
		expected = strings.TrimSpace(expected)
		if key == "" || expected == "" {
			continue
		}
		if existing := strings.TrimSpace(asStoreString(session.Data[key])); existing != "" && existing != expected {
			return domain.BootstrapSession{}, fmt.Errorf("%w: %s", ErrBootstrapIdentityMismatch, key)
		}
	}
	updated := cloneStoreBootstrapSession(session)
	if updated.Data == nil {
		updated.Data = map[string]any{}
	}
	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for key, value := range request.StepData {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		updated.Data[key] = value
	}
	if request.TokenUsedAtKey != "" {
		if strings.TrimSpace(asStoreString(updated.Data[request.TokenUsedAtKey])) == "" {
			updated.Data[request.TokenUsedAtKey] = now.Format(time.RFC3339Nano)
		}
	}
	updated.UpdatedAt = now
	return updated, nil
}

func cloneStoreBootstrapSession(session domain.BootstrapSession) domain.BootstrapSession {
	cloned := session
	if session.Data != nil {
		cloned.Data = make(map[string]any, len(session.Data))
		for key, value := range session.Data {
			cloned.Data[key] = value
		}
	}
	return cloned
}

func asStoreString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
