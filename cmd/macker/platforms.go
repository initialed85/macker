// This file implements platform reporting for image references.
// It deliberately uses only the Go standard library.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	indexMediaType         = "application/vnd.oci.image.index.v1+json"
	dockerManifestListV1   = "application/vnd.docker.distribution.manifest.list.v1+json"
	dockerManifestListV2   = "application/vnd.docker.distribution.manifest.list.v2+json"
	maxPlatformSearchDepth = 4
)

// imagePlatformListing describes every platform an image reference offers and
// whether this host can execute one of them.
type imagePlatformListing struct {
	Reference   string
	Source      string // "local" or "registry"
	HostOS      string
	HostArch    string
	Platforms   []platform
	HasRunnable bool // a darwin manifest matching the host architecture exists
}

// runnableOnHost reports whether a platform can be executed by Macker: a
// Darwin image matching this host's architecture, on a Darwin host.
func runnableOnHost(candidate platform) bool {
	return candidate.OS == "darwin" && candidate.Architecture == runtime.GOARCH && runtime.GOOS == "darwin"
}

func commandPlatforms(args []string) error {
	fs := flag.NewFlagSet("platforms", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "table", "output format: table or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("platforms requires exactly one image reference")
	}
	switch *format {
	case "table", "json":
	default:
		return fmt.Errorf("unknown format %q; use table or json", *format)
	}

	ref, err := parseImageRef(fs.Arg(0))
	if err != nil {
		return err
	}
	if isScratchImage(ref) {
		return errors.New("scratch is an implicit empty Darwin rootfs, not a registry image; run `macker platforms` against a real image reference")
	}

	listing, err := listImagePlatforms(ref)
	if err != nil {
		return err
	}
	return printPlatformListing(listing, *format)
}

func listImagePlatforms(ref imageRef) (imagePlatformListing, error) {
	home, err := mackerHome()
	if err != nil {
		return imagePlatformListing{}, err
	}
	layout := imageLayoutPath(home, ref)
	if requireLayout(layout) == nil {
		index, err := readLayoutRootIndex(layout)
		if err != nil {
			return imagePlatformListing{}, err
		}
		platforms, err := collectImagePlatforms(layout, index, 1)
		if err != nil {
			return imagePlatformListing{}, err
		}
		return newPlatformListing(ref, "local", platforms), nil
	}
	return remoteImagePlatforms(ref)
}

func newPlatformListing(ref imageRef, source string, platforms []platform) imagePlatformListing {
	listing := imagePlatformListing{
		Reference: ref.Normalized,
		Source:    source,
		HostOS:    runtime.GOOS,
		HostArch:  runtime.GOARCH,
		Platforms: platforms,
	}
	for _, candidate := range platforms {
		if runnableOnHost(candidate) {
			listing.HasRunnable = true
			break
		}
	}
	return listing
}

func remoteImagePlatforms(ref imageRef) (imagePlatformListing, error) {
	skopeo, err := skopeoBinary()
	if err != nil {
		return imagePlatformListing{}, err
	}
	output, err := runCommandOutput(skopeo, "inspect", "--format", "raw", "docker://"+ref.Normalized)
	if err != nil {
		return imagePlatformListing{}, fmt.Errorf("inspect %s: %w", ref.Normalized, err)
	}
	var index imageIndex
	if err := json.Unmarshal(output, &index); err != nil {
		return imagePlatformListing{}, fmt.Errorf("parse skopeo inspect output for %s: %w", ref.Normalized, err)
	}
	platforms, err := collectImagePlatforms("", index, 1)
	if err != nil {
		return imagePlatformListing{}, fmt.Errorf("read %s platforms: %w", ref.Normalized, err)
	}
	return newPlatformListing(ref, "registry", platforms), nil
}

// runCommandOutput runs a command and captures its standard output, leaving
// standard error attached so authentication and registry errors stay visible.
func runCommandOutput(binary string, args ...string) ([]byte, error) {
	cmd := exec.Command(binary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(binary), err)
	}
	return stdout.Bytes(), nil
}

// readLayoutRootIndex returns the OCI layout's index.json, or the manifest it
// points at when a single tagged manifest is stored as the root descriptor.
func readLayoutRootIndex(layout string) (imageIndex, error) {
	var index imageIndex
	if err := readJSONFile(filepath.Join(layout, "index.json"), &index); err != nil {
		return imageIndex{}, fmt.Errorf("read index: %w", err)
	}
	if len(index.Manifests) == 0 {
		return imageIndex{}, errors.New("OCI index contains no manifests")
	}
	if len(index.Manifests) == 1 && !isIndexMediaType(index.Manifests[0].MediaType) && index.Manifests[0].Platform == nil {
		var manifest manifest
		if err := readDescriptorJSON(layout, index.Manifests[0], &manifest); err != nil {
			return imageIndex{}, fmt.Errorf("read manifest: %w", err)
		}
		return imageIndex{
			SchemaVersion: manifest.SchemaVersion,
			MediaType:     manifest.MediaType,
			Manifests: []descriptor{{
				MediaType: manifestMediaType,
				Digest:    index.Manifests[0].Digest,
				Size:      index.Manifests[0].Size,
			}},
		}, nil
	}
	return index, nil
}

