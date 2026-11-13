package sessionconfig

import (
	"os"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state/statetest"
)

// TestMain keeps this package's tests off the host TPM should any of them open
// agent state; see statetest.PinFileKeyProvider.
func TestMain(m *testing.M) {
	statetest.PinFileKeyProvider()
	os.Exit(m.Run())
}
