package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gjpin/agent-os/internal/backend"
	"github.com/gjpin/agent-os/internal/config"
	"github.com/gjpin/agent-os/internal/credentials"
	"github.com/gjpin/agent-os/internal/host"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"github.com/gjpin/agent-os/internal/state"
	"github.com/spf13/cobra"
)

func (a *App) setupHostCommand() *cobra.Command {
	var apply, yes bool
	cmd := &cobra.Command{
		Use:   "setup-host",
		Short: "Detect the host and show or apply virtualization prerequisites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := host.Detect()
			plan := setupPlan(info)
			if !apply {
				fmt.Fprintf(a.Out, "host: %s/%s\nprovider: %s\n", info.OS, info.Architecture, info.Provider)
				if info.OS == "linux" {
					fmt.Fprintf(a.Out, "distribution: %s\n", info.Distribution)
				}
				for _, item := range plan {
					fmt.Fprintf(a.Out, "- %s\n", item)
				}
				fmt.Fprintln(a.Out, "No changes made. Re-run with --apply to install/configure prerequisites.")
				return nil
			}
			if !yes {
				if err := confirm(a.In, a.Out, isTTY(a.In), "apply host virtualization changes"); err != nil {
					return err
				}
			}
			return a.applySetup(cmd.Context(), info, plan)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply prerequisite installation instead of only showing the plan")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm host changes non-interactively")
	return cmd
}

func setupPlan(info host.Info) []string {
	family := distributionFamily(info)
	switch {
	case info.OS == "darwin" && info.Architecture == "arm64":
		return []string{"require Lima", "if Homebrew is missing, confirm and install it from the official source", "brew install lima"}
	case info.OS == "linux" && family != "" && (family != "arch" || info.Architecture == "amd64"):
		packages := prerequisitePackageNames(family)
		return []string{
			fmt.Sprintf("probe %s virtualization commands for %s", family, info.Distribution),
			"install only missing prerequisites: " + strings.Join(packages, ", "),
			"append missing libvirt QEMU hardening settings and restart the QEMU driver",
			"enable/use unprivileged QEMU with libvirt",
		}
	default:
		return []string{"unsupported host: no changes are available"}
	}
}

func (a *App) applySetup(ctx context.Context, info host.Info, plan []string) error {
	if info.OS == "darwin" && info.Architecture == "arm64" {
		if a.commandAvailable("limactl") {
			fmt.Fprintln(a.Out, "host prerequisites are already installed")
			return nil
		}
		if !a.commandAvailable("brew") {
			return a.installHomebrew(ctx)
		}
		return a.Runner.Run(ctx, "brew", []string{"install", "lima"}, nil, a.Out, a.Err)
	}
	if info.OS != "linux" {
		return fmt.Errorf("cannot configure unsupported host %s/%s", info.OS, info.Architecture)
	}
	family := distributionFamily(info)
	if family == "" {
		if strings.TrimSpace(info.Distribution) == "" {
			return fmt.Errorf("unable to detect a supported Linux distribution; supported distributions are Fedora, Ubuntu, and Arch Linux")
		}
		return fmt.Errorf("unsupported Linux distribution %q; supported distributions are Fedora, Ubuntu, and Arch Linux", info.Distribution)
	}
	if family == "arch" && info.Architecture != "amd64" {
		return fmt.Errorf("unsupported Arch Linux architecture %q; Arch Linux support requires x86_64 (linux/amd64)", info.Architecture)
	}
	missing := a.missingPrerequisitePackages(family)
	if len(missing) == 0 {
		fmt.Fprintln(a.Out, "host prerequisites are already installed")
	} else {
		// Keep the plan parameter for compatibility with the existing command
		// flow; the actual package list is derived from fresh probes above.
		_ = plan
		var executable string
		var args []string
		switch family {
		case "fedora":
			executable, args = "sudo", append([]string{"dnf", "install", "-y"}, missing...)
		case "ubuntu":
			executable, args = "sudo", append([]string{"apt-get", "install", "-y"}, missing...)
		case "arch":
			executable, args = "sudo", append([]string{"pacman", "--sync", "--needed", "--noconfirm"}, missing...)
		default:
			return fmt.Errorf("unsupported Linux distribution %q", info.Distribution)
		}
		if err := a.Runner.Run(ctx, executable, args, nil, a.Out, a.Err); err != nil {
			return err
		}
	}
	if family == "arch" {
		if err := a.Runner.Run(ctx, "sudo", []string{"systemctl", "enable", "--now", "libvirtd.service"}, nil, a.Out, a.Err); err != nil {
			return fmt.Errorf("enable and start libvirtd.service: %w", err)
		}
	}

	changed, err := a.ensureLibvirtQEMUHardeningFor(ctx, family)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintln(a.Out, "configured missing libvirt QEMU hardening settings")
	} else {
		fmt.Fprintln(a.Out, "libvirt QEMU hardening settings are already configured")
	}
	return nil
}

