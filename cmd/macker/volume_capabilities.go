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
	return fmt.Sprintf("this macOS has no null filesystem bundle at %s, so mount(8) has no helper for the null filesystem type and no mount_nullfs exists. Apple's own source for this Darwin generation still carries nullfs, but its mount entry point requires the private entitlement com.apple.private.nullfs_allow and has no superuser fallback, so a helper needs that entitlement as well as root. Point MACKER_MOUNT_NULLFS at a helper that calls mount(2) with the null filesystem type and holds that entitlement (run Macker with root or passwordless sudo), or run this workload without :ro", nullfsBundlePath)
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

func commandVolumes(args []string) error {
	fs := flag.NewFlagSet("volumes", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "table", "output format: table or json")
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
	fmt.Printf("writable volumes: %s\n", report.WritableMechanism)
	fmt.Printf("reason: %s\n", report.Reason)
	fmt.Println("overrides: MACKER_MOUNT_NULLFS, MACKER_UMOUNT")
	return nil
}
