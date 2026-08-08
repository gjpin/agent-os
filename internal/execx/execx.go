package execx

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner is the narrow subprocess boundary used by host providers. Commands
// are always an executable plus an argument slice; there is intentionally no
// shell/eval helper in this package.
type Runner interface {
	Run(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("empty executable")
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}

type Invocation struct {
	Name string
	Args []string
}

type RecordingRunner struct {
	Calls []Invocation
	Err   error
}

func (r *RecordingRunner) Run(_ context.Context, name string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	r.Calls = append(r.Calls, Invocation{Name: name, Args: append([]string(nil), args...)})
	return r.Err
}
