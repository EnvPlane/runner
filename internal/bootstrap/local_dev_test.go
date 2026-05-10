package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testRepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ".."
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func TestLocalDevEnvironmentContract(t *testing.T) {
	compose, err := os.ReadFile(filepath.Join(testRepoRoot(), "bootstrap", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	composeText := string(compose)
	for _, expected := range []string{
		"postgres:",
		"redis:",
		"api:",
		"postgres:16-alpine",
		"redis:7-alpine",
		"golang:1.25-alpine",
		`"go", "run", "./apps/api"`,
		"ENVPILOT_DATABASE_URL",
		"ENVPILOT_POSTGRES_MIGRATIONS_DIR",
		"ENVPILOT_REDIS_URL",
		"8080:8080",
	} {
		if !strings.Contains(composeText, expected) {
			t.Fatalf("docker-compose.yml does not contain %q", expected)
		}
	}

	makefile, err := os.ReadFile(filepath.Join(testRepoRoot(), "bootstrap", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if !strings.Contains(string(makefile), "dev:") || !strings.Contains(string(makefile), "docker compose up --build") {
		t.Fatal("Makefile must define make dev using docker compose")
	}
}