const libvirtQEMUConfigPath = "/etc/libvirt/qemu.conf"

type libvirtQEMUSetting struct {
	key   string
	value string
}

var requiredLibvirtQEMUSettings = []libvirtQEMUSetting{
	{key: "seccomp_sandbox", value: "1"},
	{key: "namespaces", value: `[ "mount" ]`},
	{key: "cgroup_controllers", value: `[ "devices" ]`},
}

// ensureLibvirtQEMUHardening appends only assignments whose keys are absent
// from qemu.conf. Existing values, including explicitly insecure values, are
// never rewritten; the backend will reject those values during create/start.
func (a *App) ensureLibvirtQEMUHardening(ctx context.Context) (bool, error) {
	return a.ensureLibvirtQEMUHardeningFor(ctx, "")
}

func (a *App) ensureLibvirtQEMUHardeningFor(ctx context.Context, family string) (bool, error) {
	readFile := a.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	data, err := readFile(libvirtQEMUConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read libvirt QEMU configuration %s: %w", libvirtQEMUConfigPath, err)
	}
	missing := missingLibvirtQEMUSettings(data)
	if len(missing) == 0 {
		return false, nil
	}

	var block strings.Builder
	if len(data) > 0 && data[len(data)-1] != '\n' {
		block.WriteByte('\n')
	}
	for _, setting := range missing {
		fmt.Fprintf(&block, "%s = %s\n", setting.key, setting.value)
	}
	if err := a.Runner.Run(ctx, "sudo", []string{"tee", "-a", libvirtQEMUConfigPath}, strings.NewReader(block.String()), io.Discard, a.Err); err != nil {
		return false, fmt.Errorf("append libvirt QEMU hardening settings: %w", err)
	}
	if err := a.restartLibvirtQEMUFor(ctx, family); err != nil {
		return true, err
	}
	return true, nil
}

func missingLibvirtQEMUSettings(data []byte) []libvirtQEMUSetting {
	present := make(map[string]bool, len(requiredLibvirtQEMUSettings))
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		left, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(left)
		for _, setting := range requiredLibvirtQEMUSettings {
			if key == setting.key {
				present[key] = true
			}
		}
	}

	missing := make([]libvirtQEMUSetting, 0, len(requiredLibvirtQEMUSettings))
	for _, setting := range requiredLibvirtQEMUSettings {
		if !present[setting.key] {
			missing = append(missing, setting)
		}
	}
	return missing
}

func (a *App) restartLibvirtQEMU(ctx context.Context) error {
	return a.restartLibvirtQEMUFor(ctx, "")
}

func (a *App) restartLibvirtQEMUFor(ctx context.Context, family string) error {
	if family == "arch" {
		if err := a.Runner.Run(ctx, "sudo", []string{"systemctl", "restart", "libvirtd.service"}, nil, a.Out, a.Err); err != nil {
			return fmt.Errorf("restart libvirt QEMU driver libvirtd.service: %w", err)
		}
		return nil
	}
	for _, service := range []string{"virtqemud.service", "libvirtd.service"} {
		var state bytes.Buffer
		if err := a.Runner.Run(ctx, "sudo", []string{"systemctl", "show", "--property=LoadState", "--value", service}, nil, &state, io.Discard); err != nil {
			continue
		}
		if strings.TrimSpace(state.String()) != "loaded" {
			continue
		}
		if err := a.Runner.Run(ctx, "sudo", []string{"systemctl", "restart", service}, nil, a.Out, a.Err); err != nil {
			return fmt.Errorf("restart libvirt QEMU driver %s: %w", service, err)
		}
		return nil
	}
	return errors.New("could not find a systemd libvirt QEMU service (tried virtqemud.service and libvirtd.service)")
}

