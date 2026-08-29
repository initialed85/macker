package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseImageRefDefaultsDockerHub(t *testing.T) {
	ref, err := parseImageRef("initialed85/nginx-darwin:latest")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Normalized != "docker.io/initialed85/nginx-darwin:latest" {
		t.Fatalf("normalized reference = %q", ref.Normalized)
	}
	if ref.Tag != "latest" {
		t.Fatalf("tag = %q", ref.Tag)
	}
}

func TestParseImageRefAddsDockerHubLibrary(t *testing.T) {
	ref, err := parseImageRef("nginx-darwin")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Normalized != "docker.io/library/nginx-darwin:latest" {
		t.Fatalf("normalized reference = %q", ref.Normalized)
	}
}

func TestParseImageRefKeepsExplicitRegistry(t *testing.T) {
	ref, err := parseImageRef("ghcr.io/example/nginx-darwin:v1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Normalized != "ghcr.io/example/nginx-darwin:v1" {
		t.Fatalf("normalized reference = %q", ref.Normalized)
	}
}

func TestParseMackerfile(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "Mackerfile")
	contents := `FROM scratch
COPY . / \
    
ENV FOO=bar
WORKDIR /srv
ENTRYPOINT ["/bin/app"]
CMD ["--serve"]
`
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := parseMackerfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Copies) != 1 || len(spec.Copies[0].Sources) != 1 || spec.Copies[0].Sources[0] != "." {
		t.Fatalf("copies = %#v", spec.Copies)
	}
	if spec.WorkingDir != "/srv" {
		t.Fatalf("working directory = %q", spec.WorkingDir)
	}
	if len(spec.Env) != 1 || spec.Env[0] != "FOO=bar" {
		t.Fatalf("environment = %#v", spec.Env)
	}
	if got, want := spec.Entrypoint[0], "/bin/app"; got != want {
		t.Fatalf("entrypoint = %q, want %q", got, want)
	}
}

func TestCopyDotCopiesContextContents(t *testing.T) {
	contextDir := t.TempDir()
	rootfs := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	instruction := copyInstruction{Sources: []string{"."}, Destination: "/"}
	if err := applyCopyInstruction(contextDir, rootfs, instruction); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(rootfs, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("payload = %q", data)
	}
}

func TestParseVolume(t *testing.T) {
	host := t.TempDir()
	mount, err := parseVolume(host + ":/usr/share/nginx/html")
	if err != nil {
		t.Fatal(err)
	}
	if mount.HostPath != host || mount.ContainerPath != "/usr/share/nginx/html" {
		t.Fatalf("volume = %#v", mount)
	}
}

