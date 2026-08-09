package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/gjpin/agent-os/internal/backend"
	"github.com/gjpin/agent-os/internal/execx"
)

type Info struct {
	OS           string
	Architecture string
	Provider     string
	// Distribution is the normalized Linux distribution ID from
	// /etc/os-release. It is empty for non-Linux hosts or when the release
	// metadata could not be read.
	Distribution string
	// DistributionFamily is the package/provider family selected from the exact
	// os-release ID. It lets setup-host use the same compatibility decision as
	// normal provider selection without rereading /etc/os-release.
	DistributionFamily string
}

func Detect() Info {
	return DetectAt(runtime.GOOS, runtime.GOARCH, "/etc/os-release")
}

// DetectAt is the file-backed form of Detect. Keeping the release path
// injectable makes host detection deterministic in tests and avoids making
// callers fake runtime.GOOS.
func DetectAt(goos, architecture, osReleasePath string) Info {
	var release io.Reader
	if goos == "linux" && strings.TrimSpace(osReleasePath) != "" {
		if file, err := os.Open(osReleasePath); err == nil {
			defer file.Close()
			release = file
		}
	}
	return detect(goos, architecture, release)
}

// DetectBytes is useful for embedders that already have os-release contents.
// It follows the same normalization and provider-selection rules as Detect.
func DetectBytes(goos, architecture string, osRelease []byte) Info {
	return detect(goos, architecture, strings.NewReader(string(osRelease)))
}

func detect(goos, architecture string, release io.Reader) Info {
	distribution, family := "", ""
	if goos == "linux" && release != nil {
		distribution, family = parseDistribution(release)
	}
	provider := "unsupported"
	switch goos {
	case "linux":
		if family != "" && (family != "arch" || architecture == "amd64") {
			provider = "libvirt"
		}
	case "darwin":
		// Preserve the existing macOS detection contract. Provider performs
		// the Apple Silicon architecture check and returns the user-facing
		// Intel macOS error.
		provider = "lima"
	}
	return Info{
		OS:                 goos,
		Architecture:       architecture,
		Provider:           provider,
		Distribution:       distribution,
		DistributionFamily: family,
	}
}

// SupportedDistribution reports whether the host distro is one of the
// distributions for which the libvirt setup and package names are defined.
func SupportedDistribution(distribution string) bool {
	return DistributionFamily(distribution) != ""
}

// DistributionFamily maps an os-release ID to the package family supported by
// agent-os. Only Fedora, Ubuntu, and Arch are supported; compatible
// derivatives are intentionally rejected until their package/service layouts
// are tested.
func DistributionFamily(distribution string) string {
	switch strings.ToLower(strings.TrimSpace(distribution)) {
	case "fedora":
		return "fedora"
	case "ubuntu":
		return "ubuntu"
	case "arch":
		return "arch"
	default:
		return ""
	}
}

func parseDistribution(release io.Reader) (string, string) {
	data, err := io.ReadAll(release)
	if err != nil {
		return "", ""
	}
	values := parseOSRelease(string(data))
	distribution := strings.ToLower(strings.TrimSpace(values["ID"]))
	if family := DistributionFamily(distribution); family != "" {
		return distribution, family
	}
	return distribution, ""
}

// parseOSRelease handles the quoting forms used by os-release. Only ID is
// needed for provider selection, but parsing the complete key/value syntax
// keeps detection correct for quoted values and comments.
func parseOSRelease(contents string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		key = strings.TrimSpace(key)
		values[key] = parseOSReleaseValue(strings.TrimSpace(raw))
	}
	return values
}

func parseOSReleaseValue(value string) string {
	if len(value) >= 2 {
		if value[0] == '"' && value[len(value)-1] == '"' {
			if decoded, err := strconv.Unquote(value); err == nil {
				return decoded
			}
		}
		if value[0] == '\'' && value[len(value)-1] == '\'' {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func unsupportedLinuxDistribution(info Info) error {
	if info.OS != "linux" {
		return nil
	}
	if SupportedDistribution(info.Distribution) {
		if DistributionFamily(info.Distribution) == "arch" && info.Architecture != "amd64" {
			return fmt.Errorf("unsupported Arch Linux architecture %q; Arch Linux support requires x86_64 (linux/amd64)", info.Architecture)
		}
		return nil
	}
	if strings.TrimSpace(info.Distribution) == "" {
		return fmt.Errorf("unable to detect a supported Linux distribution; supported distributions are Fedora, Ubuntu, and Arch Linux")
	}
	return fmt.Errorf("unsupported Linux distribution %q; supported distributions are Fedora, Ubuntu, and Arch Linux", info.Distribution)
}

func providerForLinux(info Info, runner execx.Runner, out, errOut io.Writer) (backend.Provider, error) {
	if err := unsupportedLinuxDistribution(info); err != nil {
		return nil, err
	}
	return backend.Libvirt{Runner: runner, Out: out, Err: errOut}, nil
}

func Provider(info Info, runner execx.Runner, out, errOut io.Writer) (backend.Provider, error) {
	switch info.Provider {
	case "libvirt":
		if info.OS != "linux" {
			return nil, fmt.Errorf("invalid %s provider selection for %s", info.Provider, info.OS)
		}
		return providerForLinux(info, runner, out, errOut)
	case "lima":
		if info.OS != "darwin" {
			return nil, fmt.Errorf("invalid Lima provider selection for %s", info.OS)
		}
		if info.Architecture != "arm64" {
			return nil, fmt.Errorf("Intel macOS is unsupported; use Apple Silicon or a supported Linux host")
		}
		return backend.Lima{Runner: runner, Out: out, Err: errOut}, nil
	default:
		if err := unsupportedLinuxDistribution(info); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unsupported host %s/%s; supported hosts are Fedora/Ubuntu Linux, x86_64 Arch Linux, and Apple Silicon macOS", info.OS, info.Architecture)
	}
}

func CheckProvider(ctx context.Context, p backend.Provider) error {
	if err := p.Available(ctx); err != nil {
		return err
	}
	return nil
}
