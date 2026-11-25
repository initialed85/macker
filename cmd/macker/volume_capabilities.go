// This file reports whether Macker can enforce a read-only host volume on the
// node it is running on, and by which mechanism. It deliberately uses only the
// Go standard library.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// nullfsBundlePath is where macOS keeps the bundle that mount(8) uses to mount a
// filesystem type. home-ops measured on icarus-2 (macOS 26.6.2, Darwin 25G83)
// that /Library/Filesystems/null.fs is gone, so mount(8) fails before it reaches
// the kernel:
//
//	mount: exec /Library/Filesystems/null.fs/Contents/Resources/mount_null for /private/tmp/mdst: No such file or directory
//	mount: /private/tmp/mdst failed with 72
//
// It is a variable so a test can point it at a directory that exists.
var nullfsBundlePath = "/Library/Filesystems/null.fs"

// nullfsBundleFound reports whether this node's OS still ships the bundle
// mount(8) needs for the null filesystem type.
func nullfsBundleFound() bool {
	info, err := os.Stat(nullfsBundlePath)
	return err == nil && info.IsDir()
}

// readOnlyMountBlockage returns the reason Macker cannot enforce a read-only
// volume on a given host OS, or an empty string when it may attempt the mount.
// It does not consult runtime.GOOS, so a test can ask the Darwin question on any
// node. MACKER_TEST_ALLOW_NULLFS is the documented test seam that lets Linux CI
// exercise mount plumbing without a Darwin kernel. The explicit helper override
// is exempt from the bundle check because a helper that calls mount(2) with the
// null filesystem type does not need mount(8)'s bundle.
func readOnlyMountBlockage(hostOS string) string {
	if hostOS != "darwin" {
		if nullfsAvailable() {
			return ""
		}
		return fmt.Sprintf("requires a Darwin node: current host is %s; Macker enforces :ro with a Darwin nullfs mount (mount_nullfs, or mount -t null) and rejects :ro rather than pretending to enforce it", hostOS)
	}
	if os.Getenv("MACKER_MOUNT_NULLFS") != "" {
		return ""
	}
	if nullfsBundleFound() {
		return ""
	}
	return fmt.Sprintf("this macOS has no null filesystem bundle at %s, so mount(8) has no helper for the null filesystem type and no mount_nullfs exists. Apple's own source for this Darwin generation still carries nullfs, but its mount entry point requires the private entitlement com.apple.private.nullfs_allow and has no superuser fallback, so a helper needs that entitlement as well as root - and on macOS 26.6.2 a calibrated mount(2) probe (devfs mounts through the same call form, apfs and hfs draw EINVAL for a bogus spec) returns ENODEV for the type null, the same class as a made-up filesystem name, so the built kernel does not register the vfs at all and the entitlement route is moot. Run `macker volumes --probe` on the node for a verdict from a mount the kernel accepted and a write it refused, point MACKER_MOUNT_NULLFS at a helper that calls mount(2) with the null filesystem type if this OS has one, or run this workload without :ro", nullfsBundlePath)
}

// volumeCapabilityReport answers, for a caller such as a node agent, whether a
// read-only host volume would actually be enforced on this node.
type volumeCapabilityReport struct {
	HostOS            string   `json:"host_os"`
	HostArch          string   `json:"host_arch"`
	ReadOnlySupported bool     `json:"read_only_supported"`
	Mechanism         string   `json:"mechanism"`
	Helpers           []string `json:"helpers"`
	MountForms        []string `json:"mount_forms"`
	SearchPaths       []string `json:"search_paths"`
	NullfsBundle      string   `json:"nullfs_bundle"`
	NullfsBundleFound bool     `json:"nullfs_bundle_found"`
	WritableMechanism string   `json:"writable_mechanism"`
	Reason            string   `json:"reason"`
	// Probe is only present when the command was asked to prove the capability
	// by mounting something, which is the only answer that cannot be a guess.
	Probed bool              `json:"probed"`
	Probe  *readOnlyProbeRun `json:"probe,omitempty"`
}

// readOnlyProbeRun is what actually happened when Macker mounted a throwaway
// directory read-only and then tried to write inside it. Three facts decide the
// capability: the mount was accepted, a write inside it was refused, and the
// mount was released again.
type readOnlyProbeRun struct {
	MountForm    string `json:"mount_form,omitempty"`
	MountPoint   string `json:"mount_point,omitempty"`
	MountError   string `json:"mount_error,omitempty"`
	WriteError   string `json:"write_error,omitempty"`
	UnmountError string `json:"unmount_error,omitempty"`
}

// enforced reports whether the probe proved the guarantee macker sells: a write
// inside a :ro volume fails. A mount that succeeds but still accepts writes is a
// refusal, not a success.
func (p readOnlyProbeRun) enforced() bool {
	return p.MountError == "" && p.WriteError != "" && p.UnmountError == ""
}

