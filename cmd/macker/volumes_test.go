package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeVolumeHelper writes a stand-in for a privileged mount utility that records
// each invocation, so volume mount plumbing can be tested without a kernel.
func fakeVolumeHelper(t *testing.T, name, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, name+".log")
	script := filepath.Join(dir, name)
	scriptBody := "#!/bin/sh\nfor arg in \"$@\"; do echo \"$arg\" >> " + strconv.Quote(logPath) + "; done\n" + body + "\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, logPath
}

func readHelperLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func TestParseVolumeOptions(t *testing.T) {
	host := t.TempDir()
	tests := []struct {
		input    string
		readOnly bool
	}{
		{input: host + ":/data", readOnly: false},
		{input: host + ":/data:ro", readOnly: true},
		{input: host + ":/usr/share/nginx/html:ro", readOnly: true},
	}
	for _, test := range tests {
		mount, err := parseVolume(test.input)
		if err != nil {
			t.Fatalf("parseVolume(%q): %v", test.input, err)
		}
		if mount.hostPath() != host {
			t.Fatalf("parseVolume(%q) host = %q, want %q", test.input, mount.hostPath(), host)
		}
		if mount.containerPath() != strings.SplitN(strings.TrimSuffix(test.input, ":ro"), ":", 2)[1] {
			t.Fatalf("parseVolume(%q) container = %q", test.input, mount.containerPath())
		}
		if mount.ReadOnly != test.readOnly {
			t.Fatalf("parseVolume(%q) ReadOnly = %v, want %v", test.input, mount.ReadOnly, test.readOnly)
		}
	}
}

func TestParseVolumeRejectsUnsupportedOptions(t *testing.T) {
	host := t.TempDir()
	invalid := []string{
		host + ":/data:rw",
		host + ":/data:RO",
		host + ":/data:noexec",
		host + ":/data:ro:extra",
		host + ":/data:",
		"/data",
		":/data",
		host + ":data",
	}
	for _, input := range invalid {
		if _, err := parseVolume(input); err == nil {
			t.Fatalf("parseVolume(%q) accepted an unsupported volume spec", input)
		} else if !strings.Contains(err.Error(), "HOST:CONTAINER") && !strings.Contains(err.Error(), "only :ro") && !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("parseVolume(%q) error = %q, want an actionable message", input, err)
		}
	}
}

func TestParseVolumeReadOnlyMessageMentionsRo(t *testing.T) {
	host := t.TempDir()
	_, err := parseVolume(host + ":/data:rw")
	if err == nil {
		t.Fatal("expected :rw to be rejected")
	}
	if !strings.Contains(err.Error(), "only :ro is supported") {
		t.Fatalf("error = %q, want it to name the supported option", err)
	}
}

func TestInstallVolumeReadOnlyMountsNullfsReadOnly(t *testing.T) {
	mountHelper, mountLog := fakeVolumeHelper(t, "mount_nullfs", "exit 0")
	umountHelper, umountLog := fakeVolumeHelper(t, "umount", "exit 0")
	t.Setenv("MACKER_MOUNT_NULLFS", mountHelper)
	t.Setenv("MACKER_UMOUNT", umountHelper)
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")

	rootfs := t.TempDir()
	host := t.TempDir()
	mount := volume{HostPath: host, ContainerPath: "/data", ReadOnly: true}
	if err := installVolume(rootfs, mount); err != nil {
		t.Fatal(err)
	}

	// A writable volume must stay a live symlink; enforcement is opt-in only.
	writable := volume{HostPath: host, ContainerPath: "/rw"}
	if err := installVolume(rootfs, writable); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(rootfs, "rw"))
	if err != nil || link != host {
		t.Fatalf("writable volume link = %q, want %q (err %v)", link, host, err)
	}

	calls := readHelperLog(t, mountLog)
	if len(calls) != 4 {
		t.Fatalf("mount invocation = %q, want exactly four arguments", calls)
	}
	want := []string{"-o", "ro", host, filepath.Join(rootfs, "data")}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("mount invocation = %q, want %q", calls, want)
	}
	info, err := os.Lstat(filepath.Join(rootfs, "data"))
	if err != nil {
		t.Fatalf("read-only mount point missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("a read-only volume must not be a live symlink")
	}
	if !info.IsDir() {
		t.Fatalf("read-only mount point mode = %v, want a directory", info.Mode())
	}

	if err := unmountReadOnlyVolumes(rootfs, []volume{mount, writable}); err != nil {
		t.Fatal(err)
	}
	unmountCalls := readHelperLog(t, umountLog)
	if !reflect.DeepEqual(unmountCalls, []string{filepath.Join(rootfs, "data")}) {
		t.Fatalf("umount calls = %q, want only the read-only target", unmountCalls)
	}
}

