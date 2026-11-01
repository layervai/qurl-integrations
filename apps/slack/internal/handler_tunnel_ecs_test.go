package internal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderECSFargateTunnelInstructions(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.LocalPort = 9090
	args.Environment = tunnelEnvECSFargate
	got := mustRenderECSFargateTunnelInstructions(t, args, testTunnelImageRef)

	for _, want := range []string{
		ecsFargateChecklistText,
		"non-essential qURL sidecar container",
		"Fargate's awsvpc network mode",
		ecsFargateRegionPlaceholderNote,
		"127.0.0.1:9090",
		"POSIX UID/GID `65532:65532`",
		`"readonlyRootFilesystem": true`,
		`"restartPolicy": {`,
		`"restartAttemptPeriod": 60`,
		"warm-start revision",
		"maximumPercent 100 and minimumHealthyPercent 0",
		"Store the enrollment token from the separate DM",
		"message intentionally does not contain the token",
		testTunnelImageRef,
		"Put share.yaml at `/etc/qurl/share.yaml`",
		testTunnelLocalPort9090Line,
		"crid: '" + testTunnelCRID + "'",
		"resource_id: '" + testTunnelResourceID + "'",
		"connector_routing_id: '" + testTunnelRoutingID + "'",
		"knock_resource_id: '" + testTunnelKnockID + "'",
		`"entryPoint": [`,
		`"/usr/local/bin/qurl"`,
		`"name": "QURL_ENDPOINT"`,
		`"value": "https://api.sandbox.example"`,
		`"user": "65532:65532"`,
		`"sourceVolume": "qurl-agent-state"`,
		`"sourceVolume": "qurl-config"`,
		`"sourceVolume": "qurl-bootstrap"`,
		`"readonlyRootFilesystem": true`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ECS instructions missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testTunnelAPIKey, "QURL_CONNECTOR_SLUG", "QURL_BOOTSTRAP_URL", "LAYERV_KNOCK_RESOURCE_ID", "QURL_API_KEY", "ghcr.io/layervai/qurl-connector", "/usr/local/bin/qurl-connector"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("ECS instructions leaked %q:\n%s", forbidden, got)
		}
	}
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u003e`) {
		t.Fatalf("ECS instructions escaped placeholder angle brackets:\n%s", got)
	}
	if gotFenceCount := strings.Count(got, "```"); gotFenceCount != 6 {
		t.Fatalf("ECS instructions rendered %d code fences, want 6 for three independently copyable artifacts:\n%s", gotFenceCount, got)
	}

	containerJSON, err := renderECSSidecarContainerJSON(args, testTunnelImageRef)
	if err != nil {
		t.Fatalf("renderECSSidecarContainerJSON: %v", err)
	}
	var container ecsContainerDefinition
	if err := json.Unmarshal([]byte(containerJSON), &container); err != nil {
		t.Fatalf("ECS sidecar JSON did not parse: %v", err)
	}
	if container.Essential {
		t.Fatal("ECS sidecar Essential = true, want false so the tunnel does not take down the app task")
	}
	if container.RestartPolicy == nil || !container.RestartPolicy.Enabled || container.RestartPolicy.RestartAttemptPeriod != ecsRestartAttemptPeriodSeconds {
		t.Fatalf("ECS restart policy = %+v, want enabled with %ds attempt period", container.RestartPolicy, ecsRestartAttemptPeriodSeconds)
	}
	if container.User != ecsConnectorUser {
		t.Fatalf("ECS sidecar User = %q, want connector image UID/GID", container.User)
	}
	if !container.ReadonlyRootFilesystem {
		t.Fatal("ECS sidecar ReadonlyRootFilesystem = false, want true")
	}
	if got := container.LinuxParameters.Capabilities.Drop; len(got) != 1 || got[0] != testCapabilityAll {
		t.Fatalf("ECS sidecar capability drop = %v, want [ALL]", got)
	}
	if len(container.Secrets) != 0 || container.Image != testTunnelImageRef {
		t.Fatalf("ECS sidecar = %+v, want qurl image and no environment secret", container)
	}
	env := map[string]string{}
	for _, e := range container.Environment {
		env[e.Name] = e.Value
	}
	if got := env["QURL_ENDPOINT"]; got != "https://api.sandbox.example" {
		t.Fatalf("ECS QURL_ENDPOINT = %q", got)
	}
	if !ecsMountPointPresent(container.MountPoints, "qurl-bootstrap", "/run/secrets/qurl", true) {
		t.Fatalf("ECS mountPoints = %+v, want read-only enrollment-token mount", container.MountPoints)
	}
	if _, ok := env["LAYERV_KNOCK_RESOURCE_ID"]; ok {
		t.Fatal("ECS environment rendered the advanced knock-resource override")
	}
}

