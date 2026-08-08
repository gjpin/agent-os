package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/host"
)

type setupScriptedRunner struct {
	Calls   []execx.Invocation
	Inputs  []string
	Outputs []string
	Errors  []error
}

func (r *setupScriptedRunner) Run(_ context.Context, name string, args []string, stdin io.Reader, stdout, _ io.Writer) error {
	r.Calls = append(r.Calls, execx.Invocation{Name: name, Args: append([]string(nil), args...)})
	var input bytes.Buffer
	if stdin != nil {
		_, _ = io.Copy(&input, stdin)
	}
	r.Inputs = append(r.Inputs, input.String())
	index := len(r.Calls) - 1
	if index < len(r.Outputs) && stdout != nil {
		_, _ = io.WriteString(stdout, r.Outputs[index])
	}
	if index < len(r.Errors) {
		return r.Errors[index]
	}
	return nil
}

func validLibvirtQEMUConfig() []byte {
	return []byte(`seccomp_sandbox = 1
namespaces = [ "mount" ]
cgroup_controllers = [ "devices" ]
`)
}

func lookupCommands(names ...string) func(string) (string, error) {
	available := make(map[string]struct{}, len(names))
	for _, name := range names {
		available[name] = struct{}{}
	}
	return func(name string) (string, error) {
		if _, ok := available[name]; ok {
			return "/test/bin/" + filepath.Base(name), nil
		}
		return "", errors.New("not found")
	}
}

func TestMissingPrerequisitePackagesOnlyReturnsUnprobedPackages(t *testing.T) {
	app := &App{
		LookPath: lookupCommands(
			"qemu-system-x86_64", "qemu-img", "libvirtd", "virtqemud", "virsh", "virt-install",
			"brctl", "dnsmasq", "cloud-localds", "nft",
		),
		PathExists: func(path string) bool { return path == "/usr/share/OVMF/OVMF_CODE.fd" },
	}

	missing := app.missingPrerequisitePackages("ubuntu")
	if len(missing) != 0 {
		t.Fatalf("complete prerequisite set was considered missing: %v", missing)
	}

	app.LookPath = lookupCommands("qemu-img", "libvirtd", "virtqemud", "virsh", "virt-install", "brctl", "dnsmasq", "cloud-localds", "nft")
	missing = app.missingPrerequisitePackages("ubuntu")
	if !containsPackage(missing, "qemu-system") {
		t.Fatalf("missing qemu-system was not reported: %v", missing)
	}
	if containsPackage(missing, "qemu-system-x86") {
		t.Fatalf("implementation package leaked into prerequisite list: %v", missing)
	}
}

func TestApplySetupSkipsPackageManagerWhenPrerequisitesExist(t *testing.T) {
	runner := &execx.RecordingRunner{}
	app := &App{
		Out:    &bytes.Buffer{},
		Runner: runner,
		ReadFile: func(string) ([]byte, error) {
			return validLibvirtQEMUConfig(), nil
		},
		LookPath: lookupCommands(
			"qemu-system-x86_64", "qemu-img", "libvirtd", "virtqemud", "virsh", "virt-install",
			"brctl", "dnsmasq", "cloud-localds", "nft",
		),
		PathExists: func(path string) bool { return path == "/usr/share/OVMF/OVMF_CODE.fd" },
	}

	if err := app.applySetup(context.Background(), host.Info{OS: "linux", Distribution: "ubuntu"}, setupPlan(host.Info{OS: "linux", Distribution: "ubuntu"})); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("package manager ran despite complete prerequisites: %+v", runner.Calls)
	}
}

func TestApplySetupSkipsLimaInstallWhenLimaExists(t *testing.T) {
	runner := &execx.RecordingRunner{}
	app := &App{
		Out:      &bytes.Buffer{},
		Runner:   runner,
		LookPath: lookupCommands("limactl", "brew"),
	}
	info := host.Info{OS: "darwin", Architecture: "arm64"}
	if err := app.applySetup(context.Background(), info, setupPlan(info)); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("Lima package manager ran despite limactl being present: %+v", runner.Calls)
	}
}

func TestApplySetupInstallsOnlyMissingUbuntuPackages(t *testing.T) {
	runner := &execx.RecordingRunner{}
	app := &App{
		Out:    &bytes.Buffer{},
		Runner: runner,
		ReadFile: func(string) ([]byte, error) {
			return validLibvirtQEMUConfig(), nil
		},
		LookPath:   lookupCommands(),
		PathExists: func(string) bool { return false },
	}
	info := host.Info{OS: "linux", Distribution: "ubuntu"}
	if err := app.applySetup(context.Background(), info, setupPlan(info)); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls) != 1 {
		t.Fatalf("expected one conditional apt invocation, got %+v", runner.Calls)
	}
	call := runner.Calls[0]
	if call.Name != "sudo" || len(call.Args) < 4 || call.Args[0] != "apt-get" || call.Args[1] != "install" || call.Args[2] != "-y" {
		t.Fatalf("unexpected package-manager invocation: %+v", call)
	}
	if !containsString(call.Args, "qemu-system") {
		t.Fatalf("Ubuntu meta-package was not requested: %+v", call.Args)
	}
	if containsString(call.Args, "qemu-system-x86") {
		t.Fatalf("Ubuntu implementation package was requested: %+v", call.Args)
	}
}

