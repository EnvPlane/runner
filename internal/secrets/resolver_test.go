package secrets

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/envpilot/runner/internal/domain"
)

func TestResolverResolvesEnvSecret(t *testing.T) {
	t.Setenv("ENVPILOT_TEST_TOKEN", "env-token")
	resolver := NewResolver()

	value, err := resolver.Resolve(context.Background(), domain.SecretReference{
		ID:        "git-token",
		Provider:  "env",
		Reference: "ENVPILOT_TEST_TOKEN",
	})
	if err != nil {
		t.Fatalf("resolve env: %v", err)
	}
	if value != "env-token" {
		t.Fatalf("value = %q", value)
	}
}

func TestValidateDoesNotExposeSecretValue(t *testing.T) {
	t.Setenv("ENVPILOT_TEST_TOKEN", "super-secret-token")
	resolver := NewResolver()

	result := resolver.Validate(context.Background(), domain.SecretReference{
		ID:        "git-token",
		Provider:  "env",
		Reference: "ENVPILOT_TEST_TOKEN",
	})
	if !result.Valid {
		t.Fatalf("expected valid result: %+v", result)
	}
	if result.Message == "super-secret-token" {
		t.Fatal("validation exposed secret value")
	}
}

func TestResolverResolvesKubernetesSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/namespaces/platform/secrets/git-token" {
			t.Fatalf("path = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer service-account-token" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"token":"` + base64.StdEncoding.EncodeToString([]byte("k8s-token")) + `"}}`))
	}))
	defer server.Close()

	resolver := NewResolver(
		WithHTTPClient(server.Client()),
		WithKubernetesConfig(KubernetesConfig{
			APIURL:    server.URL,
			Token:     "service-account-token",
			Namespace: "default",
		}),
	)
	value, err := resolver.Resolve(context.Background(), domain.SecretReference{
		ID:        "git-token",
		Provider:  "kubernetes",
		Reference: "platform/git-token:token",
	})
	if err != nil {
		t.Fatalf("resolve kubernetes: %v", err)
	}
	if value != "k8s-token" {
		t.Fatalf("value = %q", value)
	}
}

func TestResolverResolvesExternalSecretsMaterializedKubernetesSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"password":"` + base64.StdEncoding.EncodeToString([]byte("external-token")) + `"}}`))
	}))
	defer server.Close()

	resolver := NewResolver(
		WithHTTPClient(server.Client()),
		WithKubernetesConfig(KubernetesConfig{APIURL: server.URL, Token: "token", Namespace: "platform"}),
	)
	value, err := resolver.Resolve(context.Background(), domain.SecretReference{
		ID:        "git-token",
		Provider:  "external-secrets",
		Reference: "git-token:password",
	})
	if err != nil {
		t.Fatalf("resolve external-secrets: %v", err)
	}
	if value != "external-token" {
		t.Fatalf("value = %q", value)
	}
}

func TestResolverResolvesVaultKVV2Secret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/secret/data/gitops" {
			t.Fatalf("path = %q", got)
		}
		if got := r.Header.Get("X-Vault-Token"); got != "vault-token" {
			t.Fatalf("vault token = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"token":"vault-secret"}}}`))
	}))
	defer server.Close()

	resolver := NewResolver(
		WithHTTPClient(server.Client()),
		WithVaultConfig(VaultConfig{Address: server.URL, Token: "vault-token"}),
	)
	value, err := resolver.Resolve(context.Background(), domain.SecretReference{
		ID:        "git-token",
		Provider:  "vault",
		Reference: "secret/data/gitops#token",
	})
	if err != nil {
		t.Fatalf("resolve vault: %v", err)
	}
	if value != "vault-secret" {
		t.Fatalf("value = %q", value)
	}
}

func TestKubernetesReferenceRequiresKey(t *testing.T) {
	if _, err := parseKubernetesReference("platform/git-token", "default"); err == nil {
		t.Fatal("expected missing key error")
	}
}
