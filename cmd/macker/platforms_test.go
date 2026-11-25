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

// captureStdout collects standard output written by fn, including writes made
// through the os.Stdout file descriptor.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create capture pipe: %v", err)
	}
	os.Stdout = writer
	done := make(chan string, 1)
	go func() {
		data, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Errorf("read capture pipe: %v", readErr)
		}
		done <- string(data)
	}()
	fn()
	if err := writer.Close(); err != nil {
		t.Errorf("close capture pipe: %v", err)
	}
	os.Stdout = original
	output := <-done
	if err := reader.Close(); err != nil {
		t.Errorf("close capture reader: %v", err)
	}
	return output
}

func writeFakeLayout(t *testing.T, layout string, index imageIndex) {
	t.Helper()
	if err := writeJSON(filepath.Join(layout, "oci-layout"), imageLayout{ImageLayoutVersion: layoutVersion}); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}
	if err := writeJSON(filepath.Join(layout, "index.json"), index); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

func writeFakeManifestBlob(t *testing.T, layout string, osName, architecture string) descriptor {
	t.Helper()
	configBytes, err := json.Marshal(imageConfig{
		Architecture: architecture,
		OS:           osName,
		RootFS:       rootFS{Type: "layers"},
	})
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	configDigest := digestBytes(configBytes)
	if err := installBlobBytes(layout, configDigest, configBytes); err != nil {
		t.Fatalf("write config blob: %v", err)
	}
	manifestBytes, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config: descriptor{
			MediaType: configMediaType,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
	})
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	manifestDigest := digestBytes(manifestBytes)
	if err := installBlobBytes(layout, manifestDigest, manifestBytes); err != nil {
		t.Fatalf("write manifest blob: %v", err)
	}
	return descriptor{
		MediaType: manifestMediaType,
		Digest:    manifestDigest,
		Size:      int64(len(manifestBytes)),
	}
}

func TestPlatformListingLocalDarwinImage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MACKER_HOME", home)
	ref, err := parseImageRef("hello-darwin:latest")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	layout := imageLayoutPath(home, ref)
	if err := buildImage(buildOptions{
		RootFS:       t.TempDir(),
		Output:       layout,
		Tag:          ref.Tag,
		Architecture: "arm64",
		Entrypoint:   "/app/hello",
		Quiet:        true,
	}); err != nil {
		t.Fatalf("build image: %v", err)
	}
	if err := registerImageState(home, ref, layout); err != nil {
		t.Fatalf("register image: %v", err)
	}

	listing, err := listImagePlatforms(ref)
	if err != nil {
		t.Fatalf("list platforms: %v", err)
	}
	if listing.Source != "local" {
		t.Fatalf("source = %q, want local", listing.Source)
	}
	if len(listing.Platforms) != 1 || listing.Platforms[0].OS != "darwin" || listing.Platforms[0].Architecture != "arm64" {
		t.Fatalf("platforms = %+v, want one darwin/arm64", listing.Platforms)
	}
	// The command only claims runnability when the host itself is Darwin.
	wantRunnable := runtime.GOOS == "darwin"
	if listing.HasRunnable != wantRunnable {
		t.Fatalf("runnable = %v, want %v on %s", listing.HasRunnable, wantRunnable, runtime.GOOS)
	}

	tableOutput := captureStdout(t, func() {
		if err := printPlatformListing(listing, "table"); err != nil {
			t.Errorf("print table: %v", err)
		}
	})
	if !strings.Contains(tableOutput, "platforms: darwin/arm64") {
		t.Fatalf("table output missing platform list:\n%s", tableOutput)
	}
	jsonOutput := captureStdout(t, func() {
		if err := printPlatformListing(listing, "json"); err != nil {
			t.Errorf("print json: %v", err)
		}
	})
	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonOutput), &payload); err != nil {
		t.Fatalf("decode json output: %v\n%s", err, jsonOutput)
	}
	if payload["source"] != "local" {
		t.Fatalf("json source = %v, want local", payload["source"])
	}
}

