# Macker architecture

Macker is an experimental OCI workload tool for running native macOS
Mach-O programs on Apple Silicon and Intel Macs. It uses OCI images as a
distribution and packaging format, but it is **not** a Linux container runtime
and does not provide a security boundary.

Macker's product boundary is deliberately small:

- build a trusted Darwin workload into a local OCI layout;
- push, pull, and combine Darwin manifests with ordinary multi-platform images;
- report which platforms an image offers (`macker platforms IMAGE`) and refuse to
  store a reference with no Darwin manifest for the host architecture;
- materialize an independent per-container rootfs and run the native workload;
- provide enough host networking, port publishing, lifecycle state, logs, and
  `exec` support for a maclet-managed Kubernetes node.

## System overview

The CLI is a single Go binary. High-level commands coordinate images, rootfs
materialization, host networking, PF, and persistent state. The lower-level
`macker oci` commands operate directly on OCI layouts and are also used by the
high-level runtime.

```mermaid
flowchart LR
    User[User or maclet] --> CLI[macker CLI]
    CLI --> Build[macker build]
    CLI --> Registry[macker push / pull / bundle]
    CLI --> Run[macker run]
    CLI --> Lifecycle[exec / logs / inspect / ps / pause / unpause / stop / rm]

    Build --> Parser[Mackerfile parser]
    Parser --> HostRun[Host /bin/bash RUN]
    Parser --> Copy[COPY into build rootfs]
    HostRun --> BuildRootfs[Temporary build rootfs]
    Copy --> BuildRootfs
    BuildRootfs --> OCIBuilder[OCI layout builder]
    OCIBuilder --> Images[(MACKER_HOME/images)]
    OCIBuilder --> ImageState[(Locked image index)]

    Registry --> Skopeo[Skopeo]
    Skopeo --> Images
    Registry --> ImageState
    Registry --> Remote[(OCI registry)]

    Run --> Resolve[Resolve local Darwin image]
    Resolve --> Unpack[OCI unpack]
    Unpack --> Config[Runtime token substitution]
    Config --> Volumes[Install volumes: symlink, or read-only nullfs mount]
    Volumes --> Network[Network setup]
    Network --> PF[PF port redirects]
    PF --> Metadata[Container metadata and state]
    Metadata --> OCIRun[macker oci run]
    OCIRun --> Native[Native macOS workload process]
    OCIRun --> Exit[exit.json and run.log]
    Exit --> Metadata

    Lifecycle --> Metadata
    Lifecycle --> Native
    Lifecycle --> PF
    Lifecycle --> Network
```

The arrows represent coordination, not isolation. A workload remains an
ordinary host process with host-visible process, filesystem, and network
resources.

## Main components

### CLI and command layers

`cmd/macker/main.go` owns command dispatch, high-level build/registry/runtime
orchestration, persistent state, container metadata, and lifecycle operations.
`cmd/macker/oci.go` owns the low-level OCI layout reader/writer, image selection,
unpack, command resolution, and native process execution. `cmd/macker/network.go`
contains Darwin interface setup and cleanup. `cmd/macker/ports.go` allocates
automatic workload-side ports.

The high-level runtime invokes the same executable again for detached workloads:

```text
macker run -d ...
  -> macker oci run --skip-unpack --rootfs ...
       -> resolved native command
```

This keeps the process that owns the workload's wait status, stdout/stderr
capture, signal forwarding, and exit record separate from the CLI invocation
that created the container.

### Build path

A Mackerfile is intentionally Dockerfile-like but supports only a small subset:

- `FROM scratch`
- `RUN`
- `COPY`
- `ENV`
- `WORKDIR`
- JSON-array `ENTRYPOINT` and `CMD`
- metadata-only `EXPOSE`

Build steps are applied in order to one temporary rootfs. `RUN` executes
`/bin/bash -c` on the host, using the build context as its working directory.
It receives `MACKER_CONTEXT`, `MACKER_ROOTFS`, the target `MACKER_GOARCH`
(`arm64` or `amd64`), the host environment, and image build environment
values. It can therefore read or modify anything available to the invoking user
and must be treated as trusted build code.

`COPY` sources are constrained to the build context, while destinations must be
absolute paths in the image rootfs. The resulting image currently contains one
uncompressed tar layer plus its OCI config and manifest metadata; Macker does
not implement general Docker layer caching, multi-stage builds, or a non-scratch
base image.

### OCI and registry path

