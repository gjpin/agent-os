package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gjpin/agent-os/internal/artifacts"
	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/logging"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"github.com/gjpin/agent-os/internal/releases"
)

var provisioningTimeout = 20 * time.Minute

type Lima struct {
	Runner execx.Runner
	Out    io.Writer
	Err    io.Writer
	// VMType is the host VM driver: qemu on Linux and vz on Apple Silicon.
	// Keeping it explicit makes artifact generation independent of the runtime
	// executing tests or embedding this provider.
	VMType string
}

func (l Lima) Name() string { return "lima" }

func (l Lima) Available(ctx context.Context) error {
	if err := command(l.Runner, ctx, "limactl", []string{"--version"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("Lima is unavailable: %w", err)
	}
	return nil
}

var limaVersionPattern = regexp.MustCompile(`(^|[^0-9])v?([0-9]+)\.([0-9]+)(\.([0-9]+))?`)

func (l Lima) EnableAutostart(ctx context.Context, name string) error {
	if !model.VMNameIsValid(name) {
		return fmt.Errorf("invalid VM name %q", name)
	}
	version, major, minor, err := l.version(ctx)
	if err != nil {
		return fmt.Errorf("cannot determine Lima version for autostart; install Lima 2.2 or newer: %w", err)
	}
	if major < 2 || (major == 2 && minor < 2) {
		return fmt.Errorf("Lima %s does not support autostart; upgrade to Lima 2.2 or newer and retry", version)
	}
	args := []string{"autostart", "enable"}
	if l.VMType == "vz" {
		args = append(args, "--condition=boot")
	}
	return command(l.Runner, ctx, "limactl", append(args, name), nil, l.Out, l.Err)
}

func (l Lima) DisableAutostart(ctx context.Context, name string) error {
	if !model.VMNameIsValid(name) {
		return fmt.Errorf("invalid VM name %q", name)
	}
	return command(l.Runner, ctx, "limactl", []string{"autostart", "disable", name}, nil, l.Out, l.Err)
}

func (l Lima) version(ctx context.Context) (string, int, int, error) {
	var output bytes.Buffer
	if err := command(l.Runner, ctx, "limactl", []string{"--version"}, nil, &output, l.Err); err != nil {
		return "", 0, 0, err
	}
	matches := limaVersionPattern.FindStringSubmatch(output.String())
	if len(matches) < 4 {
		return "", 0, 0, fmt.Errorf("unrecognized version output %q", strings.TrimSpace(output.String()))
	}
	major, err := strconv.Atoi(matches[2])
	if err != nil {
		return "", 0, 0, err
	}
	minor, err := strconv.Atoi(matches[3])
	if err != nil {
		return "", 0, 0, err
	}
	version := matches[2] + "." + matches[3]
	if len(matches) > 5 && matches[5] != "" {
		version += "." + matches[5]
	}
	return version, major, minor, nil
}

// Lima owns the host listener from the static portForwards rule in lima.yaml.
// In plain mode only static rules are honored; this method verifies the
// selected WireGuard interface before the VM is started.
func (l Lima) ConfigureForwarding(ctx context.Context, spec Spec) error {
	if spec.Config.AccessMode != model.AccessWireGuard {
		return nil
	}
	tool, args := "ip", []string{"link", "show", "dev", spec.Config.WireGuardInterface}
	if l.VMType == "vz" {
		tool, args = "ifconfig", []string{spec.Config.WireGuardInterface}
	}
	if err := command(l.Runner, ctx, tool, args, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("WireGuard interface %q is unavailable: %w", spec.Config.WireGuardInterface, err)
	}
	return nil
}

func (l Lima) Create(ctx context.Context, spec Spec) error {
	if spec.Distribution == "" {
		spec.Distribution = model.DistributionFedora
	}
	if !spec.Distribution.Valid() {
		return fmt.Errorf("unsupported distro %q", spec.Distribution)
	}
	definition := artifacts.FromConfig(spec.Config, spec.Architecture, spec.Distribution)
	definition.VMType = l.VMType
	definition.AgentInstructions = spec.AgentInstructions
	profileInfo, _, profileFound, err := loadProfile(spec)
	if err != nil {
		return err
	}
	definition.ProfileDiskID = profileInfo.Metadata.DiskID
	definition.ProfileDiskLabel = profileInfo.Metadata.Label
	definition.ProfileDiskFormat = !profileFound
	artifactDir := filepath.Join(spec.Config.StateDir, "v1", "vms", spec.Config.VMName, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return err
	}
	profile := filepath.Join(artifactDir, "lima.yaml")
	var image releases.Image
	if spec.Distribution == model.DistributionDebian {
		image, err = releases.DebianSidDaily(spec.Architecture)
	} else {
		image, err = releases.FedoraCloudBase44(spec.Architecture)
	}
	if err != nil {
		return err
	}
	imagePath := filepath.Join(artifactDir, image.Filename)
	definition.ImagePath = imagePath
	contents, err := artifacts.LimaYAML(definition)
	if err != nil {
		return err
	}
	contents += artifacts.LimaPortForward(spec.Config)
	if err := os.WriteFile(profile, []byte(contents), 0o600); err != nil {
		return err
	}
	if spec.DryRun {
		return nil
	}
	if err := ensureLimaProfile(ctx, l.Runner, l.Out, l.Err, spec); err != nil {
		return err
	}
	if _, err := os.Stat(imagePath); err != nil {
		if err := releases.DownloadVerified(ctx, l.Runner, image, imagePath, l.Out, l.Err); err != nil {
			return err
		}
	}
	return command(l.Runner, ctx, "limactl", []string{"create", "--name", spec.Config.VMName, "--tty=false", profile}, nil, l.Out, l.Err)
}

func (l Lima) Start(ctx context.Context, name string) error {
	readyCtx, cancel := context.WithTimeout(ctx, provisioningTimeout)
	defer cancel()
	if err := command(l.Runner, readyCtx, "limactl", []string{"start", "--timeout", provisioningTimeout.String(), name}, nil, l.Out, l.Err); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(readyCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s waiting for required VM provisioning: %w", provisioningTimeout, context.DeadlineExceeded)
		}
		return fmt.Errorf("required Lima provisioning failed: %w", err)
	}
	if err := command(l.Runner, readyCtx, "limactl", []string{"shell", name, "--", "sudo", "/bin/bash", "-s"}, strings.NewReader(limaProvisioningWaitScript()), io.Discard, l.Err); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(readyCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s waiting for required VM provisioning: %w", provisioningTimeout, context.DeadlineExceeded)
		}
		return fmt.Errorf("provisioning readiness marker %s is missing: %w", provision.ProvisioningReadyPath, err)
	}
	return nil
}

