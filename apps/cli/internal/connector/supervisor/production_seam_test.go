package supervisor

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
)

// routingID builds the server-issued routing identity shape frpgen requires:
// "c-" plus 52 canonical lowercase unpadded base32 characters.
func routingID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return "c-" + strings.ToLower(enc.EncodeToString(digest[:]))
}

// TestProductionConfigKeepsTheWatchdogOnTheOpenSeam pins the invariant the
// reconnect watchdog rests on: in the config the command actually generates,
// the physical dial belongs to Open.
//
// Why it matters. noteRedialLocked counts every refresh as one control-
// connection redial, which is only true on the Open seam. If TCPMux were ever
// disabled the refresh would move to Connect, where the pinned fork dials once
// per WORK connection — so a busy, perfectly healthy tunnel would accumulate a
// storm and eventually be restarted for no reason. frpgen models no TCPMux
// field today, so the value stays at FRP's default; this test is what turns a
// future change to that into a failure here rather than a false restart in
// production.
func TestProductionConfigKeepsTheWatchdogOnTheOpenSeam(t *testing.T) {
	t.Parallel()
	cfg, err := frpgen.Generate(&frpgen.Route{
		Slug:               "reports",
		ResourceID:         testResource,
		ConnectorRoutingID: routingID("watchdog-seam"),
		LocalIP:            "127.0.0.1",
		LocalPort:          8080,
	}, &frpgen.Options{ReplicaDiscriminator: "abc123", ClientVersion: "test"})
	if err != nil {
		t.Fatalf("generate the production client config: %v", err)
	}
	common, _ := cfg.FRPClientConfig()
	if err := common.Complete(); err != nil {
		t.Fatalf("complete the production common config: %v", err)
	}
	if !physicalDialInOpen(common) {
		t.Fatalf("the generated config puts the physical dial on Connect, not Open: the reconnect watchdog would count one dial per work connection and restart healthy cycles. TCPMux = %v", common.Transport.TCPMux)
	}
}
