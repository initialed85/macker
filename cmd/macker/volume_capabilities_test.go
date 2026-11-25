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

// TestReadOnlyMountBlockageStaysShortAndMakesNoPromise states the contract maclet
// asked for: maclet copies this text into a Pod's status, so `kubectl describe pod`
// shows it, and a remedy that cannot work on this OS must not appear in it.
func TestReadOnlyMountBlockageStaysShortAndMakesNoPromise(t *testing.T) {
	useNullfsBundle(t, false)
	t.Setenv("MACKER_MOUNT_NULLFS", "")
	blockage := readOnlyMountBlockage("darwin")
	for _, want := range []string{
		"no null filesystem bundle",
		nullfsBundlePath,
		"no mount_nullfs exists",
		"does not register the null filesystem",
		"Run this workload without :ro",
	} {
		if !strings.Contains(blockage, want) {
			t.Fatalf("blockage = %q, want it to contain %q", blockage, want)
		}
	}
	for _, forbidden := range []string{
		// The unreachable override route and the entitlement discussion belong in
		// guidance, not in a Pod status a user reads.
		"MACKER_MOUNT_NULLFS",
		"com.apple.private.nullfs_allow",
		"macker volumes --probe",
	} {
		if strings.Contains(blockage, forbidden) {
			t.Fatalf("blockage = %q, want it to stay out of %q", blockage, forbidden)
		}
	}
	if length := len([]rune(blockage)); length > 450 {
		t.Fatalf("blockage is %d characters, want under 450 because a node agent puts it in a Pod status: %q", length, blockage)
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

// TestReadOnlyMountGuidanceCarriesTheOperatorChecklist keeps the long diagnosis
// available where a node operator will actually read it: `macker volumes`.
func TestReadOnlyMountGuidanceCarriesTheOperatorChecklist(t *testing.T) {
	useNullfsBundle(t, false)
	t.Setenv("MACKER_MOUNT_NULLFS", "")
	guidance := readOnlyMountGuidance("darwin")
	for _, want := range []string{
		// Named exactly so a reader can check it against xnu's
		// bsd/miscfs/nullfs/null_vfsops.c.
		"com.apple.private.nullfs_allow",
		"no superuser fallback",
		"ENODEV",
		"MACKER_MOUNT_NULLFS",
		"macker volumes --probe",
		"ls -l /sbin/mount_nullfs",
		nullfsBundlePath,
	} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("guidance = %q, want it to contain %q", guidance, want)
		}
	}

	if got := readOnlyMountGuidance("linux"); !strings.Contains(got, "Darwin") || !strings.Contains(got, "MACKER_MOUNT_NULLFS") {
		t.Fatalf("guidance = %q, want it to name the platform requirement and the override route", got)
	}

	useNullfsBundle(t, true)
	if got := readOnlyMountGuidance("darwin"); !strings.Contains(got, "the null filesystem bundle exists") || !strings.Contains(got, "macker volumes --probe") {
		t.Fatalf("guidance = %q, want a node that has the bundle to get the confirmation commands rather than the macOS 26 story", got)
	}

	// The report carries both, so a caller can put one in a Pod status and log the
	// other on the node.
	report := volumeCapabilities("darwin", "arm64")
	if report.Guidance == "" || report.Reason == "" {
		t.Fatalf("report = %+v, want both a short reason and operator guidance", report)
	}
	text := renderVolumeCapabilities(report)
	if !strings.Contains(text, "guidance: "+report.Guidance) {
		t.Fatalf("rendered report = %q, want the guidance line", text)
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

func TestRenderedReportQualifiesAHelperThatCannotMountNullOnDarwin(t *testing.T) {
	useNullfsBundle(t, false)
	helper, _ := fakeVolumeHelper(t, "mount_nullfs", "exit 0")
	restrictMountSearch(t, []string{helper}, []string{"/nonexistent/mount"})
	t.Setenv("MACKER_MOUNT_NULLFS", "")

	// A Darwin node with no null filesystem bundle still has /sbin/mount, so the
	// helper line has to say what the helper cannot do on this OS.
	text := renderVolumeCapabilities(volumeCapabilities("darwin", "arm64"))
	for _, want := range []string{
		"mount helpers resolved: " + helper + " (found, but mount(8) needs ",
		"to mount the null type, so a found helper proves nothing here)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered report = %q, want it to contain %q", text, want)
		}
	}

	useNullfsBundle(t, true)
	qualified := renderVolumeCapabilities(volumeCapabilities("darwin", "arm64"))
	if strings.Contains(qualified, "proves nothing here") {
		t.Fatalf("rendered report = %q, want no warning when the null filesystem bundle exists", qualified)
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

// useReadOnlyProbeSeam replaces the mount the probe would perform so a test can
// say what the kernel answered without asking a kernel to mount anything.
func useReadOnlyProbeSeam(t *testing.T, run readOnlyProbeRun) {
	t.Helper()
	original := readOnlyProbeRunner
	t.Cleanup(func() { readOnlyProbeRunner = original })
	readOnlyProbeRunner = func() readOnlyProbeRun { return run }
}

func TestReadOnlyProbeRunClassifiesEveryFailureMode(t *testing.T) {
	cases := []struct {
		name       string
		run        readOnlyProbeRun
		wantStatus string
	}{
		{"enforced", readOnlyProbeRun{MountPoint: "/tmp/mnt", WriteError: "read-only file system"}, "enforced"},
		{"kernel refused the mount", readOnlyProbeRun{MountPoint: "/tmp/mnt", MountError: "/sbin/mount -t null -o ro src dst: exit status 72"}, "mount_refused"},
		{"no helper exists", readOnlyProbeRun{MountError: "no mount helper was found: tried /sbin/mount_nullfs, /sbin/mount"}, "helper_unavailable"},
		{"nobody was allowed to try", readOnlyProbeRun{MountError: "mount src dst read-only: root or passwordless sudo is required"}, "mount_not_attempted_privilege"},
		{"sudo wants a password", readOnlyProbeRun{MountError: "exit status 1: sudo: a password is required"}, "mount_not_attempted_privilege"},
		{"mount accepted a write", readOnlyProbeRun{MountPoint: "/tmp/mnt"}, "write_accepted"},
		{"mount could not be released", readOnlyProbeRun{MountPoint: "/tmp/mnt", WriteError: "read-only file system", UnmountError: "umount: /tmp/mnt: Resource busy"}, "unmount_failed"},
	}
	for _, tc := range cases {
		status := tc.run.classify()
		if status != tc.wantStatus {
			t.Errorf("%s: classify() = %q, want %q", tc.name, status, tc.wantStatus)
		}
		if enforced := tc.run.enforced(); enforced != (tc.wantStatus == "enforced") {
			t.Errorf("%s: enforced() = %t, want it to agree with the status %q", tc.name, enforced, tc.wantStatus)
		}
	}
}

func TestReadOnlyProbeRunVerdictAndReason(t *testing.T) {
	mountForm := "/sbin/mount_nullfs -o ro /tmp/src /tmp/mnt"

	supported := readOnlyProbeRun{
		MountForm:  mountForm,
		MountPoint: "/tmp/mnt",
		WriteError: "read-only file system",
	}
	if !supported.enforced() {
		t.Fatalf("probe = %+v, want enforced when the mount was accepted, the write was refused and the unmount succeeded", supported)
	}
	if !strings.Contains(supported.reason(), "enforced read-only") || !strings.Contains(supported.reason(), mountForm) {
		t.Fatalf("reason = %q, want it to name the mount form that enforced read-only", supported.reason())
	}

	mountRefused := readOnlyProbeRun{
		MountForm:  mountForm,
		MountPoint: "/tmp/mnt",
		MountError: "/sbin/mount -t null -o ro /tmp/src /tmp/mnt: exit status 72",
	}
	if mountRefused.enforced() {
		t.Fatalf("probe = %+v, want not enforced when the mount was refused", mountRefused)
	}
	if !strings.Contains(mountRefused.reason(), "probe mount was refused") || !strings.Contains(mountRefused.reason(), "exit status 72") {
		t.Fatalf("reason = %q, want it to carry the mount failure verbatim", mountRefused.reason())
	}

	writeAccepted := readOnlyProbeRun{MountForm: mountForm, MountPoint: "/tmp/mnt"}
	if writeAccepted.enforced() {
		t.Fatalf("probe = %+v, want a mount that accepts a write inside it to count as refusal", writeAccepted)
	}
	if !strings.Contains(writeAccepted.reason(), "was accepted") {
		t.Fatalf("reason = %q, want it to say the write was accepted", writeAccepted.reason())
	}

	unmountFailed := readOnlyProbeRun{
		MountForm:    mountForm,
		MountPoint:   "/tmp/mnt",
		WriteError:   "read-only file system",
		UnmountError: "umount: /tmp/mnt: Resource busy",
	}
	if unmountFailed.enforced() {
		t.Fatalf("probe = %+v, want a mount that could not be released to count as refusal", unmountFailed)
	}
	for _, want := range []string{"sudo", "/tmp/mnt", "Resource busy"} {
		if !strings.Contains(unmountFailed.reason(), want) {
			t.Fatalf("reason = %q, want it to contain %q so a human can release the mount", unmountFailed.reason(), want)
		}
	}
}

func TestProbeWriteRefusedReportsWhatAWritableDirectoryDoes(t *testing.T) {
	dir := t.TempDir()
	if got := probeWriteRefused(dir); got != "" {
		t.Fatalf("probeWriteRefused = %q, want an empty string when the write was accepted", got)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("directory = %v, want the probe file removed after a write that was accepted (err %v)", entries, err)
	}
}

func TestVolumeCapabilitiesWithProbeReportsWhatTheKernelAnswered(t *testing.T) {
	useNullfsBundle(t, true)
	t.Setenv("MACKER_MOUNT_NULLFS", "")

	useReadOnlyProbeSeam(t, readOnlyProbeRun{
		MountForm:  "/sbin/mount_nullfs -o ro src mnt",
		MountPoint: "/tmp/mnt",
		WriteError: "read-only file system",
	})
	report := volumeCapabilitiesWithProbe("darwin", "arm64")
	if !report.Probed || report.Probe == nil {
		t.Fatalf("report = %+v, want the probe record present", report)
	}
	if !report.ReadOnlySupported {
		t.Fatalf("report = %+v, want supported when the kernel mounted read-only and refused the write", report)
	}
	if !strings.Contains(report.Mechanism, "proven on this node") {
		t.Fatalf("mechanism = %q, want it to say the verdict was proven by a mount", report.Mechanism)
	}
	if report.ProbeStatus != "enforced" || report.Probe == nil || report.Probe.Status != "enforced" {
		t.Fatalf("report = %+v, want probe_status enforced in the report and the probe record", report)
	}

	// A helper that exists is not a kernel that mounts: the same node reports
	// refused once the probe says the mount was accepted but a write got through.
	useReadOnlyProbeSeam(t, readOnlyProbeRun{MountForm: "helper -o ro src mnt", MountPoint: "/tmp/mnt"})
	refused := volumeCapabilitiesWithProbe("darwin", "arm64")
	if refused.ReadOnlySupported {
		t.Fatalf("report = %+v, want refused when a write inside the mount was accepted", refused)
	}
	if refused.Mechanism != "none" {
		t.Fatalf("mechanism = %q, want none once the probe contradicts the helper", refused.Mechanism)
	}

	// On a macOS with no null filesystem the probe is not attempted: the OS fact
	// is enough, and no mount command is run.
	useNullfsBundle(t, false)
	useReadOnlyProbeSeam(t, readOnlyProbeRun{MountForm: "should not run"})
	blocked := volumeCapabilitiesWithProbe("darwin", "arm64")
	if blocked.Probe != nil {
		t.Fatalf("report = %+v, want no probe record when the OS blocks the mount", blocked)
	}
	if !strings.Contains(blocked.Reason, "probe not attempted") || !strings.Contains(blocked.Reason, "no null filesystem bundle") {
		t.Fatalf("reason = %q, want it to say the probe was skipped and why", blocked.Reason)
	}
	if blocked.ProbeStatus != "not_attempted_os" {
		t.Fatalf("probe_status = %q, want not_attempted_os so a gate can tell an OS blockage from a kernel refusal", blocked.ProbeStatus)
	}

	// A node whose user cannot mount anything must not be read as a node whose
	// kernel refused the mount.
	useNullfsBundle(t, true)
	useReadOnlyProbeSeam(t, readOnlyProbeRun{MountError: "mount src dst read-only: root or passwordless sudo is required"})
	unprivileged := volumeCapabilitiesWithProbe("darwin", "arm64")
	if unprivileged.ProbeStatus != "mount_not_attempted_privilege" {
		t.Fatalf("probe_status = %q, want the privilege failure classified apart from a kernel refusal (report %+v)", unprivileged.ProbeStatus, unprivileged)
	}
	if unprivileged.ReadOnlySupported {
		t.Fatalf("report = %+v, want an unprivileged probe to report refused", unprivileged)
	}
}

func TestVolumesCommandProbeFlagReportsTheProbeRecord(t *testing.T) {
	useNullfsBundle(t, true)
	t.Setenv("MACKER_MOUNT_NULLFS", "")
	// The command path asks about the host it is running on, so a Linux CI node
	// needs the documented seam to be allowed to reach the mount plumbing at all.
	t.Setenv("MACKER_TEST_ALLOW_NULLFS", "1")
	useReadOnlyProbeSeam(t, readOnlyProbeRun{
		MountForm:  "/sbin/mount_nullfs -o ro src mnt",
		MountPoint: "/tmp/mnt",
		WriteError: "read-only file system",
	})

	encoded := captureStdout(t, func() {
		if err := commandVolumes([]string{"--probe", "--format", "json"}); err != nil {
			t.Errorf("volumes --probe --format json: %v", err)
		}
	})
	var report volumeCapabilityReport
	if err := json.Unmarshal([]byte(encoded), &report); err != nil {
		t.Fatalf("json output is not a volumeCapabilityReport: %v (%q)", err, encoded)
	}
	if !report.Probed || report.Probe == nil || report.Probe.WriteError == "" {
		t.Fatalf("report = %+v, want the probe record in the json output", report)
	}
	if report.ProbeStatus != "enforced" {
		t.Fatalf("probe_status = %q, want enforced", report.ProbeStatus)
	}
	if !strings.Contains(encoded, `"probe_status": "enforced"`) || !strings.Contains(encoded, `"status": "enforced"`) {
		t.Fatalf("json output = %q, want probe_status and the probe record's status visible to a machine", encoded)
	}

	table := captureStdout(t, func() {
		if err := commandVolumes([]string{"--probe"}); err != nil {
			t.Errorf("volumes --probe: %v", err)
		}
	})
	for _, want := range []string{
		"read-only volumes: supported",
		"verdict source: probed on this node",
		"probe status: enforced",
		"probe mount: /sbin/mount_nullfs -o ro src mnt",
		"probe write inside the mount: refused with read-only file system",
	} {
		if !strings.Contains(table, want) {
			t.Fatalf("table output = %q, want it to contain %q", table, want)
		}
	}

	// Without --probe the command still answers the question, and says it did not
	// mount anything.
	plain := captureStdout(t, func() {
		if err := commandVolumes([]string{"--format", "json"}); err != nil {
			t.Errorf("volumes --format json: %v", err)
		}
	})
	var unprobed volumeCapabilityReport
	if err := json.Unmarshal([]byte(plain), &unprobed); err != nil {
		t.Fatalf("json output is not a volumeCapabilityReport: %v (%q)", err, plain)
	}
	if unprobed.Probed || unprobed.Probe != nil {
		t.Fatalf("report = %+v, want probed false and no probe record without --probe", unprobed)
	}
	if unprobed.ProbeStatus != "not_requested" {
		t.Fatalf("probe_status = %q, want not_requested when --probe was not passed", unprobed.ProbeStatus)
	}
}

func TestReadOnlyProbeRunnerIsTheRealMountProbe(t *testing.T) {
	if readOnlyProbeRunner == nil {
		t.Fatal("readOnlyProbeRunner must default to the real probe")
	}
}
