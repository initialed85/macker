// This file implements host volume mounts, including the read-only shape
// Macker enforces with a Darwin nullfs mount rather than a permission promise.
// It deliberately uses only the Go standard library.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

type volume struct {
	HostPath      string `json:"host_path,omitempty"`
	ContainerPath string `json:"container_path,omitempty"`
	ReadOnly      bool   `json:"read_only,omitempty"`

	// LegacyHostPath and LegacyContainerPath exist only so container metadata
	// written before volume JSON tags decoded on read. They are never written.
	LegacyHostPath      string `json:"HostPath,omitempty"`
	LegacyContainerPath string `json:"ContainerPath,omitempty"`
}

// hostPath returns the usable host path whether it was decoded from the tagged
// or the pre-tag JSON shape.
func (v volume) hostPath() string {
	if v.HostPath != "" {
		return v.HostPath
	}
	return v.LegacyHostPath
}

func (v volume) containerPath() string {
	if v.ContainerPath != "" {
		return v.ContainerPath
	}
	return v.LegacyContainerPath
}

func containerVolumeInspectionKey(mount volume) string {
	return mount.hostPath() + "\x00" + mount.containerPath()
}

// volumeInspection is the machine-readable volume record published by
// `macker inspect --format json`, so a caller such as maclet can confirm the
// read-only state it asked for instead of assuming it.
type volumeInspection struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
	ReadOnly      bool   `json:"read_only"`
}

func volumeInspections(input []volume) []volumeInspection {
	if len(input) == 0 {
		return []volumeInspection{}
	}
	out := make([]volumeInspection, 0, len(input))
	for _, mount := range input {
		out = append(out, volumeInspection{
			HostPath:      mount.hostPath(),
			ContainerPath: mount.containerPath(),
			ReadOnly:      mount.ReadOnly,
		})
	}
	return out
}

func parseVolume(raw string) (volume, error) {
	parts := strings.Split(raw, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return volume{}, fmt.Errorf("invalid volume %q; expected HOST:CONTAINER or HOST:CONTAINER:ro", raw)
	}
	readOnly := false
	if len(parts) == 3 {
		if parts[2] != "ro" {
			return volume{}, fmt.Errorf("invalid volume option %q in %q; only :ro is supported, and writable volumes omit the third field", parts[2], raw)
		}
		readOnly = true
	}
	host, err := filepath.Abs(parts[0])
	if err != nil {
		return volume{}, fmt.Errorf("resolve volume host path: %w", err)
	}
	if !filepath.IsAbs(parts[1]) {
		return volume{}, fmt.Errorf("volume container path %q must be absolute", parts[1])
	}
	if _, err := os.Lstat(host); err != nil {
		return volume{}, fmt.Errorf("volume host path %q: %w", host, err)
	}
	container := path.Clean(parts[1])
	if container == "/" || container == "." || strings.HasPrefix(container, "/../") || container == ".." {
		return volume{}, fmt.Errorf("invalid volume container path %q", parts[1])
	}
	return volume{HostPath: host, ContainerPath: container, ReadOnly: readOnly}, nil
}

func installVolume(rootfs string, mount volume) error {
	relative := strings.TrimPrefix(filepath.FromSlash(mount.containerPath()), string(filepath.Separator))
	target := filepath.Join(rootfs, relative)
	rel, err := filepath.Rel(rootfs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("volume target escapes container rootfs: %q", mount.containerPath())
	}
	if err := ensureNoSymlinkParents(rootfs, target); err != nil {
		return fmt.Errorf("volume target %q: %w", mount.containerPath(), err)
	}
	if err := removeExisting(target); err != nil {
		return fmt.Errorf("replace volume target %q: %w", mount.containerPath(), err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create volume parent %q: %w", mount.containerPath(), err)
	}
	if mount.ReadOnly {
		return mountReadOnlyVolume(mount.hostPath(), target, mount)
	}
	if err := os.Symlink(mount.hostPath(), target); err != nil {
		return fmt.Errorf("link volume %q: %w", mount.containerPath(), err)
	}
	return nil
}

// mountReadOnlyVolume enforces read-only by handing the mapping to the kernel:
// on Darwin the target becomes a nullfs mount mounted with `ro`, so create,
// modify and unlink inside it fail with EROFS for the launched workload and for
// every other process on the node while the mount is live. Macker does not
// pretend a symlink or a permission change can do this.
func mountReadOnlyVolume(hostPath, target string, mount volume) error {
	if blockage := readOnlyMountBlockage(runtime.GOOS); blockage != "" {
		// The reason is stated before any mount is attempted. On Darwin the
		// decisive fact is measurable without privilege: mount(8) finds a
		// filesystem type's helper inside /Library/Filesystems/<type>.fs, so a
		// missing null filesystem bundle is an OS fact, not a lookup failure.
		return fmt.Errorf("read-only volume %q: %s", mount.containerPath(), blockage)
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		return fmt.Errorf("stat read-only volume host path %q: %w", hostPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("read-only volume host path %q is not a directory; mount_nullfs cannot mount a single file read-only, so drop :ro for a file mount", hostPath)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create read-only volume mount point %q: %w", mount.containerPath(), err)
	}
	attempts, err := nullfsMountAttempts(hostPath, target)
	if err != nil {
		return err
	}
	// mount(8) wrappers differ in where they stop scanning options, so each
	// helper is tried in the documented option placement and then the trailing
	// placement. Every failure is reported, because the reason a node could not
	// mount - missing helper, no passwordless sudo, a kernel without nullfs - is
	// the thing the caller has to act on next.
	failures := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if _, err := runPrivileged(attempt.Binary, attempt.Args...); err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: %v", attempt.Binary, strings.Join(attempt.Args, " "), err))
			continue
		}
		return nil
	}
	return fmt.Errorf("mount %q read-only at %q: %s; read-only volumes require root or passwordless sudo", hostPath, target, strings.Join(failures, "; "))
}

