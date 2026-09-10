package state

import (
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func testHeadlessYAML(t *testing.T) string {
	t.Helper()
	binding := testResourceBinding(t, "headless-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	return "version: 2\nowner_id: owner-one\nshares:\n" +
		"  - crid: " + binding.CRID + "\n" +
		"    resource_id: " + binding.ResourceID + "\n" +
		"    connector_id: " + binding.ConnectorID + "\n" +
		"    connector_routing_id: " + binding.ConnectorRoutingID + "\n" +
		"    knock_resource_id: " + binding.KnockResourceID + "\n" +
		"    target_url: http://127.0.0.1:8080\n" +
		"    local_ip: 127.0.0.1\n" +
		"    local_port: 8080\n" +
		"    desired_state: on\n" +
		"    serving_epoch: 1\n"
}
