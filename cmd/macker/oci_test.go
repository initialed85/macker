package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestEnsureTerminalEnvironment(t *testing.T) {
	t.Setenv("TERM", "screen-256color")
	got := ensureTerminalEnvironment([]string{"PATH=/bin"})
	if environmentValue(got, "TERM") != "screen-256color" {
		t.Fatalf("TERM = %q, want caller TERM", environmentValue(got, "TERM"))
	}
	if environmentValue(ensureTerminalEnvironment([]string{"TERM=custom"}), "TERM") != "custom" {
		t.Fatal("explicit TERM was overwritten")
	}

	t.Setenv("TERM", "")
	got = ensureTerminalEnvironment([]string{"PATH=/bin"})
	if environmentValue(got, "TERM") != "xterm-256color" {
		t.Fatalf("fallback TERM = %q, want xterm-256color", environmentValue(got, "TERM"))
	}
}

func TestSelectImageCommandEntrypointOverride(t *testing.T) {
	got := selectImageCommand("bash", []string{"/usr/local/bin/nginx"}, []string{"--serve"}, nil, []string{"-c", "env"})
	want := []string{"bash", "-c", "env"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}

	got = selectImageCommand("bash", []string{"/usr/local/bin/nginx"}, []string{"--serve"}, nil, nil)
	want = []string{"bash", "--serve"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command without override = %#v, want %#v", got, want)
	}

	got = selectImageCommand("", []string{"/usr/local/bin/nginx"}, []string{"--serve"}, nil, []string{"/custom", "arg"})
	want = []string{"/custom", "arg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command override = %#v, want %#v", got, want)
	}
}

func TestBuildAndUnpackRoundTrip(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "app", "hello"), []byte("native workload\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello", filepath.Join(rootfs, "app", "current")); err != nil {
		t.Fatal(err)
	}

	layout := filepath.Join(t.TempDir(), "image")
	if err := buildImage(buildOptions{
		RootFS:       rootfs,
		Output:       layout,
		Tag:          "test:latest",
		Architecture: runtime.GOARCH,
		Entrypoint:   "/app/hello",
		WorkingDir:   "/",
	}); err != nil {
		t.Fatal(err)
	}

	image, err := loadImage(layout, "test:latest")
	if err != nil {
		t.Fatal(err)
	}
	if image.Config.OS != "darwin" || image.Config.Architecture != runtime.GOARCH {
		t.Fatalf("unexpected platform: %s/%s", image.Config.OS, image.Config.Architecture)
	}
	if len(image.Manifest.Layers) != 1 {
		t.Fatalf("expected one layer, got %d", len(image.Manifest.Layers))
	}

	unpacked := filepath.Join(t.TempDir(), "rootfs")
	if err := unpackImage(image, unpacked, false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(unpacked, "app", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "native workload\n" {
		t.Fatalf("unexpected file content %q", content)
	}
	link, err := os.Readlink(filepath.Join(unpacked, "app", "current"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "hello" {
		t.Fatalf("unexpected symlink target %q", link)
	}
}

func TestApplyTarLayerWhiteouts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"remove":    "remove me",
		"keep":      "keep me",
		"dir/old":   "old",
		"dir/other": "other",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	layer := tarBytes(t, []tarEntry{
		{name: ".wh.remove", kind: tar.TypeReg},
		{name: "dir/.wh..wh..opq", kind: tar.TypeReg},
		{name: "dir/new", kind: tar.TypeReg, content: "new"},
	})
	if err := applyTarLayer(root, bytes.NewReader(layer)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(root, "remove")); !os.IsNotExist(err) {
		t.Fatalf("whiteout did not remove file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep")); err != nil {
		t.Fatalf("whiteout removed unrelated file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dir", "old")); !os.IsNotExist(err) {
		t.Fatalf("opaque whiteout did not remove old entry: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "dir", "new")); err != nil || string(content) != "new" {
		t.Fatalf("new entry missing after opaque whiteout: %q, %v", content, err)
	}
}

func TestApplyTarLayerRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "escaped-by-oci-test")
	_ = os.Remove(outside)
	defer os.Remove(outside)

	layer := tarBytes(t, []tarEntry{{name: "../escaped-by-oci-test", kind: tar.TypeReg, content: "nope"}})
	if err := applyTarLayer(root, bytes.NewReader(layer)); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside rootfs: %v", err)
	}
}

type tarEntry struct {
	name    string
	kind    byte
	content string
}

func tarBytes(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o644,
			Size:     int64(len(entry.content)),
			Typeflag: entry.kind,
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.content != "" {
			if _, err := io.WriteString(writer, entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
