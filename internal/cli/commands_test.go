package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/backend"
	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/state"
)

type noopRunner struct{}

func (noopRunner) Run(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

type cliProvider struct {
	autostartCalls []string
	autostartErr   error
}

func (*cliProvider) Name() string                               { return "lima" }
func (*cliProvider) Available(context.Context) error            { return nil }
func (*cliProvider) Create(context.Context, backend.Spec) error { return nil }
func (*cliProvider) Start(context.Context, string) error        { return nil }
func (*cliProvider) Stop(context.Context, string) error         { return nil }
func (*cliProvider) Status(_ context.Context, name string) (backend.Status, error) {
	return backend.Status{Name: name, Provider: "lima", Lifecycle: model.StatusStopped}, nil
}
func (*cliProvider) Logs(context.Context, string, io.Writer, io.Writer) error { return nil }
func (*cliProvider) Destroy(context.Context, string) error                    { return nil }
func (*cliProvider) Upgrade(context.Context, string, backend.Spec) error      { return nil }
func (*cliProvider) Exec(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
func (*cliProvider) SyncProfile(context.Context, backend.Spec, bool) error { return nil }
func (*cliProvider) PurgeProfile(context.Context, backend.Spec) error      { return nil }
func (*cliProvider) RefreshAgentInstructions(context.Context, string, string) error {
	return nil
}
func (*cliProvider) ConfigureForwarding(context.Context, backend.Spec) error { return nil }
func (*cliProvider) ExecAsUser(context.Context, string, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
func (*cliProvider) ExecAsRoot(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
func (p *cliProvider) EnableAutostart(_ context.Context, name string) error {
	p.autostartCalls = append(p.autostartCalls, "enable "+name)
	return p.autostartErr
}
func (p *cliProvider) DisableAutostart(_ context.Context, name string) error {
	p.autostartCalls = append(p.autostartCalls, "disable "+name)
	return p.autostartErr
}

func TestChangeAutostartPersistsOnlySuccessfulRegistration(t *testing.T) {
	ctx := context.Background()
	config := model.DefaultConfig(t.TempDir())
	store := state.NewStore(config.StateDir)
	initial := state.State{Name: config.VMName, Provider: "lima", Lifecycle: model.StatusStopped}
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	app := &App{Out: io.Discard, Err: io.Discard}
	provider := &cliProvider{}
	if err := app.changeAutostart(ctx, provider, config, store, true); err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(config.VMName)
	if err != nil || value.Autostart == nil || !value.Autostart.Enabled {
		t.Fatalf("state=%+v err=%v", value, err)
	}
	if err := app.changeAutostart(ctx, provider, config, store, false); err != nil {
		t.Fatal(err)
	}
	value, err = store.Load(config.VMName)
	if err != nil || value.Autostart != nil {
		t.Fatalf("state=%+v err=%v", value, err)
	}
	if got := strings.Join(provider.autostartCalls, ","); got != "enable agents,disable agents" {
		t.Fatalf("calls=%q", got)
	}

	provider.autostartErr = errors.New("registration failed")
	if err := app.changeAutostart(ctx, provider, config, store, true); err == nil {
		t.Fatal("provider failure ignored")
	}
	value, err = store.Load(config.VMName)
	if err != nil || value.Autostart != nil {
		t.Fatalf("failed registration changed state: %+v err=%v", value, err)
	}
}

func TestCreateConfigPromptFailsOnNonTTY(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("vm:\n  name: agents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := New(bytes.NewBuffer(nil), &bytes.Buffer{}, &bytes.Buffer{}, &execx.RecordingRunner{})
	app.Command().SetArgs([]string{"--config", configPath, "--state-dir", dir, "create"})
	err := app.Command().Execute()
	if err == nil || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("expected non-TTY create prompt error, got %v", err)
	}
}

func TestAuthAgentCommands(t *testing.T) {
	tests := map[string]string{"codex": "orca account add --agent codex", "claude": "orca account add --agent claude", "opencode": "opencode auth login", "copilot": "copilot login", "pi": "pi"}
	for agent, want := range tests {
		got, ok := authAgentCommand(agent)
		if !ok || strings.Join(got, " ") != want {
			t.Errorf("authAgentCommand(%q)=(%q,%t), want %q", agent, strings.Join(got, " "), ok, want)
		}
	}
	if got, ok := authAgentCommand("unknown"); ok || got != nil {
		t.Fatalf("unknown=(%v,%t)", got, ok)
	}
}

func TestAutostartDescriptionExplainsHostTiming(t *testing.T) {
	app := New(bytes.NewBuffer(nil), io.Discard, io.Discard, noopRunner{})
	command, _, err := app.Command().Find([]string{"autostart"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Short != "Register a VM to start at Linux login or macOS boot" {
		t.Fatalf("description = %q", command.Short)
	}
}
