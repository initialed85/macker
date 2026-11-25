# Macker

Macker is an experimental OCI workload tool for native macOS executables on
Apple Silicon and Intel Macs. It packages Mach-O programs as a native
`darwin/arm64` or `darwin/amd64` OCI image and runs them as ordinary host
processes.

Macker is **not** a Linux container runtime. It provides no process,
filesystem, network, or resource isolation. Treat images and `RUN` commands as
trusted host workloads.

## Why?

I run K3s at home and use my Mac for local AI work. Macker is an experiment
toward blending those worlds: building and distributing OCI images containing
Darwin binaries, then scheduling AI workloads on the Mac as transparently as
possible alongside Linux nodes in the K3s cluster.

## Requirements

- macOS on Apple Silicon or Intel for building and running Darwin workloads
- Go 1.22+ (Go 1.22 is the last Go release whose binaries run on macOS Catalina)
  when Catalina compatibility is required
- `ifconfig` and `pfctl` (included with macOS) plus root or passwordless `sudo` for Darwin network setup
- [Skopeo](https://github.com/containers/skopeo) for registry operations
- Homebrew and its `nginx`, `pcre2`, `openssl@3`, and `ca-certificates` packages
  for the nginx example
- Docker and Python 3 only for the full distribution test

The Go tool uses the standard library. Docker is not required for local OCI
builds or native runs.

## Quick start

```sh
git clone git@github.com:initialed85/macker.git
cd macker
make macker

./bin/macker build \
  -f example/hello/Mackerfile \
  -t hello-darwin:latest \
  .
./bin/macker run --net=host --name=hello hello-darwin:latest
./bin/macker rm hello
```

State is stored under `~/.macker` by default. Set `MACKER_HOME` to use another
location.

## Mackerfiles

Macker supports a deliberately small Dockerfile-like syntax:

```Dockerfile
FROM scratch
RUN command
COPY source /absolute/destination
ENV KEY=value
WORKDIR /
ENTRYPOINT ["/absolute/executable"]
CMD ["argument"]
EXPOSE 8080
```

Supported instructions are `FROM scratch`, `RUN`, `COPY`, `ENV`, `WORKDIR`,
JSON-array `ENTRYPOINT`/`CMD`, and `EXPOSE`. `EXPOSE` is metadata only; runtime
port publishing is explicit with `run -p`. Mackerfile entrypoints and commands
must be absolute paths; runtime `run --entrypoint COMMAND` may instead select a
command by the image's `PATH`.

`RUN` executes `/bin/bash -c` on the host with the build context as its working
directory. It receives `MACKER_CONTEXT`, `MACKER_ROOTFS`, the target
`MACKER_GOARCH` (`arm64` or `amd64`), the host environment, and Mackerfile
`ENV` values, and can read or modify the host as the invoking user. `RUN` and
`COPY` steps mutate one temporary rootfs in order; they do not create separate
cached layers. The final image contains one uncompressed tar
layer.

`COPY` sources are relative to the build context and cannot escape it;
destinations must be absolute image paths. Non-`scratch` bases, shell-form
commands, build arguments, and other Docker build features are not implemented.

## Runtime configuration substitution

When Macker materializes a rootfs for `run` or `oci run`, it replaces explicit
runtime tokens in regular UTF-8 text/configuration files. Supported extensions
include `.conf`, `.env`, `.ini`, `.json`, `.properties`, `.sh`, `.toml`, `.txt`,
`.xml`, `.yaml`, and `.yml`. For example:

```text
listen ____MACKER_PORT_1____;
```

becomes `listen 8080;` when `MACKER_PORT_1=8080` is present. Tokens use the
form `____MACKER_NAME____`; the available `MACKER_*` values come from the final
workload environment. An unset Macker token fails startup rather than leaving
an invalid configuration behind, and the failure names the flag that would have
supplied the value. A port token needs a published port: `MACKER_PORT_1`,
`MACKER_PORT_2`, and so on come from the first, second, and later `-p`
mappings in flag order, so an image whose configuration carries port tokens
must be run with at least that many `-p` mappings, or with
`--env MACKER_PORT_N=PORT` supplied explicitly. On a maclet node the two shapes
are chosen by Pod class: a Pod IP workload gets `-p <containerPort>:auto/tcp`
so PF can accept traffic at the Pod IP, while a `hostNetwork` Pod or a Pod
labelled `k8s-darwin.dev/disable-port-forward` gets `--env MACKER_PORT_N=<containerPort>`
and no published port, because there the pod's port already is the host port.
maclet owns those Kubernetes-side rules and documents them, with measured tables,
in maclet's `KUBERNETES-PARITY.md` under "Ports and Macker config tokens"; this
document covers only what Macker does with the value. Macker skips symlinks, binary-looking files,
and unlisted extensions, and only changes the per-container rootfs—not the
stored image or host-backed volume targets. Direct `oci run` callers can supply
values with repeated `--env` flags.

## Registry and multi-platform images

Authenticate Skopeo before using registry commands:

```sh
skopeo login docker.io
```

Push or pull a Darwin image:

```sh
./bin/macker push initialed85/nginx-darwin:latest
./bin/macker pull initialed85/nginx-darwin:latest
```

`pull` selects the native Darwin manifest matching the host architecture
(`darwin/arm64` on Apple Silicon or `darwin/amd64` on Intel) from a
multi-platform image and installs it locally. Registry image references must be
tagged; Docker Hub is assumed when no registry is specified. The local `scratch` sentinel is the
exception and may be written as either `scratch` or `scratch:latest`.

`bundle` combines a source image with a locally stored Darwin image. A source
is read from local storage when present, otherwise pulled from its registry.
Source platforms are preserved, and any existing Darwin entry matching the
host architecture is replaced. The result is pushed to the Darwin image
reference by default:

```sh
./bin/macker bundle nginx:latest initialed85/nginx-darwin:latest
```

Use `--no-push` to keep the merged image local. Bundling only combines image
manifests; it does not reconcile entrypoints, arguments, environment, or
application behaviour across platforms.

Ask what an image offers with `platforms`. It reads local storage when the
reference is stored, otherwise it asks the registry through Skopeo:

```sh
./bin/macker platforms busybox:1.36
# docker.io/library/busybox:1.36 (registry)
# platforms: linux/386, linux/amd64, linux/arm64, ...
# runnable here (darwin/arm64): no
# reason: docker.io/library/busybox:1.36 offers linux/386, linux/amd64, ...; Macker needs a
# darwin/arm64 manifest, and Linux manifests cannot execute on macOS. Use
# `macker run --entrypoint COMMAND -v HOST:CONTAINER[:ro] scratch` for volume-mounted Darwin jobs,
# or build a Darwin image for the workload

./bin/macker platforms --format json nginx:latest
```

`pull` runs the same check before copying anything, so a reference with no
Darwin manifest for the host architecture fails with the platform list instead
of storing an image that can never start.

## Running workloads

`run` requires explicit `--net=host` or `--net=external` networking and a
name. The special image references `scratch` and `scratch:latest` create an
empty Darwin rootfs on demand without pulling or registering an image. Scratch
has no image command, so Macker uses the trusted host-command fallback (default
`/bin/sh`, or an explicit `--entrypoint`) and is intended for volume-mounted
jobs and scripts. Repeat `--env KEY=VALUE` to add or override image environment values;
these values are persisted with the container and inherited by `exec`. Foreground
runs inherit the terminal for output; use `-i` to attach
standard input and `-t` to attach the caller's terminal (`-it` is the usual
interactive shell form). TTY runs preserve an image or explicit `TERM`;
otherwise they pass through the caller's `TERM` or use the safe
`xterm-256color` default. Interactive runs must stay in the foreground. Detached runs write to
`~/.macker/containers/<name>/run.log` (or the corresponding `MACKER_HOME` path).
Add `--rm` to remove the container rootfs, metadata, and state entry after a
foreground exit or when `stop`/`ps` observes the workload exit. Host-network
setup requires root or passwordless `sudo` because Macker creates host bridge
interfaces. External networking only inspects an existing interface and
address; PF publishing still requires root or passwordless `sudo`.