func isIndexMediaType(mediaType string) bool {
	switch mediaType {
	case indexMediaType, dockerManifestListV1, dockerManifestListV2:
		return true
	}
	return false
}

// collectImagePlatforms walks descriptors, reading manifest and config blobs
// when a descriptor does not carry platform metadata. layout is empty for a
// registry index, where blobs cannot be read locally.
func collectImagePlatforms(layout string, index imageIndex, depth int) ([]platform, error) {
	if depth > maxPlatformSearchDepth {
		return nil, fmt.Errorf("image index nests deeper than %d levels", maxPlatformSearchDepth)
	}
	var found []platform
	for _, candidate := range index.Manifests {
		if candidate.Platform != nil {
			found = append(found, *candidate.Platform)
			continue
		}
		if layout == "" {
			continue
		}
		if candidate.MediaType != "" && !isIndexMediaType(candidate.MediaType) && candidate.MediaType != manifestMediaType {
			continue
		}
		if isIndexMediaType(candidate.MediaType) {
			var nested imageIndex
			if err := readDescriptorJSON(layout, candidate, &nested); err != nil {
				return nil, err
			}
			nestedPlatforms, err := collectImagePlatforms(layout, nested, depth+1)
			if err != nil {
				return nil, err
			}
			found = append(found, nestedPlatforms...)
			continue
		}
		var manifest manifest
		if err := readDescriptorJSON(layout, candidate, &manifest); err != nil {
			return nil, err
		}
		var config imageConfig
		if err := readDescriptorJSON(layout, manifest.Config, &config); err != nil {
			return nil, err
		}
		found = append(found, platform{OS: config.OS, Architecture: config.Architecture})
	}
	return dedupePlatforms(found), nil
}

func dedupePlatforms(input []platform) []platform {
	seen := make(map[platform]struct{}, len(input))
	outlined := make([]platform, 0, len(input))
	for _, candidate := range input {
		if candidate.OS == "" || candidate.Architecture == "" {
			continue
		}
		// BuildKit-style attestation and unknown entries are not platforms.
		if candidate.OS == "unknown" || candidate.Architecture == "unknown" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		outlined = append(outlined, candidate)
	}
	sort.Slice(outlined, func(i, j int) bool {
		if outlined[i].OS != outlined[j].OS {
			return outlined[i].OS < outlined[j].OS
		}
		return outlined[i].Architecture < outlined[j].Architecture
	})
	return outlined
}

func platformListText(platforms []platform) string {
	if len(platforms) == 0 {
		return "none"
	}
	names := make([]string, 0, len(platforms))
	for _, candidate := range platforms {
		names = append(names, candidate.OS+"/"+candidate.Architecture)
	}
	return strings.Join(names, ", ")
}

// platformUnsuitableReason explains why an image cannot run on this host in
// one actionable sentence, the message operators otherwise reconstruct from a
// runtime-level "no image found in image index" failure.
func platformUnsuitableReason(listing imagePlatformListing) string {
	host := listing.HostOS + "/" + listing.HostArch
	if listing.HostOS != "darwin" {
		return fmt.Sprintf("this host is %s; Macker runs workloads on macOS, and %s offers %s", host, listing.Reference, platformListText(listing.Platforms))
	}
	return fmt.Sprintf("%s offers %s; Macker needs a darwin/%s manifest, and Linux manifests cannot execute on macOS. Use `macker run --entrypoint COMMAND -v HOST:CONTAINER scratch` for volume-mounted Darwin jobs, or build a Darwin image for the workload",
		listing.Reference, platformListText(listing.Platforms), listing.HostArch)
}

func printPlatformListing(listing imagePlatformListing, format string) error {
	if format == "json" {
		payload := struct {
			Reference   string     `json:"reference"`
			Source      string     `json:"source"`
			HostOS      string     `json:"host_os"`
			HostArch    string     `json:"host_arch"`
			Platforms   []platform `json:"platforms"`
			Runnable    bool       `json:"runnable"`
			Unsupported string     `json:"unsupported_reason,omitempty"`
		}{
			Reference: listing.Reference,
			Source:    listing.Source,
			HostOS:    listing.HostOS,
			HostArch:  listing.HostArch,
			Platforms: listing.Platforms,
			Runnable:  listing.HasRunnable,
		}
		if !listing.HasRunnable {
			payload.Unsupported = platformUnsuitableReason(listing)
		}
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return fmt.Errorf("encode platforms: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("%s (%s)\n", listing.Reference, listing.Source)
	fmt.Printf("platforms: %s\n", platformListText(listing.Platforms))
	host := listing.HostOS + "/" + listing.HostArch
	if listing.HasRunnable {
		fmt.Printf("runnable here (%s): yes\n", host)
		return nil
	}
	fmt.Printf("runnable here (%s): no\n", host)
	fmt.Printf("reason: %s\n", platformUnsuitableReason(listing))
	return nil
}
