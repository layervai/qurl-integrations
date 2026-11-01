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

type ecsTaskVolume struct {
	Name                   string                    `json:"name"`
	EFSVolumeConfiguration ecsEFSVolumeConfiguration `json:"efsVolumeConfiguration"`
}

type ecsEFSVolumeConfiguration struct {
	FileSystemID        string              `json:"fileSystemId"`
	RootDirectory       string              `json:"rootDirectory"`
	TransitEncryption   string              `json:"transitEncryption"`
	AuthorizationConfig ecsEFSAuthorization `json:"authorizationConfig"`
}

type ecsEFSAuthorization struct {
	AccessPointID string `json:"accessPointId"`
	IAM           string `json:"iam"`
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
	ecsWarmStartInstructions = "After qURL connects, deploy a warm-start revision without the `--enrollment-token-file` flag and path, qurl-bootstrap mount, or qurl-bootstrap entry in task volumes. Remove the bootstrap access point from task-role ClientMount allows in both identity and file-system policies. Verify reconnect from qurl-agent-state, then delete the enrollment-token file."
	ecsAgentStateVolume      = "qurl-agent-state"
	ecsConfigVolume          = "qurl-config"
	ecsBootstrapVolume       = "qurl-bootstrap"
	ecsFargateChecklistText  = "ECS/Fargate task-definition checklist"
	connectorContainerName   = "qurl"
	// TODO(upstream-contract): keep in lockstep with the qurl image USER.
	ecsConnectorUser                = "65532:65532"
	ecsLogRegionOption              = "awslogs-region"
	ecsLogRegionPlaceholder         = "<region>"
	ecsFargateRegionPlaceholderNote = "Also replace the `" + ecsLogRegionPlaceholder + "` placeholder in the `" + ecsLogRegionOption + "` field below."
	ecsRestartAttemptPeriodSeconds  = 60
)

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
	volumeInstructions, err := renderECSEFSVolumeInstructions(args.Slug)
	if err != nil {
		return "", err
	}
	intro := strings.Join([]string{
		"Use this as an " + ecsFargateChecklistText + ".",
		"Write the temporary enrollment token delivered separately by DM to `enrollment-token` on the `qurl-bootstrap` EFS access point (read-only to the task role). Do not place it in the task definition, environment, or command line.",
		ecsFargateRegionPlaceholderNote,
		"Create the shared CloudWatch Logs group `/ecs/qurl-connector` in the task region and let the task execution role write to it. Each task has a separate `qurl/qurl/<task-id>` stream; filter by task ID to find one install. For separate access or retention, replace `awslogs-group` with an install-specific group and update the execution role permissions.",
		"Fargate's awsvpc network mode shares one task ENI across containers, so no explicit network_mode is needed; `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the target container.",
		"The generated sidecar drops every Linux capability.",
		"This shared-state sidecar template requires a single-task application service and replacement downtime. Replicated application tasks must not share this connector state.",
		"Its ECS restart policy retries the non-essential qURL container indefinitely after the 60-second minimum run window without restarting the target application container.",
	}, " ")
	return intro + "\n\n" +
		"1. " + volumeInstructions + "\n\n" +
		"2. Store the enrollment token from the separate DM as the `enrollment-token` file at the qurl-bootstrap access-point root. This message intentionally does not contain the token.\n\n" +
		"3. Put share.yaml at the qurl-config access-point root (the container reads `/etc/qurl/share.yaml`):\n\n" +
		configBlock + "\n\n" +
		"4. Add this non-essential qURL sidecar container to the same task definition as the target container:\n\n" +
		containerBlock + "\n\n" +
		"5. " + ecsWarmStartInstructions, nil
}

// TODO(upstream-contract): recheck ECS EFS fields, access-point roots, and EFS default-policy behavior when AWS changes them.
func renderECSEFSVolumeInstructions(slug string) (string, error) {
	if !tunnelSlugPattern.MatchString(slug) {
		return "", fmt.Errorf("invalid ECS volume slug %q", slug)
	}
	definition := struct {
		Volumes []ecsTaskVolume `json:"volumes"`
	}{}
	for _, name := range []string{ecsAgentStateVolume, ecsConfigVolume, ecsBootstrapVolume} {
		definition.Volumes = append(definition.Volumes, ecsTaskVolume{
			Name: name,
			EFSVolumeConfiguration: ecsEFSVolumeConfiguration{
				FileSystemID: "<efs-file-system-id>", RootDirectory: "/", TransitEncryption: "ENABLED",
				AuthorizationConfig: ecsEFSAuthorization{AccessPointID: "<" + name + "-access-point-id>", IAM: "ENABLED"},
			},
		})
	}
	volumeJSON, err := marshalECSContainerJSON(definition, "ECS task volumes JSON")
	if err != nil {
		return "", err
	}
	block, err := slackCodeBlock(volumeJSON)
	if err != nil {
		return "", err
	}
	return "Merge this task-level volumes object into the task definition. Replace the file-system ID and all three access-point IDs:\n\n" + block + "\n\n" +
		"Create separate EFS access points rooted at `/qurl/" + slug + "/agent-state`, `/qurl/" + slug + "/config`, and `/qurl/" + slug + "/bootstrap`, respectively. Set each access point's POSIX user and root CreationInfo owner to UID/GID `" + ecsConnectorUser + "`, with root permissions `0700`. Let the access points create these directories; pre-existing directories keep their current ownership and permissions. These paths belong on the access points; task volume rootDirectory stays `/`.\n\n" +
		"Use a separate provisioning principal with ClientMount and ClientWrite on the config/bootstrap access points to write these files through those access points. Write `share.yaml` at the config access-point root and `enrollment-token` at the bootstrap access-point root; the container sees `/etc/qurl/share.yaml` and `/run/secrets/qurl/enrollment-token`. qURL creates `state/` under the agent-state root.\n\n" +
		"Use a different slug and access points for every concurrently running sidecar. Keep one active task per agent-state access point. Stop the old task before its replacement mounts the same state; for an ECS service use desiredCount 1, deployment maximumPercent 100 and minimumHealthyPercent 0.\n\n" +
		"Attach an explicit file-system policy: the absent/default policy grants full access to clients that can reach a mount target. In the task identity policy and file-system policy, allow elasticfilesystem:ClientMount on the file-system ARN for the task role, conditioned on the three access-point ARNs via elasticfilesystem:AccessPointArn; allow elasticfilesystem:ClientWrite on that file-system ARN only with the agent-state access-point ARN condition. Separately allow the provisioning principal mount/write on config/bootstrap. Add a task-role-only Deny for ClientWrite on config/bootstrap so wider grants cannot enable writes; do not deny the provisioning principal. Allow NFS TCP 2049 from the task security group to EFS mount targets.", nil
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
			{SourceVolume: ecsAgentStateVolume, ContainerPath: "/var/lib/qurl-volume"},
			{SourceVolume: ecsConfigVolume, ContainerPath: "/etc/qurl", ReadOnly: true},
			{SourceVolume: ecsBootstrapVolume, ContainerPath: "/run/secrets/qurl", ReadOnly: true},
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