In host mode, each high-level `run` creates or reuses the host-side `bridge88` with
`172.31.88.1/24`, then allocates a per-container bridge beginning at `bridge881`
with `172.31.89.1/24`, `172.31.90.1/24`, and so on. This stays within the
RFC1918 private `172.16.0.0/12` range while avoiding the commonly used `10.0.0.0/8`
space. These are initial host-side networking primitives, not network
namespaces: the workload remains an ordinary host process and the bridges have
no members or VXLAN plumbing yet.

Macker supplies the workload with `MACKER_INTERFACE`, `MACKER_IP`,
`MACKER_HOST_INTERFACE`, and `MACKER_HOST_IP` (for example, `bridge881` and
`172.31.89.1`). Use `--workdir /absolute/path` to override the image's
configured working directory; omitting it preserves the image default. The
normalized override is persisted in metadata and inherited by later `exec`
children. In external mode, `MACKER_INTERFACE` and `MACKER_IP` are the
supplied existing interface and Pod IP; host-owned values are empty because
Macker does not own or infer that network's gateway, unless optional
`--host-interface IFACE --host-ip HOST_IP` values are supplied. For each published mapping
it also supplies `MACKER_PORT_1`, `MACKER_PORT_2`, and so on, containing the
workload-side port. The workload is expected—but cannot be forced—to bind only
to its supplied IP and ports.

