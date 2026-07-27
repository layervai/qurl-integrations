package internal

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	defaultTunnelImage  = "ghcr.io/layervai/qurl-connector:latest"
	defaultTunnelPort   = 8080
	tunnelEnvDocker     = "docker"
	tunnelEnvCompose    = "docker-compose"
	tunnelEnvComposeAlt = "compose"
	tunnelEnvECSFargate = "ecs-fargate"
	tunnelEnvKubernetes = "kubernetes"
)

var tunnelSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)
var tunnelServicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type TunnelInstallArgs struct {
	Slug          string
	Alias         string
	Environment   string
	Port          int
	Service       string
	TunnelImage   string
	BootstrapKey  string
	BootstrapNote string
}

func renderTunnelInstallMessage(args TunnelInstallArgs) (string, error) {
	if err := validateTunnelSlug(args.Slug); err != nil {
		return "", err
	}
	env, err := normalizeTunnelEnvironment(args.Environment)
	if err != nil {
		return "", err
	}
	if err := validateTunnelService(args.Service); err != nil {
		return "", err
	}
	if args.Service != "" && env != tunnelEnvCompose {
		return "", errors.New("service is only supported with env:compose or env:docker-compose")
	}
	port := args.Port
	if port <= 0 {
		port = defaultTunnelPort
	}
	image := strings.TrimSpace(args.TunnelImage)
	if err := ValidateTunnelImageRef(image); err != nil {
		return "", err
	}
	if image == "" {
		image = defaultTunnelImage
	}
	var body strings.Builder
	fmt.Fprintf(&body, "qURL Connector `%s` is protected in this channel as `$%s`.\n", args.Slug, args.Alias)
	body.WriteString("The bootstrap key below is short-lived. Remove it after the connector comes online.\n\n")
	if args.BootstrapNote != "" {
		body.WriteString(args.BootstrapNote)
		body.WriteString("\n\n")
	}
	body.WriteString("Bootstrap key:\n")
	body.WriteString("```text\n")
	body.WriteString(args.BootstrapKey)
	body.WriteString("\n```\n\n")
	switch env {
	case "compose", "docker-compose":
		service := args.Service
		if service == "" {
			service = "web"
		}
		fmt.Fprintf(&body, "Docker Compose update for service `%s`:\n", service)
		body.WriteString("```yaml\n")
		fmt.Fprintf(&body, "services:\n  %s:\n    environment:\n      QURL_BOOTSTRAP_KEY: %s\n      QURL_CONNECTOR_ID: %s\n      QURL_LOCAL_PORT: %s\n    image: %s\n", service, strconv.Quote(args.BootstrapKey), strconv.Quote(args.Slug), strconv.Quote(strconv.Itoa(port)), strconv.Quote(image))
		body.WriteString("```\n")
	case "ecs-fargate":
		body.WriteString("ECS/Fargate environment:\n")
		body.WriteString("```text\n")
		fmt.Fprintf(&body, "image=%s\nQURL_BOOTSTRAP_KEY=%s\nQURL_CONNECTOR_ID=%s\nQURL_LOCAL_PORT=%d\n", image, args.BootstrapKey, args.Slug, port)
		body.WriteString("```\n")
	case "kubernetes":
		body.WriteString("Kubernetes fragment:\n")
		body.WriteString("```yaml\n")
		fmt.Fprintf(&body, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: qurl-bootstrap-%s\ntype: Opaque\nstringData:\n  api_key: %s\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: qurl-connector-%s\nspec:\n  template:\n    spec:\n      containers:\n        - name: qurl-connector\n          image: %s\n          env:\n            - name: QURL_BOOTSTRAP_KEY\n              valueFrom:\n                secretKeyRef:\n                  name: qurl-bootstrap-%s\n                  key: api_key\n            - name: QURL_CONNECTOR_ID\n              value: %s\n            - name: QURL_LOCAL_PORT\n              value: %s\n", args.Slug, strconv.Quote(args.BootstrapKey), args.Slug, strconv.Quote(image), args.Slug, strconv.Quote(args.Slug), strconv.Quote(strconv.Itoa(port)))
		body.WriteString("```\n")
	default:
		body.WriteString("Docker command:\n")
		body.WriteString("```bash\n")
		fmt.Fprintf(&body, "docker run -d --restart unless-stopped \\\n  -e QURL_BOOTSTRAP_KEY=%s \\\n  -e QURL_CONNECTOR_ID=%s \\\n  -e QURL_LOCAL_PORT=%d \\\n  %s\n", shellQuote(args.BootstrapKey), shellQuote(args.Slug), port, shellQuote(image))
		body.WriteString("```\n")
	}
	body.WriteString("\nAfter the connector is healthy, remove `QURL_BOOTSTRAP_KEY` from the runtime and keep only the connector's durable state.")
	return body.String(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func ValidateTunnelImageRef(image string) error {
	if image == "" {
		return nil
	}
	for _, r := range image {
		if !isTunnelImageRefRune(r) {
			return errors.New("tunnel image reference must use only letters, numbers, dots, slashes, colons, at signs, underscores, or hyphens; Docker tags do not support '+' build metadata")
		}
	}
	return nil
}

func isTunnelImageRefRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		strings.ContainsRune("./:@_-", r)
}

func normalizeTunnelEnvironment(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", tunnelEnvDocker:
		return tunnelEnvDocker, nil
	case tunnelEnvCompose, tunnelEnvComposeAlt:
		return tunnelEnvCompose, nil
	case tunnelEnvECSFargate:
		return tunnelEnvECSFargate, nil
	case tunnelEnvKubernetes:
		return tunnelEnvKubernetes, nil
	default:
		return "", errors.New("env must be one of docker, compose, docker-compose, ecs-fargate, or kubernetes")
	}
}

func validateTunnelSlug(slug string) error {
	if !tunnelSlugPattern.MatchString(strings.TrimSpace(slug)) {
		return errors.New("connector id must be 3-64 chars, lowercase letters/numbers/hyphens, start with a letter, and end with a letter or number")
	}
	return nil
}

func validateTunnelService(service string) error {
	service = strings.TrimSpace(service)
	if service == "" {
		return nil
	}
	if !tunnelServicePattern.MatchString(service) {
		return errors.New("service must use letters, numbers, underscores, or hyphens")
	}
	return nil
}
