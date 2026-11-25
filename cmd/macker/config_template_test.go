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

// TestSubstituteMackerConfigNamesTheMissingPortFlag is the message a node agent
// sees when an image bakes a port token (example/nginx/nginx.conf carries
// `listen ____MACKER_PORT_1____`) and the run published no ports. The refusal
// is correct; it is only useful if it names the flag shape that supplies the
// value, which is what unblocks a caller that cannot see macker's internals.
func TestSubstituteMackerConfigNamesTheMissingPortFlag(t *testing.T) {
	rootfs := t.TempDir()
	filename := filepath.Join(rootfs, "nginx.conf")
	if err := os.WriteFile(filename, []byte("listen ____MACKER_PORT_1____;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := substituteMackerConfig(rootfs, networkEnvironmentValues(networkConfig{
		Interface:     "bridge881",
		IP:            "172.31.89.1",
		HostInterface: "bridge88",
		HostIP:        "172.31.88.1",
	}, nil))
	if err == nil {
		t.Fatal("expected an unresolved-token error when no ports are published")
	}
	message := err.Error()
	for _, want := range []string{
		"token ____MACKER_PORT_1____ has no runtime value",
		"MACKER_PORT_1 is filled from the first published port",
		"-p HOST_PORT:NODE_PORT[/tcp|/udp]",
		"--env MACKER_PORT_1=PORT",
		"values this run does provide: MACKER_HOST_INTERFACE, MACKER_HOST_IP, MACKER_INTERFACE, MACKER_IP",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error = %q, want it to contain %q", message, want)
		}
	}
	assertFileContent(t, filename, "listen ____MACKER_PORT_1____;\n")
}

func TestSubstituteMackerConfigListsEveryMissingTokenOnce(t *testing.T) {
	rootfs := t.TempDir()
	filename := filepath.Join(rootfs, "service.conf")
	if err := os.WriteFile(filename, []byte("listen ____MACKER_PORT_1____;\nproxy ____MACKER_PORT_2____ ____MACKER_PORT_1____\nlisten_addr ____MACKER_LISTEN____;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := substituteMackerConfig(rootfs, map[string]string{"MACKER_IP": "172.31.89.1"})
	if err == nil {
		t.Fatal("expected an unresolved-token error")
	}
	message := err.Error()
	if !strings.Contains(message, "tokens ____MACKER_PORT_1____, ____MACKER_PORT_2____, ____MACKER_LISTEN____ have no runtime value") {
		t.Fatalf("error = %q, want every missing token listed once", message)
	}
	if strings.Count(message, "____MACKER_PORT_1____") != 1 {
		t.Fatalf("error = %q, want MACKER_PORT_1 listed once although it appears twice", message)
	}
	if !strings.Contains(message, "MACKER_LISTEN is not a value Macker derives from a run, so supply it with --env MACKER_LISTEN=VALUE") {
		t.Fatalf("error = %q, want a non-port token explained as an --env value", message)
	}
}

// TestPublishedPortFillsPortToken pins the contract maclet must satisfy: the
// node port of the first -p mapping is what MACKER_PORT_1 resolves to, in flag
// order, and a run without a published port cannot resolve it.
func TestPublishedPortFillsPortToken(t *testing.T) {
	rootfs := t.TempDir()
	filename := filepath.Join(rootfs, "nginx.conf")
	if err := os.WriteFile(filename, []byte("listen ____MACKER_PORT_1____;\nbackup ____MACKER_PORT_2____;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replaced, err := substituteMackerConfig(rootfs, networkEnvironmentValues(networkConfig{
		Interface: "bridge881",
		IP:        "172.31.89.1",
	}, []portMapping{
		{HostPort: 18080, NodePort: 8080, Protocol: "tcp"},
		{HostPort: 18443, NodePort: 8443, Protocol: "tcp"},
	}))
	if err != nil {
		t.Fatalf("publishing two ports must resolve both port tokens: %v", err)
	}
	if replaced != 1 {
		t.Fatalf("replaced files = %d, want 1", replaced)
	}
	assertFileContent(t, filename, "listen 8080;\nbackup 8443;\n")
}
