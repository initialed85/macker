// Command macker is a user-facing CLI for native Darwin OCI workloads. It
// deliberately supports a small, explicit subset of Docker's
// UX rather than pretending to provide Linux container semantics.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type stringList []string

func expandInteractiveShortFlags(args []string) []string {
	expanded := make([]string, 0, len(args)+2)
	afterSeparator := false
	for _, arg := range args {
		if afterSeparator {
			expanded = append(expanded, arg)
			continue
		}
		if arg == "--" {
			expanded = append(expanded, arg)
			afterSeparator = true
			continue
		}
		switch arg {
		case "-it":
			expanded = append(expanded, "-i", "-t")
		case "-ti":
			expanded = append(expanded, "-t", "-i")
		default:
			expanded = append(expanded, arg)
		}
	}
	return expanded
}

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type imageRef struct {
	Registry   string
	Repository string
	Tag        string
	Normalized string
}

type copyInstruction struct {
	Sources     []string
	Destination string
}

type copySource struct {
	Path         string
	CopyContents bool
}

type buildStep struct {
	Copy *copyInstruction
	Run  string
}

type buildSpec struct {
	Steps       []buildStep
	Copies      []copyInstruction
	Env         []string
	Entrypoint  []string
	Cmd         []string
	WorkingDir  string
	FromScratch bool
}

type volume struct {
	HostPath      string
	ContainerPath string
}

type portMapping struct {
	HostPort uint16 `json:"host_port"`
	NodePort uint16 `json:"node_port"`
	Protocol string `json:"protocol"`
}

type pfPortState struct {
	Anchor string
	Token  string
}

type containerMetadata struct {
	Name          string         `json:"name"`
	Image         string         `json:"image"`
	Network       string         `json:"network"`
	Volumes       []volume       `json:"volumes,omitempty"`
	Ports         []portMapping  `json:"ports,omitempty"`
	PFAnchor      string         `json:"pf_anchor,omitempty"`
	PFToken       string         `json:"pf_token,omitempty"`
	NetworkConfig *networkConfig `json:"network_config,omitempty"`
	AutoRemove    bool           `json:"auto_remove,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	PID           int            `json:"pid,omitempty"`
	LogPath       string         `json:"log_path,omitempty"`
	StoppedAt     *time.Time     `json:"stopped_at,omitempty"`
}

type imageRecord struct {
	Reference string    `json:"reference"`
	CreatedAt time.Time `json:"created_at"`
}

type mackerState struct {
	Version    int                    `json:"version"`
	Containers map[string]struct{}    `json:"containers"`
	Images     map[string]imageRecord `json:"images"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "build":
		err = commandBuild(os.Args[2:])
	case "oci":
		err = commandOCI(os.Args[2:])
	case "push":
		err = commandPush(os.Args[2:])
	case "pull":
		err = commandPull(os.Args[2:])
	case "bundle":
		err = commandBundle(os.Args[2:])
	case "run":
		err = commandRun(os.Args[2:])
	case "exec":
		err = commandExec(os.Args[2:])
	case "logs":
		err = commandLogs(os.Args[2:])
	case "stop":
		err = commandStop(os.Args[2:])
	case "ps":
		err = commandPS(os.Args[2:])
	case "rm":
		err = commandRM(os.Args[2:])
	case "images":
		err = commandImages(os.Args[2:])
	case "rmi":
		err = commandRMI(os.Args[2:])
	case "help", "-h", "--help":

		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `macker - native macOS OCI workload tool

Usage:
  macker build -f Mackerfile -t IMAGE CONTEXT
  macker oci <build|inspect|unpack|run> [flags]
  macker push IMAGE
  macker pull IMAGE
  macker bundle [--no-push] SOURCE DARWIN-IMAGE
  macker run [-d|--detach] [--rm] [-i|--interactive] [-t|--tty] --net=host|external --name=NAME [--interface IFACE --ip POD_IP] [--host-interface IFACE --host-ip HOST_IP] [-v HOST:CONTAINER] [-p HOST_PORT:NODE_PORT[/tcp|/udp]] [--entrypoint COMMAND] IMAGE [-- COMMAND ARG...]
  macker exec [-i|--interactive] [-t|--tty] NAME [-- COMMAND ARG...]
  macker logs [-f|--follow] NAME
  macker stop NAME
  macker rm [-f|--force] NAME
  macker ps [-a|--all]
  macker images
  macker rmi IMAGE

Storage defaults to ~/.macker. Set MACKER_HOME to use another location.
Only FROM scratch images and host or explicitly attached external networking are supported.`)
}

func commandBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	mackerfile := fs.String("f", "Mackerfile", "Mackerfile path")
	tag := fs.String("t", "", "image reference, for example initialed85/nginx-darwin:latest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("build requires exactly one context directory")
	}
	if *tag == "" {
		return errors.New("build requires -t IMAGE")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("macker builds Darwin images; current host is %q", runtime.GOOS)
	}

	ref, err := parseImageRef(*tag)
	if err != nil {
		return err
	}
	contextDir, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("resolve context: %w", err)
	}
	contextInfo, err := os.Stat(contextDir)
	if err != nil {
		return fmt.Errorf("stat context: %w", err)
	}
	if !contextInfo.IsDir() {
		return fmt.Errorf("context %q is not a directory", contextDir)
	}
	mackerfilePath, err := filepath.Abs(*mackerfile)
	if err != nil {
		return fmt.Errorf("resolve Mackerfile: %w", err)
	}
	spec, err := parseMackerfile(mackerfilePath)
	if err != nil {
		return err
	}

	home, err := mackerHome()
	if err != nil {
		return err
	}
	if err := ensureStorage(home); err != nil {
		return err
	}
	buildRoot, err := os.MkdirTemp(filepath.Join(home, "tmp"), "build-")
	if err != nil {
		return fmt.Errorf("create build rootfs: %w", err)
	}
	defer os.RemoveAll(buildRoot)

	for _, step := range spec.Steps {
		switch {
		case step.Run != "":
			if err := runBuildCommand(contextDir, buildRoot, step.Run, spec.Env); err != nil {
				return fmt.Errorf("RUN: %w", err)
			}
		case step.Copy != nil:
			if err := applyCopyInstruction(contextDir, buildRoot, *step.Copy); err != nil {
				return fmt.Errorf("COPY: %w", err)
			}
		}
	}

	layout := imageLayoutPath(home, ref)
	if err := os.RemoveAll(layout); err != nil {
		return fmt.Errorf("replace image layout: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout), 0o755); err != nil {
		return fmt.Errorf("create image storage: %w", err)
	}

	entrypoint, imageArgs, err := commandForSpec(spec)
	if err != nil {
		return err
	}
	if err := buildImage(buildOptions{
		RootFS:       buildRoot,
		Output:       layout,
		Tag:          ref.Tag,
		Architecture: runtime.GOARCH,
		Entrypoint:   entrypoint,
		Args:         imageArgs,
		Env:          spec.Env,
		WorkingDir:   spec.WorkingDir,
	}); err != nil {
		_ = os.RemoveAll(layout)
		return err
	}
	if err := registerImageState(home, ref, layout); err != nil {
		return err
	}
	fmt.Printf("stored %s at %s\n", ref.Normalized, layout)
	return nil
}

func commandPush(args []string) error {
	if len(args) != 1 {
		return errors.New("push requires exactly one image reference")
	}
	ref, err := parseImageRef(args[0])
	if err != nil {
		return err
	}
	home, err := mackerHome()
	if err != nil {
		return err
	}
	layout := imageLayoutPath(home, ref)
	if err := requireLayout(layout); err != nil {
		return fmt.Errorf("push %s: %w", ref.Normalized, err)
	}
	skopeo, err := skopeoBinary()
	if err != nil {
		return err
	}
	pushSource, cleanup, err := bundleSourceForPush(home, layout, ref.Tag)
	if err != nil {
		return err
	}
	defer cleanup()
	return runCommand(skopeo,
		"copy",
		"--all",
		"--format", "oci",
		"--preserve-digests",
		pushSource,
		"docker://"+ref.Normalized,
	)
}

func commandPull(args []string) error {
	if len(args) != 1 {
		return errors.New("pull requires exactly one image reference")
	}
	ref, err := parseImageRef(args[0])
	if err != nil {
		return err
	}
	home, err := mackerHome()
	if err != nil {
		return err
	}
	if err := ensureStorage(home); err != nil {
		return err
	}
	layout := imageLayoutPath(home, ref)
	staging := layout + ".pulling"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove old pull staging: %w", err)
	}
	skopeo, err := skopeoBinary()
	if err != nil {
		return err
	}
	if err := runCommand(skopeo,
		"--override-os", "darwin",
		"--override-arch", runtime.GOARCH,
		"copy",
		"--format", "oci",
		"--dest-oci-accept-uncompressed-layers",
		"docker://"+ref.Normalized,
		"oci:"+staging+":"+ref.Tag,
	); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.RemoveAll(layout); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("replace pulled image: %w", err)
	}
	if err := os.Rename(staging, layout); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("install pulled image: %w", err)
	}
	if err := registerImageState(home, ref, layout); err != nil {
		return err
	}
	fmt.Printf("stored %s at %s\n", ref.Normalized, layout)
	return nil
}

