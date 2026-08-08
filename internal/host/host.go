package host

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/zero/agent-os/internal/backend"
	"github.com/zero/agent-os/internal/execx"
)

type Info struct {
	OS           string
	Architecture string
	Provider     string
}

func Detect() Info {
	provider := "unsupported"
	switch runtime.GOOS {
	case "linux":
		provider = "libvirt"
	case "darwin":
		provider = "lima"
	}
	return Info{OS: runtime.GOOS, Architecture: runtime.GOARCH, Provider: provider}
}

func Provider(info Info, runner execx.Runner, out, errOut io.Writer) (backend.Provider, error) {
	switch info.Provider {
	case "libvirt":
		return backend.Libvirt{Runner: runner, Out: out, Err: errOut}, nil
	case "lima":
		if info.Architecture != "arm64" {
			return nil, fmt.Errorf("Intel macOS is unsupported; use Apple Silicon or a supported Linux host")
		}
		return backend.Lima{Runner: runner, Out: out, Err: errOut}, nil
	default:
		return nil, fmt.Errorf("unsupported host %s/%s; supported hosts are Fedora/Ubuntu Linux and Apple Silicon macOS", info.OS, info.Architecture)
	}
}

func CheckProvider(ctx context.Context, p backend.Provider) error {
	if err := p.Available(ctx); err != nil {
		return err
	}
	return nil
}