func TestRenderedECSVolumeArtifactsMatchBothInstallers(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"tunnel", "s3-website"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var instructions, slug string
			var err error
			if kind == "tunnel" {
				args := testTunnelInstallArgs()
				slug = args.Slug
				instructions, err = renderECSFargateTunnelInstructions(args, testTunnelImageRef)
			} else {
				args := testS3WebsiteArgs(tunnelEnvECSFargate)
				slug = args.Slug
				instructions, err = renderECSS3WebsiteInstructions(args, testTunnelImageRef, testTunnelImageRef)
			}
			if err != nil {
				t.Fatal(err)
			}
			blocks := strings.Split(instructions, "```")
			if len(blocks) != 7 {
				t.Fatalf("want three complete copyable artifacts, got %d fences", len(blocks)-1)
			}
			var task struct {
				Volumes []struct {
					Name string `json:"name"`
					EFS  struct {
						FileSystemID      string `json:"fileSystemId"`
						RootDirectory     string `json:"rootDirectory"`
						TransitEncryption string `json:"transitEncryption"`
						Authorization     struct {
							AccessPointID string `json:"accessPointId"`
							IAM           string `json:"iam"`
						} `json:"authorizationConfig"`
					} `json:"efsVolumeConfiguration"`
				} `json:"volumes"`
			}
			if err := json.Unmarshal([]byte(blocks[5]), &task); err != nil {
				t.Fatalf("task volumes JSON: %v", err)
			}
			if len(task.Volumes) != 3 {
				t.Fatalf("want three volumes, got %d", len(task.Volumes))
			}
			volumes, accessPoints := map[string]bool{}, map[string]bool{}
			for _, volume := range task.Volumes {
				if volumes[volume.Name] || accessPoints[volume.EFS.Authorization.AccessPointID] {
					t.Fatal("duplicate volume or shared access-point placeholder")
				}
				volumes[volume.Name] = true
				accessPoints[volume.EFS.Authorization.AccessPointID] = true
				if volume.EFS.FileSystemID != "<efs-file-system-id>" || volume.EFS.RootDirectory != "/" || volume.EFS.TransitEncryption != "ENABLED" || volume.EFS.Authorization.IAM != "ENABLED" || volume.EFS.Authorization.AccessPointID != "<"+volume.Name+"-access-point-id>" {
					t.Fatalf("unsafe or incomplete EFS volume: %+v", volume)
				}
			}
			var containers []ecsContainerDefinition
			if kind == "tunnel" {
				var container ecsContainerDefinition
				if err := json.Unmarshal([]byte(blocks[3]), &container); err != nil {
					t.Fatal(err)
				}
				containers = []ecsContainerDefinition{container}
			} else if err := json.Unmarshal([]byte(blocks[3]), &containers); err != nil {
				t.Fatal(err)
			}
			mounts := 0
			for _, container := range containers {
				for _, mount := range container.MountPoints {
					mounts++
					if !volumes[mount.SourceVolume] {
						t.Fatalf("mount references absent task volume %q", mount.SourceVolume)
					}
					if mount.ReadOnly != (mount.SourceVolume != "qurl-agent-state") {
						t.Fatalf("unsafe mount %+v", mount)
					}
				}
			}
			if mounts != 3 {
				t.Fatalf("want three corresponding mounts, got %d", mounts)
			}
			for _, want := range []string{"/qurl/" + slug + "/agent-state", "/qurl/" + slug + "/config", "/qurl/" + slug + "/bootstrap", "rootDirectory stays `/`", "POSIX user and root CreationInfo owner", "0700", "different slug and access points", "one active task", "maximumPercent 100 and minimumHealthyPercent 0", "file-system ARN", "elasticfilesystem:AccessPointArn", "/etc/qurl/share.yaml", "/run/secrets/qurl/enrollment-token"} {
				if !strings.Contains(instructions, want) {
					t.Fatalf("missing EFS setup rule %q", want)
				}
			}
		})
	}
}

func TestECSEFSVolumeInstructionsRejectUnsafeSlug(t *testing.T) {
	t.Parallel()
	for _, slug := range []string{"", "other/../../shared", "bad`slug", "bad\nslug"} {
		if _, err := renderECSEFSVolumeInstructions(slug); err == nil {
			t.Fatalf("accepted unsafe slug %q", slug)
		}
	}
}
