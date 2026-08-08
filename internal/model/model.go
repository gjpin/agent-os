package model

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/zero/agent-os/internal/provision"
)

// AccessMode controls which host address Orca binds to.
type AccessMode string

const (
	AccessLocal        AccessMode = "local"
	AccessWireGuard    AccessMode = "wireguard"
	DefaultVMName                 = "agents"
	DefaultOrcaPort               = 6768
	DefaultVMMemoryMiB            = 4096
	DefaultVMDiskGiB              = 120
	DefaultVMCPUs                 = 2
)

func (m AccessMode) Valid() bool { return m == AccessLocal || m == AccessWireGuard }

type LogFormat string

const (
	LogHuman LogFormat = "human"
	LogJSON  LogFormat = "json"
)

func (f LogFormat) Valid() bool { return f == LogHuman || f == LogJSON }

// Config is the non-secret operational configuration. It deliberately does
// not contain private-key contents, passphrases, tokens, or confirmations.
type Config struct {
	VMName             string
	VMCPUs             int
	VMMemoryMiB        int
	VMDiskGiB          int
	AccessMode         AccessMode
	OrcaPort           int
	WireGuardInterface string
	WireGuardAddress   string
	RepositoryKeyPath  string
	AllowedCIDRs       []string
	ReleaseRepository  string
	StateDir           string
	LogFormat          LogFormat
	Packages           []string
}

func DefaultConfig(stateDir string) Config {
	return Config{
		VMName:            DefaultVMName,
		VMCPUs:            DefaultVMCPUs,
		VMMemoryMiB:       DefaultVMMemoryMiB,
		VMDiskGiB:         DefaultVMDiskGiB,
		AccessMode:        AccessLocal,
		OrcaPort:          DefaultOrcaPort,
		ReleaseRepository: "zero/agent-os",
		StateDir:          stateDir,
		LogFormat:         LogHuman,
		Packages: []string{
			"git", "curl", "jq", "ripgrep", "fd-find", "tmux", "vim-enhanced",
		},
	}
}

func (c Config) Validate() error {
	var problems []string
	if !validVMName(c.VMName) {
		problems = append(problems, "vm.name must contain 1-63 letters, numbers, dots, underscores, or hyphens and may not begin with a hyphen")
	}
	if c.VMCPUs < 1 || c.VMCPUs > 8 {
		problems = append(problems, "vm.cpus must be between 1 and 8")
	}
	if c.VMMemoryMiB < 512 || c.VMMemoryMiB > 16*1024 {
		problems = append(problems, "vm.memory_mib must be between 512 and 16384")
	}
	if c.VMDiskGiB < 20 {
		problems = append(problems, "vm.disk_gib must be at least 20")
	}
	if !c.AccessMode.Valid() {
		problems = append(problems, "access.mode must be local or wireguard")
	}
	if c.OrcaPort < 1 || c.OrcaPort > 65535 {
		problems = append(problems, "orca.port must be between 1 and 65535")
	}
	if c.AccessMode == AccessWireGuard {
		if strings.TrimSpace(c.WireGuardInterface) == "" {
			problems = append(problems, "wireguard.interface is required when access.mode is wireguard")
		} else if !validInterfaceName(c.WireGuardInterface) {
			problems = append(problems, "wireguard.interface must be a valid host interface name")
		}
		if strings.TrimSpace(c.WireGuardAddress) == "" {
			problems = append(problems, "wireguard.address is required when access.mode is wireguard")
		} else if !validAddress(c.WireGuardAddress) {
			problems = append(problems, "wireguard.address must be an IP address or CIDR")
		}
	}
	for _, cidr := range c.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			problems = append(problems, fmt.Sprintf("network.allowed_cidrs contains invalid CIDR %q", cidr))
		}
	}
	if strings.TrimSpace(c.ReleaseRepository) == "" {
		problems = append(problems, "release.repository must not be empty")
	} else {
		parts := strings.Split(c.ReleaseRepository, "/")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || strings.ContainsAny(c.ReleaseRepository, " \t\r\n") {
			problems = append(problems, "release.repository must be an owner/name pair")
		}
	}
	if strings.TrimSpace(c.StateDir) == "" {
		problems = append(problems, "state.dir must not be empty")
	}
	if !c.LogFormat.Valid() {
		problems = append(problems, "log.format must be human or json")
	}
	if err := provision.ValidatePackages(c.Packages); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

func validAddress(value string) bool {
	if net.ParseIP(value) != nil {
		return true
	}
	_, _, err := net.ParseCIDR(value)
	return err == nil
}

func validInterfaceName(value string) bool {
	if len(value) == 0 || len(value) > 15 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// AdaptResources retains host capacity while honoring the documented maximum
// of eight vCPUs and 16 GiB. Zero host values mean that probing was not
// available, so only the documented maxima are applied.
func AdaptResources(c Config, hostCPUs, hostMemoryMiB int) Config {
	if hostCPUs > 1 {
		maxCPUs := hostCPUs - 1
		if maxCPUs < c.VMCPUs {
			c.VMCPUs = maxCPUs
		}
	}
	if hostMemoryMiB > 1024 {
		maxMemory := hostMemoryMiB - 1024
		if maxMemory < c.VMMemoryMiB {
			c.VMMemoryMiB = maxMemory
		}
	}
	if c.VMCPUs > 8 {
		c.VMCPUs = 8
	}
	if c.VMMemoryMiB > 16*1024 {
		c.VMMemoryMiB = 16 * 1024
	}
	return c
}

var vmNamePattern = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,62}$`)

func validVMName(name string) bool {
	return vmNamePattern.MatchString(name) && !strings.HasSuffix(name, ".")
}

// VMNameIsValid is exported for callers that validate positional names
// before loading state.
func VMNameIsValid(name string) bool { return validVMName(name) }

type VMStatus string

const (
	StatusUnknown  VMStatus = "unknown"
	StatusStopped  VMStatus = "stopped"
	StatusRunning  VMStatus = "running"
	StatusCreating VMStatus = "creating"
	StatusFailed   VMStatus = "failed"
)