Local images are stored below `${MACKER_HOME:-$HOME/.macker}/images`, keyed by
normalized image reference. A locked state index records known images. Macker
uses Skopeo for registry authentication and transfer, so credentials come from
Skopeo's normal credential stores (with `REGISTRY_AUTH_FILE` available as an
override).

`pull` selects the native Darwin manifest matching the host architecture from a
multi-platform image. `bundle` recursively flattens source indexes, preserves
their non-Darwin platform descriptors, replaces any existing Darwin descriptor
matching the host architecture, and can push the merged index. Platform-
specific entrypoints, arguments, environments, and behavior are not reconciled
by bundling.

The local OCI loader verifies referenced blob sizes and SHA-256 digests before
using descriptor JSON. Low-level OCI commands accept layout directories rather
than Docker daemon image names.

## Runtime path

A high-level `run` performs these phases before launching the workload:

1. Parse and validate networking, environment, volume, entrypoint, working
   directory, and publish options. An absolute `--workdir` override is
   normalized and persisted; omission preserves the image default.
2. Resolve the image. `scratch` and `scratch:latest` are local sentinels: Macker
   creates/reuses a minimal empty Darwin OCI layout under its temporary state
   directory and never pulls or registers it as a user image.
3. Resolve `NODE_PORT=auto` or `NODE_PORT=0` mappings. Automatic ports are
   selected randomly from `30000-32767`, avoiding live Macker mappings and
   currently bindable host TCP/UDP ports.
4. Check published host-port conflicts. External target-IP mappings may reuse a
   host port when their target Pod IPs differ; host-mode mappings reserve the
   port globally.
5. Create or validate the requested network.
6. Unpack the selected Darwin image into a fresh container rootfs.
7. Merge image and run environment values, then substitute `____MACKER_*____`
   tokens in recognized regular UTF-8 configuration files.
8. Install volume mappings: writable volumes as host-path symlinks, `:ro` volumes as read-only nullfs mounts.
9. Install PF redirects for published ports.
10. Persist metadata and launch the OCI process.

The stored image layout is never used as a writable container rootfs. Each run
gets its own materialized rootfs, so runtime mutations do not modify the image.
The current materialization is a copy/unpack operation; it is not an overlay
filesystem and does not provide copy-on-write isolation.

### Native execution

`macker oci run` loads the image config, selects the Darwin workload matching
the host architecture (`darwin/arm64` or `darwin/amd64`), resolves its
entrypoint and arguments, applies an optional absolute `--workdir` override
(or the image working directory), sets the configured environment, and launches
the native Mach-O command. Non-chroot mode
runs from the extracted rootfs directory while retaining the host's process,
filesystem, and network view. An explicit runtime entrypoint may use a host
executable fallback for debugging when the command is absent from the image.

The experimental `--chroot` option changes the child filesystem root only. A
usable Darwin chroot needs a compatible dyld/system-library environment, and
macOS chroot is not treated as process or filesystem isolation. The normal
Macker path is therefore non-chroot native execution. Scratch runs have no
image entrypoint, command, or files; the high-level runtime allows the trusted
host-command fallback so `/bin/sh`, an explicit entrypoint, or a
volume-mounted executable can be used against the empty rootfs.

### Pause and unpause

Detached workloads can be suspended without tearing down their runtime
resources. `macker pause NAME` sends `SIGSTOP` to the workload's process group
and any tracked exec process groups, persists `PausedAt`, and leaves the
materialized rootfs, volume links, PF mappings, and network configuration in
place. `macker unpause NAME` sends `SIGCONT` and clears `PausedAt`. `ps` and
`inspect` report `paused`, while reconciliation deliberately does not clean up
paused resources. This is best-effort process-group control: a workload that
creates a new session can escape the tracked group, and no namespace or
resource-limit isolation is implied. Before signaling, Macker validates the
recorded launcher/workload PID start times to reduce PID-reuse risk. Missing or
mismatched identity fails without changing the persisted paused state; signal
or metadata failures attempt to roll back the process state. `stop` remains
separate and resumes a paused workload before terminating it and cleaning its
PF/network resources.

The pause path does not replay or reconstruct the original invocation. A
future restart operation will need persisted command/entrypoint configuration
and separate desired-network metadata.

### Read-only volumes and mount lifecycle

