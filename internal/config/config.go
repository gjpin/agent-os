package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/gjpin/agent-os/internal/model"
)

const (
	defaultConfigDirName = "agent-os"
	defaultConfigName    = "config.yaml"
)

// Key names are intentionally enumerated. AutomaticEnv would make every
// spelling accepted by Viper an implicit public API and could accidentally
// expose a future setting through the environment.
var documentedEnv = map[string]string{
	"vm.name":               "AGENT_OS_VM_NAME",
	"vm.cpus":               "AGENT_OS_VM_CPUS",
	"vm.memory_mib":         "AGENT_OS_VM_MEMORY_MIB",
	"vm.disk_gib":           "AGENT_OS_VM_DISK_GIB",
	"access.mode":           "AGENT_OS_ACCESS_MODE",
	"orca.port":             "AGENT_OS_ORCA_PORT",
	"wireguard.interface":   "AGENT_OS_WIREGUARD_INTERFACE",
	"wireguard.address":     "AGENT_OS_WIREGUARD_ADDRESS",
	"repository.key_path":   "AGENT_OS_REPOSITORY_KEY_PATH",
	"network.allowed_cidrs": "AGENT_OS_ALLOWED_CIDRS",
	"release.repository":    "AGENT_OS_RELEASE_REPOSITORY",
	"state.dir":             "AGENT_OS_STATE_DIR",
	"log.format":            "AGENT_OS_LOG_FORMAT",
	"packages":              "AGENT_OS_PACKAGES",
}

// FlagValues contains only flags that were explicitly changed by a caller.
// Positional arguments can be represented by adding vm.name to this map.
type LoadOptions struct {
	ExplicitConfigPath string
	FlagValues         map[string]any
	EnvLookup          func(string) (string, bool)
	PromptRequired     bool
	PromptIn           io.Reader
	PromptOut          io.Writer
	IsTTY              bool
}

type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "environment"
	SourceFile    Source = "config-file"
	SourceDefault Source = "default"
	SourcePrompt  Source = "prompt"
)

type Resolved struct {
	Config      model.Config
	Sources     map[string]Source
	ConfigPath  string
	ConfigFound bool
}

func DefaultConfigPath(env func(string) (string, bool)) string {
	if env == nil {
		env = os.LookupEnv
	}
	base, ok := env("XDG_CONFIG_HOME")
	if !ok || strings.TrimSpace(base) == "" {
		home, homeOK := env("HOME")
		if !homeOK || strings.TrimSpace(home) == "" {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, defaultConfigDirName, defaultConfigName)
}

func DefaultStateDir(env func(string) (string, bool)) string {
	if env == nil {
		env = os.LookupEnv
	}
	if base, ok := env("XDG_STATE_HOME"); ok && strings.TrimSpace(base) != "" {
		return filepath.Join(base, defaultConfigDirName)
	}
	if home, ok := env("HOME"); ok && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "state", defaultConfigDirName)
	}
	return ""
}

