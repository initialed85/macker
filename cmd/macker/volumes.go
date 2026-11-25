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
	if !nullfsAvailable() {
		return fmt.Errorf("read-only volume %q requires a Darwin node: current host is %s/%s; Macker enforces :ro with mount_nullfs", mount.containerPath(), runtime.GOOS, runtime.GOARCH)
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
	binary, err := mountNullfsBinary()
	if err != nil {
		return err
	}
	// mount(8) wrappers differ in where they stop scanning options, so try the
	// documented placement first and then the trailing-option form.
	attempts := [][]string{
		{"-o", "ro", hostPath, target},
		{hostPath, target, "-o", "ro"},
	}
	var lastErr error
	for _, args := range attempts {
		if _, err := runPrivileged(binary, args...); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("mount %q read-only at %q: %w; read-only volumes require root or passwordless sudo", hostPath, target, lastErr)
}

// nullfsAvailable reports whether this node has the Darwin nullfs mount path.
// MACKER_TEST_ALLOW_NULLFS is a test-only seam so the mount plumbing can be
// exercised on a Linux CI node; it never changes what a real node accepts.
func nullfsAvailable() bool {
	return runtime.GOOS == "darwin" || os.Getenv("MACKER_TEST_ALLOW_NULLFS") != ""
}

func mountNullfsBinary() (string, error) {
	if configured := os.Getenv("MACKER_MOUNT_NULLFS"); configured != "" {
		return configured, nil
	}
	if !nullfsAvailable() {
		return "", fmt.Errorf("mount_nullfs is a Darwin utility; current host is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	candidate := "/sbin/mount_nullfs"
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	if found, err := exec.LookPath("mount_nullfs"); err == nil {
		return found, nil
	}
	return "", errors.New("mount_nullfs was not found; it ships with macOS")
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

func umountBinary() string {
	if configured := os.Getenv("MACKER_UMOUNT"); configured != "" {
		return configured
	}
	return "/sbin/umount"
}
