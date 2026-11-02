#!/bin/sh
# Assemble a native Darwin rootfs for the Woodpecker CI agent.
#
# Both binaries come from upstream pinned releases and are verified against the
# upstream checksums.txt before they are copied into the rootfs. The agent is
# the same release artifact Woodpecker publishes for its Docker image, so the
# Darwin workload speaks the same gRPC protocol as the Linux agents in the
# cluster. plugin-git is included because the local backend resolves the clone
# plugin with exec.LookPath and would otherwise download an unpinned "latest"
# binary into every workflow.
set -eu

output=${1:?usage: build.sh ROOTFS}

if [ "$(uname -s)" != "Darwin" ]; then
    echo "the Woodpecker agent Darwin rootfs must be built on macOS" >&2
    exit 1
fi

agent_version=${WOODPECKER_AGENT_VERSION:-3.20.1}
plugin_git_version=${PLUGIN_GIT_VERSION:-2.11.0}

case "${MACKER_GOARCH:-$(uname -m)}" in
    arm64|aarch64)
        agent_arch=arm64
        agent_sha256=cf8b0cc0623af6a307a5d1c3140512a0542ae16c50f69fe7e0d6c186ac6c6e95
        plugin_git_arch=arm64
        plugin_git_sha256=171c24aa695d1b87cafd4f209d3b5f574a84a49ef9c11e7c1bfc9b749797500b
        ;;
    x86_64|amd64)
        agent_arch=amd64
        agent_sha256=35c1427069c0622e619ac3b0301b98f67598412de848fc95db1a91648ca4a22b
        plugin_git_arch=amd64
        plugin_git_sha256=1a5229844e81d7771b89d13c0387a56070e95f9e28d5e465c08951cb0e5207ff
        ;;
    *)
        echo "unsupported Darwin architecture: ${MACKER_GOARCH:-$(uname -m)}" >&2
        exit 1
        ;;
esac

# Release assets are named without the version; the tag carries it.
agent_archive="woodpecker-agent_darwin_${agent_arch}.tar.gz"
agent_url="https://github.com/woodpecker-ci/woodpecker/releases/download/v${agent_version}/${agent_archive}"
plugin_git_binary="darwin-${plugin_git_arch}_plugin-git"
plugin_git_url="https://github.com/woodpecker-ci/plugin-git/releases/download/${plugin_git_version}/${plugin_git_binary}"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/macker-woodpecker-agent.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$agent_url" -o "$tmp/$agent_archive"
printf '%s  %s\n' "$agent_sha256" "$tmp/$agent_archive" | shasum -a 256 -c - >/dev/null
tar -xzf "$tmp/$agent_archive" -C "$tmp"
agent_source="$tmp/woodpecker-agent"
test -x "$agent_source"

curl -fsSL "$plugin_git_url" -o "$tmp/$plugin_git_binary"
printf '%s  %s\n' "$plugin_git_sha256" "$tmp/$plugin_git_binary" | shasum -a 256 -c - >/dev/null
plugin_git_source="$tmp/$plugin_git_binary"

rm -rf "$output"
mkdir -p "$output/usr/local/bin" "$output/var/lib/woodpecker"

cp "$agent_source" "$output/usr/local/bin/woodpecker-agent"
cp "$plugin_git_source" "$output/usr/local/bin/plugin-git"
chmod 0755 "$output/usr/local/bin/woodpecker-agent" "$output/usr/local/bin/plugin-git"

# The upstream tarballs are signed by CI, but a fresh download carries no local
# quarantine. Re-sign ad-hoc so the copy runs cleanly on a machine that has
# verified nothing, and record the pinned versions for `inspect`-style auditing.
codesign --force --sign - "$output/usr/local/bin/woodpecker-agent" \
    "$output/usr/local/bin/plugin-git" >/dev/null
codesign --verify "$output/usr/local/bin/woodpecker-agent" \
    "$output/usr/local/bin/plugin-git"

printf 'woodpecker-agent %s\nplugin-git %s\n' \
    "$agent_version" "$plugin_git_version" > "$output/usr/local/VERSIONS"
