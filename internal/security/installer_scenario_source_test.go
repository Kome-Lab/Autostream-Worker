package security

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// readInstallerScenarioSource follows the real runner's ordered source calls.
// The original source assertions continue to inspect every executed module.
func readInstallerScenarioSource(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pattern := regexp.MustCompile(`^source "\$\{SCRIPT_DIR\}/(installer-tests/[a-z0-9-]+/[a-z0-9-]+\.sh)"$`)
	seen := make(map[string]bool)
	var result strings.Builder
	for _, line := range strings.SplitAfter(string(body), "\n") {
		text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if !strings.HasPrefix(text, "source ") {
			result.WriteString(line)
			continue
		}
		match := pattern.FindStringSubmatch(text)
		if len(match) != 2 || seen[match[1]] {
			return nil, fmt.Errorf("invalid or duplicate installer scenario source")
		}
		seen[match[1]] = true
		modulePath := filepath.Join(filepath.Dir(path), filepath.FromSlash(match[1]))
		info, err := os.Lstat(modulePath)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("installer scenario is not a regular source")
		}
		module, err := os.ReadFile(modulePath)
		if err != nil {
			return nil, err
		}
		if len(module) == 0 {
			return nil, fmt.Errorf("empty installer scenario source")
		}
		result.Write(module)
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("installer runner has no scenario modules")
	}
	return []byte(result.String()), nil
}
