package backend

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gjpin/agent-os/internal/execx"
)

func command(runner execx.Runner, ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("empty host command")
	}
	return runner.Run(ctx, name, args, stdin, stdout, stderr)
}