Use `--entrypoint COMMAND` to replace the image entrypoint while debugging or
running a different process. Arguments after the image replace the image CMD;
for example, `run --entrypoint bash IMAGE -c 'env'` runs `bash -c 'env'`.
Non-chroot execution prefers the image rootfs, but an explicitly overridden
entrypoint may fall back to a matching host executable for debugging; Macker
prints a warning when it does so. Chroot execution does not use that fallback.
Use Docker-style
`-p HOST_PORT:NODE_PORT[/tcp|/udp]` to publish a port through macOS PF.
`NODE_PORT` is the port the workload actually listens on; the default protocol
is TCP. Use `auto` or `0` for `NODE_PORT` to allocate a random free port in the
Kubernetes-style `30000-32767` range. The resolved port is injected through
`MACKER_PORT_N`, persisted in container metadata, and returned by
`inspect --format json` under `ports`.

Host-mode rules retain the existing any-destination behavior. External
attached-network rules match the supplied workload IP as the PF destination,
then redirect that IP and `HOST_PORT` to the workload's `NODE_PORT`. This lets
multiple external workloads reuse the same `HOST_PORT` when their target IPs
differ, while automatic node-port allocation prevents native wildcard
listeners from colliding on the host. Macker uses macOS's existing dynamic PF
anchor and does not replace the main ruleset. Repeat `-p` for additional
mappings, such as `-p 53:30553/udp`. This initial implementation is IPv4-only
and does not add special VXLAN or bridge-member handling. PF redirects ingress
traffic; a connection from the Mac to its own published address may require
testing from another host.

```sh
mkdir -p /tmp/macker-www
printf 'hello from a Macker volume\n' > /tmp/macker-www/index.html
./bin/macker run \
  --detach \
  --rm \
  --net=host \
  --name=nginx \
  -p 80:8080/tcp \
  -v /tmp/macker-www:/usr/share/nginx/html \
  initialed85/nginx-darwin:latest

./bin/macker ps
./bin/macker stop nginx
```

For a network already attached by another component, such as maclet, pass the
existing interface and Pod IP explicitly. Macker validates both and does not
create or destroy any `bridge88*` interface in this mode. Optional host interface
and IP values can also be supplied when the attached network exposes them:

```sh
./bin/macker run \
  --net=external \
  --interface bridge101 \
  --ip 10.42.1.3 \
  --host-interface bridge101 \
  --host-ip 10.42.1.1 \
  --name=nginx \
  -p 80:8080/tcp \
  initialed85/nginx-darwin:latest

# A volume-mounted script can run without building or pulling an image.
./bin/macker run --rm --net=external \
  --interface bridge101 --ip 10.42.1.4 \
  --name=job \
  --entrypoint /job/run.sh \
  -v "$PWD/job:/job" \
  scratch
```

For an interactive debug shell, use an image containing a shell or let Macker
fall back to the host shell in non-chroot mode:

```sh
./bin/macker run --rm -it --net=host --name=debug \
  --entrypoint bash initialed85/nginx-darwin:latest -- -i

# Against an already-running detached container:
./bin/macker exec -it NAME -- /bin/sh
./bin/macker logs --follow NAME
```

`exec` requires a running detached container, starts an additional process
with the image and run-supplied environment, working directory, network values,
and materialized rootfs, and defaults to `/bin/sh`. It does not inherit
environment or directory changes made by the entrypoint. `logs` reads the captured stdout/stderr of a
detached run; foreground runs do not create a log file. `-t` uses the caller's
terminal rather than providing process or filesystem isolation.

