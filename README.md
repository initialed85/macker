# Macker

Macker is an experimental OCI workload tool for native macOS executables on
Apple Silicon. It packages Mach-O programs as `darwin/arm64` OCI images and
runs them as ordinary host processes.

Macker is **not** a Linux container runtime. It provides no process,
filesystem, network, or resource isolation. Treat images and `RUN` commands as
trusted host workloads.

## But... Why?

I run K3s at home, and I also do an amount of local AI stuff on my Mac- I'm ultimately trying
to work towards blending those two worlds in some kind of unholy union, allowing me to build
and distribute OCI images with code compiled for Darwin inside then, then join my Mac to my 
K3s cluster, scheduling AI workloads on it as transparently as if it was one of my Linux nodes.

## Requirements

- Apple Silicon macOS for building and running Darwin workloads
- Go 1.22+
- [Skopeo](https://github.com/containers/skopeo) for registry operations
- Homebrew and its `nginx`, `pcre2`, `openssl@3`, and `ca-certificates` packages
  for the nginx example
- Docker and Python 3 only for the full distribution test

The Go tools use the standard library. Docker is not required for local OCI
builds or native runs. `macker` finds `darwin-oci` beside itself, in `PATH`, or
at `MACKER_DARWIN_OCI`.

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
JSON-array `ENTRYPOINT`/`CMD`, and `EXPOSE`. `EXPOSE` is accepted but does not
publish or map a port. The selected entrypoint or command must be an absolute
path.

`RUN` executes `/bin/bash -c` on the host with the build context as its working
directory. It receives `MACKER_CONTEXT`, `MACKER_ROOTFS`, the host environment,
and Mackerfile `ENV` values, and can read or modify the host as the invoking
user. `RUN` and `COPY` steps mutate one temporary rootfs in order; they do not
create separate cached layers. The final image contains one uncompressed tar
layer.

`COPY` sources are relative to the build context and cannot escape it;
destinations must be absolute image paths. Non-`scratch` bases, shell-form
commands, build arguments, and other Docker build features are not implemented.

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

`run` requires explicit host networking and a name. Foreground runs inherit the
terminal; detached runs write to `~/.macker/containers/<name>/run.log` (or the
corresponding `MACKER_HOME` path):

```sh
mkdir -p /tmp/macker-www
printf 'hello from a Macker volume\n' > /tmp/macker-www/index.html
./bin/macker run \
  --detach \
  --net=host \
  --name=nginx \
  -v /tmp/macker-www:/usr/share/nginx/html \
  initialed85/nginx-darwin:latest

./bin/macker ps
./bin/macker stop nginx
./bin/macker rm nginx
```

Use `macker rm --force NAME` to stop and remove a running container. `macker
images` lists local images and `macker rmi IMAGE` removes an image layout;
removing an image does not remove existing container rootfs directories.
Volumes are live symlinks to host paths, not kernel mounts. Host networking is
shared, so wildcard listeners can collide on ports.

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
uses `/usr/share/nginx/html` as its volume-backed document root.

For the direct OCI hello experiment, which uses the legacy local layout tool:

```sh
make image
./bin/darwin-oci inspect --tag hello-darwin:latest ./example/image
./bin/darwin-oci unpack --output /tmp/hello-rootfs ./example/image
./bin/darwin-oci run --arg from --arg OCI ./example/image
```

`darwin-oci` accepts local OCI layout directories rather than Docker daemon
images. Its default execution mode is non-chroot. The experimental
`--chroot` mode requires root and changes only the filesystem root; it is not a
security boundary and may not work for scratch images because macOS system
libraries and dyld are host-provided. `make nginx-run` builds the nginx image
with the legacy shell builder and runs it in the foreground.

Run the Go checks with:

```sh
make test
```

The full `test.sh` workflow builds Linux amd64 and arm64 images with Docker,
combines them with Darwin images, verifies the resulting multi-platform images,
and runs the native workloads. It pushes intermediate images and publishes
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
Darwin/arm64 cross-build for both binaries. It uploads a tarball and SHA-256
checksum, and creates a GitHub release for pushes to `master` and manual runs.
Pull requests run the checks and build but do not create releases.

## Deliberate limitations

- Native workloads are ordinary macOS processes with host-visible resources.
- There are no Linux namespaces, cgroups, capabilities, seccomp, overlayfs, or
  per-workload network interfaces.
- CPU, memory, and process limits are not enforced.
- Volumes are symlinks rather than isolated mounts.
- Only `sha256` OCI blobs and one uncompressed image layer are supported by the
  local builder.
- File ownership, xattrs, devices, FIFOs, and extended ACLs are not fully
  preserved.
- The tested Macker builder and runtime target Darwin/arm64; `bundle` can retain
  other platform manifests supplied by a source image.
