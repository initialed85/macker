package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	mackerHostInterface    = "bridge88"
	mackerHostIP           = "172.31.88.1"
	mackerNetworkBaseOctet = 88
	mackerNetworkMask      = "255.255.255.0"
	// The host bridge consumes 172.31.88.0/24. The remaining third octets
	// through 255 provide 167 per-workload /24 networks.
	mackerNetworkLimit = 255 - mackerNetworkBaseOctet
)

// networkConfig describes the host-side interfaces used as the initial
// Darwin workload network. The interfaces are ordinary host interfaces, not
// per-process network namespaces or isolation boundaries.
type networkConfig struct {
	HostInterface  string `json:"host_interface"`
	HostIP         string `json:"host_ip"`
	Interface      string `json:"interface"`
	IP             string `json:"ip"`
	HostOwned      bool   `json:"host_owned"`
	InterfaceOwned bool   `json:"interface_owned"`
}

func setupMackerNetwork(home string) (networkConfig, error) {
	if runtime.GOOS != "darwin" {
		return networkConfig{}, errors.New("network setup requires a Darwin host")
	}

	hostOwned, hostCreated, err := ensureMackerHostBridge(home)
	if err != nil {
		return networkConfig{}, err
	}
	cleanupNewHost := func() {
		if hostCreated {
			_ = destroyMackerInterface(mackerHostInterface)
		}
	}

	usedInterfaces, err := mackerNetworkInterfacesInUse(home)
	if err != nil {
		cleanupNewHost()
		return networkConfig{}, err
	}
	for index := 1; index <= mackerNetworkLimit; index++ {
		interfaceName := fmt.Sprintf("bridge88%d", index)
		if usedInterfaces[interfaceName] {
			continue
		}
		exists, err := mackerInterfaceExists(interfaceName)
		if err != nil {
			cleanupNewHost()
			return networkConfig{}, err
		}
		if exists {
			// Do not claim or modify an interface that Macker did not
			// create. This also avoids relying on bridge137 or any other
			// accidentally existing bridge.
			continue
		}

		ip := fmt.Sprintf("172.31.%d.1", mackerNetworkBaseOctet+index)
		if err := createMackerInterface(interfaceName, ip); err != nil {
			cleanupNewHost()
			return networkConfig{}, err
		}
		return networkConfig{
			HostInterface:  mackerHostInterface,
			HostIP:         mackerHostIP,
			Interface:      interfaceName,
			IP:             ip,
			HostOwned:      hostOwned,
			InterfaceOwned: true,
		}, nil
	}

	cleanupNewHost()
	return networkConfig{}, fmt.Errorf("no free Macker bridge interface is available in bridge881..bridge88%d", mackerNetworkLimit)
}

func setupExternalMackerNetwork(interfaceName, ip, hostInterface, hostIP string) (networkConfig, error) {
	if runtime.GOOS != "darwin" {
		return networkConfig{}, errors.New("network setup requires a Darwin host")
	}
	return validateExternalMackerNetwork(interfaceName, ip, hostInterface, hostIP)
}

func validateExternalMackerNetwork(interfaceName, ip, hostInterface, hostIP string) (networkConfig, error) {
	if hostInterface == "" && hostIP != "" {
		return networkConfig{}, errors.New("external network --host-ip requires --host-interface")
	}
	if hostInterface != "" && hostIP == "" {
		return networkConfig{}, errors.New("external network --host-interface requires --host-ip")
	}
	if err := validateExistingMackerAddress(interfaceName, ip, "external network", "--interface", "--ip"); err != nil {
		return networkConfig{}, err
	}
	config := networkConfig{
		Interface: interfaceName,
		IP:        ip,
	}
	if hostInterface != "" {
		if err := validateExistingMackerAddress(hostInterface, hostIP, "external network host", "--host-interface", "--host-ip"); err != nil {
			return networkConfig{}, err
		}
		config.HostInterface = hostInterface
		config.HostIP = hostIP
	}
	return config, nil
}

