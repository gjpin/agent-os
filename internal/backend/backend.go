package backend

import (
	"context"
	"io"

	"github.com/zero/agent-os/internal/model"
)

type Spec struct {
	Config       model.Config
	Architecture string
	DryRun       bool
}

type Status struct {
	Name      string
	Provider  string
	Lifecycle model.VMStatus
	BackendID string
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
}

// The narrower interfaces keep lifecycle, networking, forwarding,
// provisioning, and inspection independently fakeable in unit tests. Provider
// combines the operations needed by the CLI while implementations may expose
// these facets separately.
type Lifecycle interface {
	Create(context.Context, Spec) error
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Destroy(context.Context, string) error
	Upgrade(context.Context, string, Spec) error
}

type Networking interface {
	EnsureNetwork(context.Context, Spec) error
}

type Forwarding interface {
	ConfigureForwarding(context.Context, Spec) error
}

type Provisioning interface {
	Provision(context.Context, string, Spec) error
}

type Inspection interface {
	Status(context.Context, string) (Status, error)
	Logs(context.Context, string, io.Writer, io.Writer) error
}

// UserExecutor lets providers keep operator-only administration separate from
// the unprivileged guest agent account.
type UserExecutor interface {
	ExecAsUser(context.Context, string, string, []string, io.Reader, io.Writer, io.Writer) error
}
