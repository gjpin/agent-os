package provision

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var packageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_.:@=-]*$`)

var executableMap = map[string]string{
	"git":          "git",
	"curl":         "curl",
	"jq":           "jq",
	"ripgrep":      "rg",
	"fd-find":      "fd",
	"tmux":         "tmux",
	"vim-enhanced": "vim",
}

func ValidatePackages(packages []string) error {
	seen := make(map[string]struct{}, len(packages))
	for _, pkg := range packages {
		if !packageName.MatchString(pkg) || strings.Contains(pkg, "--") {
			return fmt.Errorf("invalid package name %q", pkg)
		}
		if _, ok := seen[pkg]; ok {
			return fmt.Errorf("duplicate package %q", pkg)
		}
		seen[pkg] = struct{}{}
	}
	return nil
}

func RequiredExecutables(packages []string) []string {
	result := make([]string, 0, len(packages))
	for _, pkg := range packages {
		if executable, ok := executableMap[pkg]; ok {
			result = append(result, executable)
		}
	}
	sort.Strings(result)
	return result
}

func InstallCommand(packages []string) []string {
	result := []string{"dnf", "install", "-y", "--"}
	result = append(result, packages...)
	return result
}

func AgentUserCloudInit() string {
	return "name: agent\n  shell: /bin/bash\n  lock_passwd: true\n  groups: []\n  sudo: []\n"
}
