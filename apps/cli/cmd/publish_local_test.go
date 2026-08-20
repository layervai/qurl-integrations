package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
)

func TestLocalPublishRejectsUnsupportedInputsBeforeStateOrNetwork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "description", args: []string{"publish", "http://127.0.0.1:3000", "--description", "local"}, want: "--description is not supported"},
		{name: "empty description still explicit", args: []string{"publish", "http://127.0.0.1:3000", "--description="}, want: "--description is not supported"},
		{name: "tag", args: []string{"publish", "http://localhost:3000", "--tag", "dev"}, want: "--tag is not supported"},
		{name: "alias", args: []string{"publish", "http://[::1]:3000", "--alias", "dev"}, want: "--alias is not supported"},
		{name: "https", args: []string{"publish", "https://localhost:3000"}, want: "cleartext http"},
		{name: "path", args: []string{"publish", "http://127.0.0.1:3000/api"}, want: "without a path"},
		{name: "remote id", args: []string{"publish", "https://example.com", "--id", "local-test"}, want: "--id applies only"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			srv := apitest.NewServer(t)
			opens := 0
			args := append([]string{"--endpoint", srv.URL}, test.args...)
			res := runCLI(t, &runOpts{
				args: args,
				env:  map[string]string{},
				connectorOpen: func(context.Context, *agent.Config) (*agent.Runtime, error) {
					opens++
					return nil, errors.New("unexpected Connector open")
				},
			})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
			if !strings.Contains(res.stderr.String(), test.want) {
				t.Errorf("stderr missing %q:\n%s", test.want, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			if opens != 0 {
				t.Errorf("invalid input opened Connector state %d times", opens)
			}
			if got := len(srv.Requests()); got != 0 {
				t.Errorf("invalid input sent %d API requests", got)
			}
		})
	}
}

func TestLocalPublishRejectsInvalidExplicitConnectorIDBeforeOpen(t *testing.T) {
	t.Parallel()
	opens := 0
	res := runCLI(t, &runOpts{
		args: []string{"publish", "http://127.0.0.1:3000", "--id", "NOT_VALID"},
		env:  map[string]string{},
		connectorOpen: func(context.Context, *agent.Config) (*agent.Runtime, error) {
			opens++
			return nil, errors.New("unexpected Connector open")
		},
	})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "invalid Connector ID") {
		t.Fatalf("result = exit %d stderr %q", res.code, res.stderr.String())
	}
	if opens != 0 {
		t.Fatalf("invalid ID opened Connector state %d times", opens)
	}
}