func Load(opts LoadOptions) (Resolved, error) {
	env := opts.EnvLookup
	if env == nil {
		env = os.LookupEnv
	}
	defaults := model.DefaultConfig(DefaultStateDir(env))

	v := viper.New()
	v.SetConfigType("yaml")
	for key, envName := range documentedEnv {
		if err := v.BindEnv(key, envName); err != nil {
			return Resolved{}, fmt.Errorf("bind %s: %w", envName, err)
		}
	}
	setDefaults(v, defaults)

	configPath := opts.ExplicitConfigPath
	if strings.TrimSpace(configPath) == "" {
		configPath = DefaultConfigPath(env)
	}
	configFound := false
	if strings.TrimSpace(configPath) != "" {
		v.SetConfigFile(configPath)
		err := v.ReadInConfig()
		if err == nil {
			configFound = true
		} else {
			var notFound viper.ConfigFileNotFoundError
			_, isNotFound := err.(viper.ConfigFileNotFoundError)
			if !isNotFound {
				isNotFound = errors.As(err, &notFound)
			}
			if !isNotFound {
				isNotFound = errors.Is(err, os.ErrNotExist)
			}
			if opts.ExplicitConfigPath != "" || !isNotFound {
				return Resolved{}, fmt.Errorf("read config %q: %w", configPath, err)
			}
		}
	}
	// Make injected environments deterministic for tests and keep the
	// production path equivalent to Viper's explicitly bound environment
	// variables. No unlisted environment variable is consulted.
	for key, envName := range documentedEnv {
		if value, ok := env(envName); ok && strings.TrimSpace(value) != "" {
			v.Set(key, value)
		}
	}

	// Viper's Set values are explicit overrides and therefore take precedence
	// over env/config/default values.
	for key, value := range opts.FlagValues {
		v.Set(key, value)
	}

	result := Resolved{
		ConfigPath:  configPath,
		ConfigFound: configFound,
		Sources:     make(map[string]Source, len(documentedEnv)),
		Config:      readConfig(v),
	}
	if opts.PromptRequired && opts.IsTTY && result.Config.AccessMode == model.AccessWireGuard {
		promptReader := bufio.NewReader(opts.PromptIn)
		if result.Config.WireGuardInterface == "" {
			value, err := promptReaderValue(promptReader, opts.PromptOut, "WireGuard interface")
			if err != nil {
				return Resolved{}, err
			}
			result.Config.WireGuardInterface = value
			result.Sources["wireguard.interface"] = SourcePrompt
		}
		if result.Config.WireGuardAddress == "" {
			value, err := promptReaderValue(promptReader, opts.PromptOut, "WireGuard address")
			if err != nil {
				return Resolved{}, err
			}
			result.Config.WireGuardAddress = value
			result.Sources["wireguard.address"] = SourcePrompt
		}
	}
	for key := range documentedEnv {
		if _, alreadyPrompted := result.Sources[key]; !alreadyPrompted {
			result.Sources[key] = sourceFor(key, opts, env, configFound, configPath, v)
		}
	}
	if err := result.Config.Validate(); err != nil {
		return Resolved{}, err
	}
	return result, nil
}

func setDefaults(v *viper.Viper, c model.Config) {
	v.SetDefault("vm.name", c.VMName)
	v.SetDefault("vm.cpus", c.VMCPUs)
	v.SetDefault("vm.memory_mib", c.VMMemoryMiB)
	v.SetDefault("vm.disk_gib", c.VMDiskGiB)
	v.SetDefault("access.mode", string(c.AccessMode))
	v.SetDefault("orca.port", c.OrcaPort)
	v.SetDefault("network.allowed_cidrs", []string{})
	v.SetDefault("wireguard.interface", "")
	v.SetDefault("wireguard.address", "")
	v.SetDefault("repository.key_path", "")
	v.SetDefault("release.repository", c.ReleaseRepository)
	v.SetDefault("state.dir", c.StateDir)
	v.SetDefault("log.format", string(c.LogFormat))
	v.SetDefault("packages", c.Packages)
}

func readConfig(v *viper.Viper) model.Config {
	allowedCIDRs := stringSlice(v.Get("network.allowed_cidrs"))
	if allowedCIDRs == nil {
		allowedCIDRs = []string{}
	}
	return model.Config{
		VMName:             v.GetString("vm.name"),
		VMCPUs:             v.GetInt("vm.cpus"),
		VMMemoryMiB:        v.GetInt("vm.memory_mib"),
		VMDiskGiB:          v.GetInt("vm.disk_gib"),
		AccessMode:         model.AccessMode(v.GetString("access.mode")),
		OrcaPort:           v.GetInt("orca.port"),
		WireGuardInterface: v.GetString("wireguard.interface"),
		WireGuardAddress:   v.GetString("wireguard.address"),
		RepositoryKeyPath:  v.GetString("repository.key_path"),
		AllowedCIDRs:       allowedCIDRs,
		ReleaseRepository:  v.GetString("release.repository"),
		StateDir:           v.GetString("state.dir"),
		LogFormat:          model.LogFormat(v.GetString("log.format")),
		Packages:           stringSlice(v.Get("packages")),
	}
}

func stringSlice(value any) []string {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		parts := strings.Split(v, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	default:
		return nil
	}
}

func sourceFor(key string, opts LoadOptions, env func(string) (string, bool), configFound bool, configPath string, v *viper.Viper) Source {
	if _, ok := opts.FlagValues[key]; ok {
		return SourceFlag
	}
	if envName, ok := documentedEnv[key]; ok {
		if value, exists := env(envName); exists && strings.TrimSpace(value) != "" {
			return SourceEnv
		}
	}
	if configFound && configHasKey(v, key) {
		return SourceFile
	}
	_ = configPath // retained in the result for diagnostics and future source types
	return SourceDefault
}