func validateExistingMackerAddress(interfaceName, ip, description, interfaceFlag, ipFlag string) error {
	if interfaceName == "" {
		return fmt.Errorf("%s requires %s IFACE", description, interfaceFlag)
	}
	if strings.TrimSpace(interfaceName) != interfaceName || strings.ContainsAny(interfaceName, "/\t\r\n") || strings.HasPrefix(interfaceName, "-") {
		return fmt.Errorf("invalid %s interface %q", description, interfaceName)
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil || parsedIP.To4() == nil || ip == "" || strings.TrimSpace(ip) != ip {
		return fmt.Errorf("%s requires an IPv4 %s POD_IP, got %q", description, ipFlag, ip)
	}
	if normalized := parsedIP.To4().String(); normalized != ip {
		return fmt.Errorf("%s %s must be a canonical IPv4 address, got %q", description, ipFlag, ip)
	}
	exists, err := mackerInterfaceExists(interfaceName)
	if err != nil {
		return fmt.Errorf("inspect %s interface %q: %w", description, interfaceName, err)
	}
	if !exists {
		return fmt.Errorf("%s interface %q does not exist", description, interfaceName)
	}
	hasAddress, err := mackerInterfaceHasAddress(interfaceName, ip)
	if err != nil {
		return fmt.Errorf("inspect address %s on %s interface %q: %w", ip, description, interfaceName, err)
	}
	if !hasAddress {
		return fmt.Errorf("%s interface %q does not have address %s", description, interfaceName, ip)
	}
	return nil
}

func ensureMackerHostBridge(home string) (owned, created bool, err error) {
	exists, err := mackerInterfaceExists(mackerHostInterface)
	if err != nil {
		return false, false, err
	}
	if exists {
		hasAddress, err := mackerInterfaceHasAddress(mackerHostInterface, mackerHostIP)
		if err != nil {
			return false, false, err
		}
		if !hasAddress {
			return false, false, fmt.Errorf("%s already exists without expected address %s; refusing to modify it", mackerHostInterface, mackerHostIP)
		}
		owned, err := mackerHostBridgeOwned(home)
		return owned, false, err
	}

	if err := createMackerInterface(mackerHostInterface, mackerHostIP); err != nil {
		return false, false, err
	}
	return true, true, nil
}

func createMackerInterface(interfaceName, ip string) error {
	if _, err := runIfconfig(interfaceName, "create"); err != nil {
		return fmt.Errorf("create %s: %w; network setup requires root or passwordless sudo", interfaceName, err)
	}
	if _, err := runIfconfig(interfaceName, "inet", ip, "netmask", mackerNetworkMask, "up"); err != nil {
		_ = destroyMackerInterface(interfaceName)
		return fmt.Errorf("configure %s with %s/24: %w", interfaceName, ip, err)
	}
	return nil
}

func destroyMackerInterface(interfaceName string) error {
	exists, err := mackerInterfaceExists(interfaceName)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := runIfconfig(interfaceName, "destroy"); err != nil {
		return fmt.Errorf("destroy %s: %w", interfaceName, err)
	}
	return nil
}

func cleanupMackerNetwork(home, name string, config networkConfig) error {
	var cleanupErrors []error
	if config.InterfaceOwned && config.Interface != "" {
		if err := destroyMackerInterface(config.Interface); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if config.HostOwned && config.HostInterface != "" {
		inUse, err := mackerHostBridgeInUse(home, name, config.HostInterface)
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
		} else if !inUse {
			if err := destroyMackerInterface(config.HostInterface); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

func formatNetworkConfig(config *networkConfig) string {
	if config == nil || config.Interface == "" || config.IP == "" {
		return ""
	}
	return config.Interface + ":" + config.IP
}

func networkEnvironmentValues(config networkConfig, ports []portMapping) map[string]string {
	values := map[string]string{
		"MACKER_INTERFACE":      config.Interface,
		"MACKER_IP":             config.IP,
		"MACKER_HOST_INTERFACE": config.HostInterface,
		"MACKER_HOST_IP":        config.HostIP,
	}
	for index, port := range ports {
		number := index + 1
		values[fmt.Sprintf("MACKER_PORT_%d", number)] = strconv.Itoa(int(port.NodePort))
	}
	return values
}

func networkEnvironmentArgs(config networkConfig, ports []portMapping) []string {
	values := networkEnvironmentValues(config, ports)
	args := []string{
		"--env", "MACKER_INTERFACE=" + values["MACKER_INTERFACE"],
		"--env", "MACKER_IP=" + values["MACKER_IP"],
		"--env", "MACKER_HOST_INTERFACE=" + values["MACKER_HOST_INTERFACE"],
		"--env", "MACKER_HOST_IP=" + values["MACKER_HOST_IP"],
	}
	for index := range ports {
		number := index + 1
		key := fmt.Sprintf("MACKER_PORT_%d", number)
		args = append(args, "--env", key+"="+values[key])
	}
	return args
}

func cleanupContainerResources(home, name string, metadata *containerMetadata) error {
	var cleanupErrors []error
	if metadata.PFAnchor != "" || metadata.PFToken != "" {
		if err := cleanupPFPortMappings(pfPortState{Anchor: metadata.PFAnchor, Token: metadata.PFToken}); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("clean up port mappings for container %q: %w", name, err))
		} else {
			metadata.PFAnchor = ""
			metadata.PFToken = ""
		}
	}
	if metadata.NetworkConfig != nil {
		if err := cleanupMackerNetwork(home, name, *metadata.NetworkConfig); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("clean up network for container %q: %w", name, err))
		} else {
			metadata.NetworkConfig = nil
		}
	}
	return errors.Join(cleanupErrors...)
}

