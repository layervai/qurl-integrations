package state

import (
	"os"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state/statetest"
)

// TestMain keeps this package's tests off the host TPM; see
// statetest.PinFileKeyProvider.
func TestMain(m *testing.M) {
	statetest.PinFileKeyProvider()
	os.Exit(m.Run())
}