func TestEnsureLibvirtQEMUHardeningAppendsOnlyMissingSettings(t *testing.T) {
	runner := &setupScriptedRunner{Outputs: []string{"", "loaded"}}
	app := &App{
		Out:    &bytes.Buffer{},
		Err:    &bytes.Buffer{},
		Runner: runner,
		ReadFile: func(path string) ([]byte, error) {
			if path != libvirtQEMUConfigPath {
				t.Fatalf("unexpected configuration path %q", path)
			}
			return []byte("custom_setting = 42\n# namespaces = [ \"mount\" ]\n# cgroup_controllers = [ \"devices\" ]\nseccomp_sandbox = 1\n"), nil
		},
	}
	changed, err := app.ensureLibvirtQEMUHardening(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("missing hardening settings were not appended")
	}
	if got, want := runner.Inputs[0], "namespaces = [ \"mount\" ]\ncgroup_controllers = [ \"devices\" ]\n"; got != want {
		t.Fatalf("unexpected append payload: got %q want %q", got, want)
	}
	if len(runner.Calls) != 3 {
		t.Fatalf("unexpected setup calls: %+v", runner.Calls)
	}
	if got := strings.Join(runner.Calls[0].Args, " "); got != "tee -a /etc/libvirt/qemu.conf" {
		t.Fatalf("unexpected append command: %s", got)
	}
	if got := strings.Join(runner.Calls[1].Args, " "); !strings.Contains(got, "systemctl show --property=LoadState --value virtqemud.service") {
		t.Fatalf("unexpected service probe: %s", got)
	}
	if got := strings.Join(runner.Calls[2].Args, " "); got != "systemctl restart virtqemud.service" {
		t.Fatalf("unexpected service restart: %s", got)
	}
}

func TestEnsureLibvirtQEMUHardeningCreatesMissingConfig(t *testing.T) {
	runner := &setupScriptedRunner{Outputs: []string{"", "loaded"}}
	app := &App{
		Out:    &bytes.Buffer{},
		Err:    &bytes.Buffer{},
		Runner: runner,
		ReadFile: func(string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	}
	changed, err := app.ensureLibvirtQEMUHardening(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || runner.Inputs[0] != "seccomp_sandbox = 1\nnamespaces = [ \"mount\" ]\ncgroup_controllers = [ \"devices\" ]\n" {
		t.Fatalf("unexpected missing-file setup: changed=%t input=%q", changed, runner.Inputs[0])
	}
}

func TestEnsureLibvirtQEMUHardeningLeavesExistingValuesUntouched(t *testing.T) {
	runner := &setupScriptedRunner{}
	app := &App{
		Runner: runner,
		ReadFile: func(string) ([]byte, error) {
			return []byte(`seccomp_sandbox = 0
namespaces = []
cgroup_controllers = []
`), nil
		},
	}
	changed, err := app.ensureLibvirtQEMUHardening(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("existing, conflicting settings were rewritten")
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("existing settings triggered host mutations: %+v", runner.Calls)
	}
}

func TestEnsureLibvirtQEMUHardeningFallsBackToLegacyService(t *testing.T) {
	runner := &setupScriptedRunner{
		Outputs: []string{"", "", "loaded"},
		Errors:  []error{nil, errors.New("virtqemud is not installed")},
	}
	app := &App{
		Runner: runner,
		ReadFile: func(string) ([]byte, error) {
			return []byte("seccomp_sandbox = 1\n"), nil
		},
	}
	if _, err := app.ensureLibvirtQEMUHardening(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.Calls[3].Args, " "); got != "systemctl restart libvirtd.service" {
		t.Fatalf("unexpected legacy service restart: %s", got)
	}
}

func TestSetupPlanRejectsUnsupportedLinuxDistro(t *testing.T) {
	plan := setupPlan(host.Info{OS: "linux", Distribution: "debian"})
	if len(plan) != 1 || !strings.Contains(plan[0], "unsupported") {
		t.Fatalf("unexpected unsupported-distro plan: %v", plan)
	}
	plan = setupPlan(host.Info{OS: "linux", Distribution: "rocky", DistributionFamily: "fedora"})
	if len(plan) != 1 || !strings.Contains(plan[0], "unsupported") {
		t.Fatalf("provider family metadata bypassed distro rejection: %v", plan)
	}
}

func TestCreateConfigPromptFailsOnNonTTY(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("vm:\n  name: agents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := New(bytes.NewBuffer(nil), &bytes.Buffer{}, &bytes.Buffer{}, &execx.RecordingRunner{})
	app.Command().SetArgs([]string{"--config", configPath, "--state-dir", dir, "create"})
	err := app.Command().Execute()
	if err == nil || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("expected non-TTY create prompt error, got %v", err)
	}
}

func containsPackage(values []string, wanted string) bool { return containsString(values, wanted) }

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
