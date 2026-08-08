package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
)

func TestLimaUsesArgumentArrays(t *testing.T) {
	runner := &execx.RecordingRunner{}
	provider := Lima{Runner: runner}
	if err := provider.Start(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 || runner.Calls[0].Name != "limactl" {
		t.Fatalf("unexpected calls: %+v", runner.Calls)
	}
	for _, arg := range runner.Calls[0].Args {
		if arg == "sh" || arg == "-c" || arg == "eval" {
			t.Fatalf("shell execution leaked into provider args: %+v", runner.Calls[0].Args)
		}
	}
}

func TestLibvirtForwardingUsesWireGuardInterface(t *testing.T) {
	runner := &execx.RecordingRunner{}
	c := model.DefaultConfig("/state")
	c.AccessMode = model.AccessWireGuard
	c.WireGuardInterface = "wg0"
	c.WireGuardAddress = "10.64.0.2/32"
	provider := Libvirt{Runner: runner}
	if err := provider.ConfigureForwarding(context.Background(), Spec{Config: c}); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) < 5 {
		t.Fatalf("expected interface check, sysctl, cleanup, and nft install: %+v", runner.Calls)
	}
	if runner.Calls[0].Name != "sudo" || len(runner.Calls[0].Args) < 5 || runner.Calls[0].Args[0] != "ip" || runner.Calls[0].Args[4] != "wg0" {
		t.Fatalf("WireGuard interface was not checked: %+v", runner.Calls[0])
	}
	last := runner.Calls[len(runner.Calls)-1]
	if last.Name != "sudo" || len(last.Args) != 3 || last.Args[0] != "nft" || last.Args[1] != "-f" || last.Args[2] != "-" {
		t.Fatalf("nft rules were not installed: %+v", last)
	}
}

func TestDryRunProvidersIncludeAgentInstructions(t *testing.T) {
	content := "# embedded from the release\n\nDo not depend on the caller's cwd.\n"
	providers := []struct {
		name     string
		provider Provider
		artifact string
	}{
		{name: "libvirt", provider: Libvirt{}, artifact: "user-data"},
		{name: "lima", provider: Lima{}, artifact: "lima.yaml"},
	}

	for _, test := range providers {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			config := model.DefaultConfig(stateDir)
			config.VMName = "dry-run-" + test.name
			spec := Spec{
				Config:            config,
				Architecture:      "x86_64",
				AgentInstructions: content,
				DryRun:            true,
			}
			if err := test.provider.Create(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			artifactPath := filepath.Join(stateDir, "v1", "vms", config.VMName, "artifacts", test.artifact)
			data, err := os.ReadFile(artifactPath)
			if err != nil {
				t.Fatal(err)
			}
			artifact := string(data)
			for _, expected := range []string{
				"# embedded from the release",
				"Do not depend on the caller",
				provision.AgentInstructionsCanonicalPath,
				provision.AgentInstructionsOpencodePath,
				provision.AgentInstructionsCodexPath,
				provision.AgentInstructionsClaudePath,
				provision.AgentInstructionsPiPath,
				provision.AgentInstructionsCopilotPath,
				"rm -rf --",
				"ln -s --",
			} {
				if !strings.Contains(artifact, expected) {
					t.Errorf("artifact omits %q", expected)
				}
			}
		})
	}
}
