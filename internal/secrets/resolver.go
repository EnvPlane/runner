package secrets

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"envpilot/internal/domain"
)

type Resolver struct {
	client     *http.Client
	kubernetes KubernetesConfig
	vault      VaultConfig
}

type KubernetesConfig struct {
	APIURL    string
	Token     string
	Namespace string
	CACert    string
}

type VaultConfig struct {
	Address string
	Token   string
}

type ValidationResult struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Scope    string `json:"scope,omitempty"`
	Valid    bool   `json:"valid"`
	Message  string `json:"message"`
}

type Option func(*Resolver)

func NewResolver(options ...Option) *Resolver {
	resolver := &Resolver{
		kubernetes: KubernetesConfig{
			APIURL:    kubernetesAPIURLFromEnv(),
			Token:     firstNonEmpty(os.Getenv("KUBERNETES_BEARER_TOKEN"), readTrimmed("/var/run/secrets/kubernetes.io/serviceaccount/token")),
			Namespace: firstNonEmpty(os.Getenv("KUBERNETES_NAMESPACE"), readTrimmed("/var/run/secrets/kubernetes.io/serviceaccount/namespace"), "default"),
			CACert:    firstNonEmpty(os.Getenv("KUBERNETES_CA_CERT"), "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
		},
		vault: VaultConfig{
			Address: strings.TrimRight(strings.TrimSpace(os.Getenv("VAULT_ADDR")), "/"),
			Token:   strings.TrimSpace(os.Getenv("VAULT_TOKEN")),
		},
	}
	for _, option := range options {
		option(resolver)
	}
	if resolver.client == nil {
		client, err := HTTPClientWithCA(resolver.kubernetes.CACert)
		if err == nil {
			resolver.client = client
		} else {
			resolver.client = &http.Client{Timeout: 10 * time.Second}
		}
	}
	return resolver
}

func WithHTTPClient(client *http.Client) Option {
	return func(resolver *Resolver) {
		resolver.client = client
	}
}

func WithKubernetesConfig(config KubernetesConfig) Option {
	return func(resolver *Resolver) {
		resolver.kubernetes = config
	}
}

func WithVaultConfig(config VaultConfig) Option {
	return func(resolver *Resolver) {
		config.Address = strings.TrimRight(strings.TrimSpace(config.Address), "/")
		resolver.vault = config
	}
}

func (r *Resolver) Resolve(ctx context.Context, secret domain.SecretReference) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(secret.Provider))
	switch provider {
	case "", "env", "environment":
		return resolveEnv(secret)
	case "kubernetes", "k8s":
		return r.resolveKubernetes(ctx, secret)
	case "external-secrets", "external-secret", "externalsecrets", "externalsecret":
		return r.resolveKubernetes(ctx, secret)
	case "vault", "hashicorp-vault":
		return r.resolveVault(ctx, secret)
	default:
		return "", fmt.Errorf("secret reference provider %q is not supported", secret.Provider)
	}
}

func (r *Resolver) Validate(ctx context.Context, secret domain.SecretReference) ValidationResult {
	result := ValidationResult{
		ID:       strings.TrimSpace(secret.ID),
		Provider: strings.TrimSpace(secret.Provider),
		Scope:    strings.TrimSpace(secret.Scope),
	}
	if err := validateReferenceFormat(secret); err != nil {
		result.Message = err.Error()
		return result
	}
	if _, err := r.Resolve(ctx, secret); err != nil {
		result.Message = safeValidationMessage(secret.Provider)
		return result
	}
	result.Valid = true
	result.Message = "secret reference resolved"
	return result
}

func validateReferenceFormat(secret domain.SecretReference) error {
	provider := strings.ToLower(strings.TrimSpace(secret.Provider))
	reference := strings.TrimSpace(secret.Reference)
	if reference == "" {
		return fmt.Errorf("secret reference %q has empty reference", secret.ID)
	}
	switch provider {
	case "", "env", "environment":
		return nil
	case "kubernetes", "k8s", "external-secrets", "external-secret", "externalsecrets", "externalsecret":
		_, err := parseKubernetesReference(reference, "default")
		return err
	case "vault", "hashicorp-vault":
		_, _, err := parseVaultReference(reference)
		return err
	default:
		return fmt.Errorf("secret reference provider %q is not supported", secret.Provider)
	}
}

func safeValidationMessage(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "env", "environment":
		return "environment variable is not configured or is empty"
	case "kubernetes", "k8s", "external-secrets", "external-secret", "externalsecrets", "externalsecret":
		return "kubernetes secret could not be resolved with current credentials"
	case "vault", "hashicorp-vault":
		return "vault secret could not be resolved with current credentials"
	default:
		return "secret reference could not be resolved"
	}
}

func resolveEnv(secret domain.SecretReference) (string, error) {
	reference := strings.TrimSpace(secret.Reference)
	if reference == "" {
		return "", fmt.Errorf("secret reference %q has empty reference", secret.ID)
	}
	value := strings.TrimSpace(os.Getenv(reference))
	if value == "" {
		return "", fmt.Errorf("secret reference %q points to empty env var %q", secret.ID, reference)
	}
	return value, nil
}

