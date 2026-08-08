package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zero/agent-os/internal/artifacts"
	"github.com/zero/agent-os/internal/execx"
	"github.com/zero/agent-os/internal/logging"
	"github.com/zero/agent-os/internal/model"
	"github.com/zero/agent-os/internal/releases"
)

type Libvirt struct {
	Runner execx.Runner
	Out    io.Writer
	Err    io.Writer
}

func (l Libvirt) Name() string { return "libvirt" }

func (l Libvirt) Available(ctx context.Context) error {
	if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "uri"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("libvirt is unavailable: %w", err)
	}
	return nil
}

func (l Libvirt) Create(ctx context.Context, spec Spec) error {
	definition := artifacts.FromConfig(spec.Config, spec.Architecture)
	definition.SecurityModel = libvirtSecurityModel()
	artifactDir := filepath.Join(spec.Config.StateDir, "v1", "vms", spec.Config.VMName, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return err
	}
	disk := filepath.Join(artifactDir, "disk.qcow2")
	seed := filepath.Join(artifactDir, "cloud-init.iso")
	xmlPath := filepath.Join(artifactDir, "domain.xml")
	xml, err := artifacts.LibvirtXML(definition, disk, seed)
	if err != nil {
		return err
	}
	if err := os.WriteFile(xmlPath, []byte(xml), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "network.xml"), []byte(artifacts.LibvirtNetworkXML(definition)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "orca.service"), []byte(artifacts.OrcaSystemdUnit(definition.OrcaPort, definition.BindAddress, definition.PairingAddress)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "user-data"), []byte(artifacts.CloudInit(definition, spec.Config.RepositoryKeyPath)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "meta-data"), []byte("instance-id: "+spec.Config.VMName+"\nlocal-hostname: "+spec.Config.VMName+"\n"), 0o600); err != nil {
		return err
	}
	if spec.DryRun {
		return nil
	}
	image, err := releases.FedoraServer44(spec.Architecture)
	if err != nil {
		return err
	}
	base := filepath.Join(artifactDir, image.Filename)
	if _, err := os.Stat(base); err != nil {
		if err := releases.DownloadVerified(ctx, l.Runner, image, base, l.Out, l.Err); err != nil {
			return err
		}
	}
	if err := l.ensureNetwork(ctx, filepath.Join(artifactDir, "network.xml")); err != nil {
		return err
	}
	// All arguments are fixed values or validated paths. No shell is involved.
	if err := command(l.Runner, ctx, "qemu-img", []string{"create", "-f", "qcow2", "-F", "qcow2", "-b", base, "-o", "size=" + fmt.Sprintf("%dG", definition.DiskGiB), disk}, nil, l.Out, l.Err); err != nil {
		return err
	}
	if err := command(l.Runner, ctx, "cloud-localds", []string{seed, filepath.Join(artifactDir, "user-data"), filepath.Join(artifactDir, "meta-data")}, nil, l.Out, l.Err); err != nil {
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "define", xmlPath}, nil, l.Out, l.Err)
}

func (l Libvirt) EnsureNetwork(ctx context.Context, spec Spec) error {
	artifactDir := filepath.Join(spec.Config.StateDir, "v1", "vms", spec.Config.VMName, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return err
	}
	definition := artifacts.FromConfig(spec.Config, spec.Architecture)
	path := filepath.Join(artifactDir, "network.xml")
	if err := os.WriteFile(path, []byte(artifacts.LibvirtNetworkXML(definition)), 0o600); err != nil {
		return err
	}
	return l.ensureNetwork(ctx, path)
}