`-v HOST:CONTAINER:ro` is enforced by the kernel rather than by a promise to the
workload, when the node has a nullfs mount path at all. Macker creates a real
mount point under the container rootfs and mounts the host path read-only with
`mount_nullfs -o ro`, falling back to `mount -t null -o ro`, so `open(O_WRONLY)`,
`creat` and `unlink` inside that path fail with `EROFS` for the launched workload
and for every other process on the node while the mount exists. That is the
closest Darwin equivalent of Kubernetes `hostPath` with `readOnly: true`:
node-wide for the mount's lifetime, and removed when the container is torn down.

The constraints follow from the mechanism, and Macker reports them instead of
working around them:

- Darwin nodes only; a Linux node rejects `:ro` rather than pretending to enforce
  it.
- A nullfs mount path that this macOS release actually provides. home-ops measured
  icarus-2 (macOS 26.6.2, Darwin 25G83): `/Library/Filesystems/null.fs` is gone, so
  `mount(8)` cannot reach the null filesystem type - it fails while looking for the
  type's helper, with exit 72, before the kernel is consulted. The same is true of the
  type name this kernel actually registers: `mount -t nullfs` fails the same way
  looking for `nullfs.fs`. Macker tries both names (`nullfs` first, then `null`) and
  refuses `:ro` when neither resolves. The refusal is the guarantee: Macker never
  reports a read-only volume it could not enforce.
- What closes the route is a policy gate in the kernel, not the absence of the
  filesystem, and the type name matters. Measured on icarus-2 with `mount(2)` run as
  root through syscall 167, one mount point, one source directory, three repeat runs
  of the discriminating names:

  | type asked for | answer |
  | --- | --- |
  | `devfs` (control) | mounted, then released |
  | `apfs`, `hfs`, `autofs`, `lifs` | `EINVAL` - registered, bad spec |
  | `nfs` | errno 75 - registered, userspace data version rejected |
  | `bogusxyz` | errno 19 |
  | `null` | errno 19, byte-for-byte the answer a made-up name draws |
  | `nullfs` | `EPERM` - 1, three runs out of three |

  `nullfs` is the only name in that table that draws a refusal other than "no such
  filesystem type", which is what Apple's source for this Darwin generation predicts:
  nullfs is still there (`bsd/miscfs/nullfs/`) and its mount entry point opens with
  `if (!IOCurrentTaskHasEntitlement("com.apple.private.nullfs_allow")) return EPERM;`
  with no fallback for root, while its unmount entry point accepts the entitlement
  *or* superuser. So the userland route - the bundle, `mount_null`, `mount_nullfs` -
  was removed and the kernel route was gated. Reopening it needs a helper that calls
  `mount(2)` with the type `nullfs` **and** holds that entitlement, which a third-party
  binary cannot obtain. Nothing on the node serves as one: `/Library/Filesystems`
  contains only `NetFSPlugins`, the sealed `/System/Library/Filesystems` ships 17
  bundles with no `null.fs` or `nullfs.fs` among them, and no `mount_null*` exists in
  `/sbin`, `/usr/sbin` or `/usr/bin`.
- Two measurement traps that produced the wrong conclusion here once, recorded
  because both are easy to fall into again. First, **errno 19 on Darwin is `ENOTSUP`,
  "operation not supported by device", not `ENODEV`, "no such device"** (Linux numbers
  them the other way round); a probe that prints 19 found an unregistered *name*, and
  that is exactly how "the kernel registers no nullfs at all" got reported here. It is
  false: `nullfs` is registered and answers `EPERM`. Second, **`sysctl vfs.generic` is
  not the filesystem registry**: on this node it lists `apfs autofs hfs lifs nfs` and
  omits `devfs`, which mounts and unmounts fine, so `sysctl vfs.generic.nullfs`
  reporting an unknown oid proves nothing. The mount(2) errno classes above are the
  discriminator. Third, the ABI you use decides whether you reach the vfs layer at
  all: the four-argument form `syscall(SYS_MOUNT, type, mount_point, flags, data)`
  works on macOS 26 arm64 (the `devfs` control above mounts through it), while the
  struct-pointer form `syscall(SYS_MOUNT, &struct mount_args, 0, 0)` used by the helper
  left at `/tmp/mnull3.c` on that node draws `EFAULT` for every type including `devfs`.
  Pass the arguments directly: fstype first, mount point second, and the source
  directory in `data` as a `struct null_mount_conf { uint64_t flags }` followed by the
  NUL-terminated path. Macker is stdlib-only Go, and Go defines no `Mount` wrapper for
  darwin but does define `SYS_MOUNT = 167` and `SYS_UNMOUNT = 159` in its darwin
  tables, so the raw call is reachable from Go; the entitlement, not the language, is
  what blocks it. `MACKER_MOUNT_NULLFS` names a helper for a node that does have one.
  macOS nullfs also forces `MNT_RDONLY` (with `MNT_NOSUID`, `MNT_DONTBROWSE` and
  `MNT_MULTILABEL`) on every mount, so it could only ever serve `:ro` and never a
  writable volume.
