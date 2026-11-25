package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
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
	indexJSON := strings.Join([]string{
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaa","size":10,"platform":{"os":"linux","architecture":"arm64"}},`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbb","size":10,"platform":{"os":"linux","architecture":"amd64"}}]}`,
	}, "")
	fake, calls := fakeSkopeoForPlatforms(t, indexJSON, `{}`)
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
	if got := readCommandCalls(t, calls); !reflect.DeepEqual(got, []string{"inspect --raw docker://docker.io/library/busybox:1.36"}) {
		t.Fatalf("skopeo calls = %q, want one raw-manifest inspect", got)
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

// fakeSkopeoForPlatforms writes a skopeo stand-in that reproduces how skopeo
// 1.25.2 handles its inspect arguments, which is what hid the `--format raw`
// defect in the first round of tests: only `--raw` writes the stored manifest
// verbatim, and every other inspect form takes the parsed path, which selects
// an instance matching the host platform and fails a Linux-only image on a Mac
// node with upstream's "no image found in image index" error.
func fakeSkopeoForPlatforms(t *testing.T, indexJSON, configJSON string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "skopeo")
	calls := filepath.Join(dir, "calls")
	upstreamFailure := `level=fatal msg="Error parsing manifest for image: choosing image instance: no image found in image index for architecture \"arm64\", variant \"v8\", OS \"darwin\""`
	script := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "$*" >> ` + strconv.Quote(calls),
		`if [ "$1" = inspect ] && [ "$2" = --raw ] && [ $# -eq 3 ]; then`,
		"  cat <<'INDEX'",
		indexJSON,
		"INDEX",
		"  exit 0",
		"fi",
		`if [ "$1" = inspect ] && [ "$2" = --config ] && [ "$3" = --raw ] && [ $# -eq 4 ]; then`,
		"  cat <<'CONFIG'",
		configJSON,
		"CONFIG",
		"  exit 0",
		"fi",
		// Upstream's own failure shape, for arguments skopeo would send down the
		// instance-selecting path.
		`printf '%s\n' ` + strconv.Quote(upstreamFailure) + " >&2",
		"exit 1",
		"",
	}, "\n")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake skopeo: %v", err)
	}
	return binary, calls
}

func readCommandCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read recorded calls: %v", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestRemotePlatformsDoesNotAskSkopeoToSelectAnInstance is the regression test
// for the defect home-ops measured on icarus-2.local with skopeo 1.25.2: a
// Linux-only remote image made `macker platforms` fail with skopeo's own
// instance-selection error instead of printing the platform list.
func TestRemotePlatformsDoesNotAskSkopeoToSelectAnInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MACKER_HOME", home)
	indexJSON := strings.Join([]string{
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:1111","size":10,"platform":{"os":"linux","architecture":"386"}},`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:2222","size":10,"platform":{"os":"linux","architecture":"amd64"}},`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:3333","size":10,"platform":{"os":"linux","architecture":"arm64"},`,
		`"annotations":{"vnd.docker.reference.type":"attestation_manifest"}},`,
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:4444","size":10,"platform":{"os":"unknown","architecture":"unknown"}}]}`,
	}, "")
	fake, calls := fakeSkopeoForPlatforms(t, indexJSON, `{}`)
	t.Setenv("MACKER_SKOPEO", fake)

	output := captureStdout(t, func() {
		if err := commandPlatforms([]string{"busybox:1.36"}); err != nil {
			t.Errorf("commandPlatforms must succeed for a Linux-only remote image: %v", err)
		}
	})
	if !strings.Contains(output, "platforms: linux/386, linux/amd64, linux/arm64") {
		t.Fatalf("output missing the full platform list:\n%s", output)
	}
	if strings.Contains(output, "unknown/unknown") {
		t.Fatalf("output reported attestation entries as platforms:\n%s", output)
	}
	if !strings.Contains(output, "runnable here ("+runtime.GOOS+"/"+runtime.GOARCH+"): no") {
		t.Fatalf("output missing the runnability line:\n%s", output)
	}
	if got := readCommandCalls(t, calls); !reflect.DeepEqual(got, []string{"inspect --raw docker://docker.io/library/busybox:1.36"}) {
		t.Fatalf("skopeo calls = %q, want exactly one `inspect --raw`", got)
	}
}

func TestRemotePlatformsReportsSingleManifestPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MACKER_HOME", home)
	manifestJSON := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaa","size":2},` +
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:bbbb","size":2}]}`
	fake, calls := fakeSkopeoForPlatforms(t, manifestJSON, `{"architecture":"amd64","os":"linux"}`)
	t.Setenv("MACKER_SKOPEO", fake)

	output := captureStdout(t, func() {
		if err := commandPlatforms([]string{"busybox:1.36"}); err != nil {
			t.Errorf("commandPlatforms: %v", err)
		}
	})
	if !strings.Contains(output, "platforms: linux/amd64") {
		t.Fatalf("output missing the single-manifest platform:\n%s", output)
	}
	want := []string{
		"inspect --raw docker://docker.io/library/busybox:1.36",
		"inspect --config --raw docker://docker.io/library/busybox:1.36",
	}
	if got := readCommandCalls(t, calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("skopeo calls = %q, want %q", got, want)
	}
}

func TestRemotePlatformsFailureNamesTheRawInspect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MACKER_HOME", home)
	t.Setenv("MACKER_SKOPEO", "/bin/false")
	err := commandPlatforms([]string{"busybox:1.36"})
	if err == nil {
		t.Fatal("expected a failure when skopeo cannot read the manifest")
	}
	message := err.Error()
	if !strings.Contains(message, "read the manifest of docker.io/library/busybox:1.36") {
		t.Fatalf("error = %q, want it to name the manifest it could not read", message)
	}
	if !strings.Contains(message, "skopeo inspect --raw docker://docker.io/library/busybox:1.36") {
		t.Fatalf("error = %q, want it to name the exact raw inspect an operator can repeat", message)
	}
}

// TestRemotePlatformsLiveRegistry is the integration check that synthetic
// layouts and a permissive fake skopeo could not provide: it runs against a
// real registry with a real skopeo. Set MACKER_TEST_NETWORK=1 on a machine with
// skopeo installed and network access to run it.
func TestRemotePlatformsLiveRegistry(t *testing.T) {
	if os.Getenv("MACKER_TEST_NETWORK") == "" {
		t.Skip("set MACKER_TEST_NETWORK=1 to query a real registry with a real skopeo")
	}
	skopeo, err := exec.LookPath("skopeo")
	if err != nil {
		t.Skipf("this machine has no skopeo: %v", err)
	}
	t.Setenv("MACKER_SKOPEO", skopeo)
	t.Setenv("MACKER_HOME", t.TempDir())

	for _, reference := range []string{"busybox:1.36", "python:3.12-slim"} {
		ref, err := parseImageRef(reference)
		if err != nil {
			t.Fatalf("parse %s: %v", reference, err)
		}
		listing, err := listImagePlatforms(ref)
		if err != nil {
			t.Fatalf("list %s platforms: %v", reference, err)
		}
		if listing.Source != "registry" {
			t.Fatalf("%s source = %q, want registry", reference, listing.Source)
		}
		if len(listing.Platforms) < 2 {
			t.Fatalf("%s platforms = %+v, want a multi-platform list", reference, listing.Platforms)
		}
		linuxSeen := false
		for _, candidate := range listing.Platforms {
			if candidate.OS == "linux" {
				linuxSeen = true
			}
			if candidate.OS == "" || candidate.Architecture == "" {
				t.Fatalf("%s reported an incomplete platform %+v", reference, candidate)
			}
		}
		if !linuxSeen {
			t.Fatalf("%s platforms = %+v, want at least one linux manifest", reference, listing.Platforms)
		}
		if listing.HasRunnable {
			t.Fatalf("%s was reported runnable on %s/%s", reference, listing.HostOS, listing.HostArch)
		}
	}
}