func limaProvisioningWaitScript() string {
	return `#!/bin/bash
set -euo pipefail
while ! test -f ` + provision.ProvisioningReadyPath + `; do
  if systemctl is-failed --quiet agent-os-provision.service; then
    systemctl status --no-pager agent-os-provision.service >&2 || true
    exit 1
  fi
  sleep 2
done
`
}

func (l Lima) Stop(ctx context.Context, name string) error {
	return command(l.Runner, ctx, "limactl", []string{"stop", name}, nil, l.Out, l.Err)
}

func (l Lima) Status(ctx context.Context, name string) (Status, error) {
	var output bytes.Buffer
	if err := command(l.Runner, ctx, "limactl", []string{"list", "--format", "{{.Status}}", name}, nil, &output, l.Err); err != nil {
		return Status{Name: name, Provider: l.Name(), Lifecycle: model.StatusUnknown}, err
	}
	value := strings.ToLower(strings.TrimSpace(output.String()))
	lifecycle := model.StatusUnknown
	switch value {
	case "running":
		lifecycle = model.StatusRunning
	case "stopped", "unknown", "":
		lifecycle = model.StatusStopped
	}
	return Status{Name: name, Provider: l.Name(), Lifecycle: lifecycle, Detail: value}, nil
}

func (l Lima) Logs(ctx context.Context, name string, stdout, stderr io.Writer) error {
	return command(l.Runner, ctx, "limactl", []string{"shell", name, "journalctl", "-u", "orca.service", "-f", "-n", "100", "--no-pager"}, nil, logging.RedactingWriter{Writer: stdout}, logging.RedactingWriter{Writer: stderr})
}

func (l Lima) Destroy(ctx context.Context, name string) error {
	return command(l.Runner, ctx, "limactl", []string{"delete", name}, nil, l.Out, l.Err)
}

func (l Lima) SyncProfile(ctx context.Context, spec Spec, restore bool) error {
	action := "sync"
	if restore {
		action = "restore"
	}
	return command(l.Runner, ctx, "limactl", []string{"shell", spec.Config.VMName, "--", "sudo", provision.ProfileSyncPath, action}, nil, l.Out, l.Err)
}

func (l Lima) RefreshAgentInstructions(ctx context.Context, name, content string) error {
	return command(l.Runner, ctx, "limactl", []string{"shell", name, "--", "sudo", "/bin/bash", "-s"}, strings.NewReader(provision.AgentInstructionsScript(content)), l.Out, l.Err)
}

func (l Lima) PurgeProfile(ctx context.Context, spec Spec) error {
	info, _, found, err := loadProfile(spec)
	if err != nil {
		return err
	}
	if !found {
		present, _, err := limaDiskDetails(ctx, l.Runner, info.Metadata.DiskID, l.Err)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("Lima profile disk %q exists without trusted metadata; refusing to purge it", info.Metadata.DiskID)
		}
		return nil
	}
	if err := command(l.Runner, ctx, "limactl", []string{"disk", "delete", info.Metadata.DiskID}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("delete Lima profile disk: %w", err)
	}
	return purgeProfileMetadata(spec)
}

func (l Lima) Upgrade(ctx context.Context, name string, spec Spec) error {
	if err := ensureLimaProfile(ctx, l.Runner, l.Out, l.Err, spec); err != nil {
		return err
	}
	return command(l.Runner, ctx, "limactl", []string{"shell", name, "sudo", "systemctl", "restart", "orca.service"}, nil, l.Out, l.Err)
}

func (l Lima) Exec(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return command(l.Runner, ctx, "limactl", append([]string{"shell", name, "--"}, args...), stdin, stdout, stderr)
}

func (l Lima) ExecAsRoot(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return command(l.Runner, ctx, "limactl", append([]string{"shell", name, "--", "sudo"}, args...), stdin, stdout, stderr)
}

func (l Lima) ExecAsUser(ctx context.Context, name, user string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if user != "agent" {
		return fmt.Errorf("unsupported guest user %q", user)
	}
	commandArgs := []string{
		"shell", "--workdir", provision.AgentHome, name, "--", "sudo", "-u", user, "-H", "--", "/usr/bin/env",
		"HOME=" + provision.AgentHome,
		"SHELL=/bin/bash",
		"PATH=" + provision.AgentManagedPath,
		"CODEX_HOME=" + provision.AgentHome + "/.codex",
		"COPILOT_HOME=" + provision.AgentHome + "/.copilot",
	}
	commandArgs = append(commandArgs, args...)
	return command(l.Runner, ctx, "limactl", commandArgs, stdin, stdout, stderr)
}
