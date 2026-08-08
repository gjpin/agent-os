package provision

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	AgentInstructionsCanonicalPath = "/home/agent/.agent-os/AGENTS.md"
	AgentInstructionsOpencodePath  = "/home/agent/.config/opencode/AGENTS.md"
	AgentInstructionsCodexPath     = "/home/agent/.codex/AGENTS.md"
	AgentInstructionsClaudePath    = "/home/agent/.claude/CLAUDE.md"
	AgentInstructionsPiPath        = "/home/agent/.pi/agent/AGENTS.md"
	AgentInstructionsCopilotPath   = "/home/agent/.copilot/copilot-instructions.md"
)

// AgentInstructionsScript returns a root-run, idempotent provisioning script
// for the repository instructions. The content is shell-quoted so its bytes,
// including a trailing newline, are written without interpreting it as shell
// syntax.
func AgentInstructionsScript(content string) string {
	return agentInstructionsScriptAt(content, "/home/agent", "agent", "agent")
}

func agentInstructionsScriptAt(content, home, user, group string) string {
	canonical := path.Join(home, ".agent-os", "AGENTS.md")
	links := []string{
		path.Join(home, ".config", "opencode", "AGENTS.md"),
		path.Join(home, ".codex", "AGENTS.md"),
		path.Join(home, ".claude", "CLAUDE.md"),
		path.Join(home, ".pi", "agent", "AGENTS.md"),
		path.Join(home, ".copilot", "copilot-instructions.md"),
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -eu\n\n")
	fmt.Fprintf(&b, "rm -rf -- %s\n", shellQuote(canonical))
	fmt.Fprintf(&b, "install -d -o %s -g %s -m 0755 %s\n", shellQuote(user), shellQuote(group), shellQuote(path.Dir(canonical)))
	fmt.Fprintf(&b, "printf '%%s' %s > %s\n", shellQuote(content), shellQuote(canonical))
	fmt.Fprintf(&b, "chown %s:%s %s\nchmod 0644 %s\n\n", shellQuote(user), shellQuote(group), shellQuote(canonical), shellQuote(canonical))
	for _, link := range links {
		fmt.Fprintf(&b, "rm -rf -- %s\n", shellQuote(link))
		fmt.Fprintf(&b, "install -d -o %s -g %s -m 0755 %s\n", shellQuote(user), shellQuote(group), shellQuote(path.Dir(link)))
		fmt.Fprintf(&b, "ln -s -- %s %s\n\n", shellQuote(canonical), shellQuote(link))
	}
	return b.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

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
