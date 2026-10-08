package runner

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCommandPollingMatchesNarrowRuntimeAuthRecoveryContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		recover bool
	}{
		{"unissued", 401, `{"error":"runner auth token is not issued"}`, true},
		{"invalid", 401, `{"error":"invalid runner auth token"}`, true},
		{"missing stored credential", 401, `{"error":"missing runtime auth credential"}`, true},
		{"forbidden", 403, `{"error":"invalid runner auth token"}`, false},
		{"forbidden mentions 401", 403, `{"error":"invalid runner auth token; previous attempt was 401"}`, false},
		{"generic unauthorized", 401, `{"error":"unauthorized"}`, false},
		{"bootstrap expired", 401, `{"error":"runner bootstrap credentials have expired","code":"runner_bootstrap_credential_expired"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}
			_, _, err := nextRunnerCommand(context.Background(), runnerConfig{ControlPlaneURL: "http://runner.test",
				ProjectID: "scoped-project", ClusterID: "remote", RunnerID: "scoped-runner", RunnerAuthToken: "test-stale"}, client)
			if err == nil || isRunnerAuthTokenNotIssuedError(err) != tc.recover {
				t.Fatalf("recovery=%t want=%t", isRunnerAuthTokenNotIssuedError(err), tc.recover)
			}
		})
	}
}
