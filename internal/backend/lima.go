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

type Lima struct {
	Runner execx.Runner
	Out    io.Writer
	Err    io.Writer
}

func (l Lima) Name() string { return "lima" }

func (l Lima) Available(ctx context.Context) error {
	if err := command(l.Runner, ctx, "limactl", []string{"--version"}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("Lima is unavailable: %w", err)
	}
	return nil
}

// Lima owns the host listener from the static portForwards rule in lima.yaml.
// In plain mode only static rules are honored; this method verifies the
// selected WireGuard interface before the VM is started.
func (l Lima) ConfigureForwarding(ctx context.Context, spec Spec) error {
	if spec.Config.AccessMode != model.AccessWireGuard {
		return nil
	}
	if err := command(l.Runner, ctx, "ifconfig", []string{spec.Config.WireGuardInterface}, nil, l.Out, l.Err); err != nil {
		return fmt.Errorf("WireGuard interface %q is unavailable: %w", spec.Config.WireGuardInterface, err)
	}
	return nil
}

func (l Lima) RemoveForwarding(context.Context, Spec) error { return nil }

func (l Lima) EnsureNetwork(context.Context, Spec) error { return nil }

func (l Lima) Create(ctx context.Context, spec Spec) error {
	definition := artifacts.FromConfig(spec.Config, spec.Architecture)
	artifactDir := filepath.Join(spec.Config.StateDir, "v1", "vms", spec.Config.VMName, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return err
	}
	profile := filepath.Join(artifactDir, "lima.yaml")
	image, err := releases.FedoraServer44(spec.Architecture)
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
	if _, err := os.Stat(imagePath); err != nil {
		if err := releases.DownloadVerified(ctx, l.Runner, image, imagePath, l.Out, l.Err); err != nil {
			return err
		}
	}
	return command(l.Runner, ctx, "limactl", []string{"create", "--name", spec.Config.VMName, "--tty=false", profile}, nil, l.Out, l.Err)
}

func (l Lima) Start(ctx context.Context, name string) error {
	return command(l.Runner, ctx, "limactl", []string{"start", name}, nil, l.Out, l.Err)
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

func (l Lima) Upgrade(ctx context.Context, name string, spec Spec) error {
	return command(l.Runner, ctx, "limactl", []string{"shell", name, "sudo", "systemctl", "restart", "orca.service"}, nil, l.Out, l.Err)
}

func (l Lima) Exec(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return command(l.Runner, ctx, "limactl", append([]string{"shell", name, "--"}, args...), stdin, stdout, stderr)
}

func (l Lima) ExecAsUser(ctx context.Context, name, user string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if user != "agent" {
		return fmt.Errorf("unsupported guest user %q", user)
	}
	commandArgs := append([]string{"shell", name, "--", "sudo", "-u", user, "--"}, args...)
	return command(l.Runner, ctx, "limactl", commandArgs, stdin, stdout, stderr)
}
