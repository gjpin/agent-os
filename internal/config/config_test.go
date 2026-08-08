package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func envMap(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadPrecedenceAndSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	contents := []byte("vm:\n  cpus: 3\n  memory_mib: 2048\naccess:\n  mode: local\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	env := envMap(map[string]string{
		"HOME":                 dir,
		"XDG_STATE_HOME":       filepath.Join(dir, "state"),
		"AGENT_OS_VM_CPUS":     "4",
		"AGENT_OS_ORCA_PORT":   "7000",
		"UNDOCUMENTED_SETTING": "must-not-be-read",
	})
	resolved, err := Load(LoadOptions{
		ExplicitConfigPath: path,
		EnvLookup:          env,
		FlagValues:         map[string]any{"vm.cpus": "6"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Config.VMCPUs != 6 || resolved.Sources["vm.cpus"] != SourceFlag {
		t.Fatalf("flag did not win: value=%d source=%s", resolved.Config.VMCPUs, resolved.Sources["vm.cpus"])
	}
	if resolved.Config.OrcaPort != 7000 || resolved.Sources["orca.port"] != SourceEnv {
		t.Fatalf("environment did not win: value=%d source=%s", resolved.Config.OrcaPort, resolved.Sources["orca.port"])
	}
	if resolved.Config.VMMemoryMiB != 2048 || resolved.Sources["vm.memory_mib"] != SourceFile {
		t.Fatalf("file did not win: value=%d source=%s", resolved.Config.VMMemoryMiB, resolved.Sources["vm.memory_mib"])
	}
	if resolved.Config.VMDiskGiB != 120 || resolved.Sources["vm.disk_gib"] != SourceDefault {
		t.Fatalf("default was not used: value=%d source=%s", resolved.Config.VMDiskGiB, resolved.Sources["vm.disk_gib"])
	}
}

func TestLoadDoesNotSearchCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := os.WriteFile("config.yaml", []byte("vm:\n  cpus: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Load(LoadOptions{EnvLookup: envMap(map[string]string{
		"HOME":            dir,
		"XDG_CONFIG_HOME": filepath.Join(dir, "not-current"),
		"XDG_STATE_HOME":  filepath.Join(dir, "state"),
	})})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Config.VMCPUs != 2 || resolved.ConfigPath == filepath.Join(dir, "config.yaml") {
		t.Fatalf("current directory influenced config: %+v", resolved)
	}
}

func TestRedaction(t *testing.T) {
	resolved, err := Load(LoadOptions{EnvLookup: envMap(map[string]string{
		"HOME":                         "/tmp/agent-os-test-home",
		"XDG_STATE_HOME":               "/tmp/agent-os-test-state",
		"AGENT_OS_REPOSITORY_KEY_PATH": "/secret/id_ed25519",
	})})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.RedactedValues()["repository.key_path"]; got != "<redacted>" {
		t.Fatalf("key path was not redacted: %v", got)
	}
}

func TestWireGuardValidation(t *testing.T) {
	_, err := Load(LoadOptions{EnvLookup: envMap(map[string]string{
		"HOME":                         "/tmp/agent-os-test-home",
		"XDG_STATE_HOME":               "/tmp/agent-os-test-state",
		"AGENT_OS_ACCESS_MODE":         "wireguard",
		"AGENT_OS_WIREGUARD_INTERFACE": "",
	})})
	if err == nil {
		t.Fatal("expected missing WireGuard settings to fail")
	}
}

func TestInteractiveRequiredValuesArePromptedWithoutPersistence(t *testing.T) {
	resolved, err := Load(LoadOptions{
		EnvLookup: envMap(map[string]string{
			"HOME":                 "/tmp/agent-os-test-home",
			"XDG_STATE_HOME":       "/tmp/agent-os-test-state",
			"AGENT_OS_ACCESS_MODE": "wireguard",
		}),
		PromptRequired: true,
		PromptIn:       bytes.NewBufferString("wg0\n10.0.0.2/32\n"),
		PromptOut:      &bytes.Buffer{},
		IsTTY:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Config.WireGuardInterface != "wg0" || resolved.Sources["wireguard.interface"] != SourcePrompt {
		t.Fatalf("interface was not prompted: %+v", resolved)
	}
	if resolved.Sources["wireguard.address"] != SourcePrompt {
		t.Fatalf("address was not prompted: %+v", resolved)
	}
}