// commandBundle combines all platform manifests from SOURCE with the local
// darwin/arm64 image stored under DARWIN-IMAGE. The Darwin image reference is
// also the output reference; the merged image is pushed there by default.
func commandBundle(args []string) error {
	fs := flag.NewFlagSet("bundle", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	noPush := fs.Bool("no-push", false, "keep the merged image local without pushing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("bundle requires SOURCE and DARWIN-IMAGE")
	}

	sourceRef, err := parseImageRef(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("source image: %w", err)
	}
	darwinRef, err := parseImageRef(fs.Arg(1))
	if err != nil {
		return fmt.Errorf("Darwin image: %w", err)
	}

	home, err := mackerHome()
	if err != nil {
		return err
	}
	if err := ensureStorage(home); err != nil {
		return err
	}
	darwinLayout := imageLayoutPath(home, darwinRef)
	if err := requireLayout(darwinLayout); err != nil {
		return fmt.Errorf("local Darwin image %s: %w; build or pull it first", darwinRef.Normalized, err)
	}

	sourceLayout, cleanup, err := bundleSourceLayout(home, sourceRef)
	if err != nil {
		return err
	}
	defer cleanup()
	if filepath.Clean(sourceLayout) == filepath.Clean(darwinLayout) {
		return errors.New("bundle source and Darwin image resolve to the same local layout")
	}

	staging := filepath.Join(home, "tmp", fmt.Sprintf("bundle-%d", time.Now().UnixNano()))
	defer os.RemoveAll(staging)
	if err := mergeBundleLayouts(sourceLayout, darwinLayout, staging, darwinRef.Tag); err != nil {
		return err
	}
	if err := replaceBundleLayout(darwinLayout, staging); err != nil {
		return err
	}
	if err := registerImageState(home, darwinRef, darwinLayout); err != nil {
		return err
	}

	if !*noPush {
		skopeo, err := skopeoBinary()
		if err != nil {
			return err
		}
		pushSource, cleanup, err := bundleSourceForPush(home, darwinLayout, darwinRef.Tag)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := runCommand(skopeo,
			"copy",
			"--all",
			"--format", "oci",
			"--preserve-digests",
			pushSource,
			"docker://"+darwinRef.Normalized,
		); err != nil {
			return fmt.Errorf("push bundled image: %w", err)
		}
	}

	if *noPush {
		fmt.Printf("bundled %s into %s locally\n", sourceRef.Normalized, darwinRef.Normalized)
	} else {
		fmt.Printf("bundled %s into and pushed %s\n", sourceRef.Normalized, darwinRef.Normalized)
	}
	return nil
}

func bundleSourceLayout(home string, ref imageRef) (string, func(), error) {
	local := imageLayoutPath(home, ref)
	if _, err := os.Stat(local); err == nil {
		if err := requireLayout(local); err != nil {
			return "", func() {}, fmt.Errorf("local source image %s: %w", ref.Normalized, err)
		}
		return local, func() {}, nil
	} else if !os.IsNotExist(err) {
		return "", func() {}, fmt.Errorf("inspect local source image %s: %w", ref.Normalized, err)
	}

	staging := filepath.Join(home, "tmp", fmt.Sprintf("bundle-source-%d", time.Now().UnixNano()))
	skopeo, err := skopeoBinary()
	if err != nil {
		return "", func() {}, err
	}
	if err := runCommand(skopeo,
		"copy",
		"--all",
		"--format", "oci",
		"--dest-oci-accept-uncompressed-layers",
		"docker://"+ref.Normalized,
		"oci:"+staging+":"+ref.Tag,
	); err != nil {
		_ = os.RemoveAll(staging)
		return "", func() {}, fmt.Errorf("pull bundle source %s: %w", ref.Normalized, err)
	}
	return staging, func() { _ = os.RemoveAll(staging) }, nil
}

func mergeBundleLayouts(source, darwin, output, tag string) error {
	sourceIndex, err := loadBundleIndex(source)
	if err != nil {
		return fmt.Errorf("read bundle source: %w", err)
	}
	darwinIndex, err := loadBundleIndex(darwin)
	if err != nil {
		return fmt.Errorf("read local Darwin image: %w", err)
	}
	darwinDescriptors, err := collectDarwinDescriptors(darwin, darwinIndex)
	if err != nil {
		return fmt.Errorf("find Darwin manifests: %w", err)
	}
	if len(darwinDescriptors) == 0 {
		return errors.New("local Darwin image contains no darwin/arm64 manifest")
	}

	if err := os.MkdirAll(filepath.Join(output, "blobs", "sha256"), 0o755); err != nil {
		return fmt.Errorf("create bundle output: %w", err)
	}
	if err := copyBundleBlobTree(source, output); err != nil {
		return fmt.Errorf("copy source blobs: %w", err)
	}
	if err := copyBundleBlobTree(darwin, output); err != nil {
		return fmt.Errorf("copy Darwin blobs: %w", err)
	}

	merged, err := mergeBundleIndices(sourceIndex, darwinDescriptors, source, tag)
	if err != nil {
		return err
	}
	if err := writeBundleJSON(filepath.Join(output, "oci-layout"), map[string]string{
		"imageLayoutVersion": "1.0.0",
	}); err != nil {
		return fmt.Errorf("write bundle oci-layout: %w", err)
	}
	if err := writeBundleJSON(filepath.Join(output, "index.json"), merged); err != nil {
		return fmt.Errorf("write bundle index: %w", err)
	}
	return nil
}

func mergeBundleIndices(source bundleIndex, darwinDescriptors []bundleDescriptor, sourceLayout, tag string) (bundleIndex, error) {
	if source.SchemaVersion == 0 {
		source.SchemaVersion = 2
	}
	if source.SchemaVersion != 2 {
		return bundleIndex{}, fmt.Errorf("unsupported source OCI index schema version %d", source.SchemaVersion)
	}
	if len(source.Manifests) == 0 {
		return bundleIndex{}, errors.New("bundle source contains no manifests")
	}

	sourceDescriptors, err := flattenBundleDescriptors(sourceLayout, source.Manifests)
	if err != nil {
		return bundleIndex{}, fmt.Errorf("flatten source manifests: %w", err)
	}
	merged := bundleIndex{
		SchemaVersion: source.SchemaVersion,
		MediaType:     source.MediaType,
		Manifests:     make([]bundleDescriptor, 0, len(sourceDescriptors)+len(darwinDescriptors)),
	}
	if merged.MediaType == "" {
		merged.MediaType = "application/vnd.oci.image.index.v1+json"
	}
	for _, descriptor := range sourceDescriptors {
		platform, known, err := bundleDescriptorPlatform(sourceLayout, descriptor)
		if err != nil {
			return bundleIndex{}, fmt.Errorf("inspect source manifest %s: %w", descriptor.Digest, err)
		}
		if known {
			descriptor.Platform = &platform
			if platform.OS == "darwin" && platform.Architecture == "arm64" {
				// The explicitly supplied local Darwin image wins over an old
				// darwin/arm64 entry in the source image.
				continue
			}
		}
		if !bundleDescriptorPresent(merged.Manifests, descriptor) {
			merged.Manifests = append(merged.Manifests, descriptor)
		}
	}
	for _, descriptor := range darwinDescriptors {
		if !bundleDescriptorPresent(merged.Manifests, descriptor) {
			merged.Manifests = append(merged.Manifests, descriptor)
		}
	}
	if len(merged.Manifests) == 0 {
		return bundleIndex{}, errors.New("bundle produced no manifests")
	}

	// OCI layout tags are annotations on the root descriptor. Keep one
	// deterministic ref.name annotation for the resulting multi-platform tag.
	for i := range merged.Manifests {
		if merged.Manifests[i].Annotations != nil {
			delete(merged.Manifests[i].Annotations, "org.opencontainers.image.ref.name")
		}
	}
	if merged.Manifests[0].Annotations == nil {
		merged.Manifests[0].Annotations = make(map[string]string)
	}
	merged.Manifests[0].Annotations["org.opencontainers.image.ref.name"] = tag
	return merged, nil
}

func loadBundleIndex(layout string) (bundleIndex, error) {
	var index bundleIndex
	data, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		return index, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, err
	}
	if index.SchemaVersion != 0 && index.SchemaVersion != 2 {
		return index, fmt.Errorf("unsupported OCI index schema version %d", index.SchemaVersion)
	}
	return index, nil
}

// Skopeo's OCI transport treats a root index with multiple descriptors as an
// ambiguous collection unless it has a tagged descriptor pointing to one
// image. Wrap a multi-platform layout in a tagged index descriptor for
// registry transfers, while keeping the local layout flat for the local OCI
// commands and macker run.
func bundleSourceForPush(home, layout, tag string) (string, func(), error) {
	index, err := loadBundleIndex(layout)
	if err != nil {
		return "", func() {}, fmt.Errorf("read image index for push: %w", err)
	}
	if len(index.Manifests) <= 1 {
		return "oci:" + layout, func() {}, nil
	}

	staging := filepath.Join(home, "tmp", fmt.Sprintf("push-%d", time.Now().UnixNano()))
	cleanup := func() { _ = os.RemoveAll(staging) }
	if err := os.MkdirAll(filepath.Join(staging, "blobs", "sha256"), 0o755); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("create push staging: %w", err)
	}
	if err := copyBundleBlobTree(layout, staging); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("copy image blobs for push: %w", err)
	}

	inner, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("encode image index for push: %w", err)
	}
	inner = append(inner, '\n')
	hash := sha256.Sum256(inner)
	innerDigest := "sha256:" + hex.EncodeToString(hash[:])
	innerPath, err := bundleBlobPath(staging, innerDigest)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := os.WriteFile(innerPath, inner, 0o644); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write image index for push: %w", err)
	}

	root := bundleIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []bundleDescriptor{{
			MediaType: "application/vnd.oci.image.index.v1+json",
			Digest:    innerDigest,
			Size:      int64(len(inner)),
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": tag,
			},
		}},
	}
	if err := writeBundleJSON(filepath.Join(staging, "oci-layout"), map[string]string{
		"imageLayoutVersion": "1.0.0",
	}); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write push oci-layout: %w", err)
	}
	if err := writeBundleJSON(filepath.Join(staging, "index.json"), root); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write push index: %w", err)
	}
	return "oci:" + staging + ":" + tag, cleanup, nil
}

