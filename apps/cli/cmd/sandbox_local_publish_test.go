//go:build clisandbox

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
)

// TestSandboxLocalPublishSmoke exercises the unified customer command against
// the live sandbox. Unlike the legacy Connector smoke, it starts with an
// ordinary login key and no pre-issued enrollment token or device state. A
// passing run proves the login key can mint the exact one-shot credential,
// native UDP enrollment exchanges it for a device identity, the device creates
// the tunnel resource, and the FRP server admits the resulting route. The
// hermetic command journey separately sends HTTP bytes over that admitted
// route; this live smoke avoids depending on a public sandbox access hostname.
//
// The test arms automatically when the existing sandbox API variables and the
// qurl-connector sandbox Hub variables are all present. It creates a unique
// device/resource in a temporary state directory and deletes the resource by
// CRID before returning.
func TestSandboxLocalPublishSmoke(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("QURL_API_KEY"))
	endpoint := strings.TrimSpace(os.Getenv("QURL_ENDPOINT"))
	missing := []string{}
	for name, value := range map[string]string{
		"QURL_API_KEY":         key,
		"QURL_ENDPOINT":        endpoint,
		hub.EnvHost:            os.Getenv(hub.EnvHost),
		hub.EnvPort:            os.Getenv(hub.EnvPort),
		hub.EnvServerPublicKey: os.Getenv(hub.EnvServerPublicKey),
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Skipf("SKIPPED LOUDLY: unified local-publish sandbox smoke is disarmed — missing %v", missing)
	}

	t.Setenv(state.EnvStateDirPrimary, t.TempDir())
	t.Setenv(state.EnvAgentID, "")
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sandbox-local-publish-smoke")
	}))
	t.Cleanup(echo.Close)

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(45*time.Second, cancel)
	t.Cleanup(func() {
		timer.Stop()
		cancel()
	})
	res := runCLI(t, &runOpts{
		ctx:         ctx,
		args:        []string{"--endpoint", endpoint, "--quiet", "publish", echo.URL},
		env:         map[string]string{"QURL_API_KEY": key},
		syncStreams: true,
		realSleep:   true,
	})

	id := strings.TrimSpace(res.stdout.String())
	if assessment, err := cridux.Assess(id); err == nil && assessment.Kind == cridux.KindCRID {
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			deleted := runCLI(t, &runOpts{
				ctx:       cleanupCtx,
				args:      []string{"--endpoint", endpoint, "delete", id, "--yes"},
				env:       map[string]string{"QURL_API_KEY": key},
				realSleep: true,
			})
			if deleted.code != 0 {
				t.Errorf("cleanup delete exit = %d, want 0; stderr: %s", deleted.code, deleted.stderr.String())
			}
		})
	} else {
		t.Errorf("quiet local publish stdout = %q, want exactly one CRID: %v", res.stdout.String(), err)
	}
	if res.code != 130 {
		t.Fatalf("local publish exit = %d, want 130 after graceful cancellation; stderr: %s", res.code, res.stderr.String())
	}
	for _, evidence := range []string{"login_success", "Stopped."} {
		if !strings.Contains(res.stderr.String(), evidence) {
			t.Errorf("sandbox local publish lacks %q evidence:\n%s", evidence, res.stderr.String())
		}
	}
	if strings.Contains(res.stdout.String()+res.stderr.String(), key) {
		t.Fatal("sandbox login credential leaked into command output")
	}
}