func distributionFamily(info host.Info) string {
	return host.DistributionFamily(info.Distribution)
}

type prerequisite struct {
	packageName string
	probes      []string
	anyProbe    bool
}

func prerequisitesFor(distribution string) []prerequisite {
	distribution = host.DistributionFamily(distribution)
	switch strings.ToLower(strings.TrimSpace(distribution)) {
	case "fedora":
		return []prerequisite{
			{packageName: "@virtualization", probes: []string{"virsh", "virt-install", "qemu-system-x86_64"}},
			{packageName: "qemu-img", probes: []string{"qemu-img"}},
			{packageName: "libvirt-daemon-config-network", probes: []string{"virtnetworkd", "libvirtd"}, anyProbe: true},
			{packageName: "cloud-utils", probes: []string{"cloud-localds"}},
			{packageName: "nftables", probes: []string{"nft"}},
		}
	case "ubuntu":
		return []prerequisite{
			// Ubuntu's qemu-system meta-package is intentional: qemu-system-x86
			// is an implementation package and is not the requested interface.
			{packageName: "qemu-system", probes: []string{"qemu-system-x86_64"}},
			{packageName: "qemu-utils", probes: []string{"qemu-img"}},
			{packageName: "libvirt-daemon-system", probes: []string{"libvirtd", "virtqemud"}, anyProbe: true},
			{packageName: "libvirt-daemon-driver-qemu", probes: []string{"virtqemud"}},
			{packageName: "libvirt-clients", probes: []string{"virsh"}},
			{packageName: "virtinst", probes: []string{"virt-install"}},
			{packageName: "ovmf", probes: []string{"/usr/share/OVMF/OVMF_CODE.fd"}},
			{packageName: "bridge-utils", probes: []string{"brctl"}},
			{packageName: "dnsmasq", probes: []string{"dnsmasq"}},
			{packageName: "cloud-image-utils", probes: []string{"cloud-localds"}},
			{packageName: "nftables", probes: []string{"nft"}},
		}
	case "arch":
		return []prerequisite{
			{packageName: "qemu-base", probes: []string{"qemu-system-x86_64", "qemu-img"}},
			{packageName: "libvirt", probes: []string{"libvirtd", "virsh"}},
			{packageName: "virt-install", probes: []string{"virt-install"}},
			{packageName: "dnsmasq", probes: []string{"dnsmasq"}},
			{packageName: "cloud-image-utils", probes: []string{"cloud-localds"}},
			{packageName: "nftables", probes: []string{"nft"}},
			{packageName: "iptables", probes: []string{"iptables"}},
		}
	default:
		return nil
	}
}

func prerequisitePackageNames(distribution string) []string {
	requirements := prerequisitesFor(distribution)
	names := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		names = append(names, requirement.packageName)
	}
	return names
}

func (a *App) commandAvailable(name string) bool {
	lookup := a.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	_, err := lookup(name)
	return err == nil
}

func (a *App) pathAvailable(path string) bool {
	if a.PathExists != nil {
		return a.PathExists(path)
	}
	_, err := os.Stat(path)
	return err == nil
}

func (a *App) probeAvailable(name string) bool {
	if strings.ContainsRune(name, '/') {
		return a.pathAvailable(name)
	}
	return a.commandAvailable(name)
}

func (a *App) prerequisiteInstalled(requirement prerequisite) bool {
	if len(requirement.probes) == 0 {
		return false
	}
	if requirement.anyProbe {
		for _, probe := range requirement.probes {
			if a.probeAvailable(probe) {
				return true
			}
		}
		return false
	}
	for _, probe := range requirement.probes {
		if !a.probeAvailable(probe) {
			return false
		}
	}
	return true
}

func (a *App) missingPrerequisitePackages(distribution string) []string {
	missing := make([]string, 0)
	for _, requirement := range prerequisitesFor(distribution) {
		if !a.prerequisiteInstalled(requirement) {
			missing = append(missing, requirement.packageName)
		}
	}
	return missing
}

