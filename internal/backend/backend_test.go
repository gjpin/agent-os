package backend

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/profile"
	"github.com/gjpin/agent-os/internal/releases"
)

type call struct {
	name string
	args []string
}
type scriptedRunner struct {
	outputs []string
	calls   []call
}

func (r *scriptedRunner) Run(_ context.Context, name string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	r.calls = append(r.calls, call{name, append([]string(nil), args...)})
	if len(r.outputs) > 0 && stdout != nil {
		_, _ = io.WriteString(stdout, r.outputs[0])
		r.outputs = r.outputs[1:]
	}
	return nil
}

var _ execx.Runner = (*scriptedRunner)(nil)

func TestLimaAutostartIsHostSpecific(t *testing.T) {
	for _, tc := range []struct{ driver, want string }{{"qemu", "autostart enable agents"}, {"vz", "autostart enable --condition=boot agents"}} {
		r := &scriptedRunner{outputs: []string{"limactl version 2.2.0\n"}}
		if err := (Lima{Runner: r, VMType: tc.driver}).EnableAutostart(context.Background(), "agents"); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(r.calls[1].args, " "); got != tc.want {
			t.Fatalf("%s call = %q", tc.driver, got)
		}
	}
	r := &scriptedRunner{}
	if err := (Lima{Runner: r, VMType: "qemu"}).DisableAutostart(context.Background(), "agents"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[0].args, " "); got != "autostart disable agents" {
		t.Fatalf("disable call=%q", got)
	}
}

func TestLimaUpgradeReconcilesGuestThenRestartsOrca(t *testing.T) {
	config := model.DefaultConfig(t.TempDir())
	r := &limaDiskRunner{diskJSON: `[]`}
	if err := (Lima{Runner: r, VMType: "qemu"}).Upgrade(context.Background(), config.VMName, Spec{Config: config}); err != nil {
		t.Fatal(err)
	}
	got := commandList(r.calls)
	for _, want := range []string{
		"limactl disk list --json",
		"limactl disk create " + profileDiskID(config.VMName),
		"limactl shell agents -- sudo /bin/rm -f /var/lib/agent-os/coding-agents-ready",
		"limactl shell agents -- sudo /bin/bash -s",
		"limactl shell agents -- sudo systemctl restart orca.service",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("upgrade calls omit %q:\n%s", want, got)
		}
	}
}

func profileDiskID(name string) string {
	return profile.DiskID(name)
}

func TestLimaAutostartRejectsOldVersion(t *testing.T) {
	r := &scriptedRunner{outputs: []string{"limactl version 2.1.0\n"}}
	if err := (Lima{Runner: r, VMType: "qemu"}).EnableAutostart(context.Background(), "agents"); err == nil {
		t.Fatal("old version accepted")
	}
	if len(r.calls) != 1 {
		t.Fatalf("autostart registered: %+v", r.calls)
	}
}

