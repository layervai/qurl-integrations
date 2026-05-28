package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type ecsContainerDefinition struct {
	Name             string              `json:"name"`
	Image            string              `json:"image"`
	Essential        bool                `json:"essential"`
	Environment      []ecsEnvironmentVar `json:"environment"`
	Secrets          []ecsSecret         `json:"secrets"`
	MountPoints      []ecsMountPoint     `json:"mountPoints"`
	LogConfiguration ecsLogConfiguration `json:"logConfiguration"`
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
	ecsFargateChecklistText         = "ECS/Fargate task-definition checklist"
	ecsFargateRegionPlaceholderNote = "Also replace the `<region>` placeholder in the `awslogs-region` field below."
)

func renderECSFargateTunnelInstructions(args *tunnelInstallArgs, image string) (string, error) {
	containerJSON, err := renderECSSidecarContainerJSON(args, image)
	if err != nil {
		return "", err
	}
	secretName := "qurl-tunnel-" + args.Slug
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
		"Create the AWS Secrets Manager secret as `" + secretName + "` so the task definition's `valueFrom` ARN resolves.",
		"Replace `<region>`, `<account-id>`, and `<suffix>` with the full secret ARN shown by Secrets Manager; AWS appends a random suffix to secret ARNs.",
		ecsFargateRegionPlaceholderNote,
		"Fargate's awsvpc network mode shares one task ENI across containers, so no explicit network_mode is needed; `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the target container.",
	}, " ")
	return intro + "\n\n" +
		"1. Store the bootstrap key shown above in AWS Secrets Manager, then treat this Slack message as secret until the sidecar connects.\n\n" +
		"2. Put qurl-proxy.yaml at `/work/qurl-proxy.yaml` on an EFS access point mounted into the task as the `qurl-config` volume:\n\n" +
		configBlock + "\n\n" +
		"3. Add this non-essential sidecar container to the same task definition as the target container. ECS injects this bootstrap secret as `QURL_API_KEY`, which is an environment variable; file-mounted secret runtimes should use `QURL_API_KEY_FILE` instead:\n\n" +
		containerBlock + "\n\n" +
		"4. Add durable EFS-backed volumes named qurl-agent-state and qurl-config. Do not share qurl-agent-state across concurrently running sidecars. After the task logs show the tunnel connected, delete the bootstrap secret. For future bootstrap rotation, prefer a file-mounted secret runtime so new bootstrap keys are not exposed through task environment variables.", nil
}

func renderECSSidecarContainerJSON(args *tunnelInstallArgs, image string) (string, error) {
	container := ecsContainerDefinition{
		Name:      "qurl-tunnel",
		Image:     image,
		Essential: false,
		Environment: []ecsEnvironmentVar{
			{Name: "QURL_TUNNEL_SLUG", Value: args.Slug},
		},
		// TODO(qurl-tunnel-ecs-secret-file): prefer QURL_API_KEY_FILE once the
		// ECS/Fargate guide uses a file-mounted secret runtime instead of native
		// Secrets Manager environment injection.
		Secrets: []ecsSecret{
			{Name: tunnelEnvAPIKey, ValueFrom: "arn:aws:secretsmanager:<region>:<account-id>:secret:qurl-tunnel-" + args.Slug + "-<suffix>"},
		},
		MountPoints: []ecsMountPoint{
			{SourceVolume: "qurl-agent-state", ContainerPath: "/var/lib/layerv/agent"},
			{SourceVolume: "qurl-config", ContainerPath: "/work", ReadOnly: true},
		},
		LogConfiguration: ecsLogConfiguration{
			LogDriver: "awslogs",
			Options: map[string]string{
				"awslogs-group":         "/ecs/qurl-tunnel",
				"awslogs-region":        "<region>",
				"awslogs-stream-prefix": "qurl",
			},
		},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(container); err != nil {
		return "", fmt.Errorf("marshal ECS sidecar JSON: %w", err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}
