package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gjpin/agent-os/internal/backend"
	"github.com/gjpin/agent-os/internal/config"
	"github.com/gjpin/agent-os/internal/credentials"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"github.com/gjpin/agent-os/internal/state"
	"github.com/spf13/cobra"
)

func (a *App) createCommand() *cobra.Command {
	var dryRun bool
	var distroValue string
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a Fedora or Debian agent VM",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			distribution, err := model.ParseDistribution(distroValue)
			if err != nil {
				return err
			}
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
				var existingAutostart *state.AutostartState
				if existing, loadErr := store.Load(c.VMName); loadErr == nil && existing.Provider != p.Name() {
					return fmt.Errorf("VM state belongs to provider %q, refusing to create it with provider %q", existing.Provider, p.Name())
				} else if loadErr == nil {
					if existing.Distribution != distribution {
						return fmt.Errorf("VM state belongs to distro %q, refusing to create it with distro %q", existing.Distribution, distribution)
					}
					existingAutostart = existing.Autostart
				} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
					return fmt.Errorf("load existing VM state: %w", loadErr)
				}
				value := state.State{SchemaVersion: state.SchemaVersion, Name: c.VMName, Provider: p.Name(), Distribution: distribution, Lifecycle: model.StatusCreating, Autostart: existingAutostart}
				if err := store.Save(value); err != nil {
					return err
				}
				err := p.Create(cmd.Context(), a.backendSpec(c, distribution, dryRun))
				if err != nil {
					value.Lifecycle = model.StatusFailed
					_ = store.Save(value)
					return fmt.Errorf("create %s: %w", c.VMName, err)
				}
				value.Lifecycle = model.StatusStopped
				if err := store.Save(value); err != nil {
					return err
				}
				a.emit(c, "created", fmt.Sprintf("created %s VM %s with %s", distribution, c.VMName, p.Name()), map[string]any{"name": c.VMName, "provider": p.Name(), "distribution": distribution, "dry_run": dryRun})
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "generate artifacts without mutating the host provider")
	cmd.Flags().StringVar(&distroValue, "distro", "", "guest distro: fedora or debian (required)")
	_ = cmd.MarkFlagRequired("distro")
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
				startedByCommand := false
				alreadyRunning := false
				if action == "start" {
					if err := p.ConfigureForwarding(cmd.Context(), a.backendSpec(c, value.Distribution, false)); err != nil {
						return err
					}
					providerStatus, statusErr := p.Status(cmd.Context(), c.VMName)
					if statusErr != nil {
						return fmt.Errorf("inspect VM before start: %w", statusErr)
					}
					alreadyRunning = providerStatus.Lifecycle == model.StatusRunning
					if !alreadyRunning {
						startedByCommand = true
						opErr = p.Start(cmd.Context(), c.VMName)
					}
					if opErr == nil {
						opErr = p.RefreshAgentInstructions(cmd.Context(), c.VMName, a.agentInstructions.For(value.Distribution))
					}
					if opErr == nil {
						opErr = p.SyncProfile(cmd.Context(), a.backendSpec(c, value.Distribution, false), true)
					}
				} else {
					if err := p.SyncProfile(cmd.Context(), a.backendSpec(c, value.Distribution, false), false); err != nil {
						return fmt.Errorf("sync persistent profile before stop: %w", err)
					}
					opErr = p.Stop(cmd.Context(), c.VMName)
				}
				if opErr != nil {
					if action == "start" {
						cleanupErrors := []error{opErr}
						cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						if startedByCommand {
							if stopErr := p.Stop(cleanupCtx, c.VMName); stopErr != nil {
								cleanupErrors = append(cleanupErrors, fmt.Errorf("stop VM after failed start: %w", stopErr))
							}
						}
						cancel()
						if startedByCommand {
							value.Lifecycle = model.StatusStopped
						} else if alreadyRunning {
							value.Lifecycle = model.StatusRunning
						}
						if stateErr := store.Save(value); stateErr != nil {
							cleanupErrors = append(cleanupErrors, fmt.Errorf("save stopped state after failed start: %w", stateErr))
						}
						return errors.Join(cleanupErrors...)
					}
					return opErr
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

func (a *App) autostartCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autostart",
		Short: "Register a VM to start at Linux login or macOS boot",
	}
	for _, action := range []string{"enable", "disable"} {
		action := action
		subcommand := &cobra.Command{
			Use:   action + " [name]",
			Short: strings.Title(action) + " VM autostart registration",
			Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
			RunE: func(cmd *cobra.Command, args []string) error {
				p, c, store, err := a.providerAndConfig(argName(args))
				if err != nil {
					return err
				}
				enable := action == "enable"
				return a.changeAutostart(cmd.Context(), p, c, store, enable)
			},
		}
		cmd.AddCommand(subcommand)
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "status [name]",
		Short: "Show VM autostart registration",
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
			if stateErr == nil && value.Provider != p.Name() {
				return fmt.Errorf("state provider %q does not match host provider %q", value.Provider, p.Name())
			}
			enabled := stateErr == nil && value.Autostart != nil && value.Autostart.Enabled
			if c.LogFormat == model.LogJSON {
				return a.emitJSON(map[string]any{
					"name":        c.VMName,
					"provider":    p.Name(),
					"autostart":   enabled,
					"local_state": stateErr == nil,
				})
			}
			state := "disabled"
			if enabled {
				state = "enabled"
			}
			fmt.Fprintf(a.Out, "%s\t%s\t%s\n", c.VMName, p.Name(), state)
			return nil
		},
	})
	return cmd
}

