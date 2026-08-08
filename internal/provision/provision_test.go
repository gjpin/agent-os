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