func (a *App) installHomebrew(ctx context.Context) error {
	file, err := os.CreateTemp("", "agent-os-homebrew-install-*.sh")
	if err != nil {
		return err
	}
	path := file.Name()
	_ = file.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	const installerURL = "https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh"
	if err := a.Runner.Run(ctx, "curl", []string{"--fail", "--location", "--proto", "=https", "--tlsv1.2", "--output", path, installerURL}, nil, a.Out, a.Err); err != nil {
		return fmt.Errorf("download the official Homebrew installer: %w", err)
	}
	if err := a.Runner.Run(ctx, "/bin/bash", []string{path}, nil, a.Out, a.Err); err != nil {
		return fmt.Errorf("run the official Homebrew installer: %w", err)
	}
	return a.Runner.Run(ctx, "brew", []string{"install", "lima"}, nil, a.Out, a.Err)
}

func (a *App) createCommand() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a Fedora agent VM",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			p, c, store, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			if c.RepositoryKeyPath == "" && !dryRun {
				keyPath, promptErr := config.PromptRequired(a.In, a.Out, isTTY(a.In), "repository private-key path")
				if promptErr != nil {
					return promptErr
				}
				c.RepositoryKeyPath = keyPath
			}
			if !dryRun {
				if err := credentials.ValidatePrivateKey(c.RepositoryKeyPath); err != nil {
					return err
				}
				if err := p.Available(cmd.Context()); err != nil {
					return err
				}
			}
			if err := a.ensureStateDir(c); err != nil {
				return err
			}
			return store.WithLock(cmd.Context(), c.VMName, func() error {
				if existing, loadErr := store.Load(c.VMName); loadErr == nil && existing.Provider != p.Name() {
					return fmt.Errorf("VM state belongs to provider %q, refusing to create it with provider %q", existing.Provider, p.Name())
				} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
					return fmt.Errorf("load existing VM state: %w", loadErr)
				}
				value := state.State{SchemaVersion: state.SchemaVersion, Name: c.VMName, Provider: p.Name(), Lifecycle: model.StatusCreating}
				if err := store.Save(value); err != nil {
					return err
				}
				err := p.Create(cmd.Context(), a.backendSpec(c, dryRun))
				if err != nil {
					value.Lifecycle = model.StatusFailed
					_ = store.Save(value)
					return fmt.Errorf("create %s: %w", c.VMName, err)
				}
				value.Lifecycle = model.StatusStopped
				value.Artifacts = map[string]string{"directory": filepath.Join(c.StateDir, "v1", "vms", c.VMName, "artifacts")}
				if err := store.Save(value); err != nil {
					return err
				}
				a.emit(c, "created", fmt.Sprintf("created VM %s with %s", c.VMName, p.Name()), map[string]any{"name": c.VMName, "provider": p.Name(), "dry_run": dryRun})
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "generate artifacts without mutating the host provider")
	return cmd
}