// reason explains a verdict in the terms a node operator has to act on.
func (p readOnlyProbeRun) reason() string {
	switch {
	case p.MountError != "":
		return fmt.Sprintf("probe mount was refused: %s", p.MountError)
	case p.WriteError == "":
		return fmt.Sprintf("probe mount %s succeeded, but a file written inside %s was accepted, so a read-only volume would not be enforced", p.MountForm, p.MountPoint)
	case p.UnmountError != "":
		return fmt.Sprintf("probe mount %s enforced read-only (%s), but releasing it failed: %s. Release it with `sudo %s %s` before removing %s",
			p.MountForm, p.WriteError, p.UnmountError, umountBinary(), p.MountPoint, p.MountPoint)
	default:
		return fmt.Sprintf("probe mount %s enforced read-only: writing inside %s failed with %s, and the mount was released",
			p.MountForm, p.MountPoint, p.WriteError)
	}
}

// volumeCapabilities builds the report for a host OS and architecture without
// mounting anything, so a caller can decide at admission time.
func volumeCapabilities(hostOS, hostArch string) volumeCapabilityReport {
	report := volumeCapabilityReport{
		HostOS:            hostOS,
		HostArch:          hostArch,
		Mechanism:         "none",
		MountForms:        []string{},
		SearchPaths:       append(append([]string{}, mountNullfsCandidatePaths...), mountHelperCandidatePaths...),
		NullfsBundle:      nullfsBundlePath,
		WritableMechanism: "live symlink to the host path, no mount required",
	}
	// Mount helpers are only consulted on Darwin: listing /bin/mount on a Linux
	// node would read as a capability the node cannot use.
	if hostOS == "darwin" {
		report.Helpers = append(resolveHelper(mountNullfsCandidatePaths, "mount_nullfs"), resolveHelper(mountHelperCandidatePaths, "mount")...)
	}
	if blockage := readOnlyMountBlockage(hostOS); blockage != "" {
		report.Reason = blockage
		if hostOS == "darwin" {
			report.NullfsBundleFound = nullfsBundleFound()
		}
		return report
	}
	report.NullfsBundleFound = nullfsBundleFound()
	attempts, err := nullfsMountAttempts("HOST", "CONTAINER")
	if err != nil {
		report.Reason = err.Error()
		return report
	}
	report.ReadOnlySupported = true
	report.Mechanism = "nullfs mount mounted read-only at a mount point inside the container rootfs"
	for _, attempt := range attempts {
		report.MountForms = append(report.MountForms, attempt.Binary+" "+strings.Join(attempt.Args, " "))
	}
	report.Reason = "read-only volumes are enforced with a kernel mount; the mount itself needs root or passwordless sudo and was not attempted by this command"
	return report
}

// readOnlyProbeRunner is the seam a test uses instead of asking a kernel to mount
// something; performReadOnlyMountProbe is the real one.
var readOnlyProbeRunner = performReadOnlyMountProbe

// performReadOnlyMountProbe mounts a temporary directory read-only over another
// temporary directory, writes a file inside the mount, and unmounts. The write is
// the point: it is the guarantee a workload depends on, and only the kernel can
// answer it. The source directory is only removed once nothing is mounted on it,
// so a failed unmount cannot delete files through a live mount.
func performReadOnlyMountProbe() readOnlyProbeRun {
	dir, err := os.MkdirTemp("", "macker-read-only-probe")
	if err != nil {
		return readOnlyProbeRun{MountError: fmt.Sprintf("create a temporary directory to mount: %v", err)}
	}
	source := filepath.Join(dir, "src")
	target := filepath.Join(dir, "mnt")
	if err := os.Mkdir(source, 0o755); err != nil {
		return readOnlyProbeRun{MountError: fmt.Sprintf("create the probe source directory: %v", err)}
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return readOnlyProbeRun{MountError: fmt.Sprintf("create the probe mount point: %v", err)}
	}

	attempts, err := nullfsMountAttempts(source, target)
	if err != nil {
		return readOnlyProbeRun{MountError: err.Error()}
	}
	var failures []string
	for _, attempt := range attempts {
		if _, err := runPrivileged(attempt.Binary, attempt.Args...); err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: %v", attempt.Binary, strings.Join(attempt.Args, " "), err))
			continue
		}
		probe := readOnlyProbeRun{
			MountForm:  attempt.Binary + " " + strings.Join(attempt.Args, " "),
			MountPoint: target,
		}
		probe.WriteError = probeWriteRefused(target)
		if _, err := runPrivileged(umountBinary(), target); err != nil {
			probe.UnmountError = err.Error()
			return probe
		}
		if err := os.RemoveAll(dir); err != nil {
			probe.UnmountError = fmt.Sprintf("mount released but removing the probe directory failed: %v", err)
		}
		return probe
	}
	_ = os.RemoveAll(dir)
	return readOnlyProbeRun{MountError: strings.Join(failures, "; ")}
}

