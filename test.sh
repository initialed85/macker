#!/bin/sh
set -eu

# End-to-end smoke test for the Macker CLI and its OCI distribution path. It
# builds Linux amd64/arm64 images with Docker, manually creates two-platform
# manifest lists, adds a native Darwin image with macker bundle, verifies the
# resulting three-platform images, then runs the Darwin nginx image locally.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$SCRIPT_DIR"

MACKER=${MACKER:-./bin/macker}
TEST_ID=${TEST_ID:-macker-test-$(date +%Y%m%d%H%M%S)-$$}
SIMPLE_IMAGE=${SIMPLE_IMAGE:-initialed85/macker-test-hello:$TEST_ID}
SIMPLE_AMD64_IMAGE=${SIMPLE_AMD64_IMAGE:-initialed85/macker-test-hello:${TEST_ID}-amd64}
SIMPLE_ARM64_IMAGE=${SIMPLE_ARM64_IMAGE:-initialed85/macker-test-hello:${TEST_ID}-arm64}
SIMPLE_SOURCE_IMAGE=${SIMPLE_SOURCE_IMAGE:-initialed85/macker-test-hello:${TEST_ID}-linux}
NGINX_IMAGE=${NGINX_IMAGE:-initialed85/nginx:latest}
NGINX_AMD64_IMAGE=${NGINX_AMD64_IMAGE:-initialed85/nginx:${TEST_ID}-amd64}
NGINX_ARM64_IMAGE=${NGINX_ARM64_IMAGE:-initialed85/nginx:${TEST_ID}-arm64}
NGINX_SOURCE_IMAGE=${NGINX_SOURCE_IMAGE:-initialed85/nginx:${TEST_ID}-linux}
SIMPLE_CONTAINER=${SIMPLE_CONTAINER:-macker-test-hello}
CONTAINER=${CONTAINER:-nginx}
PORT=${PORT:-8080}
NGINX_PORT=${NGINX_PORT:-8080}
READY_TIMEOUT=${READY_TIMEOUT:-30}
SKIP_PUSH=${SKIP_PUSH:-0}
# SKIP_PUSH retains the old local-only smoke-test behavior. Set
# SKIP_DISTRIBUTION=1 explicitly when the local build/run test is desired
# without skipping the meaning of SKIP_PUSH in a wrapper script.
SKIP_DISTRIBUTION=${SKIP_DISTRIBUTION:-$SKIP_PUSH}