func (a *App) lifecycleCommand(action string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   action + " [name]",
		Short: strings.Title(action) + " an agent VM",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			p, c, store, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			return store.WithLock(cmd.Context(), c.VMName, func() error {
				value, err := store.Load(c.VMName)
				if err != nil {
					return fmt.Errorf("load VM state: %w (create the VM first)", err)
				}
				if err := p.Available(cmd.Context()); err != nil {
					return err
				}
				var opErr error
				if action == "start" {
					forwarding, hasForwarding := p.(backend.Forwarding)
					forwardingSpec := a.backendSpec(c, false)
					if hasForwarding {
						if err := forwarding.ConfigureForwarding(cmd.Context(), forwardingSpec); err != nil {
							return err
						}
					}
					opErr = p.Start(cmd.Context(), c.VMName)
					if opErr == nil {
						if refresher, ok := p.(backend.InstructionRefresher); ok {
							opErr = refresher.RefreshAgentInstructions(cmd.Context(), c.VMName, a.agentInstructions)
						}
					}
					if opErr == nil {
						if profiles, ok := p.(backend.ProfileLifecycle); ok {
							opErr = profiles.SyncProfile(cmd.Context(), a.backendSpec(c, false), true)
						}
					}
				} else {
					if profiles, ok := p.(backend.ProfileLifecycle); ok {
						if err := profiles.SyncProfile(cmd.Context(), a.backendSpec(c, false), false); err != nil {
							return fmt.Errorf("sync persistent profile before stop: %w", err)
						}
					}
					opErr = p.Stop(cmd.Context(), c.VMName)
				}
				if opErr != nil {
					if action == "start" {
						cleanupErrors := []error{opErr}
						cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						if stopErr := p.Stop(cleanupCtx, c.VMName); stopErr != nil {
							cleanupErrors = append(cleanupErrors, fmt.Errorf("stop VM after failed start: %w", stopErr))
						}
						if forwarding, ok := p.(backend.Forwarding); ok {
							if forwardingErr := forwarding.RemoveForwarding(cleanupCtx, a.backendSpec(c, false)); forwardingErr != nil {
								cleanupErrors = append(cleanupErrors, fmt.Errorf("remove forwarding after failed start: %w", forwardingErr))
							}
						}
						cancel()
						value.Lifecycle = model.StatusStopped
						if stateErr := store.Save(value); stateErr != nil {
							cleanupErrors = append(cleanupErrors, fmt.Errorf("save stopped state after failed start: %w", stateErr))
						}
						return errors.Join(cleanupErrors...)
					}
					return opErr
				}
				if action == "stop" {
					if forwarding, ok := p.(backend.Forwarding); ok {
						if err := forwarding.RemoveForwarding(cmd.Context(), a.backendSpec(c, false)); err != nil {
							return err
						}
					}
				}
				if action == "start" {
					value.Lifecycle = model.StatusRunning
				} else {
					value.Lifecycle = model.StatusStopped
				}
				if err := store.Save(value); err != nil {
					return err
				}
				a.emit(c, action, fmt.Sprintf("%s VM %s", action, c.VMName), map[string]any{"name": c.VMName, "provider": p.Name()})
				return nil
			})
		},
	}
	return cmd
}

func (a *App) statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Reconcile and display VM status",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			p, c, store, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			value, stateErr := store.Load(c.VMName)
			if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
				return stateErr
			}
			providerStatus, providerErr := p.Status(cmd.Context(), c.VMName)
			if providerErr != nil {
				return providerErr
			}
			if providerStatus.Lifecycle == model.StatusUnknown && stateErr == nil {
				providerStatus.Lifecycle = value.Lifecycle
			}
			if c.LogFormat == model.LogJSON {
				return a.emitJSON(map[string]any{"name": c.VMName, "provider": providerStatus.Provider, "lifecycle": providerStatus.Lifecycle, "backend_id": providerStatus.BackendID, "local_state": stateErr == nil})
			}
			fmt.Fprintf(a.Out, "%s\t%s\t%s\n", c.VMName, providerStatus.Provider, providerStatus.Lifecycle)
			return nil
		},
	}
}

func (a *App) sshCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh [name] [-- command...]",
		Short: "Run an interactive command in the agent VM",
		Args:  cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := argName(args)
			if len(args) > 1 && args[1] == "--" {
				args = args[1:]
			}
			p, c, _, err := a.providerAndConfig(name)
			if err != nil {
				return err
			}
			commandArgs := []string{"/bin/bash"}
			if len(args) > 1 {
				commandArgs = append([]string(nil), args[1:]...)
			}
			return execAsAgent(cmd.Context(), p, c.VMName, commandArgs, a.In, a.Out, a.Err)
		},
	}
}

func (a *App) logsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logs [name]",
		Short: "Stream the guest Orca/systemd logs",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			p, c, _, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			return p.Logs(cmd.Context(), c.VMName, a.Out, a.Err)
		},
	}
}

func (a *App) packagesCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "packages", Short: "Manage operator-controlled guest packages"}
	install := &cobra.Command{
		Use:   "install [package...]",
		Short: "Install packages as an explicit administrative operation",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, c, _, err := a.providerAndConfig("")
			if err != nil {
				return err
			}
			additions := append([]string(nil), args...)
			if len(additions) == 0 {
				additions = c.Packages
			}
			for _, pkg := range additions {
				if strings.TrimSpace(pkg) == "" || strings.ContainsAny(pkg, "\r\n;|&") {
					return fmt.Errorf("invalid package name %q", pkg)
				}
			}
			packages, err := provision.PackageManifest(additions)
			if err != nil {
				return err
			}
			commandArgs := append([]string{"sudo", "dnf", "install", "-y", "--"}, packages...)
			return p.Exec(cmd.Context(), c.VMName, commandArgs, nil, a.Out, a.Err)
		},
	}
	cmd.AddCommand(install)
	return cmd
}