- Directory host paths only, because a nullfs mount cannot make a single file
  read-only. A file mount must drop `:ro`.
- Root or passwordless sudo is required, as with PF and network operations. A node
  agent that drops privileges before exec, as maclet does, needs a narrow
  passwordless rule for the mount and umount commands, or `:ro` is refused.
- `:ro` is opt-in. A plain `HOST:CONTAINER` stays a live writable symlink, and
  third fields other than `ro` (including `:rw`) are rejected.
- `:ro` is a guard against incidental writes, not a security boundary and not
  confinement. It says: writes through that mount fail with `EROFS`. It does not
  mean the workload cannot read the tree, cannot open the same host path directly
  under its own host permissions, or is confined to the container rootfs. A
  Darwin-native workload runs as a host process with host access, and Macker
  makes no claim beyond the mount it actually created.

The requested shape is persisted in `volumes` and the live mounts in
`mounted_volumes`. Unmounting happens on workload exit, `macker stop`,
`macker rm`, failed run setup, and reconciliation by `macker ps`/`macker inspect`
when a detached workload was killed externally; a busy unmount is reported rather
than swallowed. `macker inspect --format json` publishes both lists with
`read_only`, so a caller such as maclet can confirm the enforced state instead of
assuming it.

## Networking and port publishing

Macker has two network modes:

- **Host mode** creates or reuses `bridge88` at `172.31.88.1/24` and allocates
  an owned per-container interface such as `bridge881` at `172.31.89.1/24`.
  These are host-side plumbing primitives with no network namespaces or
  bridge members. The native process still shares the host network stack.
- **External mode** validates an already-existing interface and canonical IPv4
  Pod IP, such as a VXLAN bridge managed by maclet. Macker does not create,
  destroy, or claim that interface. Optional host interface/IP identity can be
  supplied for environment and PF context.

Both modes inject:

```text
MACKER_INTERFACE
MACKER_IP
MACKER_HOST_INTERFACE
MACKER_HOST_IP
MACKER_PORT_1
MACKER_PORT_2
...
```

The numbered port values are workload-side listening ports. Workloads are
expected, but cannot be forced, to bind to their supplied IP and ports.

```mermaid
flowchart TB
    Client[Ingress client] --> Destination{Destination address}
    Destination -->|host mode| Any[Host address / any PF destination]
    Destination -->|external mode| Pod[Pod IP]

    Any --> HostRule[PF rdr: to any HOST_PORT]
    Pod --> TargetRule[PF rdr: to POD_IP HOST_PORT]
    HostRule --> WorkloadA[Workload IP:NODE_PORT]
    TargetRule --> WorkloadB[Pod IP:NODE_PORT]

    WorkloadA --> ProcessA[Native process]
    WorkloadB --> ProcessB[Native process]
```

Publishing uses Docker-style `-p HOST_PORT:NODE_PORT[/tcp|/udp]`. The protocol
defaults to TCP. `auto` and `0` are accepted for `NODE_PORT`; the resolved
value is persisted and can be read from `macker inspect --format json`.

Host mode retains the compatibility rule:

```text
rdr pass inet proto tcp from any to any port = 8080 -> 172.31.89.1 port 30080
```

External mode uses a target-specific destination:

```text
rdr pass inet proto tcp from any to 10.42.8.3 port = 8080 -> 10.42.8.3 port 31543
```

Macker loads rules into one hashed child anchor per container beneath
macOS's existing `com.apple/*` dynamic anchor. New anchors use the form:

```text
com.apple/macker-<32 hex SHA-256 characters>
```

The generated anchor is stored in container metadata so cleanup remains
possible even when the container name is long. PF is enabled with a retained
enable token and cleaned on stop, removal, foreground completion, setup
failure, or `ps` reconciliation. Macker does not replace the main PF ruleset.

