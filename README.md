# Macker

Macker is an experimental OCI workload tool for native macOS executables on
Apple Silicon. It packages Mach-O programs as `darwin/arm64` OCI images and
runs them as ordinary host processes.

Macker is **not** a Linux container runtime. It provides no process,
filesystem, network, or resource isolation. Treat images and `RUN` commands as
trusted host workloads.

## Why?

I run K3s at home and use my Mac for local AI work. Macker is an experiment
toward blending those worlds: building and distributing OCI images containing
Darwin binaries, then scheduling AI workloads on the Mac as transparently as
possible alongside Linux nodes in the K3s cluster.

## Requirements

- Apple Silicon macOS for building and running Darwin workloads
- Go 1.22+
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
directory. It receives `MACKER_CONTEXT`, `MACKER_ROOTFS`, the host environment,
and Mackerfile `ENV` values, and can read or modify the host as the invoking
user. `RUN` and `COPY` steps mutate one temporary rootfs in order; they do not
create separate cached layers. The final image contains one uncompressed tar
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
an invalid configuration behind. Macker skips symlinks, binary-looking files,
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

`pull` selects the native `darwin/arm64` manifest from a multi-platform image
and installs it locally. Image references must be tagged; Docker Hub is
assumed when no registry is specified.

`bundle` combines a source image with a locally stored Darwin image. A source
is read from local storage when present, otherwise pulled from its registry.
Source platforms are preserved, any existing `darwin/arm64` entry is replaced,
and the result is pushed to the Darwin image reference by default:

```sh
./bin/macker bundle nginx:latest initialed85/nginx-darwin:latest
```

Use `--no-push` to keep the merged image local. Bundling only combines image
manifests; it does not reconcile entrypoints, arguments, environment, or
application behaviour across platforms.

## Running workloads

`run` requires explicit `--net=host` or `--net=external` networking and a
name. Repeat `--env KEY=VALUE` to add or override image environment values;
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
`172.31.89.1`). In external mode, `MACKER_INTERFACE` and `MACKER_IP` are the
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
is TCP. Publishing uses the workload bridge IP as the PF destination
and rules without an interface restriction, so it currently covers ingress on
any interface. Macker uses macOS's existing dynamic PF anchor and does not
replace the main ruleset. Repeat `-p` for additional mappings, such as
`-p 53:30553/udp`. This initial implementation is IPv4-only and does not add
special VXLAN or bridge-member handling. PF redirects ingress traffic; a
connection from the Mac to its own published address may require testing from
another host.

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

Use `inspect` to retrieve lifecycle and exit information. The machine-readable
form is stable JSON and refreshes detached exit state from the persisted status
record before printing:

```sh
./bin/macker inspect --format json NAME
# `--json NAME` is an alias
```

The JSON object includes `status` (`running`, `exited`, or `stopped`), the
tracked launcher `pid`, the completed workload `workload_pid` when available,
`exit_code` (including the conventional `128 + signal` value for signal
termination), `started_at`, `finished_at`, `termination_signal`, and
`termination_reason` (`exited`, `exited-with-error`, `signal`, or `stopped`).
Detached `run` records this information when its OCI child exits; `inspect` and
`ps` reconcile it into `metadata.json`.

Use `macker rm --force NAME` to stop and remove a running container. `macker
images` lists local images and `macker rmi IMAGE` removes an image layout;
removing an image does not remove existing container rootfs directories.
Volumes are live symlinks to host paths, not kernel mounts. Host networking is
shared, so wildcard listeners can collide on ports. `stop`, `rm`, foreground
exit, and `ps` cleanup remove PF mappings; a detached workload that is killed
externally is cleaned up the next time it is observed by Macker.

## Examples and tests

The hello Mackerfile builds a `CGO_ENABLED=0` Darwin/arm64 Go executable. The
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
official Node `22.23.2` arm64 Darwin runtime, runs the upstream webpack build,
and packages the resulting bundle plus `global.json`:

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

The example build runs on Apple Silicon macOS and needs network access to
GitHub and nodejs.org. As with all Macker workloads, this is a trusted native
process rather than an isolated macOS container.

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
Darwin/arm64 cross-build of the `macker` binary. It uploads a tarball and
SHA-256 checksum, and creates a GitHub release for pushes to `master` and
manual runs.
Pull requests run the checks and build but do not create releases.

## Deliberate limitations

- Native workloads are ordinary macOS processes with host-visible resources.
- There are no Linux namespaces, cgroups, capabilities, seccomp, overlayfs, or
  per-process network isolation. The initial bridge interfaces are host-wide
  plumbing only.
- CPU, memory, and process limits are not enforced.
- Volumes are symlinks rather than isolated mounts.
- PF publishing is IPv4-only and is global host configuration rather than
  container isolation.
- Only `sha256` OCI blobs and one uncompressed image layer are supported by the
  local builder.
- File ownership, xattrs, devices, FIFOs, and extended ACLs are not fully
  preserved.
- The tested Macker builder and runtime target Darwin/arm64; `bundle` can retain
  other platform manifests supplied by a source image.
