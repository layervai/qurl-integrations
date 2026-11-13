package daemon

import (
	"strings"
	"testing"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestManagersProjectExclusivePipeAlongsideTCP(t *testing.T) {
	for _, mode := range []GroupMode{GroupModeSingle, GroupModePerShare} {
		t.Run(string(mode), func(t *testing.T) {
			pipe := daemonShare("a", 7, "on")
			pipe.LocalIP, pipe.LocalPort = "", 0
			pipe.LocalPipeName = `\\.\pipe\layerv-qurl-file-` + strings.Repeat("a", 64) + "-" + strings.Repeat("b", 32)
			pipe.TargetURL = "http+npipe:///" + strings.TrimPrefix(pipe.LocalPipeName, `\\.\pipe\`)
			registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": pipe, "b": daemonShare("b", 1, "on")}}
			factory := newFakeGroupFactory()
			var manager ShareManager
			if mode == GroupModeSingle {
				manager, _ = newRunningManager(t, registry, factory)
			} else {
				manager = newRunningPerShareManager(t, registry, factory)
			}
			waitPerShareServing(t, manager, "a", "b")
			seen := 0
			for _, cfg := range groupConfigs(factory) {
				for _, route := range cfg.Routes {
					switch route.RouteID {
					case "connector-a":
						seen++
						if route.LocalPipeName != pipe.LocalPipeName || route.LocalSocketPath != "" || route.LocalIP != "" || route.LocalPort != 0 || route.ResourcePublicKey != pipe.ResourceID {
							t.Fatal("private pipe lost transport or identity")
						}
						// TODO(upstream-contract): Connector Equal must distinguish launch nonces.
						rotated := route
						rotated.LocalPipeName = pipe.LocalPipeName[:len(pipe.LocalPipeName)-32] + strings.Repeat("c", 32)
						if route.Equal(rotated) {
							t.Fatal("pipe nonce change compares equal")
						}
					case "connector-b":
						seen++
						if route.LocalPipeName != "" || route.LocalSocketPath != "" || route.LocalIP != "127.0.0.1" || route.LocalPort != 3000 {
							t.Fatal("TCP sibling changed")
						}
					default:
						t.Fatal("unexpected route")
					}
				}
			}
			if seen != 2 {
				t.Fatalf("projected %d routes, want 2", seen)
			}
		})
	}
}