// ConfigureForwarding installs a per-VM nftables DNAT rule on the Linux host.
// The libvirt network itself only provides outbound NAT; it does not expose a
// guest service on a selected host address.
func (l Libvirt) ConfigureForwarding(ctx context.Context, spec Spec) error {
	if spec.Config.AccessMode == model.AccessWireGuard {
		if err := command(l.Runner, ctx, "sudo", []string{"ip", "link", "show", "dev", spec.Config.WireGuardInterface}, nil, l.Out, l.Err); err != nil {
			return fmt.Errorf("WireGuard interface %q is unavailable: %w", spec.Config.WireGuardInterface, err)
		}
	}
	if err := command(l.Runner, ctx, "sudo", []string{"sysctl", "-w", "net.ipv4.ip_forward=1"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	rules, err := artifacts.LinuxForwardingRules(spec.Config, artifacts.LibvirtGuestAddress(spec.Config.VMName))
	if err != nil {
		return err
	}
	// Reapplying start is idempotent. A stale table can remain after an
	// interrupted lifecycle operation, so remove only this VM's table first.
	_ = l.deleteForwardingTable(ctx, spec)
	if err := command(l.Runner, ctx, "sudo", []string{"nft", "-f", "-"}, strings.NewReader(rules), l.Out, l.Err); err != nil {
		return fmt.Errorf("install Orca forwarding rules: %w", err)
	}
	return nil
}

func (l Libvirt) RemoveForwarding(ctx context.Context, spec Spec) error {
	return l.deleteForwardingTable(ctx, spec)
}

func (l Libvirt) deleteForwardingTable(ctx context.Context, spec Spec) error {
	// nft reports a missing table as an error. Treat that case as success so
	// stop/destroy remain safe when forwarding was never installed.
	var listing bytes.Buffer
	if err := command(l.Runner, ctx, "sudo", []string{"nft", "list", "table", "ip", artifacts.ForwardingTableName(spec.Config.VMName)}, nil, &listing, io.Discard); err != nil {
		return nil
	}
	if err := command(l.Runner, ctx, "sudo", []string{"nft", "delete", "table", "ip", artifacts.ForwardingTableName(spec.Config.VMName)}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("remove Orca forwarding rules: %w", err)
	}
	return nil
}

func (l Libvirt) ensureNetwork(ctx context.Context, definitionPath string) error {
	var info bytes.Buffer
	infoErr := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-info", "agent-os-nat"}, nil, &info, l.Err)
	if infoErr != nil {
		if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-define", definitionPath}, nil, l.Out, l.Err); err != nil {
			return err
		}
	}
	active := strings.ToLower(info.String())
	active = strings.ReplaceAll(active, " ", "")
	if infoErr != nil || !strings.Contains(active, "active:yes") {
		if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-start", "agent-os-nat"}, nil, l.Out, l.Err); err != nil {
			return err
		}
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "net-autostart", "agent-os-nat"}, nil, l.Out, l.Err)
}

func libvirtSecurityModel() string {
	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "ID=") && strings.Trim(strings.TrimPrefix(line, "ID="), "\"") == "ubuntu" {
				return "apparmor"
			}
		}
	}
	return "selinux"
}

func (l Libvirt) Start(ctx context.Context, name string) error {
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "start", name}, nil, l.Out, l.Err)
}

func (l Libvirt) Stop(ctx context.Context, name string) error {
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "shutdown", name}, nil, l.Out, l.Err)
}

func (l Libvirt) Status(ctx context.Context, name string) (Status, error) {
	return l.status(ctx, name)
}

func (l Libvirt) status(ctx context.Context, name string) (Status, error) {
	var output bytes.Buffer
	if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "domstate", name}, nil, &output, l.Err); err != nil {
		return Status{Name: name, Provider: l.Name(), Lifecycle: model.StatusUnknown}, err
	}
	value := strings.ToLower(strings.TrimSpace(output.String()))
	lifecycle := model.StatusUnknown
	switch value {
	case "running", "idle", "paused":
		lifecycle = model.StatusRunning
	case "shut off", "shutoff", "off":
		lifecycle = model.StatusStopped
	}
	return Status{Name: name, Provider: l.Name(), Lifecycle: lifecycle, Detail: value}, nil
}

func (l Libvirt) Logs(ctx context.Context, name string, stdout, stderr io.Writer) error {
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "console", name}, nil, logging.RedactingWriter{Writer: stdout}, logging.RedactingWriter{Writer: stderr})
}

func (l Libvirt) Destroy(ctx context.Context, name string) error {
	if err := command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "destroy", name}, nil, l.Out, l.Err); err != nil {
		// Undefine may still be safe if the domain is already stopped; the
		// caller decides whether to continue based on this explicit error.
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "undefine", name, "--remove-all-storage"}, nil, l.Out, l.Err)
}

func (l Libvirt) Upgrade(ctx context.Context, name string, spec Spec) error {
	return l.Create(ctx, spec)
}

func (l Libvirt) Exec(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return command(l.Runner, ctx, "virsh", append([]string{"--connect", "qemu:///system", "qemu-agent-command", name}, args...), stdin, stdout, stderr)
}
