package main

import connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"

// externalLoginEnv is the supervisor's process contract for an external
// enrollment: a local-key sealed namespace and no account credential. extra
// holds key/value pairs layered on top of it.
func externalLoginEnv(extra ...string) map[string]string {
	env := map[string]string{
		connectoragentstate.EnvKeyProvider: connectoragentstate.KeyProviderLocalKey,
		connectoragentstate.EnvLocalKeyFD:  "3",
	}
	for i := 0; i+1 < len(extra); i += 2 {
		env[extra[i]] = extra[i+1]
	}
	return env
}