func (a *App) changeAutostart(ctx context.Context, p backend.Provider, c model.Config, store state.Store, enable bool) error {
	return store.WithLock(ctx, c.VMName, func() error {
		value, err := store.Load(c.VMName)
		if err != nil {
			return fmt.Errorf("load VM state: %w (create the VM first)", err)
		}
		if value.Provider != p.Name() {
			return fmt.Errorf("state provider %q does not match host provider %q", value.Provider, p.Name())
		}
		wasEnabled := value.Autostart != nil && value.Autostart.Enabled

		if enable {
			if err := p.EnableAutostart(ctx, c.VMName); err != nil {
				return fmt.Errorf("enable %s autostart: %w", c.VMName, err)
			}
			value.Autostart = &state.AutostartState{Enabled: true}
			if err := store.Save(value); err != nil {
				if wasEnabled {
					return fmt.Errorf("save autostart state: %w", err)
				}
				return errors.Join(fmt.Errorf("save autostart state: %w", err), autostartRollback(ctx, p, c.VMName, true))
			}
			a.emit(c, "autostart-enabled", fmt.Sprintf("enabled autostart for VM %s", c.VMName), map[string]any{"name": c.VMName, "provider": p.Name()})
			return nil
		}

		if err := p.DisableAutostart(ctx, c.VMName); err != nil {
			return fmt.Errorf("disable %s autostart: %w", c.VMName, err)
		}
		value.Autostart = nil
		if err := store.Save(value); err != nil {
			rollbackErr := autostartRollback(ctx, p, c.VMName, false)
			return errors.Join(fmt.Errorf("save autostart state: %w", err), rollbackErr)
		}
		a.emit(c, "autostart-disabled", fmt.Sprintf("disabled autostart for VM %s", c.VMName), map[string]any{"name": c.VMName, "provider": p.Name()})
		return nil
	})
}

func autostartRollback(ctx context.Context, p backend.Provider, name string, afterEnable bool) error {
	if afterEnable {
		if err := p.DisableAutostart(ctx, name); err != nil {
			return fmt.Errorf("unregister autostart during rollback: %w", err)
		}
		return nil
	}
	if err := p.EnableAutostart(ctx, name); err != nil {
		return fmt.Errorf("restore provider autostart during rollback: %w", err)
	}
	return nil
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
				return a.emitJSON(map[string]any{"name": c.VMName, "provider": providerStatus.Provider, "lifecycle": providerStatus.Lifecycle, "local_state": stateErr == nil})
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
			p, c, store, err := a.providerAndConfig("")
			if err != nil {
				return err
			}
			value, err := store.Load(c.VMName)
			if err != nil {
				return fmt.Errorf("load VM state: %w (create the VM first)", err)
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
			packages, err := provision.PackageManifest(additions, value.Distribution)
			if err != nil {
				return err
			}
			if value.Distribution == model.DistributionDebian {
				if err := execAsRoot(cmd.Context(), p, c.VMName, []string{"apt-get", "update"}, nil, a.Out, a.Err); err != nil {
					return err
				}
			}
			return execAsRoot(cmd.Context(), p, c.VMName, provision.InstallCommand(packages, value.Distribution), nil, a.Out, a.Err)
		},
	}
	cmd.AddCommand(install)
	return cmd
}