Reading that state back has one trap worth knowing on macOS 26: a plain
`pfctl -s Anchors` lists only the anchors the system ruleset names, not the
`com.apple/*` children Macker loads, while `pfctl -s Anchors -vv` does list
them. So "no `com.apple/macker-*` anchor" is what a correct node looks like to
the wrong query. `macker ports` avoids the trap: it asks for each container's
own anchor by name, and reports the rule with its kernel counters
(`Evaluations`, `States`). Flushing an anchor removes its rules but leaves the
empty anchor object behind until the system ruleset is reloaded, which Macker
never does, so an anchor with no rules is an already-removed redirect rather
than a live one.

PF publishing is IPv4-only, ingress-oriented, and global host configuration;
a same-Mac request to a published address may not traverse the same path as a
remote ingress request. It does not: a workload IP is aliased locally on the
node, and a connection the node originates to its own address is not evaluated
against the anchor at all, counters included. Node-local reachability is a
question about `127.0.0.1:<NODE_PORT>`; published-port reachability is measured
from another host.

## Persistent state and lifecycle

The default state root is `${MACKER_HOME:-$HOME/.macker}`:

```text
.macker/
├── images/
│   └── <normalized-reference-hash>/
├── containers/
│   └── <name>/
│       ├── rootfs/
│       ├── metadata.json
│       ├── exit.json
│       ├── workload.pid         # while the native workload is running
│       ├── run.log              # detached workloads
│       └── exec/*.pid           # tracked detached exec children
├── state.json
└── state.lock
```

`state.json` is versioned and contains image and container indexes. Macker uses
an advisory lock and atomic temporary-file replacement when updating it.
Container metadata is written atomically as well. The metadata records image,
network, optional working-directory override, volumes, published mappings, PF
anchor/token, environment, lifecycle timestamps, PID, and auto-remove policy.

Detached lifecycle behavior:

```mermaid
stateDiagram-v2
    [*] --> Starting: run -d
    Starting --> Running: metadata + launcher started
    Running --> Paused: pause / SIGSTOP process groups
    Paused --> Running: unpause / SIGCONT process groups
    Running --> Exited: launcher/workload exits
    Running --> Stopped: stop / SIGTERM
    Paused --> Stopped: stop resumes, then SIGTERM
    Exited --> Stopped: stop or cleanup observation
    Running --> Removed: rm --force / --rm
    Exited --> Removed: rm / ps with --rm
    Stopped --> Removed: rm / --rm
    Removed --> [*]
```

The detached launcher writes an atomic `exit.json` record containing the
workload PID, exit code, start/finish timestamps, signal, and termination
reason. While running, the OCI child also writes `workload.pid`, allowing
pause/unpause to target its dedicated process group; the file is removed when
the workload exits. `inspect` and `ps` reconcile the exit record after
confirming the launcher has exited. `inspect --format json NAME` reports the
main workload as one machine-readable object, including resolved port mappings
and `launcher_started_at`, `workload_started_at`, and `paused_at` when suspended:

```json
{
  "name": "nginx",
  "image": "docker.io/initialed85/nginx:latest",
  "status": "exited",
  "pid": 12345,
  "launcher_started_at": null,
  "workload_pid": 12347,
  "exit_code": 143,
  "started_at": "2026-08-30T10:23:27.266635Z",
  "finished_at": "2026-08-30T10:23:27.272232Z",
  "termination_signal": "SIGTERM",
  "termination_reason": "signal",
  "paused_at": null,
  "workload_started_at": null,
  "ports": [
    {"host_port": 8080, "node_port": 31543, "protocol": "tcp"}
  ]
}
```

A naturally exited detached workload reports `exited`; an explicit `stop`
reports `stopped` and sets `pid` to zero while retaining historical workload
and exit data. A paused workload retains its launcher/workload PIDs and all
PF/network metadata; pause is not a cleanup or restart operation. Foreground
runs are persisted as stopped after completion, and `--rm` removes the record. Existing metadata created before exit records were
introduced may not have lifecycle timestamps or exit details.

`exec` starts additional host processes from the existing rootfs and
reconstructs image/run environment, working directory, and network values. It
does not inherit mutable environment or directory changes made by the
entrypoint. Detached exec children are tracked separately and terminated by
container cleanup; their individual exit status is not part of the main
container inspection object.

## maclet integration contract

maclet owns Kubernetes and VXLAN concerns. Macker is used as the trusted native
workload runtime:

```sh
macker run --detach \
  --net=external \
  --interface bridge101 \
  --ip 10.42.8.3 \
  --host-interface bridge101 \
  --host-ip 10.42.8.1 \
  --env EXAMPLE=value \
  -p 8080:auto \
  --name pod-container-uid \
  initialed85/nginx-darwin:latest
```

