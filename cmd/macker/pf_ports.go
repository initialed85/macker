package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// pfPortRedirection is one PF redirect macker installed for a container, read
// back from PF itself together with the kernel's own rule counters. The
// counters are what proves a redirect is not only installed but actually being
// evaluated for the traffic that reaches it.
type pfPortRedirection struct {
	Container   string `json:"container"`
	Anchor      string `json:"anchor"`
	Mapping     string `json:"mapping,omitempty"`
	Destination string `json:"destination,omitempty"`
	Rule        string `json:"rule,omitempty"`
	Evaluations int    `json:"evaluations"`
	States      int    `json:"states"`
}

var pfRuleCountersRE = regexp.MustCompile(`Evaluations:\s*(\d+).*?States:\s*(\d+)`)

// commandPorts answers the question an operator otherwise has to guess at with
// pfctl: is the redirect for this container's published port installed, and has
// PF evaluated it? It reads each container's own anchor (the name macker chose,
// recorded in the container's inspection), so it does not depend on listing PF
// anchors - on macOS 26 a plain `pfctl -s Anchors` does not list the
// dynamically loaded com.apple/* anchors macker installs at all, only ` -vv`
// does.
func commandPorts(args []string) error {
	fs := flag.NewFlagSet("ports", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "table", "output format: table or json")
	jsonOutput := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jsonOutput {
		*format = "json"
	}

	home, err := mackerHome()
	if err != nil {
		return err
	}
	names := append([]string{}, fs.Args()...)
	sort.Strings(names)
	rows, err := readPFPortRedirections(home, names)
	if err != nil {
		return err
	}

	switch strings.ToLower(*format) {
	case "json":
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return fmt.Errorf("encode port redirections: %w", err)
		}
		fmt.Println(string(data))
		return nil
	case "table", "text":
		if len(rows) == 0 {
			fmt.Println("No published ports with a PF anchor.")
			return nil
		}
		fmt.Printf("%-44s  %-36s  %-16s  %-13s  %8s  %6s\n", "CONTAINER", "PF ANCHOR", "MAPPING", "DESTINATION", "EVALUATIONS", "STATES")
		for _, row := range rows {
			fmt.Printf("%-44s  %-36s  %-16s  %-13s  %8d  %6d\n", row.Container, row.Anchor, dashOrEmpty(row.Mapping), dashOrEmpty(row.Destination), row.Evaluations, row.States)
		}
		fmt.Println()
		fmt.Println("Evaluations count packets PF matched against the rule; a redirect that is installed but")
		fmt.Println("never evaluated shows 0 while traffic is being offered. Read the rules with:")
		fmt.Println("  sudo pfctl -a <PF ANCHOR> -sn -v")
		fmt.Println("Published ports are reached at <destination IP>:<host port> from other hosts. Traffic the")
		fmt.Println("node generates itself for a pod IP does not pass through PF, so check node-local access")
		fmt.Println("against 127.0.0.1:<node port> instead.")
		return nil
	default:
		return fmt.Errorf("unknown ports format %q", *format)
	}
}

func dashOrEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// readPFPortRedirections reads, for each named container (or every container
// when none are named), the PF anchor macker recorded for it. A container with
// an anchor that PF reports as empty is the signature of a redirect that was
// never installed or was already removed.
func readPFPortRedirections(home string, names []string) ([]pfPortRedirection, error) {
	if len(names) == 0 {
		var err error
		names, err = containerNamesInHome(home)
		if err != nil {
			return nil, err
		}
	}
	rows := make([]pfPortRedirection, 0)
	for _, name := range names {
		if !validContainerName(name) {
			return nil, fmt.Errorf("invalid container name %q", name)
		}
		metadataBytes, err := os.ReadFile(filepath.Join(home, "containers", name, "metadata.json"))
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("container %q was not found", name)
		}
		if err != nil {
			return nil, fmt.Errorf("read container %q metadata: %w", name, err)
		}
		var metadata containerMetadata
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return nil, fmt.Errorf("decode container %q metadata: %w", name, err)
		}
		if metadata.PFAnchor == "" {
			continue
		}
		mapping := describePortMappings(metadata.Ports)
		destination := ""
		if metadata.NetworkConfig != nil {
			destination = metadata.NetworkConfig.IP
		}
		out, err := runPFCTL("-a", metadata.PFAnchor, "-sn", "-v")
		if err != nil {
			return nil, fmt.Errorf("read PF anchor %q for container %q (pfctl needs root: run macker with sudo): %w", metadata.PFAnchor, name, err)
		}
		rules := parsePFAnchorRules(string(out))
		if len(rules) == 0 {
			rows = append(rows, pfPortRedirection{
				Container:   name,
				Anchor:      metadata.PFAnchor,
				Mapping:     mapping,
				Destination: destination,
			})
			continue
		}
		for _, rule := range rules {
			rows = append(rows, pfPortRedirection{
				Container:   name,
				Anchor:      metadata.PFAnchor,
				Mapping:     mapping,
				Destination: destination,
				Rule:        rule.text,
				Evaluations: rule.evaluations,
				States:      rule.states,
			})
		}
	}
	return rows, nil
}

type pfAnchorRule struct {
	text        string
	evaluations int
	states      int
}

// parsePFAnchorRules reads `pfctl -a <anchor> -sn -v` output: a rule line
// followed by a counter line, with an insertion-attribution line after it.
func parsePFAnchorRules(out string) []pfAnchorRule {
	rules := make([]pfAnchorRule, 0)
	var pending string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "[ Evaluations:") || strings.HasPrefix(trimmed, "[Evaluations:"):
			match := pfRuleCountersRE.FindStringSubmatch(trimmed)
			if pending == "" || match == nil {
				pending = ""
				continue
			}
			evaluations, _ := strconv.Atoi(match[1])
			states, _ := strconv.Atoi(match[2])
			rules = append(rules, pfAnchorRule{text: pending, evaluations: evaluations, states: states})
			pending = ""
		case strings.HasPrefix(trimmed, "[ Inserted:") || strings.HasPrefix(trimmed, "[Removed:") ||
			strings.HasPrefix(trimmed, "No ALTQ") || strings.HasPrefix(trimmed, "ALTQ"):
			continue
		default:
			pending = trimmed
		}
	}
	return rules
}

// containerNamesInHome lists container names from macker's state, pruning any
// whose directory is gone.
func containerNamesInHome(home string) ([]string, error) {
	names := make([]string, 0)
	if err := withState(home, func(state *mackerState) error {
		for name := range state.Containers {
			if _, err := os.Stat(filepath.Join(home, "containers", name)); os.IsNotExist(err) {
				delete(state.Containers, name)
				continue
			}
			names = append(names, name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// describePortMappings renders a container's published ports in the
// HOST_PORT:NODE_PORT/PROTOCOL shape run --help asks for, so `macker ports`
// lines up with what was requested.
func describePortMappings(mappings []portMapping) string {
	if len(mappings) == 0 {
		return ""
	}
	descriptions := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		descriptions = append(descriptions, fmt.Sprintf("%d:%d/%s", mapping.HostPort, mapping.NodePort, mapping.Protocol))
	}
	return strings.Join(descriptions, ",")
}