func TestPlatformListingLocalLinuxOnlyImage(t *testing.T) {
	layout := t.TempDir()
	nested := imageIndex{
		SchemaVersion: 2,
		MediaType:     indexMediaType,
		Manifests: []descriptor{
			writeFakeManifestBlob(t, layout, "linux", "amd64"),
			writeFakeManifestBlob(t, layout, "linux", "arm64"),
			{
				// BuildKit records attestations in the same index with an
				// unknown platform; they are not executable platforms.
				MediaType: "application/vnd.attest.test.attestation",
				Digest:    "sha256:" + strings.Repeat("c", 64),
				Size:      1,
				Platform:  &platform{OS: "unknown", Architecture: "unknown"},
			},
		},
	}
	nestedBytes, err := json.Marshal(nested)
	if err != nil {
		t.Fatalf("encode nested index: %v", err)
	}
	nestedDigest := digestBytes(nestedBytes)
	if err := installBlobBytes(layout, nestedDigest, nestedBytes); err != nil {
		t.Fatalf("write nested index blob: %v", err)
	}
	writeFakeLayout(t, layout, imageIndex{
		SchemaVersion: 2,
		Manifests: []descriptor{{
			MediaType: indexMediaType,
			Digest:    nestedDigest,
			Size:      int64(len(nestedBytes)),
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": "busybox:1.36",
			},
		}},
	})

	root, err := readLayoutRootIndex(layout)
	if err != nil {
		t.Fatalf("read root index: %v", err)
	}
	platforms, err := collectImagePlatforms(layout, root, 1)
	if err != nil {
		t.Fatalf("collect platforms: %v", err)
	}
	got := platformListText(platforms)
	if got != "linux/amd64, linux/arm64" {
		t.Fatalf("platforms = %q, want linux/amd64, linux/arm64", got)
	}

	listing := newPlatformListing(imageRef{Normalized: "docker.io/library/busybox:1.36"}, "local", platforms)
	if listing.HasRunnable {
		t.Fatalf("linux-only image reported runnable on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	reason := platformUnsuitableReason(listing)
	if runtime.GOOS == "darwin" {
		if !strings.Contains(reason, "Macker needs a darwin/"+runtime.GOARCH+" manifest") {
			t.Fatalf("reason = %q, want a darwin manifest requirement", reason)
		}
		if !strings.Contains(reason, "scratch") {
			t.Fatalf("reason = %q, want the scratch alternative", reason)
		}
	} else if !strings.Contains(reason, "Macker runs workloads on macOS") {
		t.Fatalf("reason = %q, want the non-Darwin host explanation", reason)
	}
}

func TestPlatformListingLocalSingleManifestWithSelector(t *testing.T) {
	layout := t.TempDir()
	selector := writeFakeManifestBlob(t, layout, "linux", "arm64")
	writeFakeLayout(t, layout, imageIndex{
		SchemaVersion: 2,
		Manifests: []descriptor{{
			MediaType: manifestMediaType,
			Digest:    selector.Digest,
			Size:      selector.Size,
			Platform:  &platform{OS: "linux", Architecture: "arm64"},
		}},
	})
	root, err := readLayoutRootIndex(layout)
	if err != nil {
		t.Fatalf("read root index: %v", err)
	}
	platforms, err := collectImagePlatforms(layout, root, 1)
	if err != nil {
		t.Fatalf("collect platforms: %v", err)
	}
	if got := platformListText(platforms); got != "linux/arm64" {
		t.Fatalf("platforms = %q, want linux/arm64", got)
	}
}

func TestPlatformListingRejectsDeeperNesting(t *testing.T) {
	layout := t.TempDir()
	deep := imageIndex{
		SchemaVersion: 2,
		MediaType:     indexMediaType,
		Manifests: []descriptor{{
			MediaType: indexMediaType,
			Digest:    "sha256:" + strings.Repeat("a", 64),
			Size:      1,
		}},
	}
	deepBytes, err := json.Marshal(deep)
	if err != nil {
		t.Fatalf("encode deep index: %v", err)
	}
	deepDigest := digestBytes(deepBytes)
	if err := installBlobBytes(layout, deepDigest, deepBytes); err != nil {
		t.Fatalf("write deep index blob: %v", err)
	}
	platforms, err := collectImagePlatforms(layout, imageIndex{
		SchemaVersion: 2,
		Manifests: []descriptor{{
			MediaType: indexMediaType,
			Digest:    deepDigest,
			Size:      int64(len(deepBytes)),
		}},
	}, maxPlatformSearchDepth+1)
	if err == nil {
		t.Fatalf("expected a nesting depth error, got platforms %+v", platforms)
	}
	if !strings.Contains(err.Error(), "nests deeper") {
		t.Fatalf("error = %v, want a nesting depth error", err)
	}
}

func TestCommandPlatformsRemoteAndPullPreflight(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MACKER_HOME", home)
	fake := filepath.Join(home, "skopeo")
	indexJSON := strings.Join([]string{
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaa","size":10,"platform":{"os":"linux","architecture":"arm64"}},`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbb","size":10,"platform":{"os":"linux","architecture":"amd64"}}]}`,
	}, "")
	fakeScript := strings.Join([]string{
		"#!/bin/sh",
		"case \"$1\" in",
		"  inspect) cat <<'JSON'",
		indexJSON,
		"JSON",
		"    ;;",
		"  copy) printf 'copy must not run\\n' >&2; exit 9 ;;",
		"  *) printf 'unexpected arguments\\n' >&2; exit 8 ;;",
		"esac",
		"",
	}, "\n")
	if err := os.WriteFile(fake, []byte(fakeScript), 0o755); err != nil {
		t.Fatalf("write fake skopeo: %v", err)
	}
	t.Setenv("MACKER_SKOPEO", fake)

	output := captureStdout(t, func() {
		if err := commandPlatforms([]string{"busybox:1.36"}); err != nil {
			t.Errorf("commandPlatforms: %v", err)
		}
	})
	if !strings.Contains(output, "docker.io/library/busybox:1.36 (registry)") {
		t.Fatalf("output missing remote reference:\n%s", output)
	}
	if !strings.Contains(output, "platforms: linux/amd64, linux/arm64") {
		t.Fatalf("output missing platform list:\n%s", output)
	}
	if !strings.Contains(output, "runnable here ("+runtime.GOOS+"/"+runtime.GOARCH+"): no") {
		t.Fatalf("output missing runnability line:\n%s", output)
	}
	if !strings.Contains(output, "reason: ") {
		t.Fatalf("output missing a reason line:\n%s", output)
	}

	err := commandPull([]string{"busybox:1.36"})
	if err == nil {
		t.Fatalf("expected pull to reject a Linux-only image")
	}
	if !strings.Contains(err.Error(), "cannot pull docker.io/library/busybox:1.36") {
		t.Fatalf("pull error = %v, want a cannot-pull explanation", err)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(err.Error(), "Macker needs a darwin/"+runtime.GOARCH+" manifest") {
		t.Fatalf("pull error = %v, want the Darwin-node explanation", err)
	}
	if runtime.GOOS != "darwin" && !strings.Contains(err.Error(), "Macker runs workloads on macOS") {
		t.Fatalf("pull error = %v, want the non-Darwin host explanation", err)
	}

	err = commandPlatforms([]string{"scratch"})
	if err == nil || !strings.Contains(err.Error(), "implicit empty Darwin rootfs") {
		t.Fatalf("scratch platforms error = %v, want an implicit scratch explanation", err)
	}
	if err := commandPlatforms([]string{"--format", "yaml", "busybox:1.36"}); err == nil {
		t.Fatalf("expected an unknown format error")
	}
}

func TestCommandPlatformsLocalScratchIsNotRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MACKER_HOME", home)
	t.Setenv("MACKER_SKOPEO", "/nonexistent/skopeo")
	err := commandPlatforms([]string{"scratch:latest"})
	if err == nil || !strings.Contains(err.Error(), "implicit empty Darwin rootfs") {
		t.Fatalf("error = %v, want an implicit scratch explanation", err)
	}
}
