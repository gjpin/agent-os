package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gjpin/agent-os/internal/model"
)

const SchemaVersion = 1

type State struct {
	SchemaVersion int               `json:"schema_version"`
	Name          string            `json:"name"`
	Provider      string            `json:"provider"`
	BackendID     string            `json:"backend_id,omitempty"`
	Lifecycle     model.VMStatus    `json:"lifecycle"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Artifacts     map[string]string `json:"artifacts,omitempty"`
	// Autostart is optional so state files written before VM autostart was
	// introduced continue to decode as disabled. A nil value is equivalent to
	// an explicitly disabled registration.
	Autostart *AutostartState `json:"autostart,omitempty"`
}

// AutostartState records the application's successful host-boot
// registration. The provider remains the source of truth for the actual
// registration; this metadata lets the CLI report intent without requiring a
// provider-specific query command.
type AutostartState struct {
	Enabled bool `json:"enabled"`
}

// Autostart is retained as a concise name for callers that want to construct
// metadata directly.
type Autostart = AutostartState

type Store struct {
	Root string
	Now  func() time.Time
}

func NewStore(root string) Store {
	return Store{Root: root, Now: time.Now}
}

func (s Store) VMDir(name string) (string, error) {
	if !model.VMNameIsValid(name) || strings.Contains(name, string(filepath.Separator)) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid VM name %q", name)
	}
	if strings.TrimSpace(s.Root) == "" {
		return "", errors.New("state directory is empty")
	}
	return filepath.Join(s.Root, "v1", "vms", name), nil
}

func (s Store) Path(name string) (string, error) {
	dir, err := s.VMDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

func (s Store) LockPath(name string) (string, error) {
	dir, err := s.VMDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.lock"), nil
}

func (s Store) Load(name string) (State, error) {
	path, err := s.Path(name)
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var value State
	if err := json.Unmarshal(data, &value); err != nil {
		return State{}, fmt.Errorf("decode state %q: %w", path, err)
	}
	if value.SchemaVersion != SchemaVersion {
		return State{}, fmt.Errorf("unsupported state schema version %d", value.SchemaVersion)
	}
	if value.Name != name {
		return State{}, fmt.Errorf("state name %q does not match %q", value.Name, name)
	}
	return value, nil
}

func (s Store) Save(value State) error {
	if value.SchemaVersion == 0 {
		value.SchemaVersion = SchemaVersion
	}
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported state schema version %d", value.SchemaVersion)
	}
	if !model.VMNameIsValid(value.Name) {
		return fmt.Errorf("invalid VM name %q", value.Name)
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if value.CreatedAt.IsZero() {
		value.CreatedAt = s.Now().UTC()
	}
	value.UpdatedAt = s.Now().UTC()
	dir, err := s.VMDir(value.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path := filepath.Join(dir, "state.json")
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s Store) Delete(name string) error {
	dir, err := s.VMDir(name)
	if err != nil {
		return err
	}
	// Provider artifacts live beside state.json and can contain large disk
	// images or boot media. A destroyed VM must not leave those artifacts or
	// the per-VM lock directory behind.
	return os.RemoveAll(filepath.Clean(dir))
}

// WithLock serializes reconciliation for one VM. The lock is a separate file
// and is never treated as lifecycle state. flock is inherited-safe for the
// short-lived process model and releases automatically on process exit.
func (s Store) WithLock(ctx context.Context, name string, fn func() error) error {
	path, err := s.LockPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := lockWithContext(ctx, int(file.Fd())); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func lockWithContext(ctx context.Context, fd int) error {
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
