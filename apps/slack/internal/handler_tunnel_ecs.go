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
	intro := strings.Join([]string{
		"Use this as an " + ecsFargateChecklistText + ".",
		"Write the temporary enrollment token delivered separately by DM to `enrollment-token` on a read-only `qurl-bootstrap` EFS access point. Do not place it in the task definition, environment, or command line.",
		ecsFargateRegionPlaceholderNote,
		"Fargate's awsvpc network mode shares one task ENI across containers, so no explicit network_mode is needed; `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the target container.",
		"Configure qurl-agent-state for POSIX UID/GID `" + ecsConnectorUser + "`; qURL creates its owner-only nested state directory. Mount qurl-config and qurl-bootstrap read-only with share.yaml and enrollment-token respectively.",
		"The generated sidecar drops every Linux capability.",
		"Its ECS restart policy retries the non-essential qURL container indefinitely after the 60-second minimum run window without restarting the target application container.",
	}, " ")
	return intro + "\n\n" +
		"1. Store the enrollment token from the separate DM as the read-only `enrollment-token` file on the qurl-bootstrap EFS access point. This message intentionally does not contain the token.\n\n" +
		"2. Put share.yaml at `/etc/qurl/share.yaml` on the read-only qurl-config EFS access point:\n\n" +
		configBlock + "\n\n" +
		"3. Add this non-essential qURL sidecar container to the same task definition as the target container:\n\n" +
		containerBlock + "\n\n" +
		"4. Add EFS-backed volumes named qurl-agent-state, qurl-config, and qurl-bootstrap. Do not share qurl-agent-state across concurrently running sidecars. After the task logs show qURL connected, deploy a warm-start revision without `--enrollment-token-file` or the qurl-bootstrap mount; verify it reconnects from qurl-agent-state, then delete the enrollment-token file.", nil
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
		Command:                []string{"daemon", "run", "--state-dir", "/var/lib/qurl-volume/state", "--headless-config", "/etc/qurl/share.yaml", "--enrollment-token-file", "/run/secrets/qurl/enrollment-token"},
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