`pause` and `unpause` provide a non-destructive suspension lifecycle for
detached workloads. `pause NAME` sends `SIGSTOP` to the workload process group
(and tracked `exec` process groups), persists a `paused` status, and retains the
rootfs, volumes, PF mappings, and network configuration. `unpause NAME` sends
`SIGCONT` and clears that state. Paused containers remain visible in `ps` and
`inspect`; `exec` is unavailable while paused. This is process-group control,
not namespace or resource isolation, and workloads that daemonize into another
session are outside Macker's process-group guarantee. Before signaling, Macker
validates the recorded launcher/workload PID start times; missing or mismatched
identity fails without marking the container paused. Signal or metadata failures
attempt to roll back the process state. `stop` remains the
terminating/cleanup operation: if a container is paused, it is resumed before
being terminated and its PF/network resources are removed. Exact command
replay after a destructive stop is not part of this lifecycle yet.

Use `inspect` to retrieve lifecycle and exit information. The machine-readable
form is stable JSON and refreshes detached exit state from the persisted status
record before printing:

```sh
./bin/macker inspect --format json NAME
# `--json NAME` is an alias
```

The JSON object includes `status` (`running`, `paused`, `exited`, or `stopped`),
`working_dir` (the normalized run override, or an empty string when the image
default is in use), the tracked launcher `pid` and `launcher_started_at`, the running or completed
workload `workload_pid` and `workload_started_at` when available, `exit_code` (including the conventional `128 + signal` value
for signal termination), `started_at`, `finished_at`, `termination_signal`,
`termination_reason` (`exited`, `exited-with-error`, `signal`, or `stopped`),
and `paused_at` when the workload is suspended. The object also includes
`ports`, an array of `{host_port, node_port, protocol}` objects containing
resolved mappings (including automatically allocated ports).
Detached `run` records this information when its OCI child exits; `inspect` and
`ps` reconcile it into `metadata.json`.

Use `macker rm --force NAME` to stop and remove a running or paused container.
`macker images` lists local images and `macker rmi IMAGE` removes an image layout;
removing an image does not remove existing container rootfs directories.
Writable volumes are live symlinks to host paths, not kernel mounts. A volume
marked `HOST:CONTAINER:ro` is kernel-enforced *when the node can mount nullfs*:
Macker creates a mount point under the container rootfs and mounts the host path
read-only (`/sbin/mount_nullfs -o ro`, falling back to `mount -t null -o ro`), so
create, modify and unlink inside it fail with `EROFS` for the launched workload
and for every other process on the node while the mount is live. That mount path
is not guaranteed to exist on a given macOS release - home-ops measured icarus-2
(Darwin 25.6, macOS 26) and found no `mount_nullfs` there - so a node either
provides a nullfs mount helper or a nullfs filesystem type for `mount(8)` and lets
Macker run mount privileged, or `:ro` is refused and the container does not start.
It works only on Darwin nodes, and applies to directory host paths because a
nullfs mount cannot make a single file read-only. The mount is recorded in the container metadata as
`mounted_volumes` and removed on workload exit, `stop`, `rm`, and the next
`ps`/`inspect` after a container is killed externally; `inspect --format json`
reports both the requested `volumes` and the currently `mounted_volumes` with
their `read_only` state. Host networking is
shared, so wildcard listeners can collide on ports. `stop`, `rm`, foreground
exit, and `ps` cleanup remove PF mappings; a detached workload that is killed
externally is cleaned up the next time it is observed by Macker.

## Volumes

`-v HOST:CONTAINER` maps a host path into the container rootfs as a live
symlink, which is what a non-chroot host process needs and nothing more.

`-v HOST:CONTAINER:ro` is enforced instead of trusted, when the node has a nullfs
mount path. Macker mounts the host path read-only with `mount_nullfs -o ro` (or
`mount -t null -o ro`) at a mount point inside the container rootfs, so the
launched workload cannot create, modify or unlink anything under it:

```sh
./bin/macker run --rm --name reader --entrypoint /bin/sh \
  -v "$HOME/.pi/agent/sessions:$HOME/.pi/agent/sessions:ro" scratch -- \
  -c 'touch "$HOME/.pi/agent/sessions/should-fail"'
# touch: .../should-fail: Read-only file system
```

That is the whole claim: writes through that mount fail with `EROFS`. It is a guard
against incidental writes, not a security boundary and not confinement. `:ro` does
not stop the workload reading the tree, does not stop it opening the same host path
directly with its own host permissions, and does not confine it to the container
rootfs — a Darwin-native workload runs as a host process with host access, and Macker
does not claim more than the mount it created.

