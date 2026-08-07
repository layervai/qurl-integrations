#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: teams-manual-deploy-test-only.sh --env-file PATH [--image-tag TAG] [--container-name NAME] [--host-port PORT] [--platform PLATFORM] [--version VERSION]

Manual deploy helper for apps/teams test environments.
Use this for sandbox/manual verification only; normal deploys should flow via CI.
EOF
}

IMAGE_TAG="${IMAGE_TAG:-qurl-bot-teams:test-only}"
CONTAINER_NAME="${CONTAINER_NAME:-qurl-bot-teams-test-only}"
HOST_PORT="${HOST_PORT:-8080}"
PLATFORM="${PLATFORM:-linux/arm64}"
VERSION="${VERSION:-manual-test-only}"
ENV_FILE=""

while [[ $# -gt 0 ]]; do
	case "$1" in
		--env-file)
			ENV_FILE="${2:-}"
			shift 2
			;;
		--image-tag)
			IMAGE_TAG="${2:-}"
			shift 2
			;;
		--container-name)
			CONTAINER_NAME="${2:-}"
			shift 2
			;;
		--host-port)
			HOST_PORT="${2:-}"
			shift 2
			;;
		--platform)
			PLATFORM="${2:-}"
			shift 2
			;;
		--version)
			VERSION="${2:-}"
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			echo "error: unknown arg: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

if [[ -z "$ENV_FILE" ]]; then
	echo "error: --env-file is required" >&2
	usage >&2
	exit 2
fi

if [[ ! -f "$ENV_FILE" ]]; then
	echo "error: env file not found: $ENV_FILE" >&2
	exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "building test-only Teams image: $IMAGE_TAG"
docker buildx build \
	--platform "$PLATFORM" \
	--load \
	-f "$REPO_ROOT/apps/teams/Dockerfile" \
	-t "$IMAGE_TAG" \
	--build-arg VERSION="$VERSION" \
	"$REPO_ROOT"

echo "restarting test-only Teams container: $CONTAINER_NAME"
docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
docker run -d \
	--restart unless-stopped \
	--name "$CONTAINER_NAME" \
	-p "${HOST_PORT}:8080" \
	--env-file "$ENV_FILE" \
	"$IMAGE_TAG"

echo "done: $CONTAINER_NAME"
echo "logs: docker logs -f $CONTAINER_NAME"