func flattenBundleDescriptors(layout string, descriptors []bundleDescriptor) ([]bundleDescriptor, error) {
	var result []bundleDescriptor
	for _, descriptor := range descriptors {
		// A platform-bearing descriptor is already a leaf image manifest for
		// our purposes. This also keeps layout tests and registries that carry
		// platform metadata on a descriptor from requiring another blob read.
		if descriptor.Platform != nil {
			result = append(result, descriptor)
			continue
		}
		isIndex, err := bundleDescriptorIsIndex(layout, descriptor)
		if err != nil {
			return nil, err
		}
		if isIndex {
			data, err := readBundleBlob(layout, descriptor.Digest)
			if err != nil {
				return nil, err
			}
			var nested bundleIndex
			if err := json.Unmarshal(data, &nested); err != nil {
				return nil, fmt.Errorf("decode nested OCI index %s: %w", descriptor.Digest, err)
			}
			nestedDescriptors, err := flattenBundleDescriptors(layout, nested.Manifests)
			if err != nil {
				return nil, err
			}
			result = append(result, nestedDescriptors...)
			continue
		}
		platform, known, err := bundleDescriptorPlatform(layout, descriptor)
		if err != nil {
			return nil, err
		}
		if known {
			descriptor.Platform = &platform
		}
		result = append(result, descriptor)
	}
	return result, nil
}

func collectDarwinDescriptors(layout string, index bundleIndex) ([]bundleDescriptor, error) {
	flattened, err := flattenBundleDescriptors(layout, index.Manifests)
	if err != nil {
		return nil, err
	}
	var result []bundleDescriptor
	for _, descriptor := range flattened {
		if descriptor.Platform != nil && descriptor.Platform.OS == "darwin" && descriptor.Platform.Architecture == "arm64" {
			result = append(result, descriptor)
		}
	}
	return result, nil
}

func bundleDescriptorPlatform(layout string, descriptor bundleDescriptor) (bundlePlatform, bool, error) {
	if descriptor.Platform != nil {
		return *descriptor.Platform, true, nil
	}
	data, err := readBundleBlob(layout, descriptor.Digest)
	if err != nil {
		return bundlePlatform{}, false, err
	}
	var manifest struct {
		Config    bundleDescriptor   `json:"config"`
		Manifests []bundleDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return bundlePlatform{}, false, err
	}
	if len(manifest.Manifests) > 0 || manifest.Config.Digest == "" {
		return bundlePlatform{}, false, nil
	}
	configData, err := readBundleBlob(layout, manifest.Config.Digest)
	if err != nil {
		return bundlePlatform{}, false, err
	}
	var config struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	if err := json.Unmarshal(configData, &config); err != nil {
		return bundlePlatform{}, false, err
	}
	if config.OS == "" || config.Architecture == "" {
		return bundlePlatform{}, false, nil
	}
	return bundlePlatform{OS: config.OS, Architecture: config.Architecture}, true, nil
}

func bundleDescriptorIsIndex(layout string, descriptor bundleDescriptor) (bool, error) {
	if strings.Contains(descriptor.MediaType, "image.index") {
		return true, nil
	}
	data, err := readBundleBlob(layout, descriptor.Digest)
	if err != nil {
		return false, err
	}
	var probe struct {
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false, nil
	}
	return len(probe.Manifests) > 0, nil
}

func bundleDescriptorPresent(descriptors []bundleDescriptor, candidate bundleDescriptor) bool {
	for _, descriptor := range descriptors {
		if descriptor.Digest == candidate.Digest {
			return true
		}
	}
	return false
}

func readBundleBlob(layout, digest string) ([]byte, error) {
	blob, err := bundleBlobPath(layout, digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(blob)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(hash[:]) != digest {
		return nil, fmt.Errorf("blob %s has invalid digest", digest)
	}
	return data, nil
}

func bundleBlobPath(layout, digest string) (string, error) {
	algorithm, encoded, ok := strings.Cut(digest, ":")
	if !ok || algorithm != "sha256" || len(encoded) != sha256.Size*2 {
		return "", fmt.Errorf("unsupported blob digest %q", digest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", fmt.Errorf("invalid blob digest %q: %w", digest, err)
	}
	return filepath.Join(layout, "blobs", "sha256", encoded), nil
}

func copyBundleBlobTree(source, output string) error {
	entries, err := os.ReadDir(filepath.Join(source, "blobs", "sha256"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || len(entry.Name()) != sha256.Size*2 {
			continue
		}
		digest := "sha256:" + entry.Name()
		sourcePath, err := bundleBlobPath(source, digest)
		if err != nil {
			return err
		}
		if err := validateBundleBlobFile(sourcePath, digest); err != nil {
			return err
		}
		destinationPath, err := bundleBlobPath(output, digest)
		if err != nil {
			return err
		}
		if _, err := os.Stat(destinationPath); err == nil {
			if err := validateBundleBlobFile(destinationPath, digest); err != nil {
				return err
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := copyBundleFile(sourcePath, destinationPath); err != nil {
			return err
		}
	}
	return nil
}

func validateBundleBlobFile(filename, digest string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("blob %s has invalid digest", digest)
	}
	return nil
}

func copyBundleFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	return nil
}

func writeBundleJSON(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, append(data, '\n'), 0o644)
}

func replaceBundleLayout(target, staging string) error {
	backup := target + ".before-bundle"
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove old bundle backup: %w", err)
	}
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("stage existing image layout: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Rename(backup, target)
		return fmt.Errorf("install bundled image layout: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove old image layout: %w", err)
	}
	return nil
}

func commandRun(args []string) error {
	args = expandInteractiveShortFlags(args)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	network := fs.String("net", "", "network mode: host or external")
	interfaceName := fs.String("interface", "", "existing external network interface (required with --net=external)")
	podIP := fs.String("ip", "", "existing IPv4 Pod IP on --interface (required with --net=external)")
	hostInterface := fs.String("host-interface", "", "existing external network host interface (optional with --net=external)")
	hostIP := fs.String("host-ip", "", "existing IPv4 external network host IP (optional with --net=external)")
	name := fs.String("name", "", "required container name")
	detach := fs.Bool("d", false, "run in the background")
	autoRemove := fs.Bool("rm", false, "remove the container after it exits")
	interactive := fs.Bool("i", false, "keep standard input open")
	tty := fs.Bool("t", false, "attach the caller's terminal")
	fs.BoolVar(detach, "detach", false, "run in the background")
	fs.BoolVar(interactive, "interactive", false, "keep standard input open")
	fs.BoolVar(tty, "tty", false, "attach the caller's terminal")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	var rawVolumes stringList
	fs.Var(&rawVolumes, "v", "host path and container path, HOST:CONTAINER (repeatable)")
	var rawPorts stringList
	fs.Var(&rawPorts, "p", "publish HOST_PORT:NODE_PORT[/tcp|/udp] (repeatable)")
	fs.Var(&rawPorts, "publish", "publish HOST_PORT:NODE_PORT[/tcp|/udp] (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *network {
	case "host":
		if *interfaceName != "" || *podIP != "" || *hostInterface != "" || *hostIP != "" {
			return errors.New("--interface, --ip, --host-interface, and --host-ip require --net=external")
		}
	case "external":
		if *interfaceName == "" || *podIP == "" {
			return errors.New("--net=external requires --interface IFACE and --ip POD_IP")
		}
		if (*hostInterface == "") != (*hostIP == "") {
			return errors.New("--net=external requires both --host-interface IFACE and --host-ip HOST_IP when either is provided")
		}
	default:
		return errors.New("run requires --net=host or --net=external")
	}
	if *detach && (*interactive || *tty) {
		return errors.New("interactive and tty runs cannot be detached")
	}
	if *name == "" {
		return errors.New("run requires --name NAME")
	}
	if !validContainerName(*name) {
		return fmt.Errorf("invalid container name %q", *name)
	}
	if fs.NArg() < 1 {
		return errors.New("run requires an image reference")
	}
	ref, err := parseImageRef(fs.Arg(0))
	if err != nil {
		return err
	}
	commandOverride := fs.Args()[1:]
	if len(commandOverride) > 0 && commandOverride[0] == "--" {
		commandOverride = commandOverride[1:]
	}
	volumes := make([]volume, 0, len(rawVolumes))
	for _, raw := range rawVolumes {
		parsed, err := parseVolume(raw)
		if err != nil {
			return err
		}
		volumes = append(volumes, parsed)
	}
	ports := make([]portMapping, 0, len(rawPorts))
	seenPorts := make(map[string]struct{}, len(rawPorts))
	for _, raw := range rawPorts {
		parsed, err := parsePortMapping(raw)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%d/%s", parsed.HostPort, parsed.Protocol)
		if _, exists := seenPorts[key]; exists {
			return fmt.Errorf("duplicate published port %q", raw)
		}
		seenPorts[key] = struct{}{}
		ports = append(ports, parsed)
	}

	home, err := mackerHome()
	if err != nil {
		return err
	}
	if err := checkPublishedPortConflicts(home, *name, ports); err != nil {
		return err
	}
	layout := imageLayoutPath(home, ref)
	if err := requireLayout(layout); err != nil {
		return fmt.Errorf("run %s: %w; pull or build it first", ref.Normalized, err)
	}
	containerDir := filepath.Join(home, "containers", *name)
	if err := os.MkdirAll(filepath.Dir(containerDir), 0o755); err != nil {
		return fmt.Errorf("create container storage: %w", err)
	}
	if err := os.Mkdir(containerDir, 0o755); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("container %q already exists", *name)
		}
		return fmt.Errorf("create container %q: %w", *name, err)
	}
	rootfs := filepath.Join(containerDir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return fmt.Errorf("create container rootfs: %w", err)
	}
	setupComplete := false
	var pfState pfPortState
	pfInstalled := false
	var networkCfg networkConfig
	networkInstalled := false
	defer func() {
		if pfInstalled {
			_ = cleanupPFPortMappings(pfState)
		}
		if networkInstalled {
			_ = cleanupMackerNetwork(home, *name, networkCfg)
		}
		if !setupComplete {
			_ = unregisterContainerState(home, *name)
			_ = os.RemoveAll(containerDir)
		}
	}()

	if *network == "external" {
		networkCfg, err = setupExternalMackerNetwork(*interfaceName, *podIP, *hostInterface, *hostIP)
	} else {
		networkCfg, err = setupMackerNetwork(home)
	}
	if err != nil {
		return err
	}
	networkInstalled = true

	if err := commandOCIUnpack([]string{"--force", "--output", rootfs, layout}); err != nil {
		return err
	}
	if replacedFiles, err := substituteMackerConfig(rootfs, networkEnvironmentValues(networkCfg, ports)); err != nil {
		return fmt.Errorf("configure container rootfs: %w", err)
	} else if replacedFiles > 0 {
		fmt.Fprintf(os.Stderr, "substituted Macker tokens in %d config file(s)\n", replacedFiles)
	}
	for _, mount := range volumes {
		if err := installVolume(rootfs, mount); err != nil {
			return err
		}
	}
	if len(ports) > 0 {
		pfState, err = installPFPortMappings(*name, networkCfg.IP, ports)
		if err != nil {
			return err
		}
		pfInstalled = true
	}
	metadata := containerMetadata{
		Name:          *name,
		Image:         ref.Normalized,
		Network:       *network,
		Volumes:       volumes,
		Ports:         ports,
		PFAnchor:      pfState.Anchor,
		PFToken:       pfState.Token,
		NetworkConfig: &networkCfg,
		AutoRemove:    *autoRemove,
		CreatedAt:     time.Now().UTC(),
	}
	ociRunArgs := []string{"--skip-unpack", "--rootfs", rootfs}
	if *interactive {
		ociRunArgs = append(ociRunArgs, "--interactive")
	}
	if *tty {
		ociRunArgs = append(ociRunArgs, "--tty")
	}
	ociRunArgs = append(ociRunArgs, networkEnvironmentArgs(networkCfg, ports)...)
	if *entrypoint != "" {
		ociRunArgs = append(ociRunArgs, "--entrypoint", *entrypoint)
	}
	ociRunArgs = append(ociRunArgs, layout)
	if len(commandOverride) > 0 {
		ociRunArgs = append(ociRunArgs, "--")
		ociRunArgs = append(ociRunArgs, commandOverride...)
	}

	if *detach {
		metadata.LogPath = filepath.Join(containerDir, "run.log")
		if err := writeContainerMetadata(containerDir, metadata); err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve macker executable: %w", err)
		}
		childArgs := append([]string{"oci", "run"}, ociRunArgs...)
		pid, err := runDetached(executable, childArgs, metadata.LogPath)
		if err != nil {
			return err
		}
		metadata.PID = pid
		if err := writeContainerMetadata(containerDir, metadata); err != nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			return err
		}
		if err := registerContainerState(home, *name); err != nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			return err
		}
		setupComplete = true
		pfInstalled = false
		networkInstalled = false
		fmt.Printf("started container %s (pid %d), network: %s/%s, log: %s\n", *name, pid, networkCfg.Interface, networkCfg.IP, metadata.LogPath)
		return nil
	}

	if err := writeContainerMetadata(containerDir, metadata); err != nil {
		return err
	}
	if err := registerContainerState(home, *name); err != nil {
		return err
	}
	setupComplete = true
	runErr := commandOCIRun(ociRunArgs)
	pfInstalled = false
	networkInstalled = false
	metadata.PID = 0
	stoppedAt := time.Now().UTC()
	metadata.StoppedAt = &stoppedAt
	resourceCleanupErr := cleanupContainerResources(home, *name, &metadata)
	metadataWriteErr := writeContainerMetadata(containerDir, metadata)
	if metadata.AutoRemove && resourceCleanupErr == nil && metadataWriteErr == nil {
		removeErr := removeContainer(home, *name)
		if removeErr == nil {
			fmt.Printf("removed container %s\n", *name)
		}
		return errors.Join(runErr, removeErr)
	}
	return errors.Join(runErr, resourceCleanupErr, metadataWriteErr)
}

