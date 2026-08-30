package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

	var output bytes.Buffer
	output.Grow(len(data))
	last := 0
	for _, match := range matches {
		key := string(data[match[2]:match[3]])
		value, ok := values[key]
		if !ok {
			return nil, false, fmt.Errorf("token ____%s____ has no runtime value", key)
		}
		output.Write(data[last:match[0]])
		output.WriteString(value)
		last = match[1]
	}
	output.Write(data[last:])
	return output.Bytes(), true, nil
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