func (a *App) authCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "auth <agent>",
		Short: "Perform an interactive credential login inside the VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "codex" {
				return fmt.Errorf("unsupported agent %q; supported agent: codex", args[0])
			}
			p, c, _, err := a.providerAndConfig("")
			if err != nil {
				return err
			}
			return execAsAgent(cmd.Context(), p, c.VMName, []string{"codex", "login"}, a.In, a.Out, a.Err)
		},
	}
}

func (a *App) verifyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "verify [name]",
		Short: "Verify host availability, state, and generated isolation inputs",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			p, c, store, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			if err := p.Available(cmd.Context()); err != nil {
				return err
			}
			value, err := store.Load(c.VMName)
			if err != nil {
				return fmt.Errorf("state verification failed: %w", err)
			}
			if value.Provider != p.Name() {
				return fmt.Errorf("state provider %q does not match host provider %q", value.Provider, p.Name())
			}
			a.emit(c, "verified", fmt.Sprintf("verified VM %s", c.VMName), map[string]any{"name": c.VMName, "provider": p.Name()})
			return nil
		},
	}
}

func (a *App) upgradeCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "upgrade [name]",
		Short: "Run an explicit guest upgrade process",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				if err := confirm(a.In, a.Out, isTTY(a.In), "upgrade the guest"); err != nil {
					return err
				}
			}
			p, c, store, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			if _, err := store.Load(c.VMName); err != nil {
				return err
			}
			if profiles, ok := p.(backend.ProfileLifecycle); ok {
				if err := profiles.SyncProfile(cmd.Context(), a.backendSpec(c, false), false); err != nil {
					return fmt.Errorf("sync persistent profile before upgrade: %w", err)
				}
			}
			return p.Upgrade(cmd.Context(), c.VMName, a.backendSpec(c, false))
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the guest upgrade non-interactively")
	return cmd
}