func mackerInterfaceExists(interfaceName string) (bool, error) {
	binary, err := ifconfigBinary()
	if err != nil {
		return false, err
	}
	cmd := exec.Command(binary, interfaceName)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", interfaceName, err)
	}
	return true, nil
}

func mackerInterfaceHasAddress(interfaceName, address string) (bool, error) {
	binary, err := ifconfigBinary()
	if err != nil {
		return false, err
	}
	output, err := exec.Command(binary, interfaceName).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w: %s", interfaceName, err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "inet" && fields[1] == address {
			return true, nil
		}
	}
	return false, nil
}

func mackerNetworkInterfacesInUse(home string) (map[string]bool, error) {
	used := make(map[string]bool)
	containersDir := filepath.Join(home, "containers")
	entries, err := os.ReadDir(containersDir)
	if os.IsNotExist(err) {
		return used, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list containers for network allocation: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(containersDir, entry.Name(), "metadata.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read container %q network metadata: %w", entry.Name(), err)
		}
		var metadata containerMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, fmt.Errorf("decode container %q network metadata: %w", entry.Name(), err)
		}
		if metadata.NetworkConfig != nil && metadata.NetworkConfig.Interface != "" {
			used[metadata.NetworkConfig.Interface] = true
		}
	}
	return used, nil
}

func mackerHostBridgeOwned(home string) (bool, error) {
	containersDir := filepath.Join(home, "containers")
	entries, err := os.ReadDir(containersDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list containers for host network ownership: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(containersDir, entry.Name(), "metadata.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read container %q host network metadata: %w", entry.Name(), err)
		}
		var metadata containerMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return false, fmt.Errorf("decode container %q host network metadata: %w", entry.Name(), err)
		}
		if metadata.NetworkConfig != nil && metadata.NetworkConfig.HostInterface == mackerHostInterface && metadata.NetworkConfig.HostOwned {
			return true, nil
		}
	}
	return false, nil
}

func mackerHostBridgeInUse(home, exclude, interfaceName string) (bool, error) {
	containersDir := filepath.Join(home, "containers")
	entries, err := os.ReadDir(containersDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list containers for network cleanup: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == exclude {
			continue
		}
		data, err := os.ReadFile(filepath.Join(containersDir, entry.Name(), "metadata.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read container %q network metadata: %w", entry.Name(), err)
		}
		var metadata containerMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return false, fmt.Errorf("decode container %q network metadata: %w", entry.Name(), err)
		}
		if metadata.NetworkConfig != nil && metadata.NetworkConfig.HostInterface == interfaceName {
			return true, nil
		}
	}
	return false, nil
}

func ifconfigBinary() (string, error) {
	if configured := os.Getenv("MACKER_IFCONFIG"); configured != "" {
		return configured, nil
	}
	if candidate, err := exec.LookPath("ifconfig"); err == nil {
		return candidate, nil
	}
	if _, err := os.Stat("/sbin/ifconfig"); err == nil {
		return "/sbin/ifconfig", nil
	}
	return "", errors.New("ifconfig was not found")
}

func runIfconfig(args ...string) ([]byte, error) {
	binary, err := ifconfigBinary()
	if err != nil {
		return nil, err
	}
	return runPrivileged(binary, args...)
}

func runPrivileged(binary string, args ...string) ([]byte, error) {
	command := binary
	commandArgs := append([]string(nil), args...)
	if os.Geteuid() != 0 {
		sudo, err := exec.LookPath("sudo")
		if err != nil {
			return nil, errors.New("root or passwordless sudo is required")
		}
		command = sudo
		commandArgs = append([]string{"-n", binary}, commandArgs...)
	}
	cmd := exec.Command(command, commandArgs...)
	cmd.Stdin = os.Stdin
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return output, fmt.Errorf("%w: %s", err, detail)
		}
		return output, err
	}
	return output, nil
}
