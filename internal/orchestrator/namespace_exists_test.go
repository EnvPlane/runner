package orchestrator

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

func TestNamespaceExistsRequiresSuccessfulExactLookup(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		exists       bool
		wantErr      bool
	}{
		{name: "absent"},
		{name: "present", output: `{"metadata":{"name":"envplane-pr-feature"}}`, exists: true},
		{name: "forbidden", err: errors.New("forbidden")},
		{name: "network", err: errors.New("connection refused")},
		{name: "malformed", output: "warning-only", wantErr: true},
		{name: "wrong identity", output: `{"metadata":{"name":"another-namespace"}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &CLIHelmExecutor{runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "kubectl" || !reflect.DeepEqual(args, []string{"get", "namespace", "envplane-pr-feature", "-o", "json", "--ignore-not-found"}) {
					t.Fatalf("unexpected lookup %s %v", name, args)
				}
				return []byte(tc.output), tc.err
			}}
			exists, err := executor.NamespaceExists(context.Background(), "envplane-pr-feature")
			if exists != tc.exists || (err != nil) != (tc.err != nil || tc.wantErr) {
				t.Fatalf("exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestNamespaceLookupUsesSeparatedStdout(t *testing.T) {
	executor := &CLIHelmExecutor{
		runCommand: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("combined warning stderr must not be parsed as namespace JSON")
			return nil, nil
		},
		runReadCommand: func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}
	exists, err := executor.NamespaceExists(context.Background(), "envplane-pr-feature")
	if err != nil || exists {
		t.Fatalf("absent stdout: exists=%v err=%v", exists, err)
	}
	if _, err := executor.NamespaceExists(context.Background(), " "); err == nil {
		t.Fatal("empty lookup accepted")
	}
}

func TestNamespaceReadRetainsFailedLookupStderr(t *testing.T) {
	exitError := &exec.ExitError{Stderr: []byte("namespaces feature not found")}
	executor := &CLIHelmExecutor{runReadCommand: func(context.Context, string, ...string) ([]byte, error) { return nil, exitError }}
	output, err := executor.namespaceRead(context.Background(), "get", "namespace", "feature")
	if !errors.Is(err, exitError) || string(output) != string(exitError.Stderr) {
		t.Fatal("failed lookup stderr lost; absence classification would be broken")
	}
}
