//go:build linux && !android

package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"
)

func TestSystemdJobPreservesProductSpecifierBytes(t *testing.T) {
	dir := t.TempDir()
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "3.4.1", "https://api.example.test", GroupModeSingle, testHubResolver)
	controller.InvocationPath = filepath.Join(dir, "bin", "qurl")
	controller.ClientProduct = "host%h$HOME/1"
	version, err := JobVersion(controller.BinaryVersion, controller.ShareGroupMode, controller.ClientProduct)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := testHubResolver()
	if err != nil {
		t.Fatal(err)
	}
	job, err := controller.jobDefinition(hub, version)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := connectorservice.RenderSystemdUserJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "--client-product=host%%h$$HOME/1") || !strings.Contains(rendered, "/client/host%%h$$HOME/1") {
		t.Fatalf("systemd expanded product bytes: %s", rendered)
	}
}
