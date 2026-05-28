package internal

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/layervai/qurl-integrations/shared/client"
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

func renderECSFargateTunnelInstructions(args *tunnelInstallArgs, _ *client.APIKey, image string) string {
	containerJSON := renderECSSidecarContainerJSON(args, image)
	secretName := "qurl-tunnel-" + args.Slug
	intro := strings.Join([]string{
		"Use this as an ECS/Fargate task-definition checklist.",
		"Create the AWS Secrets Manager secret as `" + secretName + "` so the task definition's `valueFrom` ARN resolves.",
		"Replace `<region>`, `<account-id>`, and `<suffix>` with the full secret ARN shown by Secrets Manager; AWS appends a random suffix to secret ARNs.",
		"Fargate's awsvpc network mode shares one task ENI across containers, so no explicit network_mode is needed; `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the target container.",
	}, " ")
	return intro + "\n\n" +
		"1. Store the bootstrap key shown above in AWS Secrets Manager, then treat this Slack message as secret until the sidecar connects.\n\n" +
		"2. Put qurl-proxy.yaml on an EFS access point mounted into the task:\n\n" +
		slackCodeBlock(renderTunnelConfigYAML(args)) + "\n\n" +
		"3. Add this non-essential sidecar container to the same task definition as the target container. ECS injects the bootstrap secret as `QURL_API_KEY`; file-mounted secret runtimes use `QURL_API_KEY_FILE` instead:\n\n" +
		slackCodeBlock(containerJSON) + "\n\n" +
		"4. Add durable EFS-backed volumes named qurl-agent-state and qurl-config. Do not share qurl-agent-state across concurrently running sidecars. After the task logs show the tunnel connected, delete the bootstrap secret."
}

func renderECSSidecarContainerJSON(args *tunnelInstallArgs, image string) string {
	container := ecsContainerDefinition{
		Name:      "qurl-tunnel",
		Image:     image,
		Essential: false,
		Environment: []ecsEnvironmentVar{
			{Name: "QURL_TUNNEL_SLUG", Value: args.Slug},
		},
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
	b, err := json.MarshalIndent(container, "", "  ")
	if err != nil {
		panic("marshal ECS sidecar JSON: " + err.Error())
	}
	return string(b)
}
