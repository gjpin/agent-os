package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gjpin/agent-os/internal/artifacts"
	"github.com/gjpin/agent-os/internal/backend"
	"github.com/gjpin/agent-os/internal/config"
	"github.com/gjpin/agent-os/internal/credentials"
	"github.com/gjpin/agent-os/internal/host"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"github.com/gjpin/agent-os/internal/state"
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
	switch {
	case info.OS == "darwin" && info.Architecture == "arm64":
		return []string{"require Lima", "if Homebrew is missing, confirm and install it from the official source", "brew install lima"}
	case info.OS == "linux":
		return []string{"detect Fedora or Ubuntu", "install the distribution virtualization packages", "enable/use unprivileged QEMU with libvirt"}
	default:
		return []string{"unsupported host: no changes are available"}
	}
}

func (a *App) applySetup(ctx context.Context, info host.Info, plan []string) error {
	if info.OS == "darwin" && info.Architecture == "arm64" {
		if _, err := exec.LookPath("brew"); err != nil {
			return a.installHomebrew(ctx)
		}
		return a.Runner.Run(ctx, "brew", []string{"install", "lima"}, nil, a.Out, a.Err)
	}
	if info.OS != "linux" {
		return fmt.Errorf("cannot configure unsupported host %s/%s", info.OS, info.Architecture)
	}
	distro := linuxDistro()
	var executable string
	var args []string
	switch distro {
	case "fedora", "rhel", "centos":
		executable, args = "sudo", []string{"dnf", "install", "-y", "@virtualization", "virt-install", "libvirt-client", "libvirt-daemon-config-network", "qemu-kvm", "qemu-img", "cloud-utils", "nftables"}
	case "ubuntu", "debian":
		executable, args = "sudo", []string{"apt-get", "install", "-y", "qemu-system-x86", "qemu-utils", "libvirt-daemon-system", "libvirt-daemon-driver-qemu", "libvirt-clients", "virtinst", "ovmf", "bridge-utils", "dnsmasq", "cloud-image-utils", "nftables"}
	default:
		return fmt.Errorf("unsupported Linux distribution; prerequisites would be: %s", strings.Join(plan, "; "))
	}
	return a.Runner.Run(ctx, executable, args, nil, a.Out, a.Err)
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

func linuxDistro() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "ID=") {
			return strings.Trim(strings.TrimPrefix(line, "ID="), "\"")
		}
	}
	return ""
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
				if _, err := store.Load(c.VMName); err != nil {
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
				} else {
					opErr = p.Stop(cmd.Context(), c.VMName)
				}
				if opErr != nil {
					if action == "start" {
						if forwarding, ok := p.(backend.Forwarding); ok {
							_ = forwarding.RemoveForwarding(cmd.Context(), a.backendSpec(c, false))
						}
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
				value, err := store.Load(c.VMName)
				if err != nil {
					return err
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
			packages := append([]string(nil), args...)
			if len(packages) == 0 {
				packages = artifacts.PackageManifest(c.Packages)
			}
			if len(packages) == 0 {
				return errors.New("provide at least one package")
			}
			for _, pkg := range packages {
				if strings.TrimSpace(pkg) == "" || strings.ContainsAny(pkg, "\r\n;|&") {
					return fmt.Errorf("invalid package name %q", pkg)
				}
			}
			if err := provision.ValidatePackages(packages); err != nil {
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
			return p.Upgrade(cmd.Context(), c.VMName, a.backendSpec(c, false))
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the guest upgrade non-interactively")
	return cmd
}

func (a *App) destroyCommand() *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "destroy [name]",
		Short: "Destroy a VM and its provider resources",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes && !force {
				if err := confirm(a.In, a.Out, isTTY(a.In), "destroy the VM and its persistent disk"); err != nil {
					return err
				}
			}
			p, c, store, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			return store.WithLock(cmd.Context(), c.VMName, func() error {
				if forwarding, ok := p.(backend.Forwarding); ok {
					if err := forwarding.RemoveForwarding(cmd.Context(), a.backendSpec(c, false)); err != nil && !force {
						return err
					}
				}
				if err := p.Destroy(cmd.Context(), c.VMName); err != nil && !force {
					return err
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
	return cmd
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
