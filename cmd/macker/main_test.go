package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestParsePortMapping(t *testing.T) {
	tests := []struct {
		input   string
		want    portMapping
		wantErr bool
	}{
		{input: "80:30080", want: portMapping{HostPort: 80, NodePort: 30080, Protocol: "tcp"}},
		{input: "53:30553/UDP", want: portMapping{HostPort: 53, NodePort: 30553, Protocol: "udp"}},
		{input: "1:65535/tcp", want: portMapping{HostPort: 1, NodePort: 65535, Protocol: "tcp"}},
		{input: "0:30080", wantErr: true},
		{input: "80:65536", wantErr: true},
		{input: "80:30080/sctp", wantErr: true},
		{input: "80", wantErr: true},
		{input: "80:30080/tcp/udp", wantErr: true},
	}
	for _, test := range tests {
		got, err := parsePortMapping(test.input)
		if test.wantErr {
			if err == nil {
				t.Fatalf("parsePortMapping(%q) succeeded, want error", test.input)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parsePortMapping(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("parsePortMapping(%q) = %#v, want %#v", test.input, got, test.want)
		}
	}
}

func TestBuildPFRulesIncludesTCPAndUDP(t *testing.T) {
	rules := buildPFRules("172.31.89.1", []portMapping{
		{HostPort: 80, NodePort: 30080, Protocol: "tcp"},
		{HostPort: 53, NodePort: 30553, Protocol: "udp"},
	})
	want := []string{
		"rdr pass inet proto tcp from any to any port = 80 -> 172.31.89.1 port 30080",
		"rdr pass inet proto udp from any to any port = 53 -> 172.31.89.1 port 30553",
	}
	for _, line := range want {
		if !strings.Contains(rules, line) {
			t.Fatalf("PF rules do not contain %q:\n%s", line, rules)
		}
	}
}

func TestParsePFToken(t *testing.T) {
	got, err := parsePFToken([]byte("pf enabled\nToken : 123456789\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "123456789" {
		t.Fatalf("token = %q", got)
	}
	if _, err := parsePFToken([]byte("pf enabled\n")); err == nil {
		t.Fatal("expected missing-token error")
	}
}

func TestNetworkEnvironmentArgs(t *testing.T) {
	config := networkConfig{
		HostInterface: "bridge88",
		HostIP:        "172.31.88.1",
		Interface:     "bridge881",
		IP:            "172.31.89.1",
	}
	got := networkEnvironmentArgs(config, []portMapping{
		{HostPort: 80, NodePort: 32768, Protocol: "tcp"},
		{HostPort: 53, NodePort: 32769, Protocol: "udp"},
	})
	want := []string{
		"--env", "MACKER_INTERFACE=bridge881",
		"--env", "MACKER_IP=172.31.89.1",
		"--env", "MACKER_HOST_INTERFACE=bridge88",
		"--env", "MACKER_HOST_IP=172.31.88.1",
		"--env", "MACKER_PORT_1=32768",
		"--env", "MACKER_PORT_2=32769",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment args = %#v, want %#v", got, want)
	}
	if got := formatNetworkConfig(&config); got != "bridge881:172.31.89.1" {
		t.Fatalf("network = %q", got)
	}
	if got := formatNetworkConfig(nil); got != "" {
		t.Fatalf("nil network = %q", got)
	}

	external := networkConfig{Interface: "bridge101", IP: "10.42.1.3"}
	got = networkEnvironmentArgs(external, nil)
	want = []string{
		"--env", "MACKER_INTERFACE=bridge101",
		"--env", "MACKER_IP=10.42.1.3",
		"--env", "MACKER_HOST_INTERFACE=",
		"--env", "MACKER_HOST_IP=",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("external environment args = %#v, want %#v", got, want)
	}
}

func TestCheckPublishedPortConflicts(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "existing")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:     "existing",
		Ports:    []portMapping{{HostPort: 80, NodePort: 30080, Protocol: "tcp"}},
		PFAnchor: pfAnchorForContainer("existing"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := registerContainerState(home, "existing"); err != nil {
		t.Fatal(err)
	}
	if err := checkPublishedPortConflicts(home, "new", []portMapping{{HostPort: 80, NodePort: 30081, Protocol: "tcp"}}); err == nil {
		t.Fatal("expected published-port conflict")
	}
	if err := checkPublishedPortConflicts(home, "new", []portMapping{{HostPort: 80, NodePort: 30081, Protocol: "udp"}}); err != nil {
		t.Fatalf("different protocol should not conflict: %v", err)
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

func TestInspectJSONRefreshesPersistedExitInfo(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "demo")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Unix(10, 0).UTC()
	finishedAt := time.Unix(20, 0).UTC()
	code := 7
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:      "demo",
		Image:     "docker.io/library/demo:latest",
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessExitInfo(filepath.Join(containerDir, "exit.json"), processExitInfo{
		PID:               42,
		ExitCode:          &code,
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
		TerminationSignal: "",
		TerminationReason: "exited-with-error",
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
	if inspection.Status != "exited" || inspection.PID != 0 || inspection.WorkloadPID != 42 || inspection.ExitCode == nil || *inspection.ExitCode != 7 {
		t.Fatalf("inspection = %#v", inspection)
	}
	if inspection.StartedAt == nil || inspection.FinishedAt == nil || inspection.TerminationReason != "exited-with-error" {
		t.Fatalf("inspection timestamps/termination = %#v", inspection)
	}
	metadataBytes, err := os.ReadFile(filepath.Join(containerDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata containerMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ExitPID != 42 || metadata.FinishedAt == nil {
		t.Fatalf("metadata was not refreshed: %#v", metadata)
	}
}

func TestLogsReadsCapturedOutput(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "demo")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(containerDir, "run.log")
	if err := os.WriteFile(logPath, []byte("stdout\nstderr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeContainerMetadata(containerDir, containerMetadata{Name: "demo", LogPath: logPath}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_HOME", home)
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	commandErr := commandLogs([]string{"demo"})
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
	if string(output) != "stdout\nstderr\n" {
		t.Fatalf("logs output = %q", output)
	}
}

func TestExpandInteractiveShortFlags(t *testing.T) {
	got := expandInteractiveShortFlags([]string{"-it", "demo", "--", "-ti", "value"})
	want := []string{"-i", "-t", "demo", "--", "-ti", "value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded flags = %#v, want %#v", got, want)
	}
}

func TestRemoveContainerRemovesStorageAndState(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "demo")
	if err := os.MkdirAll(filepath.Join(containerDir, "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "run.log"), []byte("output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeContainerMetadata(containerDir, containerMetadata{Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := registerContainerState(home, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := removeContainer(home, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(containerDir); !os.IsNotExist(err) {
		t.Fatalf("container directory still exists: %v", err)
	}
	if err := withState(home, func(state *mackerState) error {
		if _, ok := state.Containers["demo"]; ok {
			t.Fatal("removed container remains in state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPSRemovesExitedAutoRemoveContainer(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "auto")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	process := exec.Command("/bin/sh", "-c", "exit 0")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	pid := process.Process.Pid
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:       "auto",
		PID:        pid,
		AutoRemove: true,
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := registerContainerState(home, "auto"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_HOME", home)
	if err := commandPS([]string{"--all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(containerDir); !os.IsNotExist(err) {
		t.Fatalf("auto-remove container directory still exists: %v", err)
	}
}

func TestContainerStatusPendingForegroundIsRunning(t *testing.T) {
	status, err := containerStatus(containerMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("pending container status = %q, want running", status)
	}
	stoppedAt := time.Now().UTC()
	status, err = containerStatus(containerMetadata{StoppedAt: &stoppedAt})
	if err != nil {
		t.Fatal(err)
	}
	if status != "stopped" {
		t.Fatalf("stopped container status = %q, want stopped", status)
	}
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
		Env:       []string{"FOO=bar", "MULTI=one"},
		CreatedAt: time.Now().UTC(),
	}
	if err := writeContainerMetadata(containerDir, metadata); err != nil {
		t.Fatal(err)
	}
	if err := registerContainerState(home, "demo"); err != nil {
		t.Fatal(err)
	}
	metadataData, err := os.ReadFile(filepath.Join(containerDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip containerMetadata
	if err := json.Unmarshal(metadataData, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.Env, metadata.Env) {
		t.Fatalf("metadata env = %#v, want %#v", roundTrip.Env, metadata.Env)
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
