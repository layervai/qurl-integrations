package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type ecsContainerDefinition struct {
	Name       string   `json:"name"`
	Image      string   `json:"image"`
	EntryPoint []string `json:"entryPoint"`
	Command    []string `json:"command"`
	// User intentionally has no omitempty: every generated connector container
	// must explicitly pin the audited nonroot runtime identity.
	User                   string                   `json:"user"`
	Essential              bool                     `json:"essential"`
	ReadonlyRootFilesystem bool                     `json:"readonlyRootFilesystem"`
	Environment            []ecsEnvironmentVar      `json:"environment"`
	Secrets                []ecsSecret              `json:"secrets,omitempty"`
	MountPoints            []ecsMountPoint          `json:"mountPoints"`
	LogConfiguration       ecsLogConfiguration      `json:"logConfiguration"`
	LinuxParameters        ecsLinuxParameters       `json:"linuxParameters"`
	DependsOn              []ecsContainerDependency `json:"dependsOn,omitempty"`
	RestartPolicy          *ecsRestartPolicy        `json:"restartPolicy,omitempty"`
}

type ecsRestartPolicy struct {
	Enabled              bool `json:"enabled"`
	RestartAttemptPeriod int  `json:"restartAttemptPeriod"`
}

type ecsSecret struct {
	Name      string `json:"name"`
	ValueFrom string `json:"valueFrom"`
}

type ecsLinuxParameters struct {
	Capabilities ecsLinuxCapabilities `json:"capabilities"`
}

type ecsLinuxCapabilities struct {
	Drop []string `json:"drop"`
}

type ecsContainerDependency struct {
	ContainerName string `json:"containerName"`
	Condition     string `json:"condition"`
}

type ecsEnvironmentVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ecsMountPoint struct {
	SourceVolume  string `json:"sourceVolume"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly"`
}

type ecsLogConfiguration struct {
	LogDriver string            `json:"logDriver"`
	Options   map[string]string `json:"options"`
}

const (
	ecsFargateChecklistText = "ECS/Fargate task-definition checklist"
	connectorContainerName  = "qurl"
	// TODO(upstream-contract): keep in lockstep with the qurl image USER.
	ecsConnectorUser                = "65532:65532"
	ecsLogRegionOption              = "awslogs-region"
	ecsLogRegionPlaceholder         = "<region>"
	ecsFargateRegionPlaceholderNote = "Also replace the `" + ecsLogRegionPlaceholder + "` placeholder in the `" + ecsLogRegionOption + "` field below."
	ecsRestartAttemptPeriodSeconds  = 60
)

// TODO(upstream-contract): EFS access points select per-slug roots; ECS rootDirectory must stay "/".
const ecsFargateVolumesJSON = `{
  "volumes": [
    {
      "name": "qurl-agent-state",
      "efsVolumeConfiguration": {
        "fileSystemId": "<fs-id>",
        "rootDirectory": "/",
        "transitEncryption": "ENABLED",
        "authorizationConfig": {
          "accessPointId": "<fsap-agent-state-id>",
          "iam": "ENABLED"
        }
      }
    },
    {
      "name": "qurl-config",
      "efsVolumeConfiguration": {
        "fileSystemId": "<fs-id>",
        "rootDirectory": "/",
        "transitEncryption": "ENABLED",
        "authorizationConfig": {
          "accessPointId": "<fsap-config-id>",
          "iam": "ENABLED"
        }
      }
    },
    {
      "name": "qurl-bootstrap",
      "efsVolumeConfiguration": {
        "fileSystemId": "<fs-id>",
        "rootDirectory": "/",
        "transitEncryption": "ENABLED",
        "authorizationConfig": {
          "accessPointId": "<fsap-bootstrap-id>",
          "iam": "ENABLED"
        }
      }
    }
  ]
}`

