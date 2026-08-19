package supervisor

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"testing"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
)

// routingID builds the server-issued routing identity shape frpgen requires:
// "c-" plus 52 canonical lowercase unpadded base32 characters.
func routingID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return "c-" + strings.ToLower(enc.EncodeToString(digest[:]))
}

// productionCommon builds the completed common config the command actually
// runs: frpgen's output put through ClientCommonConfig.Complete, which is what
// every path into the FRP service does before the connector sees it. Shared by
// the tests that reason about production's shape, so the two cannot drift into
// asserting against different configs. seed only feeds the routing identity,
// which frpgen puts on the proxy rather than the common config, so callers
// passing different seeds still get identical configs back.
func productionCommon(t *testing.T, seed string) *v1.ClientCommonConfig {
	t.Helper()
	cfg, err := frpgen.Generate(&frpgen.Route{
		Slug:               "reports",
		ResourceID:         testResource,
		ConnectorRoutingID: routingID(seed),
		LocalIP:            "127.0.0.1",
		LocalPort:          8080,
	}, &frpgen.Options{ReplicaDiscriminator: "abc123", ClientVersion: "test"})
	if err != nil {
		t.Fatalf("generate the production client config: %v", err)
	}
	// The discarded second value is the proxy configurer list, not an error;
	// this helper only needs the common config.
	common, _ := cfg.FRPClientConfig()
	if err := common.Complete(); err != nil {
		t.Fatalf("complete the production common config: %v", err)
	}
	return common
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
	common := productionCommon(t, "watchdog-seam")
	if !physicalDialInOpen(common) {
		t.Fatalf("the generated config puts the physical dial on Connect, not Open: the reconnect watchdog would count one dial per work connection and restart healthy cycles. TCPMux = %v", common.Transport.TCPMux)
	}
}
