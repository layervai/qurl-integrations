package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

type previewTestClient struct {
	qurlapi.Client
	patch *qurlapi.PreviewPatch
}

func (c *previewTestClient) Preview(context.Context, string) (*qurlapi.Preview, error) {
	return &qurlapi.Preview{Revision: 7, Description: "old", Media: []qurlapi.PreviewMedia{{ID: "keep"}, {ID: "remove"}}}, nil
}
func (c *previewTestClient) UpdatePreview(_ context.Context, _ string, patch qurlapi.PreviewPatch) (*qurlapi.Preview, error) {
	c.patch = &patch
	return &qurlapi.Preview{Revision: 8}, nil
}

func TestPreviewCommandsUseLatestRevision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		usage bool
	}{
		{"clear", []string{"clear", "crid"}, false},
		{"empty description", []string{"set", "crid", "--description", ""}, false},
		{"remove", []string{"remove", "crid", "remove"}, false},
		{"missing flag", []string{"set", "crid"}, true},
		{"unknown media", []string{"remove", "crid", "missing"}, true},
		{"missing alt", []string{"upload", "crid", "file"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &previewTestClient{}
			var out bytes.Buffer
			opts := &globalOpts{registeredClient: c, streams: &output.Streams{Out: &out, Err: &out, In: &out}, resolvedFormat: output.FormatJSON, now: time.Now}
			cmd := previewCmd(opts)
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if tc.usage {
				if err == nil || exitcode.FromError(err) != exitcode.Usage {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.patch == nil || c.patch.Revision != 7 {
				t.Fatalf("patch=%+v", c.patch)
			}
			if tc.name == "clear" && (c.patch.Description == nil || *c.patch.Description != "" || c.patch.MediaIDs == nil || len(*c.patch.MediaIDs) != 0) {
				t.Fatal(c.patch)
			}
			if tc.name == "empty description" && (c.patch.Description == nil || *c.patch.Description != "") {
				t.Fatal(c.patch)
			}
			if tc.name == "remove" && (c.patch.MediaIDs == nil || len(*c.patch.MediaIDs) != 1 || (*c.patch.MediaIDs)[0] != "keep") {
				t.Fatal(c.patch)
			}
		})
	}
}
