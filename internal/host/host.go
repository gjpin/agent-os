package host

import (
	"fmt"
	"io"
	"runtime"

	"github.com/gjpin/agent-os/internal/backend"
	"github.com/gjpin/agent-os/internal/execx"
)

type Info struct {
	OS           string
	Architecture string
}

func Detect() Info { return DetectAt(runtime.GOOS, runtime.GOARCH) }

func DetectAt(goos, architecture string) Info {
	return Info{OS: goos, Architecture: architecture}
}

func Provider(info Info, runner execx.Runner, out, errOut io.Writer) (backend.Provider, error) {
	switch {
	case info.OS == "linux" && info.Architecture == "amd64":
		return backend.Lima{Runner: runner, Out: out, Err: errOut, VMType: "qemu"}, nil
	case info.OS == "darwin" && info.Architecture == "arm64":
		return backend.Lima{Runner: runner, Out: out, Err: errOut, VMType: "vz"}, nil
	default:
		return nil, unsupportedHost(info)
	}
}

func unsupportedHost(info Info) error {
	switch {
	case info.OS == "linux":
		return fmt.Errorf("unsupported Linux architecture %q; Linux support requires x86_64 (linux/amd64)", info.Architecture)
	case info.OS == "darwin" && info.Architecture == "amd64":
		return fmt.Errorf("Intel macOS is unsupported; use Apple Silicon or an x86_64 Linux host")
	default:
		return fmt.Errorf("unsupported host %s/%s; supported hosts are linux/amd64 and darwin/arm64", info.OS, info.Architecture)
	}
}
