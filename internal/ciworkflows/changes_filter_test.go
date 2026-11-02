package ciworkflows

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Pin minimum coverage rather than duplicating every optional dependency path.
func TestRequiredChangeFiltersCoverTheirSource(t *testing.T) {
	for i := range requiredWorkflowSpecs {
		spec := &requiredWorkflowSpecs[i]
		t.Run(spec.name, func(t *testing.T) {
			changes := readWorkflow(t, spec.path).Jobs[changesJobID]
			if changes == nil {
				t.Fatal("missing changes detector")
			}
			expectedOutput := fmt.Sprintf("${{ steps.base.outputs.force == 'true' && 'true' || steps.filter.outputs.%s }}", spec.changeOutput)
			switch spec.name {
			case "shared":
				expectedOutput = "${{ steps.filter.outputs.shared }}"
			case "s3-static-connector":
				expectedOutput = "${{ (github.event_name == 'workflow_dispatch' || steps.base.outputs.force == 'true')\n    && 'true' || steps.filter.outputs.s3_static_connector }}"
			case "cli":
				expectedOutput = "${{ contains(fromJson('[\"schedule\",\"workflow_dispatch\"]'), github.event_name) && 'true' || (steps.base.outputs.force == 'true' && 'true' || steps.filter.outputs.cli) }}"
			}
			if changes.Outputs[spec.changeOutput] != expectedOutput {
				t.Fatalf("detector output %s = %q, want %q", spec.changeOutput, changes.Outputs[spec.changeOutput], expectedOutput)
			}
			var filter *step
			for i := range changes.Steps {
				current := &changes.Steps[i]
				if current.ID == "filter" {
					if filter != nil {
						t.Fatal("duplicate filter step")
					}
					filter = current
				}
			}
			if filter == nil || !strings.HasPrefix(filter.Uses, "dorny/paths-filter@") {
				t.Fatal("detector output has no paths-filter producer")
			}
			raw, ok := filter.With["filters"].(string)
			if !ok {
				t.Fatal("paths-filter has no inline filter definitions")
			}
			var filters map[string][]string
			if err := yaml.Unmarshal([]byte(raw), &filters); err != nil {
				t.Fatalf("parse filter definitions: %v", err)
			}
			source := "apps/" + spec.name + "/**"
			switch spec.name {
			case "e2e", "shared":
				source = spec.name + "/**"
			case "s3-static-connector":
				source = "origins/s3-static-connector/**"
			}
			minimum := []string{source, ".github/workflows/" + spec.path}
			if spec.name == "edge-extension" {
				minimum = append(minimum, "apps/chrome-extension/**")
			}
			for _, path := range minimum {
				if !slices.Contains(filters[spec.changeOutput], path) {
					t.Errorf("filter %s does not cover %s", spec.changeOutput, path)
				}
			}
		})
	}
}

func TestDiscordChangeFilterExcludesGoSharedCode(t *testing.T) {
	workflow := readWorkflow(t, "discord.yml")
	for i := range workflow.Jobs[changesJobID].Steps {
		current := &workflow.Jobs[changesJobID].Steps[i]
		if current.ID != "filter" {
			continue
		}
		raw, ok := current.With["filters"].(string)
		if !ok {
			t.Fatal("missing filter definitions")
		}
		var filters map[string][]string
		if err := yaml.Unmarshal([]byte(raw), &filters); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(filters["discord"], "shared/**") {
			t.Fatal("Go-only shared changes must not run the Node app and image gates")
		}
		return
	}
	t.Fatal("missing Discord change filter")
}
