package backend

import (
	"context"
	"io"

	"github.com/gjpin/agent-os/internal/model"
)

type Spec struct {
	Config            model.Config
	Architecture      string
	AgentInstructions string
	DryRun            bool
}

type Status struct {
	Name      string
	Provider  string
	Lifecycle model.VMStatus
	Detail    string
}

type Provider interface {
	Name() string
	Available(context.Context) error
	Create(context.Context, Spec) error
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Status(context.Context, string) (Status, error)
	Logs(context.Context, string, io.Writer, io.Writer) error
	Destroy(context.Context, string) error
	Upgrade(context.Context, string, Spec) error
	Exec(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error
	SyncProfile(context.Context, Spec, bool) error
	PurgeProfile(context.Context, Spec) error
	RefreshAgentInstructions(context.Context, string, string) error
	EnableAutostart(context.Context, string) error
	DisableAutostart(context.Context, string) error
	ConfigureForwarding(context.Context, Spec) error
	ExecAsUser(context.Context, string, string, []string, io.Reader, io.Writer, io.Writer) error
	ExecAsRoot(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error
}
