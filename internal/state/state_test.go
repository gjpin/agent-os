package state

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zero/agent-os/internal/model"
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