func (a *App) destroyCommand() *cobra.Command {
	var yes, force, purgeProfiles bool
	cmd := &cobra.Command{
		Use:   "destroy [name]",
		Short: "Destroy a VM and its provider resources",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes && !force {
				prompt := "destroy the VM and retain its persistent profile disk"
				if purgeProfiles {
					prompt = "destroy the VM and permanently delete its persistent profile disk"
				}
				if err := confirm(a.In, a.Out, isTTY(a.In), prompt); err != nil {
					return err
				}
			}
			p, c, store, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			return store.WithLock(cmd.Context(), c.VMName, func() error {
				stopped := false
				profiles, hasProfiles := p.(backend.ProfileLifecycle)
				profileSynced := true
				providerDestroyNeeded := true
				if status, statusErr := p.Status(cmd.Context(), c.VMName); statusErr == nil {
					switch status.Lifecycle {
					case model.StatusStopped:
						stopped = true
					case model.StatusRunning:
						if hasProfiles {
							if err := profiles.SyncProfile(cmd.Context(), a.backendSpec(c, false), false); err != nil {
								profileSynced = false
								if !force {
									return fmt.Errorf("sync persistent profile before destroy: %w", err)
								}
							}
						}
						if err := p.Stop(cmd.Context(), c.VMName); err != nil {
							if !force {
								return fmt.Errorf("stop VM before destroy: %w", err)
							}
						} else if err := waitForStopped(cmd.Context(), p, c.VMName); err != nil {
							if !force {
								return err
							}
						} else {
							stopped = true
						}
					case model.StatusUnknown:
						if purgeProfiles {
							stopped = true
							providerDestroyNeeded = false
						} else if !force {
							return errors.New("verify VM state before destroy: provider returned unknown state")
						}
					}
				} else if purgeProfiles {
					// A normal destroy removes the VM state but intentionally
					// retains the profile. Purge is therefore also allowed as a
					// follow-up command; each provider's purge operation verifies
					// that no domain/instance still owns the disk.
					stopped = true
					providerDestroyNeeded = false
				} else if !force {
					return fmt.Errorf("verify VM state before destroy: %w", statusErr)
				}
				if forwarding, ok := p.(backend.Forwarding); ok {
					if err := forwarding.RemoveForwarding(cmd.Context(), a.backendSpec(c, false)); err != nil && !force {
						return err
					}
				}
				detached := true
				if hasProfiles && providerDestroyNeeded {
					if err := profiles.DetachProfile(cmd.Context(), a.backendSpec(c, false)); err != nil {
						detached = false
						if !force {
							return fmt.Errorf("detach persistent profile before destroy: %w", err)
						}
					}
				}
				destroyed := true
				if providerDestroyNeeded {
					if err := p.Destroy(cmd.Context(), c.VMName); err != nil {
						destroyed = false
						if !force {
							return err
						}
					}
				}
				if purgeProfiles {
					if !hasProfiles {
						return errors.New("provider does not support profile purge")
					}
					if !profileSynced || !stopped || !detached || !destroyed {
						return errors.New("refusing to purge profile: shutdown, synchronization, and detachment were not confirmed")
					}
					if err := profiles.PurgeProfile(cmd.Context(), a.backendSpec(c, false)); err != nil {
						return err
					}
				}
				if err := store.Delete(c.VMName); err != nil {
					return err
				}
				a.emit(c, "destroyed", fmt.Sprintf("destroyed VM %s", c.VMName), map[string]any{"name": c.VMName, "provider": p.Name()})
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm destruction non-interactively")
	cmd.Flags().BoolVar(&force, "force", false, "continue after provider deletion errors; never read from config or environment")
	cmd.Flags().BoolVar(&purgeProfiles, "purge-profiles", false, "permanently delete the retained profile disk after safe detachment")
	return cmd
}

func waitForStopped(ctx context.Context, p backend.Provider, name string) error {
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		status, err := p.Status(deadline, name)
		if err != nil {
			return fmt.Errorf("verify VM shutdown: %w", err)
		}
		if status.Lifecycle == model.StatusStopped {
			return nil
		}
		if status.Lifecycle == model.StatusUnknown {
			return errors.New("verify VM shutdown: provider returned unknown state")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-deadline.Done():
			timer.Stop()
			return fmt.Errorf("timed out waiting for VM shutdown: %w", deadline.Err())
		case <-timer.C:
		}
	}
}

func (a *App) configCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect and initialize external configuration"}
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a mode-0600 configuration template",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := a.configPath
			if path == "" {
				path = config.DefaultConfigPath(os.LookupEnv)
			}
			if path == "" {
				return errors.New("cannot determine config path; set XDG_CONFIG_HOME or pass --config")
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("config file %q already exists; remove it or pass another --config path", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			contents, err := a.resolved.ConfigYAML()
			if err != nil {
				return err
			}
			if err := config.WriteConfig(path, contents); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, path)
			return nil
		},
	}
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration without mutations",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if a.resolved.Config.Validate() != nil {
				return a.resolved.Config.Validate()
			}
			fmt.Fprintln(a.Out, "configuration is valid")
			return nil
		},
	}
	var effective bool
	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show effective configuration with source metadata",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			values := a.resolved.RedactedValues()
			if !effective {
				values = map[string]any{"config_file": a.resolved.ConfigPath, "config_file_found": a.resolved.ConfigFound}
			}
			if a.resolved.Config.LogFormat == model.LogJSON {
				result := map[string]any{"values": values}
				if effective {
					result["sources"] = a.resolved.Sources
				}
				return a.emitJSON(result)
			}
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if effective {
					fmt.Fprintf(a.Out, "%s=%v\t(source: %s)\n", key, values[key], a.resolved.Sources[key])
				} else {
					fmt.Fprintf(a.Out, "%s=%v\n", key, values[key])
				}
			}
			return nil
		},
	}
	showCmd.Flags().BoolVar(&effective, "effective", false, "show resolved values and their sources")
	cmd.AddCommand(initCmd, validateCmd, showCmd)
	return cmd
}

func (a *App) completionCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:       "completion <shell>",
		Short:     "Generate shell completion",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(_ *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(a.Out, true)
			case "zsh":
				return root.GenZshCompletion(a.Out)
			case "fish":
				return root.GenFishCompletion(a.Out, true)
			case "powershell":
				return root.GenPowerShellCompletion(a.Out)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
}
