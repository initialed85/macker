// Tests for the read-only volume capability report. They ask the Darwin question
// on a Linux CI node by passing the host OS, and they never mount anything.
package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func useNullfsBundle(t *testing.T, exists bool) {
	t.Helper()
	original := nullfsBundlePath
	t.Cleanup(func() { nullfsBundlePath = original })
	if exists {
		nullfsBundlePath = t.TempDir()
		return
	}
	nullfsBundlePath = filepath.Join(t.TempDir(), "null.fs")
}

func restrictMountSearch(t *testing.T, nullfsHelpers, mountHelpers []string) {
	t.Helper()
	originalNullfs := mountNullfsCandidatePaths
	originalMount := mountHelperCandidatePaths
	t.Cleanup(func() {
		mountNullfsCandidatePaths = originalNullfs
		mountHelperCandidatePaths = originalMount
	})
	mountNullfsCandidatePaths = nullfsHelpers
	mountHelperCandidatePaths = mountHelpers
	t.Setenv("PATH", "/nonexistent")
}

func TestReadOnlyMountBlockageNamesBundleAndOverride(t *testing.T) {
	useNullfsBundle(t, false)
	t.Setenv("MACKER_MOUNT_NULLFS", "")
	blockage := readOnlyMountBlockage("darwin")
	for _, want := range []string{
		"no null filesystem bundle",
		nullfsBundlePath,
		"mount(2)",
		// The gate Apple added for this Darwin generation, named exactly so a
		// reader can check it against xnu's bsd/miscfs/nullfs/null_vfsops.c.
		"com.apple.private.nullfs_allow",
		"no superuser fallback",
		"MACKER_MOUNT_NULLFS",
		"run this workload without :ro",
	} {
		if !strings.Contains(blockage, want) {
			t.Fatalf("blockage = %q, want it to contain %q", blockage, want)
		}
	}

	useNullfsBundle(t, true)
	if got := readOnlyMountBlockage("darwin"); got != "" {
		t.Fatalf("blockage = %q, want no blockage when the null filesystem bundle exists", got)
	}

	// An explicit helper is exempt: it can call mount(2) without mount(8)'s bundle.
	useNullfsBundle(t, false)
	t.Setenv("MACKER_MOUNT_NULLFS", "/opt/local/bin/mount_null_probe")
	if got := readOnlyMountBlockage("darwin"); got != "" {
		t.Fatalf("blockage = %q, want an explicit helper override to be trusted to attempt the mount", got)
	}
}

func TestReadOnlyMountBlockageRejectsOtherOperatingSystems(t *testing.T) {
	t.Setenv("MACKER_MOUNT_NULLFS", "")
	blockage := readOnlyMountBlockage("linux")
	if !strings.Contains(blockage, "Darwin") || !strings.Contains(blockage, "mount_nullfs") {
		t.Fatalf("blockage = %q, want it to name the platform requirement and the mechanism", blockage)
	}
	if !strings.Contains(blockage, "rather than pretending") {
		t.Fatalf("blockage = %q, want it to say refusal is deliberate", blockage)
	}
}

func TestVolumeCapabilitiesDarwinRefusesWhenTheNullFilesystemIsGone(t *testing.T) {
	useNullfsBundle(t, false)
	restrictMountSearch(t, []string{"/sbin/mount_nullfs", "/usr/sbin/mount_nullfs"}, []string{"/sbin/mount"})
	t.Setenv("MACKER_MOUNT_NULLFS", "")

	report := volumeCapabilities("darwin", "arm64")
	if report.ReadOnlySupported {
		t.Fatalf("report = %+v, want read-only refused when the null filesystem bundle is missing", report)
	}
	if report.NullfsBundleFound {
		t.Fatalf("report = %+v, want nullfs_bundle_found false", report)
	}
	if report.Mechanism != "none" {
		t.Fatalf("mechanism = %q, want none", report.Mechanism)
	}
	if !strings.Contains(report.Reason, "no null filesystem bundle") {
		t.Fatalf("reason = %q, want it to name the missing null filesystem bundle", report.Reason)
	}
	if report.WritableMechanism == "" {
		t.Fatal("report must still say how writable volumes are handled")
	}
}

