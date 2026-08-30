#!/bin/sh
# Build a native Darwin Echo-Server image from the upstream release. The
# bundled webpack output contains the application dependencies, so the runtime
# only needs Node and global.json alongside it.
set -eu

output=${1:?usage: build.sh ROOTFS}
node_version=22.23.2
node_archive="node-v${node_version}-darwin-arm64.tar.gz"
node_url="https://nodejs.org/dist/v${node_version}/${node_archive}"
node_sha256=61130f394c1630d211dd50aecc4353d379480f36d3ac913cd85dbba1aed585c6
echo_version=0.9.2
echo_archive="echo-server-${echo_version}.tar.gz"
echo_url="https://github.com/Ealenn/Echo-Server/archive/refs/tags/${echo_version}.tar.gz"
echo_sha256=212f359676b003560bb4e55d1798f8859e34d55f99930d1b2971bf8216e2d9fd

tmp=$(mktemp -d "${TMPDIR:-/tmp}/macker-echo.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$node_url" -o "$tmp/$node_archive"
printf '%s  %s\n' "$node_sha256" "$tmp/$node_archive" | shasum -a 256 -c - >/dev/null
tar -xzf "$tmp/$node_archive" -C "$tmp"
node_prefix="$tmp/node-v${node_version}-darwin-arm64"

curl -fsSL "$echo_url" -o "$tmp/$echo_archive"
printf '%s  %s\n' "$echo_sha256" "$tmp/$echo_archive" | shasum -a 256 -c - >/dev/null
mkdir "$tmp/source"
tar -xzf "$tmp/$echo_archive" --strip-components=1 -C "$tmp/source"

# Use the pinned official Node distribution for the build as well as runtime;
# npm ci and webpack produce one self-contained application bundle.
PATH="$node_prefix/bin:/usr/bin:/bin"
export PATH
cd "$tmp/source"
npm ci --include=dev --ignore-scripts --no-audit --no-fund
npm run build

rm -rf "$output"
mkdir -p "$output/usr/local/bin" "$output/app"
cp "$node_prefix/bin/node" "$output/usr/local/bin/node"
cp dist/webserver.js src/global.json "$output/app/"
chmod 0755 "$output/usr/local/bin/node"

printf 'created Echo-Server %s Darwin rootfs at %s\n' "$echo_version" "$output"
file "$output/usr/local/bin/node"