WEBROOT=$(mktemp -d "${TMPDIR:-/tmp}/macker-test.XXXXXX")
RESPONSE=$(mktemp "${TMPDIR:-/tmp}/macker-response.XXXXXX")
STARTED=0
SIMPLE_STARTED=0
DOCKER_CLEANUP=0
MACKER_HOME_DIR=${MACKER_HOME:-$HOME/.macker}
case "$MACKER_HOME_DIR" in
    /*) ;;
    *) MACKER_HOME_DIR=$SCRIPT_DIR/$MACKER_HOME_DIR ;;
esac
CONTAINER_DIR=$MACKER_HOME_DIR/containers/$CONTAINER
SIMPLE_CONTAINER_DIR=$MACKER_HOME_DIR/containers/$SIMPLE_CONTAINER

cleanup() {
    status=$?
    if [ "$SIMPLE_STARTED" -eq 1 ]; then
        "$MACKER" rm --force "$SIMPLE_CONTAINER" >/dev/null 2>&1 || true
    fi
    if [ "$STARTED" -eq 1 ]; then
        "$MACKER" stop "$CONTAINER" >/dev/null 2>&1 || true
        "$MACKER" rm --force "$CONTAINER" >/dev/null 2>&1 || true
    fi
    if [ "$DOCKER_CLEANUP" -eq 1 ] && command -v docker >/dev/null 2>&1; then
        for manifest in \
            "$SIMPLE_SOURCE_IMAGE" \
            "$NGINX_SOURCE_IMAGE"; do
            docker manifest rm "$manifest" >/dev/null 2>&1 || true
        done
        docker image rm \
            "$SIMPLE_AMD64_IMAGE" "$SIMPLE_ARM64_IMAGE" \
            "$NGINX_AMD64_IMAGE" "$NGINX_ARM64_IMAGE" \
            >/dev/null 2>&1 || true
    fi
    rm -rf "$WEBROOT" "$RESPONSE" example/nginx-rootfs
    exit "$status"
}
trap cleanup EXIT INT TERM

require_command() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "required command not found: $1" >&2
        exit 2
    fi
}

docker_build_for_platform() {
    # BuildKit otherwise emits a one-platform image plus a provenance
    # attestation as a manifest list. docker manifest create cannot use that
    # list as one of its child image references, so keep these inputs as
    # plain platform-specific image manifests.
    platform=$1
    dockerfile=$2
    image=$3
    context=$4
    build_arg=$5

    if [ -n "$build_arg" ]; then
        docker build --pull --provenance=false --platform="$platform" \
            --build-arg "$build_arg" \
            -f "$dockerfile" \
            -t "$image" \
            "$context"
    else
        docker build --pull --provenance=false --platform="$platform" \
            -f "$dockerfile" \
            -t "$image" \
            "$context"
    fi
}

# Build two pushed, architecture-specific images and manually assemble the
# corresponding two-platform manifest list. Docker manifest creation works
# from registry references, so the child images must be pushed first.
build_linux_pair() {
    label=$1
    dockerfile=$2
    context=$3
    amd64_image=$4
    arm64_image=$5
    source_image=$6
    build_arg=$7

    echo "==> building $label linux/amd64 image"
    docker_build_for_platform linux/amd64 "$dockerfile" "$amd64_image" "$context" "$build_arg"
    docker push "$amd64_image"

    echo "==> building $label linux/arm64 image"
    docker_build_for_platform linux/arm64 "$dockerfile" "$arm64_image" "$context" "$build_arg"
    docker push "$arm64_image"

    echo "==> creating $label two-platform manifest $source_image"
    docker manifest rm "$source_image" >/dev/null 2>&1 || true
    docker manifest create "$source_image" "$amd64_image" "$arm64_image"
    docker manifest annotate "$source_image" "$amd64_image" --os linux --arch amd64
    docker manifest annotate "$source_image" "$arm64_image" --os linux --arch arm64
    docker manifest push "$source_image"
}

# Verify the registry-facing image is a flat manifest list containing the
# expected platforms. The bundle implementation deliberately flattens nested
# OCI indexes produced by some Skopeo/Docker combinations.
assert_platforms() {
    image=$1
    shift
    raw=$(mktemp "${TMPDIR:-/tmp}/macker-manifest.XXXXXX")
    if ! skopeo inspect --raw "docker://$image" > "$raw"; then
        rm -f "$raw"
        return 1
    fi
    python3 - "$raw" "$@" <<'PY'
import json
import sys

filename = sys.argv[1]
expected_args = sys.argv[2:]
if len(expected_args) % 2:
    raise SystemExit("platform arguments must be OS/architecture pairs")
expected = set(zip(expected_args[::2], expected_args[1::2]))
with open(filename, encoding="utf-8") as stream:
    document = json.load(stream)

manifests = document.get("manifests")
if not isinstance(manifests, list):
    raise SystemExit("registry image is not a manifest list/index")
actual = set()
for descriptor in manifests:
    platform = descriptor.get("platform") or {}
    os_name = platform.get("os")
    architecture = platform.get("architecture")
    if os_name and architecture:
        actual.add((os_name, architecture))

missing = expected - actual
if missing:
    raise SystemExit(f"missing platforms: {sorted(missing)}; got {sorted(actual)}")
print(f"verified {filename}: {sorted(actual)}")
PY
    rm -f "$raw"
}

printf 'macker test volume\n' > "$WEBROOT/macker-test.txt"

echo '==> building macker'
make macker

echo "==> building Darwin simple workload $SIMPLE_IMAGE"
"$MACKER" build \
    -f example/hello/Mackerfile \
    -t "$SIMPLE_IMAGE" \
    .

echo "==> building Darwin nginx workload $NGINX_IMAGE"
"$MACKER" build \
    -f example/nginx/Mackerfile \
    -t "$NGINX_IMAGE" \
    .

if [ "$SKIP_DISTRIBUTION" -eq 0 ]; then
    require_command docker
    require_command python3
    require_command skopeo
    DOCKER_CLEANUP=1

    build_linux_pair \
        'simple example' \
        example/hello/Dockerfile \
        example/hello \
        "$SIMPLE_AMD64_IMAGE" \
        "$SIMPLE_ARM64_IMAGE" \
        "$SIMPLE_SOURCE_IMAGE" \
        ''
    assert_platforms "$SIMPLE_SOURCE_IMAGE" linux amd64 linux arm64

    echo "==> bundling Darwin simple workload into $SIMPLE_IMAGE"
    "$MACKER" bundle "$SIMPLE_SOURCE_IMAGE" "$SIMPLE_IMAGE"
    assert_platforms "$SIMPLE_IMAGE" linux amd64 linux arm64 darwin arm64

    echo '==> running the bundled Linux simple workload on amd64'
    docker run --pull=always --rm --platform=linux/amd64 "$SIMPLE_IMAGE"
    echo '==> running the bundled Linux simple workload on arm64'
    docker run --pull=always --rm --platform=linux/arm64 "$SIMPLE_IMAGE"

    build_linux_pair \
        'nginx example' \
        example/nginx/Dockerfile \
        example/nginx \
        "$NGINX_AMD64_IMAGE" \
        "$NGINX_ARM64_IMAGE" \
        "$NGINX_SOURCE_IMAGE" \
        "NGINX_PORT=$NGINX_PORT"
    assert_platforms "$NGINX_SOURCE_IMAGE" linux amd64 linux arm64

    echo "==> bundling Darwin nginx workload into $NGINX_IMAGE"
    "$MACKER" bundle "$NGINX_SOURCE_IMAGE" "$NGINX_IMAGE"
    assert_platforms "$NGINX_IMAGE" linux amd64 linux arm64 darwin arm64

    echo '==> validating bundled Linux nginx on amd64'
    docker run --pull=always --rm --platform=linux/amd64 "$NGINX_IMAGE" nginx -t
    echo '==> validating bundled Linux nginx on arm64'
    docker run --pull=always --rm --platform=linux/arm64 "$NGINX_IMAGE" nginx -t
else
    echo '==> skipping Docker multi-arch build and registry bundle (SKIP_DISTRIBUTION=1)'
fi

echo '==> running the Darwin simple workload selected from its local image'
SIMPLE_STARTED=1
"$MACKER" run \
    --net=host \
    --name "$SIMPLE_CONTAINER" \
    "$SIMPLE_IMAGE"
"$MACKER" rm "$SIMPLE_CONTAINER"
SIMPLE_STARTED=0

echo "==> starting bundled Darwin nginx container $CONTAINER detached"
"$MACKER" run \
    -d \
    --net=host \
    --name "$CONTAINER" \
    -v "$WEBROOT:/usr/share/nginx/html" \
    "$NGINX_IMAGE"
STARTED=1

echo '==> listing running containers'
"$MACKER" ps

log_path=$CONTAINER_DIR/run.log
response_ok=0
attempt=0
while [ "$attempt" -lt "$READY_TIMEOUT" ]; do
    if curl --fail --silent --show-error "http://127.0.0.1:$PORT/" > "$RESPONSE" 2>/dev/null \
        && grep -q 'macker-test.txt' "$RESPONSE"; then
        response_ok=1
        break
    fi
    attempt=$((attempt + 1))
    sleep 1
done

if [ "$response_ok" -ne 1 ]; then
    echo "HTTP readiness check failed after ${READY_TIMEOUT}s" >&2
    if [ -f "$log_path" ]; then
        echo '--- container log ---' >&2
        cat "$log_path" >&2
    fi
    exit 1
fi

echo '==> HTTP check passed'
cat "$RESPONSE"
echo "==> stopping $CONTAINER"
"$MACKER" stop "$CONTAINER"
STARTED=0
"$MACKER" rm "$CONTAINER"
if [ "$SKIP_DISTRIBUTION" -eq 0 ]; then
    echo "Macker multi-arch bundle smoke test passed; Darwin nginx is at $NGINX_IMAGE"
else
    echo 'Macker local Darwin smoke test passed'
fi
