package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/provision"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
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
	"profiles.disk_gib":     "AGENT_OS_PROFILE_DISK_GIB",
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
	"skills":                "AGENT_OS_SKILLS",
}

// FlagValues contains only flags that were explicitly changed by a caller.
// Positional arguments can be represented by adding vm.name to this map.
type LoadOptions struct {
	ExplicitConfigPath string
	FlagValues         map[string]any
	EnvLookup          func(string) (string, bool)
	PromptRequired     bool
	// PromptFields names fields that should be collected during an
	// interactive create flow when they are still at their built-in default.
	// Values supplied by flags, the environment, or a config file always win.
	PromptFields []string
	PromptIn     io.Reader
	PromptOut    io.Writer
	IsTTY        bool
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
	if err := validateIntegerValue(v.Get("profiles.disk_gib"), "profiles.disk_gib"); err != nil {
		return Resolved{}, err
	}

	result := Resolved{
		ConfigPath:  configPath,
		ConfigFound: configFound,
		Sources:     make(map[string]Source, len(documentedEnv)),
		Config:      readConfig(v),
	}
	if opts.PromptRequired {
		if err := promptCreateFields(&result, opts, env, configFound, configPath, v); err != nil {
			return Resolved{}, err
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

func validateIntegerValue(value any, key string) error {
	valid := false
	switch typed := value.(type) {
	case int:
		valid = true
	case int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		valid = true
	case float32, float64:
		// A YAML/JSON fractional representation is not the documented
		// integer configuration type, even when its numeric value is whole.
		valid = false
	case string:
		_, err := strconv.Atoi(strings.TrimSpace(typed))
		valid = err == nil
	}
	if !valid {
		return fmt.Errorf("invalid configuration: %s must be a positive integer", key)
	}
	return nil
}

func promptCreateFields(result *Resolved, opts LoadOptions, env func(string) (string, bool), configFound bool, configPath string, v *viper.Viper) error {
	fields := append([]string(nil), opts.PromptFields...)
	// Preserve the original behavior for callers that request interactive
	// loading without naming create fields: WireGuard details are required when
	// WireGuard access was selected explicitly.
	if len(fields) == 0 && result.Config.AccessMode == model.AccessWireGuard {
		fields = []string{"wireguard.interface", "wireguard.address"}
	}
	if len(fields) == 0 {
		return nil
	}

	for _, field := range fields {
		if promptSource(field, opts, env, configFound, configPath, v) != SourceDefault {
			continue
		}
		if !opts.IsTTY {
			return requiredPromptError(promptLabel(field))
		}
	}
	if !opts.IsTTY {
		return nil
	}
	if opts.PromptIn == nil {
		return errors.New("interactive configuration requires a readable TTY")
	}

	for _, field := range fields {
		if promptSource(field, opts, env, configFound, configPath, v) != SourceDefault {
			continue
		}
		value, err := promptField(opts.PromptIn, opts.PromptOut, field, promptDefault(result.Config, field))
		if err != nil {
			return err
		}
		if err := setPromptedField(&result.Config, field, value); err != nil {
			return err
		}
		result.Sources[field] = SourcePrompt
	}

	// Access mode may have been selected by the prompt. WireGuard's host
	// endpoint is then collected only when it was not already explicit.
	if result.Config.AccessMode == model.AccessWireGuard {
		for _, field := range []string{"wireguard.interface", "wireguard.address"} {
			if result.Sources[field] == SourcePrompt {
				continue
			}
			if promptSource(field, opts, env, configFound, configPath, v) != SourceDefault {
				continue
			}
			value, err := promptField(opts.PromptIn, opts.PromptOut, field, "")
			if err != nil {
				return err
			}
			if err := setPromptedField(&result.Config, field, value); err != nil {
				return err
			}
			result.Sources[field] = SourcePrompt
		}
	}
	return nil
}

func promptSource(key string, opts LoadOptions, env func(string) (string, bool), configFound bool, configPath string, v *viper.Viper) Source {
	return sourceFor(key, opts, env, configFound, configPath, v)
}

func requiredPromptError(label string) error {
	return fmt.Errorf("%s is required; provide it with a flag, environment variable, or config file, or run this command on a TTY", label)
}

func promptLabel(field string) string {
	switch field {
	case "vm.name":
		return "VM name"
	case "access.mode":
		return "access mode (local or wireguard)"
	case "orca.port":
		return "Orca/WireGuard TCP port"
	case "wireguard.interface":
		return "WireGuard interface"
	case "wireguard.address":
		return "WireGuard address"
	default:
		return field
	}
}

func promptDefault(c model.Config, field string) string {
	switch field {
	case "vm.name":
		return c.VMName
	case "access.mode":
		return string(c.AccessMode)
	case "orca.port":
		return strconv.Itoa(c.OrcaPort)
	default:
		return ""
	}
}

func setPromptedField(c *model.Config, field, value string) error {
	switch field {
	case "vm.name":
		c.VMName = value
	case "access.mode":
		c.AccessMode = model.AccessMode(value)
	case "orca.port":
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("Orca port must be a number: %w", err)
		}
		c.OrcaPort = port
	case "wireguard.interface":
		c.WireGuardInterface = value
	case "wireguard.address":
		c.WireGuardAddress = value
	default:
		return fmt.Errorf("unsupported interactive configuration field %q", field)
	}
	return nil
}

func setDefaults(v *viper.Viper, c model.Config) {
	v.SetDefault("vm.name", c.VMName)
	v.SetDefault("vm.cpus", c.VMCPUs)
	v.SetDefault("vm.memory_mib", c.VMMemoryMiB)
	v.SetDefault("vm.disk_gib", c.VMDiskGiB)
	v.SetDefault("profiles.disk_gib", c.ProfileDiskGiB)
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
	v.SetDefault("skills", c.Skills)
}

func readConfig(v *viper.Viper) model.Config {
	allowedCIDRs := stringSlice(v.Get("network.allowed_cidrs"))
	if allowedCIDRs == nil {
		allowedCIDRs = []string{}
	}
	skills := stringSlice(v.Get("skills"))
	// The Chrome DevTools CLI skill is a built-in capability. Configured
	// entries are additive so an operator cannot accidentally remove it from
	// a newly provisioned VM by replacing the top-level list.
	skills = provision.MergeSkills(skills)
	return model.Config{
		VMName:             v.GetString("vm.name"),
		VMCPUs:             v.GetInt("vm.cpus"),
		VMMemoryMiB:        v.GetInt("vm.memory_mib"),
		VMDiskGiB:          v.GetInt("vm.disk_gib"),
		ProfileDiskGiB:     v.GetInt("profiles.disk_gib"),
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
		Skills:             skills,
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
		"profiles.disk_gib":     c.ProfileDiskGiB,
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
		"skills":                c.Skills,
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
	fmt.Fprintf(&b, "profiles:\n  disk_gib: %d\n", r.Config.ProfileDiskGiB)
	fmt.Fprintf(&b, "access:\n  mode: %s\n", r.Config.AccessMode)
	fmt.Fprintf(&b, "orca:\n  port: %d\n", r.Config.OrcaPort)
	if r.Config.WireGuardInterface != "" || r.Config.WireGuardAddress != "" {
		fmt.Fprintf(&b, "wireguard:\n  interface: %s\n  address: %s\n", r.Config.WireGuardInterface, r.Config.WireGuardAddress)
	}
	if r.Config.RepositoryKeyPath != "" {
		fmt.Fprintf(&b, "repository:\n  key_path: %s\n", r.Config.RepositoryKeyPath)
	}
	fmt.Fprintf(&b, "network:\n  allowed_cidrs: %s\n", yamlList(r.Config.AllowedCIDRs))
	fmt.Fprintf(&b, "release:\n  repository: %s\nstate:\n  dir: %s\nlog:\n  format: %s\npackages: %s\nskills: %s\n", r.Config.ReleaseRepository, r.Config.StateDir, r.Config.LogFormat, yamlList(r.Config.Packages), yamlList(r.Config.Skills))
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
		return "", requiredPromptError(label)
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
	line, err := readPromptLine(in)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return line, nil
}

func promptField(in io.Reader, out io.Writer, field, defaultValue string) (string, error) {
	if out == nil {
		out = io.Discard
	}
	label := promptLabel(field)
	if defaultValue != "" {
		label = fmt.Sprintf("%s [%s]", label, defaultValue)
	}
	if _, err := fmt.Fprintf(out, "%s: ", label); err != nil {
		return "", err
	}
	line, err := readPromptLine(in)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		if defaultValue != "" {
			return defaultValue, nil
		}
		return "", fmt.Errorf("%s is required", promptLabel(field))
	}
	return line, nil
}

func readPromptLine(in io.Reader) (string, error) {
	var line strings.Builder
	var one [1]byte
	for {
		n, err := in.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				return line.String(), nil
			}
			line.WriteByte(one[0])
		}
		if err != nil {
			return line.String(), err
		}
	}
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