func (a *App) skillsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "skills", Short: "Manage operator-controlled guest skills"}
	install := &cobra.Command{
		Use:   "install [name]",
		Short: "Install or update the configured Chrome DevTools package and skills",
		Args:  func(cmd *cobra.Command, args []string) error { return a.nameArgs(cmd, args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			p, c, _, err := a.providerAndConfig(argName(args))
			if err != nil {
				return err
			}
			if err := provision.ValidateSkills(c.Skills); err != nil {
				return err
			}
			return execAsRoot(cmd.Context(), p, c.VMName, []string{"/bin/bash", "-s"}, strings.NewReader(provision.ChromeDevToolsScript(c.Skills)), a.Out, a.Err)
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
			commandArgs, ok := authAgentCommand(args[0])
			if !ok {
				return fmt.Errorf("unsupported agent %q; supported agents: opencode, codex, claude, pi, copilot", args[0])
			}
			p, c, _, err := a.providerAndConfig("")
			if err != nil {
				return err
			}
			if args[0] == "pi" {
				fmt.Fprintln(a.Out, "Pi is starting; enter /login in the interactive session to authenticate.")
			}
			return execAsAgent(cmd.Context(), p, c.VMName, commandArgs, a.In, a.Out, a.Err)
		},
	}
}

func authAgentCommand(agent string) ([]string, bool) {
	switch agent {
	case "codex":
		return []string{"orca", "account", "add", "--agent", "codex"}, true
	case "claude":
		return []string{"orca", "account", "add", "--agent", "claude"}, true
	case "opencode":
		return []string{"opencode", "auth", "login"}, true
	case "copilot":
		return []string{"copilot", "login"}, true
	case "pi":
		return []string{"pi"}, true
	default:
		return nil, false
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
			value, err := store.Load(c.VMName)
			if err != nil {
				return err
			}
			if value.Lifecycle != model.StatusRunning {
				return fmt.Errorf("VM %s must be running before upgrade", c.VMName)
			}
			if err := p.SyncProfile(cmd.Context(), a.backendSpec(c, value.Distribution, false), false); err != nil {
				return fmt.Errorf("sync persistent profile before upgrade: %w", err)
			}
			return p.Upgrade(cmd.Context(), c.VMName, a.backendSpec(c, value.Distribution, false))
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
				value, stateErr := store.Load(c.VMName)
				if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) && !force {
					return fmt.Errorf("load VM state before destroy: %w", stateErr)
				}
				autostartEnabled := stateErr == nil && value.Autostart != nil && value.Autostart.Enabled
				stopped := false
				profileSynced := true
				providerDestroyNeeded := true
				if status, statusErr := p.Status(cmd.Context(), c.VMName); statusErr == nil {
					switch status.Lifecycle {
					case model.StatusStopped:
						stopped = true
					case model.StatusRunning:
						if err := p.SyncProfile(cmd.Context(), a.backendSpec(c, value.Distribution, false), false); err != nil {
							profileSynced = false
							if !force {
								return fmt.Errorf("sync persistent profile before destroy: %w", err)
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
					// follow-up command; Lima verifies that no instance still owns
					// the disk before deleting it.
					stopped = true
					providerDestroyNeeded = false
				} else if !force {
					return fmt.Errorf("verify VM state before destroy: %w", statusErr)
				}
				if autostartEnabled {
					if err := p.DisableAutostart(cmd.Context(), c.VMName); err != nil {
						if !force {
							return fmt.Errorf("disable VM autostart before destroy: %w", err)
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
					if !profileSynced || !stopped || !destroyed {
						return errors.New("refusing to purge profile: shutdown and synchronization were not confirmed")
					}
					distribution := model.DistributionFedora
					if stateErr == nil {
						distribution = value.Distribution
					}
					if err := p.PurgeProfile(cmd.Context(), a.backendSpec(c, distribution, false)); err != nil {
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
