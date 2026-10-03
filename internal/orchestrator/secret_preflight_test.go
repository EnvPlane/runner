package orchestrator

import (
	"context"
	"testing"
)

func TestSecretOwnershipPreflightNeverAppliesForeignSecret(t *testing.T) {
	for _, metadata := range []string{`{"labels":{"app.kubernetes.io/managed-by":"envplane"}}`, `{"labels":{"app.kubernetes.io/managed-by":"Helm"},"annotations":{"meta.helm.sh/release-name":"foreign","meta.helm.sh/release-namespace":"feature"}}`} {
		applied := false
		e := &CLIHelmExecutor{runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "kubectl" {
				return []byte(metadata), nil
			}
			if args[0] == "template" {
				return []byte("kind: Secret\nmetadata:\n  name: backend-secret\n"), nil
			}
			applied = true
			return nil, nil
		}}
		if e.UpgradeInstall(context.Background(), HelmUpgradeOptions{ReleaseName: "own", ChartRef: "charts/app", Namespace: "feature"}) == nil || applied {
			t.Fatal("foreign Secret reached Helm apply")
		}
	}
}

func TestSecretOwnershipPreflightAllowsMissingOrSameRelease(t *testing.T) {
	for _, metadata := range []string{"", `{"labels":{"app.kubernetes.io/managed-by":"Helm"},"annotations":{"meta.helm.sh/release-name":"own","meta.helm.sh/release-namespace":"feature"}}`} {
		applied := false
		e := &CLIHelmExecutor{runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "kubectl" {
				return []byte(metadata), nil
			}
			if args[0] == "template" {
				return []byte("kind: Secret\nmetadata:\n  name: backend-secret\n"), nil
			}
			applied = true
			return nil, nil
		}}
		if err := e.UpgradeInstall(context.Background(), HelmUpgradeOptions{ReleaseName: "own", ChartRef: "charts/app", Namespace: "feature"}); err != nil || !applied {
			t.Fatalf("compatible Secret blocked: %v", err)
		}
	}
}
