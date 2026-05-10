package ttl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunCleanupCallsReconcileEndpoint(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":[{"id":"env-1"}]}`))
	}))
	defer server.Close()

	result, err := RunCleanup(context.Background(), CleanupConfig{
		ControlPlaneURL: server.URL + "/",
		Token:           "secret-token",
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("run cleanup: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotPath != "/api/v1/environments/reconcile" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].ID != "env-1" {
		t.Fatalf("deleted = %#v", result.Deleted)
	}
}

func TestRunCleanupRequiresControlPlaneURL(t *testing.T) {
	if _, err := RunCleanup(context.Background(), CleanupConfig{Timeout: time.Second}); err == nil {
		t.Fatal("expected missing control plane url error")
	}
}
