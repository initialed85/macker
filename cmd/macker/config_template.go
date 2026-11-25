package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var mackerConfigTokenPattern = regexp.MustCompile(`____(MACKER_[A-Za-z0-9_]+)____`)

var mackerConfigExtensions = map[string]struct{}{
	".conf":       {},
	".env":        {},
	".ini":        {},
	".json":       {},
	".properties": {},
	".sh":         {},
	".toml":       {},
	".txt":        {},
	".xml":        {},
	".yaml":       {},
	".yml":        {},
}

// substituteMackerConfig replaces explicit Macker tokens in the materialized
// container rootfs. It intentionally processes only known text-like config
// extensions and never follows symlinks, so host-backed volumes are untouched.
func substituteMackerConfig(rootfs string, values map[string]string) (int, error) {
	replacedFiles := 0
	err := filepath.WalkDir(rootfs, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", filename, err)
		}
		if !info.Mode().IsRegular() || !isMackerConfigFile(filename) {
			return nil
		}

		data, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}
		if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			return nil
		}
		replaced, changed, err := replaceMackerConfigTokens(data, values)
		if err != nil {
			return fmt.Errorf("substitute %s: %w", filename, err)
		}
		if !changed {
			return nil
		}
		if err := os.WriteFile(filename, replaced, info.Mode().Perm()); err != nil {
			return fmt.Errorf("write %s: %w", filename, err)
		}
		replacedFiles++
		return nil
	})
	return replacedFiles, err
}

func isMackerConfigFile(filename string) bool {
	_, ok := mackerConfigExtensions[strings.ToLower(filepath.Ext(filename))]
	return ok
}

func replaceMackerConfigTokens(data []byte, values map[string]string) ([]byte, bool, error) {
	matches := mackerConfigTokenPattern.FindAllSubmatchIndex(data, -1)
	if len(matches) == 0 {
		return data, false, nil
	}
	missing := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		key := string(data[match[2]:match[3]])
		if _, ok := values[key]; ok {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		missing = append(missing, key)
	}
	if len(missing) > 0 {
		return nil, false, missingMackerTokenError(missing, values)
	}

	var output bytes.Buffer
	output.Grow(len(data))
	last := 0
	for _, match := range matches {
		key := string(data[match[2]:match[3]])
		value := values[key]
		output.Write(data[last:match[0]])
		output.WriteString(value)
		last = match[1]
	}
	output.Write(data[last:])
	return output.Bytes(), true, nil
}

// missingMackerTokenError explains an unresolved token in terms of the flag
// that would have supplied it. A token left in a rootfs is a container that
// listens on the wrong port or not at all, so Macker refuses to start it; the
// refusal is only useful if it names the shape the caller has to add, which is
// what a node agent such as maclet needs from a failed run.
func missingMackerTokenError(missing []string, values map[string]string) error {
	tokens := make([]string, 0, len(missing))
	subject := "token "
	if len(missing) > 1 {
		subject = "tokens "
	}
	portKeys := make([]string, 0, len(missing))
	otherKeys := make([]string, 0, len(missing))
	for _, key := range missing {
		tokens = append(tokens, "____"+key+"____")
		if strings.HasPrefix(key, "MACKER_PORT_") {
			portKeys = append(portKeys, key)
		} else {
			otherKeys = append(otherKeys, key)
		}
	}
	verb := "has"
	if len(tokens) > 1 {
		verb = "have"
	}
	explanations := make([]string, 0, 3)
	if len(portKeys) > 1 {
		explanations = append(explanations, fmt.Sprintf("%s are filled from published ports in -p order, the first -p supplying MACKER_PORT_1, so publish one port per token with -p HOST_PORT:NODE_PORT[/tcp|/udp] or supply them directly with --env KEY=PORT", strings.Join(portKeys, ", ")))
	} else if len(portKeys) == 1 {
		explanations = append(explanations, fmt.Sprintf("%s is filled from the first published port, so add -p HOST_PORT:NODE_PORT[/tcp|/udp] or supply it directly with --env %s=PORT", portKeys[0], portKeys[0]))
	}
	if len(otherKeys) > 1 {
		explanations = append(explanations, fmt.Sprintf("%s are not values Macker derives from a run, so supply them with --env KEY=VALUE", strings.Join(otherKeys, ", ")))
	} else if len(otherKeys) == 1 {
		explanations = append(explanations, fmt.Sprintf("%s is not a value Macker derives from a run, so supply it with --env %s=VALUE", otherKeys[0], otherKeys[0]))
	}
	available := make([]string, 0, len(values))
	for key := range values {
		if strings.HasPrefix(key, "MACKER_") {
			available = append(available, key)
		}
	}
	sort.Strings(available)
	if len(available) > 0 {
		explanations = append(explanations, "values this run does provide: "+strings.Join(available, ", "))
	}
	return fmt.Errorf("%s%s %s no runtime value; %s", subject, strings.Join(tokens, ", "), verb, strings.Join(explanations, "; "))
}

func mackerEnvironmentValues(env []string) map[string]string {
	values := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "MACKER_") {
			values[key] = value
		}
	}
	return values
}
