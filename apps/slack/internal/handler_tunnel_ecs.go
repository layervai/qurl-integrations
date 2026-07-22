package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type ecsContainerDefinition struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// User intentionally has no omitempty: every generated connector container
	// must explicitly pin the audited nonroot runtime identity.
	User             string                   `json:"user"`
	Essential        bool                     `json:"essential"`
	Environment      []ecsEnvironmentVar      `json:"environment"`
	Secrets          []ecsSecret              `json:"secrets"`
	MountPoints      []ecsMountPoint          `json:"mountPoints"`
	LogConfiguration ecsLogConfiguration      `json:"logConfiguration"`
	LinuxParameters  ecsLinuxParameters       `json:"linuxParameters"`
	DependsOn        []ecsContainerDependency `json:"dependsOn,omitempty"`
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

type ecsSecret struct {
	Name      string `json:"name"`
	ValueFrom string `json:"valueFrom"`
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
	connectorContainerName  = "qurl-connector"
	// TODO(upstream-contract): keep in lockstep with the qurl-connector image USER.
	ecsConnectorUser                = "65532:65532"
	ecsConnectorIDEnv               = "QURL_CONNECTOR_ID"
	ecsLogRegionOption              = "awslogs-region"
	ecsLogRegionPlaceholder         = "<region>"
	ecsFargateRegionPlaceholderNote = "Also replace the `" + ecsLogRegionPlaceholder + "` placeholder in the `" + ecsLogRegionOption + "` field below."
)

func renderECSFargateTunnelInstructions(args *tunnelInstallArgs, image string) (string, error) {
	containerJSON, err := renderECSSidecarContainerJSON(args, image)
	if err != nil {
		return "", err
	}
	secretName := "qurl-connector-" + args.Slug
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
		"Create the AWS Secrets Manager secret as `" + secretName + "` using the temporary bootstrap key delivered separately by DM so the task definition's `valueFrom` ARN resolves.",
		"Replace `REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_" + args.Slug + "` with the full secret ARN shown by Secrets Manager; AWS appends a random suffix to secret ARNs.",
		ecsFargateRegionPlaceholderNote,
		"Fargate's awsvpc network mode shares one task ENI across containers, so no explicit network_mode is needed; `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the target container.",
		"Configure both EFS access points with POSIX UID/GID `" + ecsConnectorUser + "`, matching the connector image's nonroot user.",
		"The generated sidecar drops every Linux capability.",
	}, " ")
	return intro + "\n\n" +
		"1. Store the bootstrap key from the separate DM in AWS Secrets Manager. This install-instructions message intentionally does not contain the key.\n\n" +
		"2. Put qurl-proxy.yaml at `/work/qurl-proxy.yaml` on an EFS access point mounted into the task as the `qurl-config` volume:\n\n" +
		configBlock + "\n\n" +
		"3. Add this non-essential sidecar container to the same task definition as the target container. ECS injects this bootstrap secret as `QURL_API_KEY`, which is an environment variable; file-mounted secret runtimes should use `QURL_API_KEY_FILE` instead:\n\n" +
		containerBlock + "\n\n" +
		"4. Add durable EFS-backed volumes named qurl-agent-state and qurl-config. Do not share qurl-agent-state across concurrently running sidecars. After the task logs show the qURL Connector connected, delete the bootstrap secret. For future bootstrap rotation, prefer a file-mounted secret runtime so new bootstrap keys are not revealed through task environment variables.", nil
}

func renderECSSidecarContainerJSON(args *tunnelInstallArgs, image string) (string, error) {
	container := ecsContainerDefinition{
		Name:      connectorContainerName,
		Image:     image,
		User:      ecsConnectorUser,
		Essential: false,
		Environment: []ecsEnvironmentVar{
			{Name: ecsConnectorIDEnv, Value: args.Slug},
			{Name: "QURL_API_URL", Value: args.APIURL},
		},
		// TODO(qurl-connector-ecs-secret-file): prefer QURL_API_KEY_FILE once the
		// ECS/Fargate guide uses a file-mounted secret runtime instead of native
		// Secrets Manager environment injection.
		Secrets: []ecsSecret{
			{Name: tunnelEnvAPIKey, ValueFrom: "REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_" + args.Slug},
		},
		MountPoints: []ecsMountPoint{
			{SourceVolume: "qurl-agent-state", ContainerPath: "/var/lib/layerv/agent"},
			{SourceVolume: "qurl-config", ContainerPath: "/work", ReadOnly: true},
		},
		LogConfiguration: awslogsConfiguration("/ecs/qurl-connector", "qurl"),
		LinuxParameters:  hardenedECSLinuxParameters(),
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