func commandExec(args []string) error {
	args = expandInteractiveShortFlags(args)
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	interactive := fs.Bool("i", false, "keep standard input open")
	tty := fs.Bool("t", false, "attach the caller's terminal")
	fs.BoolVar(interactive, "interactive", false, "keep standard input open")
	fs.BoolVar(tty, "tty", false, "attach the caller's terminal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("exec requires a container name")
	}
	name := fs.Arg(0)
	if !validContainerName(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	command := fs.Args()[1:]
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}

	home, err := mackerHome()
	if err != nil {
		return err
	}
	containerDir := filepath.Join(home, "containers", name)
	metadataBytes, err := os.ReadFile(filepath.Join(containerDir, "metadata.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("container %q was not found", name)
		}
		return fmt.Errorf("read container metadata: %w", err)
	}
	var metadata containerMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("decode container metadata: %w", err)
	}
	status, err := containerStatus(metadata)
	if err != nil {
		return fmt.Errorf("inspect container %q: %w", name, err)
	}
	if status != "running" {
		return fmt.Errorf("container %q is not running", name)
	}
	rootfs := filepath.Join(containerDir, "rootfs")
	rootfsInfo, err := os.Lstat(rootfs)
	if err != nil {
		return fmt.Errorf("stat container %q rootfs: %w", name, err)
	}
	if !rootfsInfo.IsDir() || rootfsInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("container %q rootfs is not a normal directory", name)
	}
	ref, err := parseImageRef(metadata.Image)
	if err != nil {
		return fmt.Errorf("parse container image: %w", err)
	}
	layout := imageLayoutPath(home, ref)
	if err := requireLayout(layout); err != nil {
		return fmt.Errorf("exec %s: %w", ref.Normalized, err)
	}

	execDir := filepath.Join(containerDir, "exec")
	if err := os.MkdirAll(execDir, 0o755); err != nil {
		return fmt.Errorf("create container exec directory: %w", err)
	}
	pidFile := filepath.Join(execDir, strconv.Itoa(os.Getpid())+".pid")
	ociArgs := []string{"--skip-unpack", "--rootfs", rootfs, "--host-fallback", "--pid-file", pidFile}
	if *interactive {
		ociArgs = append(ociArgs, "--interactive")
	}
	if *tty {
		ociArgs = append(ociArgs, "--tty")
	}
	if metadata.NetworkConfig != nil {
		ociArgs = append(ociArgs, networkEnvironmentArgs(*metadata.NetworkConfig, metadata.Ports)...)
	}
	ociArgs = append(ociArgs, layout, "--")
	ociArgs = append(ociArgs, command...)
	return commandOCIRun(ociArgs)
}

func stopExecProcesses(containerDir string) error {
	execDir := filepath.Join(containerDir, "exec")
	entries, err := os.ReadDir(execDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read exec processes: %w", err)
	}
	var cleanupErrs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		pidFile := filepath.Join(execDir, entry.Name())
		data, err := os.ReadFile(pidFile)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("read exec PID file %s: %w", entry.Name(), err))
			continue
		}
		fields := strings.Fields(string(data))
		if len(fields) != 2 && len(fields) != 3 {
			_ = os.Remove(pidFile)
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ownerPID, ownerErr := strconv.Atoi(fields[1])
		processGroup := true
		if len(fields) == 3 {
			groupValue, groupErr := strconv.Atoi(fields[2])
			if groupErr != nil || (groupValue != 0 && groupValue != 1) {
				_ = os.Remove(pidFile)
				continue
			}
			processGroup = groupValue == 1
		}
		if pidErr != nil || ownerErr != nil || pid <= 0 || ownerPID <= 0 {
			_ = os.Remove(pidFile)
			continue
		}
		parentPID, parentErr := processParentPID(pid)
		if parentErr != nil || parentPID != ownerPID {
			_ = os.Remove(pidFile)
			continue
		}
		alive, err := processAlive(pid)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("check exec process %d: %w", pid, err))
			continue
		}
		if alive {
			target := pid
			if processGroup {
				target = -target
			}
			if err := syscall.Kill(target, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("signal exec process %d: %w", pid, err))
				continue
			}
			if err := waitForProcessExit(pid, 10*time.Second); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("stop exec process %d: %w", pid, err))
				continue
			}
		}
		if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove exec PID file %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(cleanupErrs...)
}

