package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMackerInterfaceInspection(t *testing.T) {
	ifconfig := filepath.Join(t.TempDir(), "ifconfig")
	script := `#!/bin/sh
case "$1" in
bridge88)
  printf 'bridge88: flags=8863<UP,BROADCAST> mtu 1500\n'
  printf '\tinet 172.31.88.1 netmask 0xffffff00 broadcast 172.31.88.255\n'
  exit 0
  ;;
existing)
  exit 0
  ;;
*)
  exit 1
  ;;
esac
`
	if err := os.WriteFile(ifconfig, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_IFCONFIG", ifconfig)

	if exists, err := mackerInterfaceExists("bridge88"); err != nil || !exists {
		t.Fatalf("bridge88 exists = %v, error = %v", exists, err)
	}
	if exists, err := mackerInterfaceExists("missing"); err != nil || exists {
		t.Fatalf("missing exists = %v, error = %v", exists, err)
	}
	if hasAddress, err := mackerInterfaceHasAddress("bridge88", "172.31.88.1"); err != nil || !hasAddress {
		t.Fatalf("bridge88 address = %v, error = %v", hasAddress, err)
	}
	if hasAddress, err := mackerInterfaceHasAddress("bridge88", "172.31.89.1"); err != nil || hasAddress {
		t.Fatalf("unexpected bridge88 address = %v, error = %v", hasAddress, err)
	}
}

func TestValidateExternalMackerNetwork(t *testing.T) {
	ifconfig := filepath.Join(t.TempDir(), "ifconfig")
	script := `#!/bin/sh
case "$1" in
bridge101)
  printf 'bridge101: flags=8863<UP,BROADCAST> mtu 1500\n'
  printf '\tinet 10.42.1.3 netmask 0xffffff00 broadcast 10.42.1.255\n'
  printf '\tinet 10.42.1.1 netmask 0xffffff00 broadcast 10.42.1.255\n'
  exit 0
  ;;
missing)
  exit 1
  ;;
esac
`
	if err := os.WriteFile(ifconfig, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_IFCONFIG", ifconfig)

	config, err := validateExternalMackerNetwork("bridge101", "10.42.1.3", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Interface != "bridge101" || config.IP != "10.42.1.3" {
		t.Fatalf("external network config = %#v", config)
	}
	if config.HostInterface != "" || config.HostIP != "" || config.HostOwned || config.InterfaceOwned {
		t.Fatalf("external network unexpectedly claims host resources: %#v", config)
	}

	config, err = validateExternalMackerNetwork("bridge101", "10.42.1.3", "bridge101", "10.42.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if config.HostInterface != "bridge101" || config.HostIP != "10.42.1.1" {
		t.Fatalf("external host config = %#v", config)
	}

	for _, test := range []struct {
		name      string
		iface     string
		ip        string
		hostIface string
		hostIP    string
	}{
		{name: "missing interface", iface: "missing", ip: "10.42.1.3"},
		{name: "missing address", iface: "bridge101", ip: "10.42.1.4"},
		{name: "missing ip", iface: "bridge101", ip: ""},
		{name: "ipv6", iface: "bridge101", ip: "2001:db8::3"},
		{name: "interface whitespace", iface: "bridge 101", ip: "10.42.1.3"},
		{name: "host interface without ip", iface: "bridge101", ip: "10.42.1.3", hostIface: "bridge101"},
		{name: "host ip without interface", iface: "bridge101", ip: "10.42.1.3", hostIP: "10.42.1.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateExternalMackerNetwork(test.iface, test.ip, test.hostIface, test.hostIP); err == nil {
				t.Fatal("expected external network validation error")
			}
		})
	}
}

func TestCleanupExternalMackerNetworkDoesNotDestroy(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ifconfig.log")
	ifconfig := filepath.Join(t.TempDir(), "ifconfig")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\n", logPath)
	if err := os.WriteFile(ifconfig, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACKER_IFCONFIG", ifconfig)

	if err := cleanupMackerNetwork(t.TempDir(), "external", networkConfig{
		Interface: "bridge101",
		IP:        "10.42.1.3",
	}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(logPath); err == nil && len(data) != 0 {
		t.Fatalf("external cleanup invoked ifconfig: %q", data)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestMackerNetworkMetadataOwnership(t *testing.T) {
	home := t.TempDir()
	for _, container := range []struct {
		name          string
		interfaceName string
	}{
		{name: "first", interfaceName: "bridge881"},
		{name: "second", interfaceName: "bridge882"},
	} {
		containerDir := filepath.Join(home, "containers", container.name)
		if err := os.MkdirAll(containerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeContainerMetadata(containerDir, containerMetadata{
			Name: container.name,
			NetworkConfig: &networkConfig{
				HostInterface:  "bridge88",
				HostIP:         "172.31.88.1",
				Interface:      container.interfaceName,
				IP:             "172.31.89.1",
				HostOwned:      true,
				InterfaceOwned: true,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	used, err := mackerNetworkInterfacesInUse(home)
	if err != nil {
		t.Fatal(err)
	}
	if !used["bridge881"] || !used["bridge882"] || len(used) != 2 {
		t.Fatalf("used interfaces = %#v", used)
	}
	owned, err := mackerHostBridgeOwned(home)
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("expected Macker host bridge ownership")
	}
	inUse, err := mackerHostBridgeInUse(home, "first", "bridge88")
	if err != nil {
		t.Fatal(err)
	}
	if !inUse {
		t.Fatal("expected host bridge to be in use by second")
	}
	inUse, err = mackerHostBridgeInUse(home, "second", "bridge88")
	if err != nil {
		t.Fatal(err)
	}
	if !inUse {
		t.Fatal("expected host bridge to be in use by first")
	}
	inUse, err = mackerHostBridgeInUse(home, "first", "bridge99")
	if err != nil {
		t.Fatal(err)
	}
	if inUse {
		t.Fatal("unexpected use of bridge99")
	}
}

func TestResolveHostCommand(t *testing.T) {
	command, err := resolveHostCommand("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(command) {
		t.Fatalf("resolved host command is not executable: %q", command)
	}
	if _, err := resolveHostCommand(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected missing host command error")
	}
}