func TestVolumeCapabilitiesDarwinReportsSupportedWithHelperAndBundle(t *testing.T) {
	useNullfsBundle(t, true)
	helper, _ := fakeVolumeHelper(t, "mount_nullfs", "exit 0")
	restrictMountSearch(t, []string{helper}, []string{"/nonexistent/mount"})
	t.Setenv("MACKER_MOUNT_NULLFS", "")

	report := volumeCapabilities("darwin", "arm64")
	if !report.ReadOnlySupported {
		t.Fatalf("report = %+v, want read-only supported", report)
	}
	if !strings.Contains(report.Mechanism, "nullfs") {
		t.Fatalf("mechanism = %q, want the nullfs mount", report.Mechanism)
	}
	if len(report.MountForms) == 0 {
		t.Fatal("report must name the mount commands it would run")
	}
	want := helper + " -o ro HOST CONTAINER"
	if report.MountForms[0] != want {
		t.Fatalf("first mount form = %q, want %q", report.MountForms[0], want)
	}
	if !strings.Contains(report.Reason, "sudo") {
		t.Fatalf("reason = %q, want it to state the privilege requirement", report.Reason)
	}
}

func TestVolumeCapabilitiesLinuxRefuses(t *testing.T) {
	t.Setenv("MACKER_MOUNT_NULLFS", "")
	report := volumeCapabilities("linux", "amd64")
	if report.ReadOnlySupported {
		t.Fatalf("report = %+v, want a Linux node to refuse :ro", report)
	}
	if !strings.Contains(report.Reason, "Darwin") {
		t.Fatalf("reason = %q, want it to name the platform requirement", report.Reason)
	}
	if len(report.Helpers) != 0 {
		t.Fatalf("helpers = %q, want no mount helpers consulted on a node that cannot use them", report.Helpers)
	}
}

func TestVolumesCommandReportsMarkerAndJSON(t *testing.T) {
	table := captureStdout(t, func() {
		if err := commandVolumes([]string{}); err != nil {
			t.Errorf("volumes: %v", err)
		}
	})
	firstLine := strings.Split(strings.TrimSpace(table), "\n")[0]
	if firstLine != "read-only volumes: supported" && firstLine != "read-only volumes: refused" {
		t.Fatalf("first line = %q, want a stable supported or refused marker", firstLine)
	}
	if !strings.Contains(table, "overrides: MACKER_MOUNT_NULLFS") {
		t.Fatalf("output = %q, want the override variables named", table)
	}

	encoded := captureStdout(t, func() {
		if err := commandVolumes([]string{"--format", "json"}); err != nil {
			t.Errorf("volumes --format json: %v", err)
		}
	})
	var report volumeCapabilityReport
	if err := json.Unmarshal([]byte(encoded), &report); err != nil {
		t.Fatalf("json output is not a volumeCapabilityReport: %v (%q)", err, encoded)
	}
	if report.HostOS != runtime.GOOS || report.HostArch != runtime.GOARCH {
		t.Fatalf("report host = %s/%s, want %s/%s", report.HostOS, report.HostArch, runtime.GOOS, runtime.GOARCH)
	}
	if report.ReadOnlySupported != volumeCapabilities(runtime.GOOS, runtime.GOARCH).ReadOnlySupported {
		t.Fatal("json output must agree with the table output")
	}

	if err := commandVolumes([]string{"/tmp"}); err == nil {
		t.Fatal("expected volumes to reject arguments; it reports this node, not a path")
	}
	if err := commandVolumes([]string{"--format", "yaml"}); err == nil {
		t.Fatal("expected an unknown format to be rejected")
	}
}

// TestUsageAdvertisesVolumesCommand keeps the diagnostic discoverable, the same
// way the platforms command is: a node agent must be able to find the probe.
func TestUsageAdvertisesVolumesCommand(t *testing.T) {
	originalStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	usage()
	_ = writer.Close()
	os.Stderr = originalStderr
	data, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "macker volumes") {
		t.Fatalf("usage must list the volumes capability command, got %q", string(data))
	}
}

func TestNullfsBlockageStaysAbsentWhenMountPlumbingIsTestedOnLinux(t *testing.T) {
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")
	t.Setenv("MACKER_MOUNT_NULLFS", "/bin/true")
	if got := readOnlyMountBlockage("linux"); got != "" {
		t.Fatalf("blockage = %q, want the documented test seam to allow mount plumbing tests", got)
	}
}