func renderECSFargateTunnelInstructions(args *tunnelInstallArgs, image string) (string, error) {
	containerJSON, err := renderECSSidecarContainerJSON(args, image)
	if err != nil {
		return "", err
	}
	configYAML, err := renderTunnelConfigYAML(args)
	if err != nil {
		return "", err
	}
	configBlock, err := slackCodeBlock(configYAML)
	if err != nil {
		return "", err
	}
	containerBlock, err := slackCodeBlock(containerJSON)
	if err != nil {
		return "", err
	}
	volumesBlock, err := slackCodeBlock(ecsFargateVolumesJSON)
	if err != nil {
		return "", err
	}
	intro := strings.Join([]string{
		"Use this as an " + ecsFargateChecklistText + ".",
		ecsFargateRegionPlaceholderNote,
		"Fargate's awsvpc network mode shares one task ENI across containers, so no explicit network_mode is needed; `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the target container.",
		"The generated sidecar drops every Linux capability.",
		"Its ECS restart policy retries the non-essential qURL container indefinitely after the 60-second minimum run window without restarting the target application container.",
	}, " ")
	return intro + "\n\n" +
		"1. Create three EFS access points with separate root directories `/qurl/" + args.Slug + "/state`, `/qurl/" + args.Slug + "/config`, and `/qurl/" + args.Slug + "/bootstrap`. Set each access point enforced user and root-directory owner to POSIX UID/GID `" + ecsConnectorUser + "` with mode `0700`. Merge this volumes block into the task definition and replace every file-system/access-point placeholder. Keep ECS rootDirectory `/`; each access point selects its own root:\n\n" +
		volumesBlock + "\n\n" +
		"2. Configure EFS permissions before mounting. Its default file system policy permits anonymous network clients; task-role allows alone do not restrict that access. Attach a file system policy allowing only the task and provisioning roles through these access points and denying non-TLS access. Explicitly deny the task role mounts outside these three access points and writes outside the state access point. Grant the task role elasticfilesystem:ClientMount on all three and elasticfilesystem:ClientWrite only on state, using elasticfilesystem:AccessPointArn conditions. Give a separate provisioning role ClientWrite on config/bootstrap for file creation and token deletion. See <https://docs.aws.amazon.com/efs/latest/ug/iam-access-control-nfs-efs.html|EFS IAM policies>. Permit NFS port 2049 between the task and EFS mount-target security groups.\n\n" +
		"3. Store the enrollment token from the separate DM using the provisioning role as `enrollment-token` at the root of the bootstrap access point. Do not place the token in the task definition, environment, or command line; this message intentionally does not contain the token. Put share.yaml at the root of the qurl-config access point, mounted read-only at `/etc/qurl`; the sidecar reads `/etc/qurl/share.yaml`. The bootstrap mount is also read-only, at `/run/secrets/qurl`. Use this share.yaml:\n\n" +
		configBlock + "\n\n" +
		"4. Add this non-essential qURL sidecar container to the same task definition as the target container:\n\n" +
		containerBlock + "\n\n" +
		"5. This task definition supports one active task: set service desiredCount: 1. Do not share qurl-agent-state across concurrently running sidecars. Additional active Connectors need separate installations, enrollment tokens, task definitions and state access points/root directories. Writable state is under `/var/lib/qurl-volume/state`. Stop the old task before a replacement mounts the same state (use deployment maximumPercent 100 and minimumHealthyPercent 0). After the task logs show qURL connected, deploy a warm-start revision without `--enrollment-token-file` or the qurl-bootstrap mount; verify it reconnects from qurl-agent-state, then use the provisioning role to delete the enrollment-token file.", nil
}

func renderECSSidecarContainerJSON(args *tunnelInstallArgs, image string) (string, error) {
	endpoint, err := qurlEndpointFromConnectorAPIURL(args.APIURL)
	if err != nil {
		return "", err
	}
	container := ecsContainerDefinition{
		Name:                   connectorContainerName,
		Image:                  image,
		EntryPoint:             []string{"/usr/local/bin/qurl"},
		Command:                append([]string{"daemon", "run", "--state-dir", "/var/lib/qurl-volume/state", "--headless-config", "/etc/qurl/share.yaml", "--enrollment-token-file", "/run/secrets/qurl/enrollment-token"}, args.Hub.flags()...),
		User:                   ecsConnectorUser,
		Essential:              false,
		ReadonlyRootFilesystem: true,
		Environment: []ecsEnvironmentVar{
			{Name: "QURL_ENDPOINT", Value: endpoint},
		},
		MountPoints: []ecsMountPoint{
			{SourceVolume: "qurl-agent-state", ContainerPath: "/var/lib/qurl-volume"},
			{SourceVolume: "qurl-config", ContainerPath: "/etc/qurl", ReadOnly: true},
			{SourceVolume: "qurl-bootstrap", ContainerPath: "/run/secrets/qurl", ReadOnly: true},
		},
		LogConfiguration: awslogsConfiguration("/ecs/qurl-connector", "qurl"),
		LinuxParameters:  hardenedECSLinuxParameters(),
		RestartPolicy: &ecsRestartPolicy{
			Enabled:              true,
			RestartAttemptPeriod: ecsRestartAttemptPeriodSeconds,
		},
	}
	return marshalECSContainerJSON(container, "ECS sidecar JSON")
}

func hardenedECSLinuxParameters() ecsLinuxParameters {
	return ecsLinuxParameters{Capabilities: ecsLinuxCapabilities{Drop: []string{"ALL"}}}
}

func awslogsConfiguration(group, streamPrefix string) ecsLogConfiguration {
	return ecsLogConfiguration{
		LogDriver: "awslogs",
		Options: map[string]string{
			"awslogs-group":         group,
			ecsLogRegionOption:      ecsLogRegionPlaceholder,
			"awslogs-stream-prefix": streamPrefix,
		},
	}
}

func marshalECSContainerJSON(v any, what string) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("marshal %s: %w", what, err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}