Read-only volumes need a Darwin node, a directory host path (a nullfs mount
cannot make a single file read-only), and root or passwordless
sudo, exactly like PF port publishing and network setup. Macker resolves the
helper by absolute path before it consults `$PATH` — `/sbin/mount_nullfs`, then
`/usr/sbin/mount_nullfs` and `/usr/local/sbin/mount_nullfs`, then `$PATH`, and
finally `mount -t null -o ro` through `/sbin/mount` and its siblings — because a
node agent that launches Macker from a service context can have a `$PATH` that
excludes `/sbin`, and that must not decide whether a node supports read-only
mounts. `MACKER_MOUNT_NULLFS` and `MACKER_UMOUNT` name the helpers explicitly if
a node keeps them elsewhere. A node with no usable helper reports every command
it tried and how each was rejected, and refuses the container rather than
reporting a read-only volume it could not enforce — home-ops measured no
`mount_nullfs` on icarus-2 (macOS 26.6.2, Darwin 25G83), where
`/Library/Filesystems/null.fs` is gone and `mount -t null` fails with exit 72
before the kernel is consulted.

`macker volumes` answers that capability question without mounting anything, so a
node agent can decide at admission time instead of launching a workload that is
bound to fail:

```sh
./bin/macker volumes
# read-only volumes: refused
# host: darwin/arm64
# mechanism: none
# nullfs bundle: /Library/Filesystems/null.fs (found: false)
# mount helpers resolved: none (searched /sbin/mount_nullfs, ...)
# reason: this macOS has no null filesystem bundle at /Library/Filesystems/null.fs, ...
```

The first line is a stable marker — `read-only volumes: supported` or
`read-only volumes: refused` — and `--format json` reports `read_only_supported`,
`nullfs_bundle_found`, the helpers resolved, the mount forms that would run, and
the reason. On macOS 26 mount-based enforcement needs a helper that calls `mount(2)`
with the `null` filesystem type, named with `MACKER_MOUNT_NULLFS`, and that helper
must also hold a private Apple entitlement: Apple's source for this Darwin generation
still contains nullfs as a filesystem, but its mount entry point returns `EPERM` unless
the calling task holds `com.apple.private.nullfs_allow`, with no fallback for root, while
its unmount entry point accepts root. So the kernel side appears intact and only the
userland route - the bundle, `mount_null`, `mount_nullfs` - was removed. Macker is
stdlib-only Go and Go defines no `Mount` wrapper for darwin, though it does define
`SYS_MOUNT = 167` and `SYS_UNMOUNT = 159` there, so the syscall is not the obstacle.
Macker will use such a helper when one exists and refuses `:ro` when it does not. `:rw` and any other
third field are rejected with an error; writable is the default and needs no
option. The mount is unmounted on exit, `stop`, `rm` and reconciliation after an
external kill, and `macker inspect --format json` reports both the requested
`volumes` and the live `mounted_volumes` with `read_only`, so a caller such as
maclet can confirm the enforcement rather than assume it. `make test-ro` runs
the create/modify/unlink acceptance test on a Mac node.

## Examples and tests

The hello Mackerfile builds a `CGO_ENABLED=0` native Darwin Go executable
(the host architecture is selected automatically). The
nginx Mackerfile uses host-side `RUN` commands to look up Homebrew, assemble a
rootfs, relocate application dylibs, and ad-hoc sign the copied Mach-O files:

```sh
./bin/macker build \
  -f example/nginx/Mackerfile \
  -t initialed85/nginx-darwin:latest \
  .
```

The nginx Macker image listens on port `8080`, serves a directory listing, and
uses `/usr/share/nginx/html` as its volume-backed document root. It is built
from `scratch` and does not include `bash`; a shell entrypoint therefore
requires an image that contains a shell (or a suitable host volume).

