package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/zero/agent-os/internal/backend"
	"github.com/zero/agent-os/internal/config"
	"github.com/zero/agent-os/internal/execx"
	"github.com/zero/agent-os/internal/host"
	"github.com/zero/agent-os/internal/logging"
	"github.com/zero/agent-os/internal/model"
	"github.com/zero/agent-os/internal/state"
)

type App struct {
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	Runner execx.Runner

	root       *cobra.Command
	resolved   config.Resolved
	configPath string
	logFormat  string
	flagValues map[string]any
}

// Version is set by release builds; development builds report dev.
var Version = "dev"

func New(in io.Reader, out, errOut io.Writer, runner execx.Runner) *App {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	if runner == nil {
		runner = execx.OSRunner{}
	}
	app := &App{In: in, Out: out, Err: errOut, Runner: runner}
	app.root = app.newRoot()
	return app
}

func Execute() {
	app := New(os.Stdin, os.Stdout, os.Stderr, execx.OSRunner{})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(app.Err, "agent-os:", err)
		os.Exit(1)
	}
}

func (a *App) Command() *cobra.Command { return a.root }

func (a *App) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "agent-os",
		Short:         "Manage isolated Fedora agent VMs",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "completion" || cmd.Name() == "setup-host" {
				return nil
			}
			a.flagValues = a.collectFlagValues(cmd)
			loadPath := a.configPath
			// `config init --config path` is the one command that is expected to
			// operate before that new file exists.
			if cmd.Name() == "init" && cmd.Parent() != nil && cmd.Parent().Name() == "config" {
				loadPath = ""
			}
			promptRequired := cmd.Name() != "show" && cmd.Name() != "init"
			resolved, err := config.Load(config.LoadOptions{
				ExplicitConfigPath: loadPath,
				FlagValues:         a.flagValues,
				PromptRequired:     promptRequired,
				PromptIn:           a.In,
				PromptOut:          a.Out,
				IsTTY:              isTTY(a.In),
			})
			if err != nil {
				return err
			}
			a.resolved = resolved
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "configuration file (default: $XDG_CONFIG_HOME/agent-os/config.yaml)")
	root.PersistentFlags().StringVar(&a.logFormat, "log-format", "", "output format: human or json")
	root.PersistentFlags().Int("vm-cpus", 0, "VM vCPUs (1-8)")
	root.PersistentFlags().Int("vm-memory-mib", 0, "VM memory in MiB")
	root.PersistentFlags().Int("vm-disk-gib", 0, "VM disk in GiB")
	root.PersistentFlags().String("access-mode", "", "access mode: local or wireguard")
	root.PersistentFlags().Int("orca-port", 0, "Orca TCP port")
	root.PersistentFlags().String("wireguard-interface", "", "existing host WireGuard interface")
	root.PersistentFlags().String("wireguard-address", "", "host WireGuard tunnel address")
	root.PersistentFlags().String("repository-key-path", "", "repo-scoped private key path")
	root.PersistentFlags().StringSlice("allowed-cidr", nil, "additional allowed guest egress CIDR (repeatable)")
	root.PersistentFlags().String("release-repository", "", "GitHub release repository (owner/name)")
	root.PersistentFlags().String("state-dir", "", "operational state directory")
	root.PersistentFlags().StringSlice("packages", nil, "requested guest package manifest")

	root.AddCommand(a.setupHostCommand())
	root.AddCommand(a.createCommand())
	root.AddCommand(a.lifecycleCommand("start"))
	root.AddCommand(a.lifecycleCommand("stop"))
	root.AddCommand(a.statusCommand())
	root.AddCommand(a.sshCommand())
	root.AddCommand(a.logsCommand())
	root.AddCommand(a.packagesCommand())
	root.AddCommand(a.authCommand())
	root.AddCommand(a.verifyCommand())
	root.AddCommand(a.upgradeCommand())
	root.AddCommand(a.destroyCommand())
	root.AddCommand(a.configCommand())
	root.AddCommand(a.completionCommand(root))
	return root
}