func processParentPID(pid int) (int, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
	if err != nil {
		return 0, err
	}
	parentPID, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("parse parent PID: %w", err)
	}
	return parentPID, nil
}

func commandLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	follow := fs.Bool("f", false, "follow log output")
	fs.BoolVar(follow, "follow", false, "follow log output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("logs requires exactly one container name")
	}
	name := fs.Arg(0)
	if !validContainerName(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	home, err := mackerHome()
	if err != nil {
		return err
	}
	containerDir := filepath.Join(home, "containers", name)
	metadataBytes, err := os.ReadFile(filepath.Join(containerDir, "metadata.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("container %q was not found", name)
		}
		return fmt.Errorf("read container metadata: %w", err)
	}
	var metadata containerMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("decode container metadata: %w", err)
	}
	if metadata.LogPath == "" {
		return fmt.Errorf("container %q has no captured log; only detached runs capture logs", name)
	}
	logFile, err := os.Open(metadata.LogPath)
	if err != nil {
		return fmt.Errorf("open container %q log: %w", name, err)
	}
	defer logFile.Close()
	for {
		if _, err := io.Copy(os.Stdout, logFile); err != nil {
			return fmt.Errorf("read container %q log: %w", name, err)
		}
		if !*follow || metadata.PID <= 0 {
			return nil
		}
		alive, err := processAlive(metadata.PID)
		if err != nil {
			return fmt.Errorf("check container %q process: %w", name, err)
		}
		if !alive {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func commandStop(args []string) error {
	if len(args) != 1 {
		return errors.New("stop requires exactly one container name")
	}
	name := args[0]
	if !validContainerName(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	home, err := mackerHome()
	if err != nil {
		return err
	}
	containerDir := filepath.Join(home, "containers", name)
	metadataPath := filepath.Join(containerDir, "metadata.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("container %q was not found", name)
		}
		return fmt.Errorf("read container metadata: %w", err)
	}
	var metadata containerMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("decode container metadata: %w", err)
	}
	if err := stopExecProcesses(containerDir); err != nil {
		return err
	}
	if metadata.PID <= 0 {
		if metadata.StoppedAt == nil {
			return fmt.Errorf("container %q has no stoppable process yet", name)
		}
		resourceCleanupErr := cleanupContainerResources(home, name, &metadata)
		if resourceCleanupErr != nil {
			return resourceCleanupErr
		}
		if err := writeContainerMetadata(containerDir, metadata); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if metadata.AutoRemove {
			if err := removeContainer(home, name); err != nil {
				return err
			}
			fmt.Printf("removed container %s\n", name)
			return nil
		}
		return fmt.Errorf("container %q is not a detached workload", name)
	}

	alive, err := processAlive(metadata.PID)
	if err != nil {
		return fmt.Errorf("check container %q process: %w", name, err)
	}
	if alive {
		if err := syscall.Kill(metadata.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal container %q: %w", name, err)
		}
		if err := waitForProcessExit(metadata.PID, 10*time.Second); err != nil {
			return fmt.Errorf("stop container %q: %w", name, err)
		}
	}
	if err := cleanupContainerResources(home, name, &metadata); err != nil {
		return err
	}

	metadata.PID = 0
	stoppedAt := time.Now().UTC()
	metadata.StoppedAt = &stoppedAt
	if err := writeContainerMetadata(containerDir, metadata); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	fmt.Printf("stopped container %s\n", name)
	if metadata.AutoRemove {
		if err := removeContainer(home, name); err != nil {
			return err
		}
		fmt.Printf("removed container %s\n", name)
	}
	return nil
}

func removeContainer(home, name string) error {
	return withState(home, func(state *mackerState) error {
		return removeContainerFromState(state, home, name)
	})
}

func removeContainerFromState(state *mackerState, home, name string) error {
	containerDir := filepath.Join(home, "containers", name)
	if err := os.RemoveAll(containerDir); err != nil {
		return fmt.Errorf("remove container %q: %w", name, err)
	}
	delete(state.Containers, name)
	return nil
}

func commandRM(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	force := fs.Bool("f", false, "stop a running container before removing it")
	fs.BoolVar(force, "force", false, "stop a running container before removing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("rm requires exactly one container name")
	}
	name := fs.Arg(0)
	if !validContainerName(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	home, err := mackerHome()
	if err != nil {
		return err
	}
	containerDir := filepath.Join(home, "containers", name)
	metadataPath := filepath.Join(containerDir, "metadata.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("container %q was not found", name)
		}
		return fmt.Errorf("read container metadata: %w", err)
	}
	var metadata containerMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("decode container metadata: %w", err)
	}
	status, err := containerStatus(metadata)
	if err != nil {
		return fmt.Errorf("inspect container %q: %w", name, err)
	}
	if status == "running" {
		if !*force {
			return fmt.Errorf("container %q is running; stop it first or use rm --force", name)
		}
		if err := commandStop([]string{name}); err != nil {
			return err
		}
		if metadata.AutoRemove {
			return nil
		}
	} else {
		if err := stopExecProcesses(containerDir); err != nil {
			return err
		}
		if err := cleanupContainerResources(home, name, &metadata); err != nil {
			return err
		}
	}

	if err := removeContainer(home, name); err != nil {
		return err
	}
	fmt.Printf("removed container %s\n", name)
	return nil
}

func commandPS(args []string) error {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	all := fs.Bool("a", false, "show stopped and exited containers")
	fs.BoolVar(all, "all", false, "show stopped and exited containers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("ps does not accept positional arguments")
	}

	home, err := mackerHome()
	if err != nil {
		return err
	}
	rows := make([]psRow, 0)
	if err := withState(home, func(state *mackerState) error {
		names := make([]string, 0, len(state.Containers))
		for name := range state.Containers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			metadataPath := filepath.Join(home, "containers", name, "metadata.json")
			metadataBytes, err := os.ReadFile(metadataPath)
			if os.IsNotExist(err) {
				delete(state.Containers, name)
				continue
			}
			if err != nil {
				return fmt.Errorf("read container %q metadata: %w", name, err)
			}
			var metadata containerMetadata
			if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
				return fmt.Errorf("decode container %q metadata: %w", name, err)
			}
			status, err := containerStatus(metadata)
			if err != nil {
				return fmt.Errorf("inspect container %q: %w", name, err)
			}
			if status != "running" {
				if err := stopExecProcesses(filepath.Join(home, "containers", name)); err != nil {
					return err
				}
			}
			if status != "running" && (metadata.PFAnchor != "" || metadata.PFToken != "" || metadata.NetworkConfig != nil) {
				if err := cleanupContainerResources(home, name, &metadata); err != nil {
					return err
				}
				if err := writeContainerMetadata(filepath.Join(home, "containers", name), metadata); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						delete(state.Containers, name)
						continue
					}
					return err
				}
			}
			if metadata.AutoRemove && status != "running" {
				if err := removeContainerFromState(state, home, name); err != nil {
					return err
				}
				continue
			}
			if !*all && status != "running" {
				continue
			}
			pid := "-"
			if metadata.PID > 0 {
				pid = strconv.Itoa(metadata.PID)
			}
			rows = append(rows, psRow{
				Name:    name,
				Image:   metadata.Image,
				Status:  status,
				Network: formatNetworkConfig(metadata.NetworkConfig),
				PID:     pid,
				Ports:   formatPortMappings(metadata.Ports),
				Created: metadata.CreatedAt.Format(time.RFC3339),
			})
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Println("NAME\tIMAGE\tSTATUS\tNETWORK\tPID\tPORTS\tCREATED")
	for _, row := range rows {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Name, row.Image, row.Status, row.Network, row.PID, row.Ports, row.Created)
	}
	return nil
}

type psRow struct {
	Name    string
	Image   string
	Status  string
	Network string
	PID     string
	Ports   string
	Created string
}

type storedImageIndex struct {
	Manifests []imageDescriptor `json:"manifests"`
}

