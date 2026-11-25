package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePFAnchorRulesReadsCounters(t *testing.T) {
	// The shape `pfctl -a <anchor> -sn -v` produces on macOS 26.
	out := `No ALTQ support in kernel
ALTQ related functions disabled
rdr pass inet proto tcp from any to 10.42.6.6 port = 8090 -> 10.42.6.6 port 30364
  [ Evaluations: 17        Packets: 9          Bytes: 742         States: 1     ]
  [ Inserted: uid 0 pid 74342 ]
rdr pass inet proto udp from any to 10.42.6.6 port = 53 -> 10.42.6.6 port 30053
  [ Evaluations: 0         Packets: 0          Bytes: 0           States: 0     ]
  [ Inserted: uid 0 pid 74342 ]
`
	rules := parsePFAnchorRules(out)
	if len(rules) != 2 {
		t.Fatalf("rules = %#v", rules)
	}
	if rules[0].text != "rdr pass inet proto tcp from any to 10.42.6.6 port = 8090 -> 10.42.6.6 port 30364" {
		t.Fatalf("rule 0 text = %q", rules[0].text)
	}
	if rules[0].evaluations != 17 || rules[0].states != 1 {
		t.Fatalf("rule 0 counters = %#v", rules[0])
	}
	if rules[1].evaluations != 0 || rules[1].states != 0 {
		t.Fatalf("rule 1 counters = %#v", rules[1])
	}
}

func TestParsePFAnchorRulesReportsEmptyAnchor(t *testing.T) {
	// An anchor that exists but holds no rules is what a container whose
	// redirect was never installed (or already removed) leaves behind.
	out := `No ALTQ support in kernel
ALTQ related functions disabled
nat cleared
`
	if rules := parsePFAnchorRules(out); len(rules) != 0 {
		t.Fatalf("rules = %#v", rules)
	}
}

func TestDescribePortMappings(t *testing.T) {
	got := describePortMappings([]portMapping{
		{HostPort: 8090, NodePort: 30364, Protocol: "tcp"},
		{HostPort: 53, NodePort: 30053, Protocol: "udp"},
	})
	if got != "8090:30364/tcp,53:30053/udp" {
		t.Fatalf("describePortMappings = %q", got)
	}
	if describePortMappings(nil) != "" {
		t.Fatal("empty mapping list should describe as empty")
	}
}

func TestReadPFPortRedirectionsSkipsContainersWithoutAnAnchor(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "noports")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:    "noports",
		Image:   "example:latest",
		Network: "external",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := readPFPortRedirections(home, []string{"noports"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %#v", rows)
	}
	if _, err := readPFPortRedirections(home, []string{"missing"}); err == nil {
		t.Fatal("expected an error for an unknown container")
	}
}

func TestInspectJSONReportsThePFAnchor(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "web")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:          "web",
		Image:         "nginx:latest",
		Network:       "external",
		Ports:         []portMapping{{HostPort: 8090, NodePort: 30364, Protocol: "tcp"}},
		PFAnchor:      "com.apple/macker-1c712ee64166b62e1c5ba734351dca47",
		NetworkConfig: &networkConfig{Interface: "bridge101", IP: "10.42.6.6"},
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
	commandErr := commandInspect([]string{"--format", "json", "web"})
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
	if inspection.PFAnchor != "com.apple/macker-1c712ee64166b62e1c5ba734351dca47" {
		t.Fatalf("inspection pf_anchor = %q", inspection.PFAnchor)
	}
	if describePortMappings(inspection.Ports) != "8090:30364/tcp" {
		t.Fatalf("inspection ports = %#v", inspection.Ports)
	}
}

func TestPortsCommandWithoutPublishedPorts(t *testing.T) {
	home := t.TempDir()
	containerDir := filepath.Join(home, "containers", "noports")
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeContainerMetadata(containerDir, containerMetadata{
		Name:    "noports",
		Image:   "example:latest",
		Network: "external",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"version":1,"containers":{"noports":{}},"images":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_HOME", home)

	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	commandErr := commandPorts([]string{})
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
	if !strings.Contains(string(output), "No published ports with a PF anchor.") {
		t.Fatalf("ports output = %q", output)
	}
}
