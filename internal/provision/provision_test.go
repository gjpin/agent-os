package provision

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAgentInstructionsScriptContainsAllManagedDestinations(t *testing.T) {
	content := "instructions '$HOME'\nwith a trailing newline\n"
	script := AgentInstructionsScript(content)
	paths := []string{
		AgentInstructionsCanonicalPath,
		AgentInstructionsOpencodePath,
		AgentInstructionsCodexPath,
		AgentInstructionsClaudePath,
		AgentInstructionsPiPath,
		AgentInstructionsCopilotPath,
	}
	for _, destination := range paths {
		if !strings.Contains(script, "rm -rf -- "+shellQuote(destination)) {
			t.Errorf("script does not force-remove %s", destination)
		}
	}
	for _, link := range paths[1:] {
		if !strings.Contains(script, "ln -s -- "+shellQuote(paths[0])+
			" "+shellQuote(link)) {
			t.Errorf("script does not create absolute link %s", link)
		}
	}
	if !strings.Contains(script, "printf '%s' "+shellQuote(content)) {
		t.Fatal("script does not contain the exact embedded content")
	}
	if !strings.Contains(script, "install -d -o 'agent' -g 'agent' -m 0755 '/home/agent/.agent-os'") {
		t.Fatal("script does not create the canonical directory as agent")
	}
}

func TestAgentInstructionsScriptIsIdempotentAndReplacesDestinations(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home", "agent")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "embedded instructions\n"
	owner := strconv.Itoa(os.Getuid())
	group := strconv.Itoa(os.Getgid())
	script := agentInstructionsScriptAt(content, home, owner, group)
	scriptPath := filepath.Join(t.TempDir(), "provision.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	destinations := []string{
		path.Join(home, ".agent-os", "AGENTS.md"),
		path.Join(home, ".config", "opencode", "AGENTS.md"),
		path.Join(home, ".codex", "AGENTS.md"),
		path.Join(home, ".claude", "CLAUDE.md"),
		path.Join(home, ".pi", "agent", "AGENTS.md"),
		path.Join(home, ".copilot", "copilot-instructions.md"),
	}
	for i, destination := range destinations {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		switch i % 3 {
		case 0:
			if err := os.Mkdir(destination, 0o755); err != nil {
				t.Fatal(err)
			}
		case 1:
			if err := os.WriteFile(destination, []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.Symlink("/tmp/stale-agent-os-link", destination); err != nil {
				t.Fatal(err)
			}
		}
	}

	for run := 0; run < 2; run++ {
		command := exec.Command("bash", scriptPath)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("provisioning run %d failed: %v\n%s", run+1, err, output)
		}
	}

	canonical := destinations[0]
	actual, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != content {
		t.Fatalf("canonical content = %q, want %q", actual, content)
	}
	for _, destination := range destinations[1:] {
		info, err := os.Lstat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink", destination)
		}
		target, err := os.Readlink(destination)
		if err != nil {
			t.Fatal(err)
		}
		if target != canonical {
			t.Fatalf("%s points to %q, want %q", destination, target, canonical)
		}
	}
}

func TestCodingAgentsScriptContainsRequiredInstallersAndSafetyControls(t *testing.T) {
	script := CodingAgentsScript()
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("coding-agent installer is not valid Bash: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"set -euo pipefail",
		"dnf install -y -- adoptium-temurin-java-repository",
		"dnf config-manager setopt adoptium.enabled=1",
		"dnf install -y -- temurin-25-jdk",
		"dnf install -y -- nodejs26 nodejs26-npm",
		"https://opencode.ai/install",
		"https://chatgpt.com/codex/install.sh",
		"https://claude.ai/install.sh",
		"@earendil-works/pi-coding-agent",
		"https://gh.io/copilot-install",
		"--retry 5 --retry-delay 2 --retry-all-errors",
		"mktemp -d /tmp/agent-os-coding-agents.XXXXXX",
		"trap cleanup EXIT",
		"/usr/sbin/runuser --user agent -- /usr/bin/env",
		"HOME=\"$agent_home\" SHELL=/bin/bash PATH=\"$managed_path\"",
		"--no-modify-path",
		"--global --ignore-scripts",
		"command -v \"$1\"",
		"grep -Fqx \"$path_line\"",
		"touch \"$ready_marker\"",
	} {
		if !strings.Contains(script, expected) {
			t.Errorf("coding-agent installer omits %q", expected)
		}
	}
	if strings.Index(script, "if [ -f \"$ready_marker\" ]") > strings.Index(script, "dnf install") {
		t.Fatal("idempotency guard runs after package installation")
	}
	repositoryInstall := strings.Index(script, "dnf install -y -- adoptium-temurin-java-repository")
	repositoryEnable := strings.Index(script, "dnf config-manager setopt adoptium.enabled=1")
	jdkInstall := strings.Index(script, "dnf install -y -- temurin-25-jdk")
	nodeInstall := strings.Index(script, "dnf install -y -- nodejs26 nodejs26-npm")
	if !(repositoryInstall < repositoryEnable && repositoryEnable < jdkInstall && jdkInstall < nodeInstall) {
		t.Fatalf("Temurin provisioning commands are out of order: repository install=%d enable=%d JDK install=%d Node install=%d", repositoryInstall, repositoryEnable, jdkInstall, nodeInstall)
	}
	javaValidation := strings.Index(script, "for executable in java javac")
	if javaValidation < jdkInstall {
		t.Fatal("Java executables are validated before the JDK is installed")
	}
	if strings.Index(script, "touch \"$ready_marker\"") < javaValidation {
		t.Fatal("readiness marker is written before java and javac validation")
	}
	if strings.Index(script, "touch \"$ready_marker\"") < strings.Index(script, "for executable in") {
		t.Fatal("readiness marker is written before executable validation")
	}
	if strings.Count(script, "readonly path_line=") != 1 {
		t.Fatal("managed PATH line is not defined exactly once")
	}
}