The Echo-Server example builds the upstream [Ealenn/Echo-Server](https://github.com/Ealenn/Echo-Server)
`0.9.2` release into a native Darwin image. Its pinned build downloads the
architecture-appropriate official Darwin Node runtime, runs the upstream
webpack build, and packages the resulting bundle plus `global.json`. Apple
Silicon uses Node `22.23.2`; Intel uses Node `20.19.5`, whose binary supports
macOS Catalina:

```sh
make echo-server
./bin/macker run --net=host --name=echo-server \
  -p 8080:80 \
  initialed85/echo-server:latest
curl http://127.0.0.1:8080/
./bin/macker stop echo-server
```

Authenticate Skopeo before publishing. `make echo-server-push` publishes the
Darwin image directly; `make echo-server-bundle` additionally merges the
upstream `ealen/echo-server:latest` Linux manifests and publishes the resulting
multi-platform image at `initialed85/echo-server:latest`:

```sh
skopeo login docker.io
make echo-server-bundle
```

The example build runs on macOS and needs network access to GitHub and
nodejs.org. The Intel artifact is intended for Catalina or newer; Homebrew
must provide Intel-compatible nginx dependencies for the nginx example. As
with all Macker workloads, this is a trusted native process rather than an
isolated macOS container.

## Woodpecker CI agent on Darwin

Woodpecker's `local` backend is the Darwin backend: it runs every pipeline step
as an ordinary macOS process, so a pipeline can use the host toolchain (the Xcode
`vmnet` framework, `pkgbuild`, `codesign`, `otool`, Homebrew rust and go). The
other backends build Linux containers, which is what the cluster agents are for.

```sh
make agent          # build initialed85/woodpecker-agent-darwin at {AGENT_TAG,latest}
make agent-push     # build both tags and publish them
make agent-version  # run the published image just far enough to print its version
make agent-pull     # pull the published image back into local storage
```

`example/woodpecker-agent/build.sh` assembles the rootfs from upstream pinned
releases and verifies them against the upstream `checksums.txt`:

- `woodpecker-ci/woodpecker` `v3.20.1`, asset `woodpecker-agent_darwin_{arm64,amd64}.tar.gz`
- `woodpecker-ci/plugin-git` `2.11.0`, asset `darwin-{arm64,amd64}_plugin-git`

`plugin-git` is bundled because the local backend resolves the clone plugin with
`exec.LookPath`; without it the agent downloads an unpinned `latest` binary for
every workflow that clones. Both binaries are re-signed ad-hoc after copying,
because `codesign --verify` is the only verification available for a rootfs that
nothing has checked. Woodpecker connection settings, routing labels, and the
agent token are deployment configuration and are supplied with `run --env` by
the launcher; see `hosts/woodpecker-agent/` in `home-ops`.

For the direct OCI hello experiment, use the `macker oci` subcommands:

```sh
make image
./bin/macker oci inspect --tag hello-darwin:latest ./example/image
./bin/macker oci unpack --output /tmp/hello-rootfs ./example/image
./bin/macker oci run --arg from --arg OCI ./example/image
```

These commands accept local OCI layout directories rather than Docker daemon
images. The default execution mode is non-chroot. The experimental
`--chroot` mode requires root and changes only the filesystem root; it is not a
security boundary and may not work for scratch images because macOS system
libraries and dyld are host-provided. `make nginx-run` builds the nginx image
with Macker and runs it in the foreground.

Run the Go checks with:

```sh
make test
```

The checks that read a registry use a skopeo that reproduces how the real one
handles its arguments. To repeat them against real registries with the real
skopeo installed, set `MACKER_TEST_NETWORK=1`; the tests that need it skip
themselves otherwise:

```sh
MACKER_TEST_NETWORK=1 go test ./cmd/... -run LiveRegistry
```

The full `test.sh` workflow builds Linux amd64 and arm64 images with Docker,
combines them with Darwin images, verifies the resulting multi-platform images,
checks `--entrypoint`, interactive `exec`, `logs`, `--rm`, and injected network
environment, then runs the native workloads. It pushes intermediate images and
publishes
`initialed85/nginx:latest` by default, so use suitable credentials and ensure
TCP port 8080 is available:

```sh
./test.sh
```

For a local-only Darwin smoke test:

```sh
SKIP_DISTRIBUTION=1 MACKER_HOME=/tmp/macker-test ./test.sh
```

`PORT` must match the Darwin nginx configuration, currently `8080`;
`NGINX_PORT` controls the Linux nginx image build. The script also accepts
image, container, and timeout overrides such as `SIMPLE_IMAGE`, `NGINX_IMAGE`,
and `READY_TIMEOUT`.

## CI and releases

`.github/workflows/release.yml` runs tests, vet, shell syntax checks, and a
Darwin/arm64 and Darwin/amd64 cross-builds of the `macker` binary. The
Darwin/amd64 release is built with Go 1.22.12 so it remains compatible with
macOS Catalina; both artifacts include a tarball and SHA-256 checksum. A
GitHub release is created for pushes to `master` and manual runs.
Pull requests run the checks and build but do not create releases.

## Deliberate limitations

- Native workloads are ordinary macOS processes with host-visible resources.
- There are no Linux namespaces, cgroups, capabilities, seccomp, overlayfs, or
  per-process network isolation. The initial bridge interfaces are host-wide
  plumbing only.
- CPU, memory, and process limits are not enforced.
- Writable volumes are symlinks rather than isolated mounts; `:ro` volumes are
  nullfs mounts, which are kernel-enforced but node-wide for the mount's
  lifetime rather than scoped to the launched process.
- PF publishing is IPv4-only and is global host configuration rather than
  container isolation.
- Only `sha256` OCI blobs and one uncompressed image layer are supported by the
  local builder.
- File ownership, xattrs, devices, FIFOs, and extended ACLs are not fully
  preserved.
- Native runtime support targets Darwin/arm64 and Darwin/amd64. The Darwin/amd64
  release requires a Catalina-compatible Go build and architecture-compatible
  native dependencies; `bundle` retains other platform manifests supplied by a
  source image.
- Non-Darwin manifests in a bundled or multi-platform image are stored for
  distribution and verification, but can never execute here. `platforms`
  reports them, and `pull` refuses a reference with no Darwin manifest matching
  the host architecture.

## Linux images and Darwin nodes

Macker is the execution half of the Mac node story in Macgrubernetes, and its
answer to Linux images is **won't-fix, with a diagnostic**:

- Macker starts workloads as ordinary Mach-O host processes. It has no Linux
  kernel, no ELF loader, no glibc, and no namespaces or cgroups to borrow, so a
  Linux rootfs cannot execute on a Mac node regardless of how its manifest is
  presented.
- Emulation or extraction is not planned. Linux translation layers exist for
  games on Linux and for Windows on macOS, but nothing trustworthy turns an
  arbitrary Linux container image into a macOS process, and inventing one would
  contradict Macker's stated trust model. A real Linux-container story on Apple
  hardware means a Virtualization.framework VM backend, which is a different
  product with different overhead and limits.
- Linux and Darwin manifests therefore coexist on purpose: `bundle` merges them
  under one tag, Kubernetes schedules Linux pods onto Linux nodes, and Mac nodes
  receive Darwin workloads. `macker platforms IMAGE` answers which platforms a
  reference offers before an image is pulled or a job is scheduled.

For a job that must run on a Mac node, the supported shapes are:

1. **A Darwin image** built with `macker build` or `macker oci build`. Go is the
   cheap path because a Linux build host can cross-compile the binary and macOS
   only has to run it; that is how `example/hello` and the Woodpecker agent
   image work, and the release CI already produces Darwin/arm64 and Darwin/amd64
   Macker binaries from Linux runners.
2. **`scratch` plus a volume-mounted host command** for scripts and one-off jobs,
   using the host's own interpreter and tools:

   ```sh
   ./bin/macker run --rm --net=external \
     --interface bridge101 --ip 10.42.1.5 \
     --name=session-uploader \
     --entrypoint /usr/bin/python3 \
     --env UPLOAD_GROUP=ed-icarus \
     -v "$HOME/.pi/agent/sessions:$HOME/.pi/agent/sessions" \
     -v "$PWD/pi_host_upload.py:$PWD/pi_host_upload.py" \
     scratch -- "$PWD/pi_host_upload.py"
   ```

   Because the execution model is non-chroot, writable mounts are host-path
   symlinks and the host-side path must already exist and be readable. Add `:ro`
   to a mount when the workload must not write back to the host; Macker then
   enforces it with a read-only nullfs mount instead of trusting the process.
   This shape is honest about what it is: a host process, not a container.
3. **Stay on a Linux node.** If the workload is genuinely "a Linux base image
   plus an interpreter", scheduling it on a Linux node is cheaper than
   reconstructing a Darwin rootfs by hand.

A Python uploader is the common case, and there is no Python-for-Darwin base
image shipped here. The two supported answers are the `scratch` shape above, or
porting the script to a single stdlib-only Go binary that cross-compiles on
Linux and runs as a Darwin workload; Homebrew Python on the Mac is a host
dependency rather than something Macker can package or verify.
