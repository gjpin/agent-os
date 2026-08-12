package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjpin/agent-os/internal/model"
)

func TestSaveIsAtomicPrivateAndVersioned(t *testing.T) {
	store := NewStore(t.TempDir())
	store.Now = func() time.Time { return time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC) }
	value := State{Name: "agents", Provider: "fake", Lifecycle: model.StatusStopped}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	path, err := store.Path("agents")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode is %o", info.Mode().Perm())
	}
	loaded, err := store.Load("agents")
	if err != nil || loaded.SchemaVersion != SchemaVersion || loaded.Name != "agents" {
		t.Fatalf("loaded state: %+v, err=%v", loaded, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secret") {
		t.Fatal("state unexpectedly contains a credential")
	}
}

func TestLockRejectsPathTraversal(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.WithLock(context.Background(), "../escape", func() error { return nil }); err == nil {
		t.Fatal("expected invalid VM name")
	}
}

func TestDeleteRemovesStateLockAndArtifacts(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Save(State{Name: "agents", Provider: "lima", Lifecycle: model.StatusStopped}); err != nil {
		t.Fatal(err)
	}
	dir, err := store.VMDir("agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath, err := store.LockPath("agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("agents"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("VM state directory still exists: %s (err=%v)", dir, err)
	}
}