func TestInstallVolumeReadOnlyFailsWhenMountFails(t *testing.T) {
	mountHelper, _ := fakeVolumeHelper(t, "mount_nullfs", "echo 'mount: unknown filesystem type' >&2; exit 1")
	t.Setenv("MACKER_MOUNT_NULLFS", mountHelper)
	t.Setenv("MACKER_UMOUNT", "umount")
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")
	rootfs := t.TempDir()
	host := t.TempDir()
	err := installVolume(rootfs, volume{HostPath: host, ContainerPath: "/data", ReadOnly: true})
	if err == nil {
		t.Fatal("expected a failed mount to fail the volume install")
	}
	if !strings.Contains(err.Error(), "read-only") || !strings.Contains(err.Error(), "root or passwordless sudo") {
		t.Fatalf("error = %q, want it to name the read-only mount and the privilege requirement", err)
	}
}

func TestInstallVolumeReadOnlyRejectsFileHostPath(t *testing.T) {
	hostFile := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(hostFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_MOUNT_NULLFS", "/bin/true")
	t.Setenv("MACKER_UMOUNT", "umount")
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")
	err := installVolume(t.TempDir(), volume{HostPath: hostFile, ContainerPath: "/etc/config", ReadOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %q, want the single-file nullfs limitation", err)
	}
}

func TestReadOnlyVolumeRequiresDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("nullfs is native on Darwin")
	}
	t.Setenv("MACKER_MOUNT_NULLFS", "/bin/true")
	t.Setenv("MACKER_UMOUNT", "umount")
	err := installVolume(t.TempDir(), volume{HostPath: t.TempDir(), ContainerPath: "/data", ReadOnly: true})
	if err == nil {
		t.Fatal("expected :ro to be rejected on a non-Darwin node")
	}
	if !strings.Contains(err.Error(), "Darwin") {
		t.Fatalf("error = %q, want it to name the platform requirement", err)
	}
	if !strings.Contains(err.Error(), "mount_nullfs") {
		t.Fatalf("error = %q, want it to name the mechanism", err)
	}
}

func TestUnmountReadOnlyVolumesReportsFailure(t *testing.T) {
	umountHelper, _ := fakeVolumeHelper(t, "umount", "echo 'umount: Device busy' >&2; exit 1")
	t.Setenv("MACKER_UMOUNT", umountHelper)
	rootfs := t.TempDir()
	target := filepath.Join(rootfs, "data")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	err := unmountReadOnlyVolumes(rootfs, []volume{{HostPath: t.TempDir(), ContainerPath: "/data", ReadOnly: true}})
	if err == nil || !strings.Contains(err.Error(), "unmount read-only volume") {
		t.Fatalf("error = %q, want a reported unmount failure", err)
	}
}