type imageDescriptor struct {
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type bundlePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type bundleDescriptor struct {
	MediaType   string            `json:"mediaType,omitempty"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *bundlePlatform   `json:"platform,omitempty"`
}

type bundleIndex struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType,omitempty"`
	Manifests     []bundleDescriptor `json:"manifests"`
}

type imageRow struct {
	Reference string
	ID        string
	Created   string
}

func commandImages(args []string) error {
	if len(args) != 0 {
		return errors.New("images does not accept arguments")
	}
	home, err := mackerHome()
	if err != nil {
		return err
	}
	rows := make([]imageRow, 0)
	if err := withState(home, func(state *mackerState) error {
		keys := make([]string, 0, len(state.Images))
		for key := range state.Images {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			left := state.Images[keys[i]]
			right := state.Images[keys[j]]
			if left.Reference == right.Reference {
				return keys[i] < keys[j]
			}
			return left.Reference < right.Reference
		})
		for _, key := range keys {
			record := state.Images[key]
			created := "-"
			if !record.CreatedAt.IsZero() {
				created = record.CreatedAt.Format(time.RFC3339)
			}
			id := key
			if len(id) > 12 {
				id = id[:12]
			}
			rows = append(rows, imageRow{Reference: record.Reference, ID: id, Created: created})
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Println("REFERENCE\tIMAGE ID\tCREATED")
	for _, row := range rows {
		fmt.Printf("%s\t%s\t%s\n", row.Reference, row.ID, row.Created)
	}
	return nil
}

func commandRMI(args []string) error {
	if len(args) != 1 {
		return errors.New("rmi requires exactly one image reference")
	}
	ref, err := parseImageRef(args[0])
	if err != nil {
		return err
	}
	home, err := mackerHome()
	if err != nil {
		return err
	}
	layout := imageLayoutPath(home, ref)
	if err := requireLayout(layout); err != nil {
		return fmt.Errorf("rmi %s: %w", ref.Normalized, err)
	}
	key := filepath.Base(layout)
	if err := withState(home, func(state *mackerState) error {
		if err := os.RemoveAll(layout); err != nil {
			return fmt.Errorf("remove image %s: %w", ref.Normalized, err)
		}
		delete(state.Images, key)
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("removed image %s\n", ref.Normalized)
	return nil
}

func containerStatus(metadata containerMetadata) (string, error) {
	if metadata.PID > 0 {
		alive, err := processAlive(metadata.PID)
		if err != nil {
			return "", err
		}
		if alive {
			return "running", nil
		}
		return "exited", nil
	}
	if metadata.StoppedAt != nil {
		return "stopped", nil
	}
	return "running", nil
}

func commandForSpec(spec buildSpec) (string, []string, error) {
	if len(spec.Entrypoint) > 0 {
		if !strings.HasPrefix(spec.Entrypoint[0], "/") {
			return "", nil, errors.New("ENTRYPOINT executable must be an absolute path")
		}
		args := append([]string{}, spec.Entrypoint[1:]...)
		args = append(args, spec.Cmd...)
		return spec.Entrypoint[0], args, nil
	}
	if len(spec.Cmd) > 0 {
		if !strings.HasPrefix(spec.Cmd[0], "/") {
			return "", nil, errors.New("CMD executable must be an absolute path when ENTRYPOINT is absent")
		}
		return spec.Cmd[0], append([]string{}, spec.Cmd[1:]...), nil
	}
	return "", nil, errors.New("Mackerfile must define an absolute ENTRYPOINT or CMD")
}

func parseImageRef(input string) (imageRef, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return imageRef{}, errors.New("image reference must not be empty")
	}
	if strings.Contains(input, "://") || strings.Contains(input, "@") {
		return imageRef{}, fmt.Errorf("unsupported image reference %q; use a tagged image name", input)
	}

	name := input
	tag := "latest"
	slash := strings.LastIndexByte(name, '/')
	colon := strings.LastIndexByte(name, ':')
	if colon > slash {
		tag = name[colon+1:]
		name = name[:colon]
	}
	if name == "" || tag == "" {
		return imageRef{}, fmt.Errorf("invalid image reference %q", input)
	}
	parts := strings.Split(name, "/")
	registry := "docker.io"
	switch {
	case len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost"):
		registry = parts[0]
		parts = parts[1:]
	}
	repository := strings.Join(parts, "/")
	if repository == "" || strings.Contains(repository, "//") {
		return imageRef{}, fmt.Errorf("invalid image reference %q", input)
	}
	if registry == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	if !validTag(tag) {
		return imageRef{}, fmt.Errorf("invalid image tag %q", tag)
	}
	return imageRef{
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
		Normalized: registry + "/" + repository + ":" + tag,
	}, nil
}

func validTag(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			if i == 0 && r == '.' {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func mackerHome() (string, error) {
	root := os.Getenv("MACKER_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		root = filepath.Join(home, ".macker")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve MACKER_HOME: %w", err)
	}
	return absolute, nil
}

func ensureStorage(home string) error {
	for _, dir := range []string{home, filepath.Join(home, "images"), filepath.Join(home, "containers"), filepath.Join(home, "tmp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create storage directory %q: %w", dir, err)
		}
	}
	return nil
}

func withState(home string, fn func(*mackerState) error) error {
	if err := ensureStorage(home); err != nil {
		return err
	}
	lockPath := filepath.Join(home, "state.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	state, err := loadState(home)
	if err != nil {
		return err
	}
	if err := reconcileState(home, &state); err != nil {
		return err
	}
	if err := fn(&state); err != nil {
		return err
	}
	return writeState(home, state)
}

func loadState(home string) (mackerState, error) {
	state := mackerState{
		Version:    1,
		Containers: make(map[string]struct{}),
		Images:     make(map[string]imageRecord),
	}
	data, err := os.ReadFile(filepath.Join(home, "state.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode state: %w", err)
	}
	if state.Version == 0 {
		state.Version = 1
	}
	if state.Version != 1 {
		return state, fmt.Errorf("unsupported state version %d", state.Version)
	}
	if state.Containers == nil {
		state.Containers = make(map[string]struct{})
	}
	if state.Images == nil {
		state.Images = make(map[string]imageRecord)
	}
	return state, nil
}

func writeState(home string, state mackerState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	statePath := filepath.Join(home, "state.json")
	temporaryPath := statePath + ".tmp"
	if err := os.WriteFile(temporaryPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(temporaryPath, statePath); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("install state: %w", err)
	}
	return nil
}

func reconcileState(home string, state *mackerState) error {
	entries, err := os.ReadDir(filepath.Join(home, "containers"))
	if err != nil {
		return fmt.Errorf("read container storage: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validContainerName(entry.Name()) {
			continue
		}
		metadataPath := filepath.Join(home, "containers", entry.Name(), "metadata.json")
		if _, err := os.Stat(metadataPath); err == nil {
			state.Containers[entry.Name()] = struct{}{}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect container %q: %w", entry.Name(), err)
		}
	}
	for name := range state.Containers {
		if !validContainerName(name) {
			delete(state.Containers, name)
			continue
		}
		metadataPath := filepath.Join(home, "containers", name, "metadata.json")
		if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
			delete(state.Containers, name)
		} else if err != nil {
			return fmt.Errorf("inspect container %q: %w", name, err)
		}
	}

	imageEntries, err := os.ReadDir(filepath.Join(home, "images"))
	if err != nil {
		return fmt.Errorf("read image storage: %w", err)
	}
	for _, entry := range imageEntries {
		if !entry.IsDir() {
			continue
		}
		layout := filepath.Join(home, "images", entry.Name())
		if _, err := os.Stat(filepath.Join(layout, "oci-layout")); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect image %q: %w", entry.Name(), err)
		}
		if _, ok := state.Images[entry.Name()]; ok {
			continue
		}
		reference := imageReferenceFromLayout(layout)
		if reference == "" {
			reference = "<unknown>:" + entry.Name()
		}
		info, err := os.Stat(layout)
		if err != nil {
			return fmt.Errorf("stat image %q: %w", entry.Name(), err)
		}
		state.Images[entry.Name()] = imageRecord{
			Reference: reference,
			CreatedAt: info.ModTime().UTC(),
		}
	}
	for key := range state.Images {
		if key == "" || filepath.Base(key) != key || key == "." {
			delete(state.Images, key)
			continue
		}
		layout := filepath.Join(home, "images", key, "oci-layout")
		if _, err := os.Stat(layout); os.IsNotExist(err) {
			delete(state.Images, key)
		} else if err != nil {
			return fmt.Errorf("inspect image %q: %w", key, err)
		}
	}
	return nil
}

func imageReferenceFromLayout(layout string) string {
	data, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		return ""
	}
	var index storedImageIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return ""
	}
	for _, manifest := range index.Manifests {
		if reference := manifest.Annotations["org.opencontainers.image.ref.name"]; reference != "" {
			return reference
		}
	}
	return ""
}

func registerContainerState(home, name string) error {
	return withState(home, func(state *mackerState) error {
		state.Containers[name] = struct{}{}
		return nil
	})
}

func unregisterContainerState(home, name string) error {
	return withState(home, func(state *mackerState) error {
		delete(state.Containers, name)
		return nil
	})
}

func registerImageState(home string, ref imageRef, layout string) error {
	return withState(home, func(state *mackerState) error {
		state.Images[filepath.Base(layout)] = imageRecord{
			Reference: ref.Normalized,
			CreatedAt: time.Now().UTC(),
		}
		return nil
	})
}

func imageLayoutPath(home string, ref imageRef) string {
	digest := sha256.Sum256([]byte(ref.Normalized))
	return filepath.Join(home, "images", hex.EncodeToString(digest[:]))
}

func requireLayout(layout string) error {
	info, err := os.Stat(filepath.Join(layout, "oci-layout"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("image is not present in local storage")
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("image layout marker is a directory")
	}
	return nil
}

func skopeoBinary() (string, error) {
	if configured := os.Getenv("MACKER_SKOPEO"); configured != "" {
		return configured, nil
	}
	if candidate, err := exec.LookPath("skopeo"); err == nil {
		return candidate, nil
	}
	return "", errors.New("skopeo was not found; install it with Homebrew")
}

func runCommand(binary string, args ...string) error {
	cmd := exec.Command(binary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(binary), err)
	}
	return nil
}

func runBuildCommand(contextDir, rootfs, command string, imageEnv []string) error {
	cmd := exec.Command("/bin/bash", "-c", command)
	cmd.Dir = contextDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = buildEnvironment(imageEnv, contextDir, rootfs)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("/bin/bash -c %q: %w", command, err)
	}
	return nil
}

func buildEnvironment(imageEnv []string, contextDir, rootfs string) []string {
	imageValues := make(map[string]string, len(imageEnv))
	for _, entry := range imageEnv {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" && key != "MACKER_CONTEXT" && key != "MACKER_ROOTFS" {
			imageValues[key] = entry
		}
	}
	env := make([]string, 0, len(os.Environ())+len(imageValues)+2)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || (key != "MACKER_CONTEXT" && key != "MACKER_ROOTFS" && imageValues[key] == "") {
			env = append(env, entry)
		}
	}
	for _, entry := range imageValues {
		env = append(env, entry)
	}
	env = append(env, "MACKER_CONTEXT="+contextDir, "MACKER_ROOTFS="+rootfs)
	return env
}