// probeWriteRefused returns the error a write inside a mounted directory drew,
// or an empty string when the write was accepted.
func probeWriteRefused(target string) string {
	probeFile := filepath.Join(target, "macker-read-only-probe.txt")
	err := os.WriteFile(probeFile, []byte("macker read-only volume probe\n"), 0o644)
	if err == nil {
		_ = os.Remove(probeFile)
		return ""
	}
	return err.Error()
}

// volumeCapabilitiesWithProbe answers the same question as volumeCapabilities,
// but the verdict comes from a mount the kernel accepted and a write it refused,
// so a caller cannot be told a node supports :ro by the presence of a helper.
// The probe needs root or passwordless sudo, which is the same privilege a real
// :ro volume mount needs, so an unprivileged probe reports refused rather than
// guessing.
func volumeCapabilitiesWithProbe(hostOS, hostArch string) volumeCapabilityReport {
	report := volumeCapabilities(hostOS, hostArch)
	report.Probed = true
	if blockage := readOnlyMountBlockage(hostOS); blockage != "" {
		report.Reason = "probe not attempted: " + blockage
		return report
	}
	probe := readOnlyProbeRunner()
	report.Probe = &probe
	report.Reason = probe.reason()
	if probe.enforced() {
		report.ReadOnlySupported = true
		report.Mechanism = "nullfs mount mounted read-only at a mount point inside the container rootfs, proven on this node by a write that the kernel refused"
		return report
	}
	report.ReadOnlySupported = false
	report.Mechanism = "none"
	return report
}

func commandVolumes(args []string) error {
	fs := flag.NewFlagSet("volumes", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "table", "output format: table or json")
	probe := fs.Bool("probe", false, "prove the verdict by mounting a temporary directory read-only and writing inside it (needs root or passwordless sudo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("volumes takes no arguments; it reports what this node can enforce")
	}
	switch *format {
	case "table", "json":
	default:
		return fmt.Errorf("unknown format %q; use table or json", *format)
	}

	report := volumeCapabilities(runtime.GOOS, runtime.GOARCH)
	if *probe {
		report = volumeCapabilitiesWithProbe(runtime.GOOS, runtime.GOARCH)
	}
	if *format == "json" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	}
	// The first line is a stable marker a node agent can grep for; the rest is
	// for the human who has to fix the node.
	if report.ReadOnlySupported {
		fmt.Println("read-only volumes: supported")
	} else {
		fmt.Println("read-only volumes: refused")
	}
	if report.Probed {
		if report.Probe != nil {
			fmt.Println("verdict source: probed on this node (a mount the kernel accepted and a write it refused)")
		} else {
			fmt.Println("verdict source: --probe was requested and no mount was attempted (see reason)")
		}
	} else {
		fmt.Println("verdict source: reported without mounting anything; add --probe for proof")
	}
	fmt.Printf("host: %s/%s\n", report.HostOS, report.HostArch)
	fmt.Printf("mechanism: %s\n", report.Mechanism)
	fmt.Printf("nullfs bundle: %s (found: %t)\n", report.NullfsBundle, report.NullfsBundleFound)
	if len(report.Helpers) > 0 {
		fmt.Printf("mount helpers resolved: %s\n", strings.Join(report.Helpers, ", "))
	} else {
		fmt.Printf("mount helpers resolved: none (searched %s)\n", strings.Join(report.SearchPaths, ", "))
	}
	if len(report.MountForms) > 0 {
		fmt.Printf("mount forms tried: %s\n", strings.Join(report.MountForms, " | "))
	}
	if report.Probe != nil {
		if report.Probe.MountForm != "" {
			fmt.Printf("probe mount: %s\n", report.Probe.MountForm)
		}
		if report.Probe.MountPoint != "" {
			fmt.Printf("probe mount point: %s\n", report.Probe.MountPoint)
		}
		if report.Probe.WriteError != "" {
			fmt.Printf("probe write inside the mount: refused with %s\n", report.Probe.WriteError)
		}
		if report.Probe.UnmountError != "" {
			fmt.Printf("probe unmount: %s\n", report.Probe.UnmountError)
		}
	}
	fmt.Printf("writable volumes: %s\n", report.WritableMechanism)
	fmt.Printf("reason: %s\n", report.Reason)
	fmt.Println("overrides: MACKER_MOUNT_NULLFS, MACKER_UMOUNT")
	fmt.Println("forms: macker volumes | macker volumes --probe")
	return nil
}