func TestUnmountReadOnlyVolumesSkipsMissingTargets(t *testing.T) {
	umountHelper, umountLog := fakeVolumeHelper(t, "umount", "exit 0")
	t.Setenv("MACKER_UMOUNT", umountHelper)
	rootfs := t.TempDir()
	if err := unmountReadOnlyVolumes(rootfs, []volume{{HostPath: t.TempDir(), ContainerPath: "/gone", ReadOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if calls := readHelperLog(t, umountLog); calls != nil {
		t.Fatalf("umount calls = %q, want none for a missing mount point", calls)
	}
}

func TestInspectJSONReportsVolumesAndReadOnlyState(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "demo")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	created := time.Unix(10, 0).UTC()
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:           "demo",
		Image:          "scratch",
		CreatedAt:      created,
		Volumes:        []volume{{HostPath: host, ContainerPath: "/data"}, {HostPath: host, ContainerPath: "/ro", ReadOnly: true}},
		MountedVolumes: []volume{{HostPath: host, ContainerPath: "/ro", ReadOnly: true}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_HOME", home)

	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	commandErr := commandInspect([]string{"--format", "json", "demo"})
	_ = writer.Close()
	os.Stdout = originalStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if commandErr != nil {
		t.Fatal(commandErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	var inspection containerInspection
	if err := json.Unmarshal(output, &inspection); err != nil {
		t.Fatalf("inspect output %q: %v", output, err)
	}
	wantVolumes := []volumeInspection{
		{HostPath: host, ContainerPath: "/data"},
		{HostPath: host, ContainerPath: "/ro", ReadOnly: true},
	}
	if !reflect.DeepEqual(inspection.Volumes, wantVolumes) {
		t.Fatalf("inspection volumes = %#v, want %#v", inspection.Volumes, wantVolumes)
	}
	if !reflect.DeepEqual(inspection.MountedVolumes, wantVolumes[1:]) {
		t.Fatalf("inspection mounted volumes = %#v, want %#v", inspection.MountedVolumes, wantVolumes[1:])
	}
	if !strings.Contains(string(output), `"read_only": true`) {
		t.Fatalf("inspect JSON = %s, want the read-only state reported", output)
	}
}

func TestInspectJSONReportsEmptyVolumes(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "demo")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	created := time.Unix(10, 0).UTC()
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:      "demo",
		Image:     "scratch",
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_HOME", home)
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	commandErr := commandInspect([]string{"--format", "json", "demo"})
	_ = writer.Close()
	os.Stdout = originalStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if commandErr != nil {
		t.Fatal(commandErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	var inspection containerInspection
	if err := json.Unmarshal(output, &inspection); err != nil {
		t.Fatal(err)
	}
	if len(inspection.Volumes) != 0 || len(inspection.MountedVolumes) != 0 {
		t.Fatalf("inspection volumes = %#v, want empty lists", inspection.Volumes)
	}
}

func TestVolumeMetadataKeepsLegacyShapeReadable(t *testing.T) {
	// Metadata written before volume JSON tags existed used Go field names.
	raw := `{"HostPath":"/old/host","ContainerPath":"/old/ctr"}`
	var mount volume
	if err := json.Unmarshal([]byte(raw), &mount); err != nil {
		t.Fatal(err)
	}
	if mount.hostPath() != "/old/host" || mount.containerPath() != "/old/ctr" {
		t.Fatalf("legacy volume decoded as %#v", mount)
	}
	if got := volumeInspections([]volume{mount}); !reflect.DeepEqual(got, []volumeInspection{{HostPath: "/old/host", ContainerPath: "/old/ctr"}}) {
		t.Fatalf("legacy volume inspection = %#v", got)
	}
}

func TestRunHelpAdvertisesReadOnlyVolumes(t *testing.T) {
	// maclet decides whether to forward `:ro` by probing this help text, so the
	// literal shape is part of the interface between the two tools.
	originalStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	helpErr := commandRun([]string{"--help"})
	_ = writer.Close()
	os.Stderr = originalStderr
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if helpErr == nil {
		t.Fatal("expected run --help to report a usage error")
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(output), "HOST:CONTAINER[:ro]") {
		t.Fatalf("run --help output = %s, want HOST:CONTAINER[:ro]", output)
	}
}

func TestUsageAdvertisesReadOnlyVolumes(t *testing.T) {
	originalStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	usage()
	_ = writer.Close()
	os.Stderr = originalStderr
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(output), "-v HOST:CONTAINER[:ro]") {
		t.Fatalf("usage output = %s, want -v HOST:CONTAINER[:ro]", output)
	}
}

func TestRemoveUnmountsLiveReadOnlyVolumesBeforeDeletingRootfs(t *testing.T) {
	umountHelper, umountLog := fakeVolumeHelper(t, "umount", "exit 0")
	t.Setenv("MACKER_UMOUNT", umountHelper)
	home := t.TempDir()
	t.Setenv("MACKER_HOME", home)
	containerDir := filepath.Join(home, "containers", "demo")
	rootfs := filepath.Join(containerDir, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	created := time.Unix(10, 0).UTC()
	stopped := time.Unix(20, 0).UTC()
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:           "demo",
		Image:          "scratch",
		CreatedAt:      created,
		StoppedAt:      &stopped,
		Volumes:        []volume{{HostPath: host, ContainerPath: "/data", ReadOnly: true}},
		MountedVolumes: []volume{{HostPath: host, ContainerPath: "/data", ReadOnly: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := commandRM([]string{"demo"}); err != nil {
		t.Fatal(err)
	}
	if calls := readHelperLog(t, umountLog); !reflect.DeepEqual(calls, []string{filepath.Join(rootfs, "data")}) {
		t.Fatalf("umount calls = %q, want the live read-only target", calls)
	}
	if _, err := os.Stat(containerDir); !os.IsNotExist(err) {
		t.Fatalf("container directory still present: %v", err)
	}
}

func TestVolumeInspectionsEmptyList(t *testing.T) {
	if got := volumeInspections(nil); got == nil || len(got) != 0 {
		t.Fatalf("volumeInspections(nil) = %#v, want an empty list so JSON reports []", got)
	}
}

// TestReadOnlyMountResolvesHelperWithoutSbinOnPath is maclet's measured failure
// on icarus-2.local: maclet launches Macker from a service context whose PATH
// does not include /sbin, and a mount helper looked up by name is then missing
// on a node that has it. Absolute candidates must be resolved before $PATH.
func TestReadOnlyMountResolvesHelperWithoutSbinOnPath(t *testing.T) {
	helper, helperLog := fakeVolumeHelper(t, "mount_nullfs", "exit 0")
	originalNullfs := mountNullfsCandidatePaths
	originalMount := mountHelperCandidatePaths
	defer func() {
		mountNullfsCandidatePaths = originalNullfs
		mountHelperCandidatePaths = originalMount
	}()
	// The helper exists at an absolute path only; $PATH cannot see it.
	mountNullfsCandidatePaths = []string{helper}
	mountHelperCandidatePaths = []string{"/nonexistent/mount"}
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")

	rootfs := t.TempDir()
	host := t.TempDir()
	mount := volume{HostPath: host, ContainerPath: "/data", ReadOnly: true}
	if err := installVolume(rootfs, mount); err != nil {
		t.Fatalf("a helper reachable only by absolute path must still be used: %v", err)
	}
	want := []string{"-o", "ro", host, filepath.Join(rootfs, "data")}
	if got := readHelperLog(t, helperLog); !reflect.DeepEqual(got, want) {
		t.Fatalf("mount invocation = %q, want %q", got, want)
	}
}

// TestReadOnlyMountFallsBackToMountWithNullFilesystemType covers a node whose
// userland has no mount_nullfs helper but does have mount(8).
func TestReadOnlyMountFallsBackToMountWithNullFilesystemType(t *testing.T) {
	helper, helperLog := fakeVolumeHelper(t, "mount", "exit 0")
	originalNullfs := mountNullfsCandidatePaths
	originalMount := mountHelperCandidatePaths
	defer func() {
		mountNullfsCandidatePaths = originalNullfs
		mountHelperCandidatePaths = originalMount
	}()
	mountNullfsCandidatePaths = []string{"/nonexistent/mount_nullfs", "/usr/sbin/mount_nullfs"}
	mountHelperCandidatePaths = []string{helper}
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")

	rootfs := t.TempDir()
	host := t.TempDir()
	mount := volume{HostPath: host, ContainerPath: "/data", ReadOnly: true}
	if err := installVolume(rootfs, mount); err != nil {
		t.Fatalf("mount(8) with the null filesystem type must be attempted: %v", err)
	}
	want := []string{"-t", "nullfs", "-o", "ro", host, filepath.Join(rootfs, "data")}
	if got := readHelperLog(t, helperLog); !reflect.DeepEqual(got, want) {
		t.Fatalf("mount invocation = %q, want %q", got, want)
	}
}

// TestReadOnlyMountNamesEveryHelperItTried: when nothing resolves, the error has
// to tell an operator what was searched and what to run on the node, rather than
// asserting that a utility ships with macOS.
func TestReadOnlyMountNamesEveryHelperItTried(t *testing.T) {
	originalNullfs := mountNullfsCandidatePaths
	originalMount := mountHelperCandidatePaths
	defer func() {
		mountNullfsCandidatePaths = originalNullfs
		mountHelperCandidatePaths = originalMount
	}()
	mountNullfsCandidatePaths = []string{"/sbin/mount_nullfs", "/usr/sbin/mount_nullfs"}
	mountHelperCandidatePaths = []string{"/sbin/mount", "/usr/sbin/mount"}
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")

	rootfs := t.TempDir()
	err := installVolume(rootfs, volume{HostPath: t.TempDir(), ContainerPath: "/data", ReadOnly: true})
	if err == nil {
		t.Fatal("expected a failure when no mount helper exists")
	}
	message := err.Error()
	for _, want := range []string{
		"no mount helper was found",
		"/sbin/mount_nullfs",
		"LookPath(mount_nullfs)",
		"/sbin/mount",
		"LookPath(mount)",
		"ls -l /sbin/mount_nullfs",
		"sudo /sbin/mount -t nullfs -o ro SOURCE TARGET",
		"MACKER_MOUNT_NULLFS",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error = %q, want it to contain %q", message, want)
		}
	}
}

// TestReadOnlyMountReportsEveryAttemptItMade turns a failed mount into the list
// of commands the node actually rejected, which is what separates a missing
// helper from a missing sudo rule from a kernel without nullfs.
func TestReadOnlyMountReportsEveryAttemptItMade(t *testing.T) {
	helper, _ := fakeVolumeHelper(t, "mount_nullfs", "echo 'mount: null file system not supported' >&2; exit 1")
	mountHelper, _ := fakeVolumeHelper(t, "mount", "echo 'mount: unknown file system type null' >&2; exit 1")
	originalNullfs := mountNullfsCandidatePaths
	originalMount := mountHelperCandidatePaths
	defer func() {
		mountNullfsCandidatePaths = originalNullfs
		mountHelperCandidatePaths = originalMount
	}()
	mountNullfsCandidatePaths = []string{helper}
	mountHelperCandidatePaths = []string{mountHelper}
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")

	rootfs := t.TempDir()
	host := t.TempDir()
	target := filepath.Join(rootfs, "data")
	err := installVolume(rootfs, volume{HostPath: host, ContainerPath: "/data", ReadOnly: true})
	if err == nil {
		t.Fatal("expected every failed mount attempt to fail the volume install")
	}
	message := err.Error()
	for _, want := range []string{
		helper + " -o ro " + host + " " + target,
		helper + " " + host + " " + target + " -o ro",
		mountHelper + " -t nullfs -o ro " + host + " " + target,
		mountHelper + " -t null -o ro " + host + " " + target,
		"null file system not supported",
		"root or passwordless sudo",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error = %q, want it to contain %q", message, want)
		}
	}
}

func TestUmountBinaryResolvesAbsolutePathWithoutPathHelp(t *testing.T) {
	helper, _ := fakeVolumeHelper(t, "umount", "exit 0")
	original := umountCandidatePaths
	defer func() { umountCandidatePaths = original }()
	umountCandidatePaths = []string{"/nonexistent/umount", helper}
	t.Setenv("PATH", "/nonexistent")
	if got := umountBinary(); got != helper {
		t.Fatalf("umount binary = %q, want the absolute candidate %q", got, helper)
	}
	t.Setenv("MACKER_UMOUNT", "/custom/umount")
	if got := umountBinary(); got != "/custom/umount" {
		t.Fatalf("umount binary = %q, want the configured override", got)
	}
}

// TestNullfsMountAttemptsAskForTheRegisteredTypeNameFirst records that the order is
// not cosmetic. mount(2) run as root on macOS 26.6.2 draws the same errno for the type
// null that it draws for a made-up name, while the type nullfs reaches a mount entry
// point that answers EPERM. Asking mount(8) for null first would make a node that does
// have a bundle look unsupported for the wrong reason.
func TestNullfsMountAttemptsAskForTheRegisteredTypeNameFirst(t *testing.T) {
	helper, _ := fakeVolumeHelper(t, "mount", "exit 0")
	originalNullfs := mountNullfsCandidatePaths
	originalMount := mountHelperCandidatePaths
	defer func() {
		mountNullfsCandidatePaths = originalNullfs
		mountHelperCandidatePaths = originalMount
	}()
	mountNullfsCandidatePaths = []string{"/nonexistent/mount_nullfs"}
	mountHelperCandidatePaths = []string{helper}
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("MACKER_MOUNT_NULLFS", "")

	attempts, err := nullfsMountAttempts("/host", "/target")
	if err != nil {
		t.Fatalf("mount attempts: %v", err)
	}
	var types []string
	for _, attempt := range attempts {
		types = append(types, attempt.Binary+" "+strings.Join(attempt.Args, " "))
	}
	if len(types) != 4 {
		t.Fatalf("attempts = %q, want the nullfs and null type names in both option placements", types)
	}
	if !strings.Contains(types[0], "-t nullfs -o ro /host /target") {
		t.Fatalf("first attempt = %q, want the type name this kernel registers", types[0])
	}
	if !strings.Contains(types[2], "-t null -o ro /host /target") {
		t.Fatalf("attempts = %q, want the older type name tried after it", types)
	}
	if got := nullfsBundleSearchText(); got != "nullfs.fs or null.fs" {
		t.Fatalf("bundle search text = %q, want the bundle names mount(8) looks for, in the order it looks for them", got)
	}
}