func TestInstallVolumeUsesLiveSymlink(t *testing.T) {
	rootfs := t.TempDir()
	host := t.TempDir()
	mount := volume{HostPath: host, ContainerPath: "/data"}
	if err := installVolume(rootfs, mount); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(rootfs, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if link != host {
		t.Fatalf("volume link = %q, want %q", link, host)
	}
}

func TestMergeBundleIndicesReplacesDarwinManifest(t *testing.T) {
	linux := testBundleDigest("linux")
	oldDarwin := testBundleDigest("old-darwin")
	newDarwin := testBundleDigest("new-darwin")
	source := bundleIndex{
		SchemaVersion: 2,
		Manifests: []bundleDescriptor{
			{Digest: linux, Platform: &bundlePlatform{OS: "linux", Architecture: "arm64"}},
			{Digest: oldDarwin, Platform: &bundlePlatform{OS: "darwin", Architecture: "arm64"}},
		},
	}
	merged, err := mergeBundleIndices(source, []bundleDescriptor{{
		Digest:   newDarwin,
		Platform: &bundlePlatform{OS: "darwin", Architecture: "arm64"},
	}}, t.TempDir(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Manifests) != 2 {
		t.Fatalf("merged manifests = %#v", merged.Manifests)
	}
	if merged.Manifests[0].Digest != linux || merged.Manifests[1].Digest != newDarwin {
		t.Fatalf("merged manifests = %#v", merged.Manifests)
	}
	if _, ok := merged.Manifests[0].Annotations["org.opencontainers.image.ref.name"]; !ok {
		t.Fatalf("merged index has no ref.name annotation: %#v", merged.Manifests[0])
	}
	if merged.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "latest" {
		t.Fatalf("ref.name = %q", merged.Manifests[0].Annotations["org.opencontainers.image.ref.name"])
	}
}

func TestMergeBundleLayoutsCopiesAndSelectsDarwin(t *testing.T) {
	source := t.TempDir()
	darwin := t.TempDir()
	output := filepath.Join(t.TempDir(), "merged")
	linuxBlob := writeTestBundleBlob(t, source, []byte("linux manifest"))
	darwinBlob := writeTestBundleBlob(t, darwin, []byte("darwin manifest"))
	writeTestBundleIndex(t, source, bundleIndex{
		SchemaVersion: 2,
		Manifests: []bundleDescriptor{{
			Digest:   linuxBlob,
			Size:     int64(len("linux manifest")),
			Platform: &bundlePlatform{OS: "linux", Architecture: "arm64"},
		}},
	})
	writeTestBundleIndex(t, darwin, bundleIndex{
		SchemaVersion: 2,
		Manifests: []bundleDescriptor{{
			Digest:   darwinBlob,
			Size:     int64(len("darwin manifest")),
			Platform: &bundlePlatform{OS: "darwin", Architecture: "arm64"},
		}},
	})

	if err := mergeBundleLayouts(source, darwin, output, "latest"); err != nil {
		t.Fatal(err)
	}
	merged, err := loadBundleIndex(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Manifests) != 2 {
		t.Fatalf("merged manifests = %#v", merged.Manifests)
	}
	for _, digest := range []string{linuxBlob, darwinBlob} {
		if _, err := readBundleBlob(output, digest); err != nil {
			t.Fatalf("read merged blob %s: %v", digest, err)
		}
	}
}

func TestBundleSourceForPushWrapsMultiPlatformLayout(t *testing.T) {
	layout := filepath.Join(t.TempDir(), "layout")
	if err := os.MkdirAll(filepath.Join(layout, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := writeTestBundleBlob(t, layout, []byte("first manifest"))
	second := writeTestBundleBlob(t, layout, []byte("second manifest"))
	writeTestBundleIndex(t, layout, bundleIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []bundleDescriptor{
			{Digest: first, Platform: &bundlePlatform{OS: "linux", Architecture: "amd64"}},
			{Digest: second, Platform: &bundlePlatform{OS: "darwin", Architecture: "arm64"}},
		},
	})

	pushSource, cleanup, err := bundleSourceForPush(filepath.Dir(layout), layout, "latest")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(pushSource, "oci:") || !strings.HasSuffix(pushSource, ":latest") {
		t.Fatalf("push source = %q", pushSource)
	}
	pushLayout := strings.TrimSuffix(strings.TrimPrefix(pushSource, "oci:"), ":latest")
	root, err := loadBundleIndex(pushLayout)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Manifests) != 1 || root.Manifests[0].MediaType != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("push root = %#v", root)
	}
	inner, err := readBundleBlob(pushLayout, root.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	var innerIndex bundleIndex
	if err := json.Unmarshal(inner, &innerIndex); err != nil {
		t.Fatal(err)
	}
	if len(innerIndex.Manifests) != 2 {
		t.Fatalf("push inner index = %#v", innerIndex)
	}
}

func writeTestBundleIndex(t *testing.T, layout string, index bundleIndex) {
	t.Helper()
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestBundleBlob(t *testing.T, layout string, data []byte) string {
	t.Helper()
	hash := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(hash[:])
	filename, err := bundleBlobPath(layout, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return digest
}

func testBundleDigest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(hash[:])
}

func TestContainerStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "demo")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := containerMetadata{
		Name:      "demo",
		Image:     "docker.io/library/demo:latest",
		Network:   "host",
		CreatedAt: time.Now().UTC(),
	}
	if err := writeContainerMetadata(containerDir, metadata); err != nil {
		t.Fatal(err)
	}
	if err := registerContainerState(home, "demo"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state mackerState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 {
		t.Fatalf("state version = %d", state.Version)
	}
	if _, ok := state.Containers["demo"]; !ok {
		t.Fatalf("state does not contain demo: %#v", state.Containers)
	}
	if _, err := os.Stat(filepath.Join(home, "state.lock")); err != nil {
		t.Fatalf("state lock was not created: %v", err)
	}

	if err := unregisterContainerState(home, "demo"); err != nil {
		t.Fatal(err)
	}
}
