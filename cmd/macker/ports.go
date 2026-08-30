package main

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// These defaults match Kubernetes' conventional NodePort range while keeping
// automatic native workload ports above the privileged-port boundary.
const (
	autoNodePortMin uint16 = 30000
	autoNodePortMax uint16 = 32767
)

func resolveNodePorts(home, name string, ports []portMapping) error {
	for index := range ports {
		if ports[index].NodePort != 0 {
			continue
		}
		nodePort, err := allocateNodePort(home, name, ports[index].Protocol, ports[:index])
		if err != nil {
			return fmt.Errorf("allocate node port for published port %d/%s: %w", ports[index].HostPort, ports[index].Protocol, err)
		}
		ports[index].NodePort = nodePort
	}
	return nil
}

func allocateNodePort(home, name, protocol string, reserved []portMapping) (uint16, error) {
	span := uint32(autoNodePortMax-autoNodePortMin) + 1
	// A random starting point avoids repeatedly selecting the same ports while
	// the bounded retry keeps allocation deterministic under exhaustion.
	var randomBytes [2]byte
	if _, err := crand.Read(randomBytes[:]); err != nil {
		return 0, fmt.Errorf("generate random node port: %w", err)
	}
	start := uint32(randomBytes[0])<<8 | uint32(randomBytes[1])
	for offset := uint32(0); offset < span; offset++ {
		candidate := autoNodePortMin + uint16((start+offset)%span)
		if portMappingUsesNodePort(reserved, candidate, protocol) {
			continue
		}
		used, err := nodePortInUseByMacker(home, name, candidate, protocol)
		if err != nil {
			return 0, err
		}
		if used || !nodePortAvailable(candidate, protocol) {
			continue
		}
		return candidate, nil
	}
	return 0, fmt.Errorf("no free automatic node port in %d-%d for %s", autoNodePortMin, autoNodePortMax, protocol)
}

func portMappingUsesNodePort(ports []portMapping, nodePort uint16, protocol string) bool {
	for _, port := range ports {
		if port.NodePort == nodePort && port.Protocol == protocol {
			return true
		}
	}
	return false
}

func nodePortInUseByMacker(home, name string, nodePort uint16, protocol string) (bool, error) {
	used := false
	err := withState(home, func(state *mackerState) error {
		for otherName := range state.Containers {
			if otherName == name {
				continue
			}
			metadataPath := filepath.Join(home, "containers", otherName, "metadata.json")
			data, err := os.ReadFile(metadataPath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read container %q metadata: %w", otherName, err)
			}
			var metadata containerMetadata
			if err := json.Unmarshal(data, &metadata); err != nil {
				return fmt.Errorf("decode container %q metadata: %w", otherName, err)
			}
			status, err := containerStatus(metadata)
			if err != nil {
				return fmt.Errorf("check container %q status: %w", otherName, err)
			}
			if status != "running" {
				continue
			}
			for _, port := range metadata.Ports {
				if port.NodePort == nodePort && port.Protocol == protocol {
					used = true
					return nil
				}
			}
		}
		return nil
	})
	return used, err
}

func nodePortAvailable(nodePort uint16, protocol string) bool {
	address := net.JoinHostPort("0.0.0.0", strconv.Itoa(int(nodePort)))
	switch protocol {
	case "tcp":
		listener, err := net.Listen("tcp4", address)
		if err != nil {
			return false
		}
		_ = listener.Close()
		return true
	case "udp":
		packet, err := net.ListenPacket("udp4", address)
		if err != nil {
			return false
		}
		_ = packet.Close()
		return true
	default:
		return false
	}
}