func (r *Resolver) resolveKubernetes(ctx context.Context, secret domain.SecretReference) (string, error) {
	if strings.TrimSpace(r.kubernetes.APIURL) == "" {
		return "", fmt.Errorf("kubernetes api url is not configured")
	}
	if strings.TrimSpace(r.kubernetes.Token) == "" {
		return "", fmt.Errorf("kubernetes bearer token is not configured")
	}
	ref, err := parseKubernetesReference(secret.Reference, r.kubernetes.Namespace)
	if err != nil {
		return "", fmt.Errorf("secret reference %q: %w", secret.ID, err)
	}
	endpoint := strings.TrimRight(r.kubernetes.APIURL, "/") + "/api/v1/namespaces/" + url.PathEscape(ref.Namespace) + "/secrets/" + url.PathEscape(ref.Name)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+r.kubernetes.Token)
	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return "", fmt.Errorf("kubernetes secret read failed: status=%d body=%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", err
	}
	encoded, ok := payload.Data[ref.Key]
	if !ok || encoded == "" {
		return "", fmt.Errorf("kubernetes secret %s/%s does not contain key %q", ref.Namespace, ref.Name, ref.Key)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode kubernetes secret key %q: %w", ref.Key, err)
	}
	value := strings.TrimSpace(string(decoded))
	if value == "" {
		return "", fmt.Errorf("kubernetes secret %s/%s key %q is empty", ref.Namespace, ref.Name, ref.Key)
	}
	return value, nil
}

func (r *Resolver) resolveVault(ctx context.Context, secret domain.SecretReference) (string, error) {
	if strings.TrimSpace(r.vault.Address) == "" {
		return "", fmt.Errorf("vault address is not configured")
	}
	if strings.TrimSpace(r.vault.Token) == "" {
		return "", fmt.Errorf("vault token is not configured")
	}
	secretPath, field, err := parseVaultReference(secret.Reference)
	if err != nil {
		return "", fmt.Errorf("secret reference %q: %w", secret.ID, err)
	}
	endpoint := strings.TrimRight(r.vault.Address, "/") + "/v1/" + strings.TrimLeft(path.Clean(secretPath), "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("X-Vault-Token", r.vault.Token)
	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return "", fmt.Errorf("vault secret read failed: status=%d body=%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", err
	}
	value, ok := vaultFieldValue(payload.Data, field)
	if !ok {
		return "", fmt.Errorf("vault secret %q does not contain field %q", secretPath, field)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("vault secret %q field %q is empty", secretPath, field)
	}
	return value, nil
}

type kubernetesSecretRef struct {
	Namespace string
	Name      string
	Key       string
}

func parseKubernetesReference(reference string, defaultNamespace string) (kubernetesSecretRef, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return kubernetesSecretRef{}, fmt.Errorf("kubernetes reference is required")
	}
	namePart, key := splitReferenceKey(reference)
	if key == "" {
		return kubernetesSecretRef{}, fmt.Errorf("kubernetes reference must use namespace/name:key or name:key")
	}
	parts := strings.Split(strings.Trim(namePart, "/"), "/")
	if len(parts) > 2 || len(parts) == 0 || parts[len(parts)-1] == "" {
		return kubernetesSecretRef{}, fmt.Errorf("invalid kubernetes reference %q", reference)
	}
	namespace := strings.TrimSpace(defaultNamespace)
	name := parts[0]
	if len(parts) == 2 {
		namespace = parts[0]
		name = parts[1]
	}
	if namespace == "" {
		namespace = "default"
	}
	return kubernetesSecretRef{Namespace: namespace, Name: name, Key: key}, nil
}

func parseVaultReference(reference string) (string, string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", "", fmt.Errorf("vault reference is required")
	}
	secretPath, field := splitReferenceKey(reference)
	if strings.TrimSpace(secretPath) == "" {
		return "", "", fmt.Errorf("vault path is required")
	}
	return strings.Trim(secretPath, "/"), field, nil
}

func splitReferenceKey(reference string) (string, string) {
	if before, after, ok := strings.Cut(reference, "#"); ok {
		return strings.TrimSpace(before), strings.TrimSpace(after)
	}
	index := strings.LastIndex(reference, ":")
	if index < 0 {
		return strings.TrimSpace(reference), ""
	}
	return strings.TrimSpace(reference[:index]), strings.TrimSpace(reference[index+1:])
}

func vaultFieldValue(data map[string]any, field string) (string, bool) {
	if nested, ok := data["data"].(map[string]any); ok {
		data = nested
	}
	if field != "" {
		return stringValue(data[field])
	}
	for _, candidate := range []string{"value", "token", "password"} {
		if value, ok := stringValue(data[candidate]); ok {
			return value, true
		}
	}
	if len(data) == 1 {
		for _, raw := range data {
			return stringValue(raw)
		}
	}
	return "", false
}

func stringValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	default:
		return "", false
	}
}

func kubernetesAPIURLFromEnv() string {
	if value := strings.TrimSpace(os.Getenv("KUBERNETES_API_URL")); value != "" {
		return strings.TrimRight(value, "/")
	}
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if host == "" || port == "" {
		return ""
	}
	return "https://" + host + ":" + port
}

func readTrimmed(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func HTTPClientWithCA(caPath string) (*http.Client, error) {
	caPath = strings.TrimSpace(caPath)
	if caPath == "" {
		return &http.Client{Timeout: 10 * time.Second}, nil
	}
	content, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(content) {
		return nil, fmt.Errorf("load kubernetes ca bundle")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}, nil
}