func (a *App) collectFlagValues(cmd *cobra.Command) map[string]any {
	values := make(map[string]any)
	flags := cmd.Flags()
	add := func(flagName, key string) {
		flag := flags.Lookup(flagName)
		if flag == nil || !flag.Changed {
			return
		}
		values[key] = flag.Value.String()
	}
	addSlice := func(flagName, key string) {
		flag := flags.Lookup(flagName)
		if flag == nil || !flag.Changed {
			return
		}
		value, err := flags.GetStringSlice(flagName)
		if err == nil {
			values[key] = value
		}
	}
	add("log-format", "log.format")
	add("vm-cpus", "vm.cpus")
	add("vm-memory-mib", "vm.memory_mib")
	add("vm-disk-gib", "vm.disk_gib")
	add("access-mode", "access.mode")
	add("orca-port", "orca.port")
	add("wireguard-interface", "wireguard.interface")
	add("wireguard-address", "wireguard.address")
	add("repository-key-path", "repository.key_path")
	addSlice("allowed-cidr", "network.allowed_cidrs")
	add("release-repository", "release.repository")
	add("state-dir", "state.dir")
	addSlice("packages", "packages")
	return values
}

func (a *App) cfg(name string) (model.Config, error) {
	c := a.resolved.Config
	if strings.TrimSpace(name) != "" {
		if !model.VMNameIsValid(name) {
			return model.Config{}, fmt.Errorf("invalid VM name %q", name)
		}
		c.VMName = name
	}
	if err := c.Validate(); err != nil {
		return model.Config{}, err
	}
	return c, nil
}

func (a *App) logger(c model.Config) logging.Logger {
	return logging.Logger{Out: a.Out, Err: a.Err, Format: c.LogFormat}
}

func (a *App) provider() (backend.Provider, error) {
	info := host.Detect()
	return host.Provider(info, a.Runner, a.Out, a.Err)
}

func (a *App) store(c model.Config) state.Store { return state.NewStore(c.StateDir) }

func (a *App) providerAndConfig(name string) (backend.Provider, model.Config, state.Store, error) {
	c, err := a.cfg(name)
	if err != nil {
		return nil, model.Config{}, state.Store{}, err
	}
	p, err := a.provider()
	if err != nil {
		return nil, model.Config{}, state.Store{}, err
	}
	return p, c, a.store(c), nil
}

func execAsAgent(ctx context.Context, p backend.Provider, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if userProvider, ok := p.(backend.UserExecutor); ok {
		return userProvider.ExecAsUser(ctx, name, "agent", args, stdin, stdout, stderr)
	}
	return p.Exec(ctx, name, args, stdin, stdout, stderr)
}

func argName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func (a *App) nameArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("%s accepts at most one VM name", cmd.Name())
	}
	return nil
}

func isTTY(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func confirm(in io.Reader, out io.Writer, tty bool, prompt string) error {
	if !tty {
		return fmt.Errorf("%s; rerun interactively to confirm", prompt)
	}
	value, err := config.PromptRequired(in, out, tty, prompt+" [type yes]")
	if err != nil {
		return err
	}
	if strings.TrimSpace(strings.ToLower(value)) != "yes" {
		return errors.New("operation cancelled")
	}
	return nil
}

func (a *App) ensureStateDir(c model.Config) error {
	if strings.TrimSpace(c.StateDir) == "" {
		return errors.New("state directory is unset; set XDG_STATE_HOME or --state-dir")
	}
	return os.MkdirAll(filepath.Join(c.StateDir, "v1", "vms"), 0o700)
}

func (a *App) emit(c model.Config, event, message string, fields map[string]any) {
	a.logger(c).Event(event, message, fields)
}

func (a *App) emitJSON(value any) error {
	encoder := json.NewEncoder(a.Out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func architectureForHost() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}