func configHasKey(v *viper.Viper, key string) bool {
	return v.InConfig(key)
}

func (r Resolved) EffectiveValues() map[string]any {
	c := r.Config
	return map[string]any{
		"vm.name":               c.VMName,
		"vm.cpus":               c.VMCPUs,
		"vm.memory_mib":         c.VMMemoryMiB,
		"vm.disk_gib":           c.VMDiskGiB,
		"access.mode":           c.AccessMode,
		"orca.port":             c.OrcaPort,
		"wireguard.interface":   c.WireGuardInterface,
		"wireguard.address":     c.WireGuardAddress,
		"repository.key_path":   c.RepositoryKeyPath,
		"network.allowed_cidrs": c.AllowedCIDRs,
		"release.repository":    c.ReleaseRepository,
		"state.dir":             c.StateDir,
		"log.format":            c.LogFormat,
		"packages":              c.Packages,
	}
}

func (r Resolved) RedactedValues() map[string]any {
	values := r.EffectiveValues()
	if _, ok := values["repository.key_path"]; ok {
		if strings.TrimSpace(fmt.Sprint(values["repository.key_path"])) != "" {
			values["repository.key_path"] = "<redacted>"
		}
	}
	return values
}

func (r Resolved) ConfigYAML() ([]byte, error) {
	// Keep this explicit instead of marshaling Config: the file shape is a
	// stable external API and does not include source metadata or secrets.
	var b strings.Builder
	b.WriteString("# agent-os configuration; secrets and confirmations do not belong here.\n")
	fmt.Fprintf(&b, "vm:\n  name: %s\n  cpus: %d\n  memory_mib: %d\n  disk_gib: %d\n", r.Config.VMName, r.Config.VMCPUs, r.Config.VMMemoryMiB, r.Config.VMDiskGiB)
	fmt.Fprintf(&b, "access:\n  mode: %s\n", r.Config.AccessMode)
	fmt.Fprintf(&b, "orca:\n  port: %d\n", r.Config.OrcaPort)
	if r.Config.WireGuardInterface != "" || r.Config.WireGuardAddress != "" {
		fmt.Fprintf(&b, "wireguard:\n  interface: %s\n  address: %s\n", r.Config.WireGuardInterface, r.Config.WireGuardAddress)
	}
	if r.Config.RepositoryKeyPath != "" {
		fmt.Fprintf(&b, "repository:\n  key_path: %s\n", r.Config.RepositoryKeyPath)
	}
	fmt.Fprintf(&b, "network:\n  allowed_cidrs: %s\n", yamlList(r.Config.AllowedCIDRs))
	fmt.Fprintf(&b, "release:\n  repository: %s\nstate:\n  dir: %s\nlog:\n  format: %s\npackages: %s\n", r.Config.ReleaseRepository, r.Config.StateDir, r.Config.LogFormat, yamlList(r.Config.Packages))
	return []byte(b.String()), nil
}

func yamlList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, strconv.Quote(item))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func WriteConfig(path string, contents []byte) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("config path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return writeAtomic(path, contents, 0o600)
}

func writeAtomic(path string, contents []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-os-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// PromptRequired obtains an unset required value only from an interactive
// terminal. It deliberately never writes the answer to Viper or a config file.
func PromptRequired(in io.Reader, out io.Writer, isTTY bool, label string) (string, error) {
	if !isTTY {
		return "", fmt.Errorf("%s is required; provide it through the command's interactive prompt on a TTY", label)
	}
	if in == nil {
		return "", fmt.Errorf("%s is required; interactive input is unavailable", label)
	}
	if out == nil {
		out = io.Discard
	}
	if _, err := fmt.Fprintf(out, "%s: ", label); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return line, nil
}

func promptReaderValue(reader *bufio.Reader, out io.Writer, label string) (string, error) {
	if out == nil {
		out = io.Discard
	}
	if _, err := fmt.Fprintf(out, "%s: ", label); err != nil {
		return "", err
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return line, nil
}

// BindChangedFlags copies only flags explicitly supplied by the user. A
// pflag default must not accidentally become a higher-precedence override.
func BindChangedFlags(flags *pflag.FlagSet, names ...string) map[string]any {
	values := make(map[string]any)
	for _, name := range names {
		flag := flags.Lookup(name)
		if flag == nil || !flag.Changed {
			continue
		}
		values[name] = flag.Value.String()
	}
	return values
}