// mountAttempt is one concrete command Macker tries when mounting a read-only
// volume.
type mountAttempt struct {
	Binary string
	Args   []string
}

// Absolute helper locations tried before $PATH. A node agent such as maclet
// launches Macker from a service context whose PATH does not include /sbin, and
// resolving a mount helper by name there fails on a node that has the helper.
var mountNullfsCandidatePaths = []string{
	"/sbin/mount_nullfs",
	"/usr/sbin/mount_nullfs",
	"/usr/local/sbin/mount_nullfs",
}

var mountHelperCandidatePaths = []string{
	"/sbin/mount",
	"/usr/sbin/mount",
	"/bin/mount",
	"/usr/bin/mount",
}

// nullfsMountAttempts returns the mount commands Macker tries, most specific
// first: the nullfs helper in its documented option placement, the same helper
// with options trailing, then mount(8) asked for the null filesystem type. The
// explicit nullfs helper is preferred because it cannot be mistaken for a
// request to mount some other filesystem type read-only.
func nullfsMountAttempts(hostPath, target string) ([]mountAttempt, error) {
	if configured := os.Getenv("MACKER_MOUNT_NULLFS"); configured != "" {
		return []mountAttempt{
			{Binary: configured, Args: []string{"-o", "ro", hostPath, target}},
			{Binary: configured, Args: []string{hostPath, target, "-o", "ro"}},
		}, nil
	}
	var attempts []mountAttempt
	for _, binary := range resolveHelper(mountNullfsCandidatePaths, "mount_nullfs") {
		attempts = append(attempts,
			mountAttempt{Binary: binary, Args: []string{"-o", "ro", hostPath, target}},
			mountAttempt{Binary: binary, Args: []string{hostPath, target, "-o", "ro"}},
		)
	}
	for _, binary := range resolveHelper(mountHelperCandidatePaths, "mount") {
		attempts = append(attempts,
			mountAttempt{Binary: binary, Args: []string{"-t", "null", "-o", "ro", hostPath, target}},
			mountAttempt{Binary: binary, Args: []string{"-t", "null", hostPath, target, "-o", "ro"}},
		)
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("no mount helper was found: tried %s and LookPath(mount_nullfs), then %s and LookPath(mount); confirm on the node with `ls -l /sbin/mount_nullfs` and `sudo /sbin/mount -t null -o ro SOURCE TARGET`, or name the helper explicitly with MACKER_MOUNT_NULLFS", strings.Join(mountNullfsCandidatePaths, ", "), strings.Join(mountHelperCandidatePaths, ", "))
	}
	return attempts, nil
}

// resolveHelper returns executable candidates by absolute path first, then the
// bare name through $PATH, which is last rather than first so a restricted
// service PATH cannot decide whether a node supports a mount.
func resolveHelper(candidates []string, name string) []string {
	found := make([]string, 0, len(candidates)+1)
	seen := make(map[string]struct{}, len(candidates)+1)
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		found = append(found, candidate)
	}
	if resolved, err := exec.LookPath(name); err == nil {
		if _, duplicate := seen[resolved]; !duplicate {
			found = append(found, resolved)
		}
	}
	return found
}

// nullfsAvailable reports whether this node has the Darwin nullfs mount path.
// MACKER_TEST_ALLOW_NULLFS is a test-only seam so the mount plumbing can be
// exercised on a Linux CI node; it never changes what a real node accepts.
func nullfsAvailable() bool {
	return runtime.GOOS == "darwin" || os.Getenv("MACKER_TEST_ALLOW_NULLFS") != ""
}

func umountBinary() string {
	if configured := os.Getenv("MACKER_UMOUNT"); configured != "" {
		return configured
	}
	resolved := resolveHelper(umountCandidatePaths, "umount")
	if len(resolved) > 0 {
		return resolved[0]
	}
	return "/sbin/umount"
}

var umountCandidatePaths = []string{
	"/sbin/umount",
	"/usr/sbin/umount",
	"/bin/umount",
	"/usr/bin/umount",
}

// unmountReadOnlyVolumes detaches read-only volume mounts before a container's
// rootfs is removed. A busy mount is reported rather than skipped: removing a
// mounted directory fails with EBUSY anyway, and silence here would leave a
// host path mounted under a directory that no longer exists.
func unmountReadOnlyVolumes(rootfs string, volumes []volume) error {
	var cleanupErrors []error
	for _, mount := range volumes {
		if !mount.ReadOnly {
			continue
		}
		target := filepath.Join(rootfs, filepath.FromSlash(mount.containerPath()))
		if _, err := os.Lstat(target); err != nil {
			continue
		}
		if _, err := runPrivileged(umountBinary(), target); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("unmount read-only volume %q: %w", mount.containerPath(), err))
		}
	}
	return errors.Join(cleanupErrors...)
}
