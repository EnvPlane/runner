package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestNamespaceExistsRequiresSuccessfulExactLookup(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		exists       bool
	}{
		{name: "absent"},
		{name: "present", output: `{"metadata":{"name":"envplane-pr-feature"}}`, exists: true},
		{name: "forbidden", err: errors.New("forbidden")},
		{name: "network", err: errors.New("connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &CLIHelmExecutor{runCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "kubectl" || !reflect.DeepEqual(args, []string{"get", "namespace", "envplane-pr-feature", "-o", "json", "--ignore-not-found"}) {
					t.Fatalf("unexpected lookup %s %v", name, args)
				}
				return []byte(tc.output), tc.err
			}}
			exists, err := executor.NamespaceExists(context.Background(), "envplane-pr-feature")
			if exists != tc.exists || (err != nil) != (tc.err != nil) {
				t.Fatalf("exists=%v err=%v", exists, err)
			}
		})
	}
}