func writeContainerMetadata(containerDir string, metadata containerMetadata) error {
	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode container metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "metadata.json"), append(metadataBytes, '\n'), 0o644); err != nil {
		return fmt.Errorf("write container metadata: %w", err)
	}
	return nil
}

func runDetached(binary string, args []string, logPath string) (int, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open container log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start detached workload: %w", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return 0, fmt.Errorf("release detached workload: %w", err)
	}
	return pid, nil
}

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("invalid process id %d", pid)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}

	// A detached process is reparented when macker exits, so it cannot be
	// waited on by this command. kill(0) still reports zombies as present;
	// inspect the process state so stop does not wait forever for a workload
	// that has already exited.
	state, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "state=").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, err
	}
	fields := strings.Fields(string(state))
	if len(fields) == 0 {
		return false, nil
	}
	return !strings.HasPrefix(fields[0], "Z"), nil
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := processAlive(pid)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %d did not exit within %s", pid, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func parseVolume(raw string) (volume, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return volume{}, fmt.Errorf("invalid volume %q; expected HOST:CONTAINER", raw)
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
	return volume{HostPath: host, ContainerPath: container}, nil
}

func checkPublishedPortConflicts(home, name string, ports []portMapping) error {
	if len(ports) == 0 {
		return nil
	}
	requested := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		requested[fmt.Sprintf("%d/%s", port.HostPort, port.Protocol)] = struct{}{}
	}
	return withState(home, func(state *mackerState) error {
		for otherName := range state.Containers {
			if otherName == name {
				continue
			}
			metadataPath := filepath.Join(home, "containers", otherName, "metadata.json")
			data, err := os.ReadFile(metadataPath)
			if err != nil {
				return fmt.Errorf("read container %q metadata: %w", otherName, err)
			}
			var metadata containerMetadata
			if err := json.Unmarshal(data, &metadata); err != nil {
				return fmt.Errorf("decode container %q metadata: %w", otherName, err)
			}
			if metadata.PFAnchor == "" {
				continue
			}
			for _, port := range metadata.Ports {
				key := fmt.Sprintf("%d/%s", port.HostPort, port.Protocol)
				if _, exists := requested[key]; exists {
					return fmt.Errorf("published port %d/%s is already used by container %q", port.HostPort, port.Protocol, otherName)
				}
			}
		}
		return nil
	})
}

func parsePortMapping(raw string) (portMapping, error) {
	value := strings.TrimSpace(raw)
	protocol := "tcp"
	if strings.Count(value, "/") > 1 {
		return portMapping{}, fmt.Errorf("invalid port mapping %q; expected HOST_PORT:NODE_PORT[/tcp|/udp]", raw)
	}
	if slash := strings.LastIndexByte(value, '/'); slash >= 0 {
		protocol = strings.ToLower(value[slash+1:])
		value = value[:slash]
		if protocol != "tcp" && protocol != "udp" {
			return portMapping{}, fmt.Errorf("invalid port mapping protocol %q; use tcp or udp", protocol)
		}
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return portMapping{}, fmt.Errorf("invalid port mapping %q; expected HOST_PORT:NODE_PORT[/tcp|/udp]", raw)
	}
	hostPort, err := parsePortNumber(parts[0])
	if err != nil {
		return portMapping{}, fmt.Errorf("invalid host port in %q: %w", raw, err)
	}
	nodePort, err := parsePortNumber(parts[1])
	if err != nil {
		return portMapping{}, fmt.Errorf("invalid node port in %q: %w", raw, err)
	}
	return portMapping{HostPort: hostPort, NodePort: nodePort, Protocol: protocol}, nil
}

func parsePortNumber(value string) (uint16, error) {
	if value == "" {
		return 0, errors.New("port must be between 1 and 65535")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("port must be a decimal number between 1 and 65535")
		}
	}
	number, err := strconv.ParseUint(value, 10, 16)
	if err != nil || number == 0 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	return uint16(number), nil
}

func formatPortMappings(ports []portMapping) string {
	formatted := make([]string, 0, len(ports))
	for _, port := range ports {
		formatted = append(formatted, fmt.Sprintf("%d:%d/%s", port.HostPort, port.NodePort, port.Protocol))
	}
	return strings.Join(formatted, ",")
}

// macOS's default /etc/pf.conf exposes com.apple/* as a dynamic rdr anchor.
// Keep one child anchor per container so Macker can replace and flush mappings
// without touching the main PF ruleset.
const pfAnchorPrefix = "com.apple/macker-"

func pfAnchorForContainer(name string) string {
	return pfAnchorPrefix + name
}

func installPFPortMappings(name, targetIP string, ports []portMapping) (state pfPortState, err error) {
	if runtime.GOOS != "darwin" {
		return state, errors.New("port publishing requires a Darwin host with pfctl")
	}
	if err := ensurePFAnchorAvailable(); err != nil {
		return state, err
	}
	anchor := pfAnchorForContainer(name)
	tokenOutput, err := runPFCTL("-E")
	if err != nil {
		return state, fmt.Errorf("enable PF: %w; port publishing requires root or passwordless sudo", err)
	}
	token, err := parsePFToken(tokenOutput)
	if err != nil {
		return state, fmt.Errorf("read PF enable token: %w", err)
	}
	state = pfPortState{Anchor: anchor, Token: token}
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			_ = cleanupPFPortMappings(state)
		}
	}()

	rulesFile, err := os.CreateTemp("", "macker-pf-*.conf")
	if err != nil {
		return state, fmt.Errorf("create PF rules: %w", err)
	}
	rulesPath := rulesFile.Name()
	defer os.Remove(rulesPath)
	if _, err := rulesFile.WriteString(buildPFRules(targetIP, ports)); err != nil {
		_ = rulesFile.Close()
		return state, fmt.Errorf("write PF rules: %w", err)
	}
	if err := rulesFile.Close(); err != nil {
		return state, fmt.Errorf("close PF rules: %w", err)
	}
	if _, err := runPFCTL("-a", anchor, "-f", rulesPath); err != nil {
		return state, fmt.Errorf("load PF rules: %w", err)
	}
	cleanupNeeded = false
	return state, nil
}

func ensurePFAnchorAvailable() error {
	output, err := runPFCTL("-a", "*", "-sn")
	if err != nil {
		return fmt.Errorf("inspect PF anchors: %w", err)
	}
	if !strings.Contains(string(output), `rdr-anchor "com.apple/*"`) {
		return errors.New(`PF does not expose macOS's com.apple/* rdr anchor; add rdr-anchor "com.apple/*" to the active PF configuration`)
	}
	return nil
}

func cleanupPFPortMappings(state pfPortState) error {
	var cleanupErrors []error
	if state.Anchor != "" {
		if _, err := runPFCTL("-a", state.Anchor, "-F", "nat"); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("flush PF anchor %s: %w", state.Anchor, err))
		}
	}
	if state.Token != "" {
		if _, err := runPFCTL("-X", state.Token); err != nil && !isStalePFTokenError(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("release PF enable token: %w", err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func isStalePFTokenError(err error) bool {
	return strings.Contains(err.Error(), "pf: token invalid")
}

func buildPFRules(targetIP string, ports []portMapping) string {
	var rules strings.Builder
	for _, port := range ports {
		// Omitting an interface makes PF evaluate the redirect on all
		// interfaces, including interfaces that Macker does not know about yet.
		fmt.Fprintf(&rules, "rdr pass inet proto %s from any to any port = %d -> %s port %d\n", port.Protocol, port.HostPort, targetIP, port.NodePort)
	}
	return rules.String()
}

func parsePFToken(output []byte) (string, error) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Token" {
			token := fields[len(fields)-1]
			if _, err := strconv.ParseUint(token, 10, 64); err == nil {
				return token, nil
			}
		}
	}
	return "", errors.New("pfctl did not return an enable token")
}

func runPFCTL(args ...string) ([]byte, error) {
	pfctlBinary := os.Getenv("MACKER_PFCTL")
	if pfctlBinary == "" {
		var err error
		pfctlBinary, err = exec.LookPath("pfctl")
		if err != nil {
			if _, statErr := os.Stat("/sbin/pfctl"); statErr != nil {
				return nil, errors.New("pfctl was not found")
			}
			pfctlBinary = "/sbin/pfctl"
		}
	}
	return runPrivileged(pfctlBinary, args...)
}

func validContainerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func installVolume(rootfs string, mount volume) error {
	relative := strings.TrimPrefix(filepath.FromSlash(mount.ContainerPath), string(filepath.Separator))
	target := filepath.Join(rootfs, relative)
	rel, err := filepath.Rel(rootfs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("volume target escapes container rootfs: %q", mount.ContainerPath)
	}
	if err := ensureNoSymlinkParents(rootfs, target); err != nil {
		return fmt.Errorf("volume target %q: %w", mount.ContainerPath, err)
	}
	if err := removeExisting(target); err != nil {
		return fmt.Errorf("replace volume target %q: %w", mount.ContainerPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create volume parent %q: %w", mount.ContainerPath, err)
	}
	if err := os.Symlink(mount.HostPath, target); err != nil {
		return fmt.Errorf("link volume %q: %w", mount.ContainerPath, err)
	}
	return nil
}

func parseMackerfile(filename string) (buildSpec, error) {
	file, err := os.Open(filename)
	if err != nil {
		return buildSpec{}, fmt.Errorf("open Mackerfile: %w", err)
	}
	defer file.Close()

	lines, err := logicalLines(file)
	if err != nil {
		return buildSpec{}, fmt.Errorf("read Mackerfile: %w", err)
	}
	spec := buildSpec{WorkingDir: "/"}
	fromSeen := false
	for _, logical := range lines {
		op, rest := instructionParts(logical.Text)
		switch op {
		case "FROM":
			if fromSeen {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: only one FROM is supported", logical.Number)
			}
			if strings.TrimSpace(rest) != "scratch" {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: only FROM scratch is supported", logical.Number)
			}
			fromSeen = true
			spec.FromScratch = true
		case "COPY":
			if !fromSeen {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: FROM must be first", logical.Number)
			}
			values, err := instructionValues(rest)
			if err != nil {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: COPY: %w", logical.Number, err)
			}
			if len(values) < 2 {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: COPY needs a source and destination", logical.Number)
			}
			for _, value := range values[:len(values)-1] {
				if strings.HasPrefix(value, "--") {
					return buildSpec{}, fmt.Errorf("Mackerfile line %d: COPY options are not supported", logical.Number)
				}
			}
			instruction := copyInstruction{Sources: values[:len(values)-1], Destination: values[len(values)-1]}
			spec.Copies = append(spec.Copies, instruction)
			spec.Steps = append(spec.Steps, buildStep{Copy: &instruction})
		case "RUN":
			if !fromSeen {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: FROM must be first", logical.Number)
			}
			command := strings.TrimSpace(rest)
			if command == "" {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: RUN needs a command", logical.Number)
			}
			spec.Steps = append(spec.Steps, buildStep{Run: command})
		case "ENV":
			if !fromSeen {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: FROM must be first", logical.Number)
			}
			values, err := parseEnv(rest)
			if err != nil {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: ENV: %w", logical.Number, err)
			}
			spec.Env = append(spec.Env, values...)
		case "WORKDIR":
			if !fromSeen {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: FROM must be first", logical.Number)
			}
			value := strings.TrimSpace(rest)
			if value == "" {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: WORKDIR needs a path", logical.Number)
			}
			if strings.HasPrefix(value, "/") {
				spec.WorkingDir = path.Clean(value)
			} else {
				spec.WorkingDir = path.Join(spec.WorkingDir, value)
			}
		case "ENTRYPOINT":
			if !fromSeen {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: FROM must be first", logical.Number)
			}
			values, err := jsonCommand(rest)
			if err != nil {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: ENTRYPOINT: %w", logical.Number, err)
			}
			spec.Entrypoint = values
		case "CMD":
			if !fromSeen {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: FROM must be first", logical.Number)
			}
			values, err := jsonCommand(rest)
			if err != nil {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: CMD: %w", logical.Number, err)
			}
			spec.Cmd = values
		case "EXPOSE":
			if !fromSeen || strings.TrimSpace(rest) == "" {
				return buildSpec{}, fmt.Errorf("Mackerfile line %d: invalid EXPOSE", logical.Number)
			}
			// EXPOSE remains metadata only; runtime publishing is explicit with -p.
		case "":
			continue
		default:
			return buildSpec{}, fmt.Errorf("Mackerfile line %d: unsupported instruction %s", logical.Number, op)
		}
	}
	if !fromSeen || !spec.FromScratch {
		return buildSpec{}, errors.New("Mackerfile must start with FROM scratch")
	}
	if spec.WorkingDir == "" || !strings.HasPrefix(spec.WorkingDir, "/") {
		return buildSpec{}, errors.New("Mackerfile WORKDIR must resolve to an absolute path")
	}
	return spec, nil
}

type logicalLine struct {
	Number int
	Text   string
}

func logicalLines(input io.Reader) ([]logicalLine, error) {
	scanner := bufio.NewScanner(input)
	var result []logicalLine
	var current strings.Builder
	startLine := 0
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if current.Len() == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if startLine == 0 {
			startLine = lineNumber
		}
		if strings.HasSuffix(line, "\\") {
			current.WriteString(strings.TrimSpace(strings.TrimSuffix(line, "\\")))
			current.WriteByte(' ')
			continue
		}
		current.WriteString(line)
		result = append(result, logicalLine{Number: startLine, Text: current.String()})
		current.Reset()
		startLine = 0
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if current.Len() != 0 {
		return nil, fmt.Errorf("line %d ends with a continuation", startLine)
	}
	return result, nil
}

func instructionParts(line string) (string, string) {
	line = strings.TrimSpace(line)
	for i, r := range line {
		if r == ' ' || r == '\t' {
			return strings.ToUpper(line[:i]), strings.TrimSpace(line[i:])
		}
	}
	return strings.ToUpper(line), ""
}

func instructionValues(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") {
		var values []string
		if err := json.Unmarshal([]byte(value), &values); err != nil {
			return nil, fmt.Errorf("invalid JSON form: %w", err)
		}
		return values, nil
	}
	if value == "" {
		return nil, errors.New("arguments are empty")
	}
	return strings.Fields(value), nil
}

func jsonCommand(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") {
		return nil, errors.New("only JSON-array form is supported; shell form cannot run FROM scratch")
	}
	var command []string
	if err := json.Unmarshal([]byte(value), &command); err != nil {
		return nil, fmt.Errorf("invalid JSON form: %w", err)
	}
	if len(command) == 0 || command[0] == "" {
		return nil, errors.New("command must not be empty")
	}
	return command, nil
}

func parseEnv(value string) ([]string, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil, errors.New("arguments are empty")
	}
	result := make([]string, 0, len(fields))
	if strings.Contains(fields[0], "=") {
		for _, field := range fields {
			key, value, ok := strings.Cut(field, "=")
			if !ok || key == "" {
				return nil, fmt.Errorf("invalid assignment %q", field)
			}
			result = append(result, key+"="+unquote(value))
		}
		return result, nil
	}
	if len(fields) < 2 {
		return nil, errors.New("expected KEY=VALUE or KEY VALUE")
	}
	return []string{fields[0] + "=" + unquote(strings.Join(fields[1:], " "))}, nil
}

func unquote(value string) string {
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
		return value[1 : len(value)-1]
	}
	return value
}

func applyCopyInstruction(contextDir, rootfs string, instruction copyInstruction) error {
	if !strings.HasPrefix(instruction.Destination, "/") {
		return fmt.Errorf("destination %q must be absolute", instruction.Destination)
	}
	destination, err := rootfsPath(rootfs, instruction.Destination)
	if err != nil {
		return err
	}
	if err := ensureNoSymlinkParents(rootfs, destination); err != nil {
		return err
	}
	matches := make([]copySource, 0)
	for _, source := range instruction.Sources {
		if strings.HasPrefix(source, "/") {
			return fmt.Errorf("source %q must be relative to the context", source)
		}
		copyContents := source == "." || source == "./" || strings.HasSuffix(source, "/")
		pattern := filepath.Join(contextDir, filepath.FromSlash(source))
		found, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("source pattern %q: %w", source, err)
		}
		if len(found) == 0 {
			return fmt.Errorf("source %q was not found in context", source)
		}
		for _, match := range found {
			rel, err := filepath.Rel(contextDir, match)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("source %q escapes context", source)
			}
			matches = append(matches, copySource{Path: match, CopyContents: copyContents})
		}
	}
	multiple := len(matches) > 1
	destinationIsDirectory := multiple || strings.HasSuffix(instruction.Destination, "/") || path.Clean(instruction.Destination) == "/"
	for _, source := range matches {
		info, err := os.Lstat(source.Path)
		if err != nil {
			return err
		}
		if info.IsDir() && source.CopyContents {
			if err := copyDirectoryContents(source.Path, destination); err != nil {
				return err
			}
			continue
		}
		target := destination
		if destinationIsDirectory {
			target = filepath.Join(destination, filepath.Base(source.Path))
		}
		if err := ensureNoSymlinkParents(rootfs, target); err != nil {
			return err
		}
		if err := copyEntry(source.Path, target); err != nil {
			return err
		}
	}
	return nil
}

func rootfsPath(rootfs, containerPath string) (string, error) {
	clean := path.Clean(containerPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || !strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("path %q escapes rootfs", containerPath)
	}
	target := filepath.Join(rootfs, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	rel, err := filepath.Rel(rootfs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes rootfs", containerPath)
	}
	return target, nil
}

func copyDirectoryContents(source, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyEntry(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyEntry(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		if err := removeExisting(destination); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(link, destination)
	case info.IsDir():
		if existing, err := os.Lstat(destination); err == nil {
			if !existing.IsDir() || existing.Mode()&os.ModeSymlink != 0 {
				if err := removeExisting(destination); err != nil {
					return err
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chmod(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyEntry(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	case info.Mode().IsRegular():
		if err := removeExisting(destination); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeOutputErr != nil {
			return closeOutputErr
		}
		if closeInputErr != nil {
			return closeInputErr
		}
		return os.Chmod(destination, info.Mode().Perm())
	default:
		return fmt.Errorf("unsupported context file type %q", source)
	}
}

func ensureNoSymlinkParents(rootfs, target string) error {
	if target == rootfs {
		return nil
	}
	relative, err := filepath.Rel(rootfs, filepath.Dir(target))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes rootfs")
	}
	current := rootfs
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent %q is a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent %q is not a directory", current)
		}
	}
	return nil
}

func removeExisting(filename string) error {
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return os.RemoveAll(filename)
	}
	return os.Remove(filename)
}
