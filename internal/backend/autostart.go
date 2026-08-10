package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gjpin/agent-os/internal/artifacts"
)

var _ Autostarter = Libvirt{}
var _ AutostartArtifacts = Libvirt{}

func (l Libvirt) EnableAutostart(ctx context.Context, name string) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "autostart", name}, nil, l.Out, l.Err)
}

func (l Libvirt) DisableAutostart(ctx context.Context, name string) error {
	if err := validateLibvirtVMName(name); err != nil {
		return err
	}
	return command(l.Runner, ctx, "virsh", []string{"--connect", "qemu:///system", "autostart", "--disable", name}, nil, l.Out, l.Err)
}

// ConfigureAutostart installs the root-owned host artifacts needed alongside
// libvirt's domain registration. The rules are generated and validated before
// the first privileged command is issued.
func (l Libvirt) ConfigureAutostart(ctx context.Context, spec Spec) error {
	rules, err := libvirtForwardingRules(spec)
	if err != nil {
		return err
	}
	unitPath := artifacts.LibvirtAutostartUnitPath(spec.Config.VMName)
	rulesPath := artifacts.LibvirtAutostartRulesPath(spec.Config.VMName)
	unitName := artifacts.LibvirtAutostartUnitName(spec.Config.VMName)
	unit := artifacts.LibvirtAutostartSystemdUnit(spec.Config.VMName)

	if err := command(l.Runner, ctx, "sudo", []string{"install", "-d", "-o", "root", "-g", "root", "-m", "0755", "/etc/agent-os"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("create host autostart directory: %w", err)
	}
	if err := installRootFile(ctx, l.Runner, rulesPath, rules, "0600", l.Out, l.Err); err != nil {
		return fmt.Errorf("install host forwarding rules: %w", err)
	}
	if err := installRootFile(ctx, l.Runner, unitPath, unit, "0644", l.Out, l.Err); err != nil {
		return fmt.Errorf("install host autostart unit: %w", err)
	}
	if err := command(l.Runner, ctx, "sudo", []string{"systemctl", "daemon-reload"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("reload systemd after installing autostart unit: %w", err)
	}
	// Enabling the unit registers the forwarding restore for the next host
	// boot. It intentionally omits --now so this operation cannot start or
	// otherwise immediately reconcile the VM.
	if err := command(l.Runner, ctx, "sudo", []string{"systemctl", "enable", unitName}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("enable host autostart unit: %w", err)
	}
	return nil
}

func installRootFile(ctx context.Context, runner interface {
	Run(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error
}, path, contents, mode string, out, errOut io.Writer) error {
	return command(runner, ctx, "sudo", []string{"install", "-o", "root", "-g", "root", "-m", mode, "/dev/stdin", path}, strings.NewReader(contents), out, errOut)
}

// RemoveAutostart disables the host restore unit and removes both generated
// files. A missing or already-disabled unit is treated as success so disable
// remains safe to repeat.
func (l Libvirt) RemoveAutostart(ctx context.Context, spec Spec) error {
	if err := validateLibvirtVMName(spec.Config.VMName); err != nil {
		return err
	}
	unitName := artifacts.LibvirtAutostartUnitName(spec.Config.VMName)
	var systemdErr bytes.Buffer
	if err := command(l.Runner, ctx, "sudo", []string{"systemctl", "disable", "--now", unitName}, nil, l.Out, &systemdErr); err != nil && !missingSystemdUnit(systemdErr.String()) {
		return fmt.Errorf("disable host autostart unit: %w", err)
	}
	if err := command(l.Runner, ctx, "sudo", []string{"rm", "-f", "--", artifacts.LibvirtAutostartRulesPath(spec.Config.VMName), artifacts.LibvirtAutostartUnitPath(spec.Config.VMName)}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("remove host autostart artifacts: %w", err)
	}
	if err := command(l.Runner, ctx, "sudo", []string{"systemctl", "daemon-reload"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("reload systemd after removing autostart unit: %w", err)
	}
	return nil
}

func missingSystemdUnit(message string) bool {
	message = strings.ToLower(message)
	for _, phrase := range []string{"not found", "not loaded", "does not exist", "not enabled", "no such file"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}