For this mode:

- maclet provisions and owns the attached interface and Pod IP;
- Macker validates the interface/address and leaves its lifecycle untouched;
- `-p 8080:auto` makes the workload listen on a unique native port while PF
  accepts traffic at Pod IP port 8080;
- the resolved port is available as `MACKER_PORT_1` and in `inspect` JSON;
- `____MACKER_PORT_1____` can be used in recognized config files such as nginx
  configuration;
- maclet chooses the shape by Pod class rather than guessing: a Pod IP workload
  gets `-p <containerPort>:auto/tcp|udp` (overlapping rollout generations would
  otherwise collide on a host port), while a `hostNetwork` Pod or a Pod labelled
  `k8s-darwin.dev/disable-port-forward` gets `--env MACKER_PORT_N=<containerPort>`
  and no published port, because there the pod's port already is the host port.
  maclet owns these Kubernetes-side rules and documents them, with measured
  tables, in its `KUBERNETES-PARITY.md` under "Ports and Macker config tokens";
  Macker only defines what a token resolves from and refuses a config that
  resolves to nothing,
- maclet can use `inspect --format json` to reconcile process and exit state;
- maclet can use `macker platforms IMAGE --format json` before assigning an image,
  so a Linux-only pod is rejected as unsupported instead of failing inside a
  pull attempt. Macker never executes a non-Darwin manifest, and there is no plan
  to change that; a Linux-container runtime on a Mac node would require a
  VM-backed runtime rather than this process model.
- maclet can use `macker volumes --probe --format json` before assigning a
  `readOnly` hostPath pod, so a node that cannot enforce a read-only mount is
  reported as unsupported at admission time instead of failing inside a mount
  attempt. `--probe` is the form a gate should call: the verdict comes from a
  temporary mount the kernel accepted, a write inside it the kernel refused, and
  the mount released again. Without `--probe` the same command answers the
  capability question by inspecting helpers and the null filesystem bundle, which
  can say "supported" on a node whose kernel would still refuse the mount. Switch
  on `probe_status` rather than reading the reason as prose: a probe that could not
  run because nobody was allowed to mount (`mount_not_attempted_privilege`) is not
  the same answer as a kernel that refused the filesystem type (`mount_refused`).
  `reason` is kept short because maclet embeds it in a Pod status - `kubectl describe
  pod` shows macker's wording - and a remedy that cannot work on that OS must not
  appear there. The node operator's checklist is `guidance`, which belongs in node
  logs instead. `unmount_failed` arrives only after three release attempts, so maclet
  can fail closed on it knowing the mount was tried for, not merely asked once; a
  probe that could not release its mount reports the live path and the release command
  whatever verdict it ended on, including `write_accepted`.

## Isolation and trust model

Macker intentionally does not claim container isolation. Native workloads and
host-side `RUN` commands can observe or affect host resources available to the
invoking user. In particular, Macker does not provide:

- Linux namespaces, cgroups, capabilities, seccomp, or overlayfs;
- per-process network namespaces or enforced Pod IP binding;
- CPU, memory, or process-count limits;
- isolated mounts for writable volumes (they are live symlinks); `:ro` volumes are
  node-wide read-only nullfs mounts for the mount's lifetime, not per-process
  sandboxing;
- a security boundary through chroot;
- automatic host secret filtering for build commands or workloads.

Use Macker for trusted Darwin workloads and artifact distribution. A future
strong-isolation design would require a VM-backed runtime or another platform
primitive rather than the current native process model.

## Source layout and validation

```text
cmd/macker/main.go          CLI, metadata, state, lifecycle, PF orchestration
cmd/macker/oci.go           OCI layouts, unpack, command execution, exit records
cmd/macker/platforms.go     platform reporting and pull pre-flight checks
cmd/macker/network.go       Darwin network setup and cleanup
cmd/macker/ports.go         automatic NodePort allocation
example/hello/              minimal native Darwin workload
example/nginx/              native Homebrew nginx image
example/echo-server/        native Node/Echo-Server image
.github/workflows/          Darwin/arm64 and Darwin/amd64 CI/release packaging
```

The normal checks are:

```sh
make test
go vet ./...
sh -n test.sh example/nginx/build-rootfs.sh example/echo-server/build.sh
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/macker
make macker
```

The full `test.sh` workflow additionally exercises local Darwin workloads,
registry distribution, multi-platform bundling, configuration substitution,
interactive execution, logs, and lifecycle cleanup.
