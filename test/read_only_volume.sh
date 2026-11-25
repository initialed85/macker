#!/bin/sh
set -eu

# Kernel read-only volume check for a Darwin node. This is the acceptance test
# for `-v HOST:CONTAINER:ro`: it proves the launched workload cannot create,
# modify, or unlink inside a read-only host volume, and that the same host path
# stays writable when :ro is omitted.
#
# It needs root or passwordless sudo because mount_nullfs is a privileged
# operation. Run it on a Mac node:
#
#   ./test/read_only_volume.sh
#
# It uses its own throwaway MACKER_HOME and host directory, unmounts and removes
# everything it creates, and touches no existing container.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$SCRIPT_DIR"

MACKER=${MACKER:-./bin/macker}
WORKDIR=$(mktemp -d "${TMPDIR:-/tmp}/macker-ro-volume.XXXXXX")
HOME_DIR="$WORKDIR/home"
HOST_DIR="$WORKDIR/host"
CONTAINER=macker-ro-volume-test
cleanup() {
  "$MACKER" rm --force "$CONTAINER" >/dev/null 2>&1 || true
  for leftover in "$CONTAINER"-reject-*; do
    "$MACKER" rm --force "$leftover" >/dev/null 2>&1 || true
  done
  sudo -n umount "$WORKDIR/mnt" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ "$(uname -s)" = "Darwin" ] || fail "this test enforces a nullfs mount and must run on a Darwin node"
[ -x "$MACKER" ] || fail "$MACKER is not executable; run make first"

# Ask Macker what this node can enforce before blaming any workload. The report
# names the OS fact - a missing null filesystem bundle is not a lookup failure -
# and every helper it searched.
CAPABILITY_REPORT="$("$MACKER" volumes 2>&1)"
if [ "$(printf '%s\n' "$CAPABILITY_REPORT" | head -n 1)" != "read-only volumes: supported" ]; then
  printf '%s\n' "$CAPABILITY_REPORT" >&2
  fail "this node cannot enforce a read-only volume; the report above says why"
fi
sudo -n /sbin/mount -h >/dev/null 2>&1 || fail "passwordless sudo for /sbin/mount is required to mount a read-only volume"

mkdir -p "$HOST_DIR"
echo "original" > "$HOST_DIR/payload"
printf 'exit 1\n' > "$HOST_DIR/existing-script.sh"
chmod +x "$HOST_DIR/existing-script.sh"

export MACKER_HOME="$HOME_DIR"

echo "==> :ro volume must deny create, modify and unlink for the launched workload"
"$MACKER" run --rm --name "$CONTAINER" --entrypoint /bin/sh \
  -v "$HOST_DIR:/host:ro" scratch -- -c '
    set -e
    test "$(cat /host/payload)" = "original" || { echo "read failed"; exit 1; }
    touch /host/new-file 2>&1 | tee /tmp/create.out
    grep -q "Read-only file system" /tmp/create.out || { echo "create was not denied"; exit 1; }
    echo rewrite >> /host/payload 2>&1 | tee /tmp/modify.out
    grep -q "Read-only file system" /tmp/modify.out || { echo "modify was not denied"; exit 1; }
    rm /host/payload 2>&1 | tee /tmp/unlink.out
    grep -q "Read-only file system" /tmp/unlink.out || { echo "unlink was not denied"; exit 1; }
    echo read-only-denies-ok
  ' | tail -1 | grep -q read-only-denies-ok || fail ":ro volume did not deny create/modify/unlink"

test "$(cat "$HOST_DIR/payload")" = "original" || fail "the workload changed host state through a read-only mount"

echo "==> inspect reports the read-only state"
"$MACKER" run -d --name "$CONTAINER" --entrypoint /bin/sh -v "$HOST_DIR:/host:ro" scratch -- -c 'sleep 30' >/dev/null
"$MACKER" inspect --format json "$CONTAINER" | grep -q '"read_only": true' || fail "inspect did not report read_only: true"
"$MACKER" inspect --format json "$CONTAINER" | grep -q '"mounted_volumes"' || fail "inspect did not report mounted_volumes"
sudo -n /sbin/mount -p | grep -q "on $HOME_DIR/containers/$CONTAINER/rootfs/host (nullfs" || fail "no nullfs mount was live for the running container"
"$MACKER" stop "$CONTAINER" >/dev/null
if sudo -n /sbin/mount -p | grep -q "rootfs/host (nullfs"; then
  fail "the nullfs mount survived macker stop"
fi
"$MACKER" rm "$CONTAINER" >/dev/null

echo "==> plain HOST:CONTAINER stays writable"
"$MACKER" run --rm --name "$CONTAINER" --entrypoint /bin/sh \
  -v "$HOST_DIR:/host" scratch -- -c '
    set -e
    echo writable >> /host/payload && echo writable-ok
  ' | tail -1 | grep -q writable-ok || fail "a plain volume was not writable"

echo "==> macker run --help advertises HOST:CONTAINER[:ro]"
"$MACKER" run --help 2>&1 | grep -q 'HOST:CONTAINER\[:ro\]' || fail "run --help does not advertise HOST:CONTAINER[:ro]"

echo "==> :rw and unknown options are rejected"
index=0
for spec in "$HOST_DIR:/host:rw" "$HOST_DIR:/host:noexec"; do
  index=$((index + 1))
  if "$MACKER" run --rm --name "$CONTAINER-reject-$index" --entrypoint /bin/sh -v "$spec" scratch -- -c 'exit 0' >/dev/null 2>&1; then
    fail "$spec was accepted"
  fi
done

echo "PASS: read-only host volumes are kernel-enforced on this node"
