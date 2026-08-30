package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubstituteMackerConfig(t *testing.T) {
	rootfs := t.TempDir()
	configDir := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "service.conf"), []byte("listen ____MACKER_IP____:____MACKER_PORT_1____;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "start.sh"), []byte("echo ____MACKER_INTERFACE____\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "source.go"), []byte("const address = \"____MACKER_IP____\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "binary.json"), append([]byte("____MACKER_IP____\x00"), 1, 2, 3), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.conf")
	if err := os.WriteFile(outside, []byte("____MACKER_IP____\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(configDir, "linked.conf")); err != nil {
		t.Fatal(err)
	}

	replaced, err := substituteMackerConfig(rootfs, map[string]string{
		"MACKER_INTERFACE": "bridge881",
		"MACKER_IP":        "172.31.89.1",
		"MACKER_PORT_1":    "8080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replaced != 2 {
		t.Fatalf("replaced files = %d, want 2", replaced)
	}
	assertFileContent(t, filepath.Join(configDir, "service.conf"), "listen 172.31.89.1:8080;\n")
	assertFileContent(t, filepath.Join(configDir, "start.sh"), "echo bridge881\n")
	assertFileContent(t, filepath.Join(configDir, "source.go"), "const address = \"____MACKER_IP____\"\n")
	binary, err := os.ReadFile(filepath.Join(configDir, "binary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binary, append([]byte("____MACKER_IP____\x00"), 1, 2, 3)) {
		t.Fatalf("binary-like file was modified: %q", binary)
	}
	assertFileContent(t, outside, "____MACKER_IP____\n")
}

func TestSubstituteMackerConfigRejectsUnsetToken(t *testing.T) {
	rootfs := t.TempDir()
	filename := filepath.Join(rootfs, "service.conf")
	if err := os.WriteFile(filename, []byte("listen ____MACKER_PORT_1____;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := substituteMackerConfig(rootfs, map[string]string{"MACKER_IP": "172.31.89.1"})
	if err == nil || !strings.Contains(err.Error(), "MACKER_PORT_1") {
		t.Fatalf("error = %v, want unset-token error", err)
	}
	assertFileContent(t, filename, "listen ____MACKER_PORT_1____;\n")
}

func TestMackerEnvironmentValues(t *testing.T) {
	got := mackerEnvironmentValues([]string{
		"PATH=/bin",
		"MACKER_IP=172.31.89.1",
		"MACKER_PORT_1=8080",
		"MACKER_EMPTY=",
		"INVALID",
	})
	want := map[string]string{
		"MACKER_IP":     "172.31.89.1",
		"MACKER_PORT_1": "8080",
		"MACKER_EMPTY":  "",
	}
	if len(got) != len(want) {
		t.Fatalf("values = %#v, want %#v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func assertFileContent(t *testing.T, filename, want string) {
	t.Helper()
	got, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", filename, got, want)
	}
}