func TestLimaWireGuardValidationUsesHostTooling(t *testing.T) {
	spec := Spec{Config: model.Config{AccessMode: model.AccessWireGuard, WireGuardInterface: "wg0"}}
	for _, tc := range []struct{ driver, name, args string }{{"qemu", "ip", "link show dev wg0"}, {"vz", "ifconfig", "wg0"}} {
		r := &scriptedRunner{}
		if err := (Lima{Runner: r, VMType: tc.driver}).ConfigureForwarding(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		if r.calls[0].name != tc.name || strings.Join(r.calls[0].args, " ") != tc.args {
			t.Fatalf("%s calls = %+v", tc.driver, r.calls)
		}
	}
}

func TestLimaExecutionUsesLimactlShell(t *testing.T) {
	r := &scriptedRunner{}
	var out bytes.Buffer
	if err := (Lima{Runner: r}).Exec(context.Background(), "agents", []string{"true"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if r.calls[0].name != "limactl" || strings.Join(r.calls[0].args, " ") != "shell agents -- true" {
		t.Fatalf("calls = %+v", r.calls)
	}
}

func TestLimaLifecycleUsesOnlyLimactl(t *testing.T) {
	ctx := context.Background()
	r := &scriptedRunner{outputs: []string{"", "Running\n"}}
	lima := Lima{Runner: r, VMType: "vz"}
	if err := lima.Available(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lima.Start(ctx, "agents"); err != nil {
		t.Fatal(err)
	}
	if err := lima.Stop(ctx, "agents"); err != nil {
		t.Fatal(err)
	}
	status, err := lima.Status(ctx, "agents")
	if err != nil || status.Lifecycle != model.StatusRunning {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if err := lima.Logs(ctx, "agents", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := lima.Destroy(ctx, "agents"); err != nil {
		t.Fatal(err)
	}
	for _, call := range r.calls {
		if call.name != "limactl" {
			t.Fatalf("non-Lima lifecycle call: %+v", call)
		}
	}
	wants := []string{
		"--version",
		"start --timeout 20m0s agents",
		"shell agents -- sudo /bin/bash -s",
		"stop agents",
		"list --format {{.Status}} agents",
		"shell agents journalctl -u orca.service -f -n 100 --no-pager",
		"delete agents",
	}
	if len(r.calls) != len(wants) {
		t.Fatalf("calls=%+v", r.calls)
	}
	for i, want := range wants {
		if got := strings.Join(r.calls[i].args, " "); got != want {
			t.Errorf("call %d=%q want %q", i, got, want)
		}
	}
}

func TestLimaGuestOperationsUseLimactlShell(t *testing.T) {
	ctx := context.Background()
	r := &scriptedRunner{}
	lima := Lima{Runner: r, VMType: "qemu"}
	spec := Spec{Config: model.DefaultConfig(t.TempDir())}
	operations := []func() error{
		func() error { return lima.SyncProfile(ctx, spec, false) },
		func() error { return lima.SyncProfile(ctx, spec, true) },
		func() error { return lima.RefreshAgentInstructions(ctx, "agents", "instructions") },
		func() error { return lima.ExecAsRoot(ctx, "agents", []string{"true"}, nil, io.Discard, io.Discard) },
		func() error {
			return lima.ExecAsUser(ctx, "agents", "agent", []string{"true"}, nil, io.Discard, io.Discard)
		},
	}
	for _, operation := range operations {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}
	for _, call := range r.calls {
		if call.name != "limactl" || len(call.args) == 0 || call.args[0] != "shell" {
			t.Fatalf("unexpected guest call: %+v", call)
		}
	}
	if err := lima.ExecAsUser(ctx, "agents", "root", []string{"true"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("unsupported guest user accepted")
	}
}

func TestLimaDryRunWritesHostSpecificArtifactWithoutCommands(t *testing.T) {
	for _, tc := range []struct {
		driver, arch string
		distribution model.Distribution
		imagePart    string
	}{{"qemu", "x86_64", model.DistributionFedora, "Fedora-Cloud-Base"}, {"vz", "aarch64", model.DistributionDebian, "debian-sid-genericcloud-arm64"}} {
		dir := t.TempDir()
		r := &scriptedRunner{}
		config := model.DefaultConfig(dir)
		if err := (Lima{Runner: r, VMType: tc.driver}).Create(context.Background(), Spec{Config: config, Distribution: tc.distribution, Architecture: tc.arch, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "v1", "vms", config.VMName, "artifacts", "lima.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "vmType: "+tc.driver) || !strings.Contains(text, "arch: "+tc.arch) || !strings.Contains(text, tc.imagePart) {
			t.Fatalf("artifact for %s/%s:\n%s", tc.driver, tc.arch, text)
		}
		if len(r.calls) != 0 {
			t.Fatalf("dry run invoked commands: %+v", r.calls)
		}
	}
}

func TestLimaCreateUsesManagedDiskAndLimactl(t *testing.T) {
	dir := t.TempDir()
	config := model.DefaultConfig(dir)
	image, err := releases.FedoraCloudBase44("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(dir, "v1", "vms", config.VMName, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, image.Filename), []byte("cached image"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &limaDiskRunner{diskJSON: `[]`}
	if err := (Lima{Runner: r, VMType: "qemu"}).Create(context.Background(), Spec{Config: config, Architecture: "x86_64"}); err != nil {
		t.Fatal(err)
	}
	got := commandList(r.calls)
	for _, want := range []string{
		"limactl disk list --json",
		"limactl disk create " + profile.DiskID(config.VMName),
		"limactl create --name agents --tty=false " + filepath.Join(artifactDir, "lima.yaml"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("create calls omit %q:\n%s", want, got)
		}
	}
	for _, call := range r.calls {
		if call.name != "limactl" {
			t.Fatalf("non-Lima create call: %+v", call)
		}
	}
}
