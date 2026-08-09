package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/execx"
	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/profile"
)

func TestLibvirtProfileDiskCreatesAndGrowsWithoutShrinking(t *testing.T) {
	stateDir := t.TempDir()
	c := model.DefaultConfig(stateDir)
	c.VMName = "profile-grow"
	c.ProfileDiskGiB = 12
	createRunner := &profileRunner{}
	if err := ensureLibvirtProfile(context.Background(), createRunner, nil, nil, Spec{Config: c}); err != nil {
		t.Fatal(err)
	}
	if len(createRunner.Calls) != 1 || createRunner.Calls[0].Name != "qemu-img" || createRunner.Calls[0].Args[0] != "create" {
		t.Fatalf("unexpected profile creation calls: %+v", createRunner.Calls)
	}
	store := profile.NewStore(stateDir)
	metadata, err := store.Load(c.VMName)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SizeGiB != 12 {
		t.Fatalf("metadata size = %d, want 12", metadata.SizeGiB)
	}

	c.ProfileDiskGiB = 8
	growRunner := &profileRunner{InfoSize: 5 << 30}
	if err := ensureLibvirtProfile(context.Background(), growRunner, nil, nil, Spec{Config: c}); err != nil {
		t.Fatal(err)
	}
	if len(growRunner.Calls) != 2 || growRunner.Calls[1].Args[0] != "resize" || !strings.HasSuffix(strings.Join(growRunner.Calls[1].Args, " "), "12G") {
		t.Fatalf("profile disk was not grown to its retained minimum: %+v", growRunner.Calls)
	}
	metadata, err = store.Load(c.VMName)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SizeGiB != 12 {
		t.Fatalf("retained metadata size = %d, want 12", metadata.SizeGiB)
	}
}

func TestLibvirtProfileRejectsProviderMismatchAndUntrustedDisk(t *testing.T) {
	stateDir := t.TempDir()
	c := model.DefaultConfig(stateDir)
	c.VMName = "profile-mismatch"
	store := profile.NewStore(stateDir)
	diskID := profile.DiskID(c.VMName)
	if err := store.Save(c.VMName, profile.Metadata{
		SchemaVersion: profile.SchemaVersion,
		Provider:      "lima",
		DiskID:        diskID,
		SizeGiB:       10,
		Filesystem:    profile.Filesystem,
		Label:         profile.DiskLabel("lima", diskID),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ensureLibvirtProfile(context.Background(), &profileRunner{}, nil, nil, Spec{Config: c}); err == nil || !strings.Contains(err.Error(), "belongs to provider") {
		t.Fatalf("provider mismatch was accepted: %v", err)
	}

	c.VMName = "profile-untrusted"
	diskPath, err := store.DiskPath(c.VMName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("not a trusted profile disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureLibvirtProfile(context.Background(), &profileRunner{}, nil, nil, Spec{Config: c}); err == nil || !strings.Contains(err.Error(), "without trusted metadata") {
		t.Fatalf("untrusted disk was accepted: %v", err)
	}
}

type profileRunner struct {
	Calls    []execx.Invocation
	InfoSize int64
}

func (r *profileRunner) Run(_ context.Context, name string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	r.Calls = append(r.Calls, execx.Invocation{Name: name, Args: append([]string(nil), args...)})
	if name == "qemu-img" && len(args) > 0 && args[0] == "create" {
		return os.WriteFile(args[len(args)-1], nil, 0o600)
	}
	if name == "qemu-img" && len(args) > 0 && args[0] == "info" {
		_, _ = fmt.Fprintf(stdout, `{"virtual-size":%d}`, r.InfoSize)
	}
	return nil
}
