package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskIDIsDeterministicAndLengthSafe(t *testing.T) {
	name := strings.Repeat("a", 63)
	first := DiskID(name)
	if first != DiskID(name) || len(first) > 48 || !validDiskID(first) {
		t.Fatalf("unexpected deterministic disk ID %q", first)
	}
	if first == DiskID("different") {
		t.Fatal("different VM names share a profile disk ID")
	}
}

func TestMetadataStoreUsesRestrictedAtomicFiles(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	name := "test-vm"
	diskID := DiskID(name)
	value := Metadata{
		SchemaVersion: SchemaVersion,
		Provider:      "libvirt",
		DiskID:        diskID,
		SizeGiB:       10,
		Filesystem:    Filesystem,
		Label:         DiskLabel("libvirt", diskID),
	}
	if err := store.Save(name, value); err != nil {
		t.Fatal(err)
	}
	path, err := store.MetadataPath(name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata mode = %o, want 600", info.Mode().Perm())
	}
	dir, err := store.Dir(name)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("profile directory mode = %o, want 700", dirInfo.Mode().Perm())
	}
	loaded, err := store.Load(name)
	if err != nil || loaded != value {
		t.Fatalf("loaded metadata = %+v, err=%v", loaded, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["credential"]; ok {
		t.Fatal("metadata unexpectedly contains a credential field")
	}
	if filepath.Base(path) != MetadataName {
		t.Fatalf("metadata path = %q", path)
	}
}

func TestMetadataRejectsUnexpectedFilesystemAndProvider(t *testing.T) {
	diskID := DiskID("agents")
	base := Metadata{SchemaVersion: SchemaVersion, Provider: "libvirt", DiskID: diskID, SizeGiB: 10, Filesystem: Filesystem, Label: DiskLabel("libvirt", diskID)}
	for _, mutate := range []func(*Metadata){func(m *Metadata) { m.Provider = "unknown" }, func(m *Metadata) { m.Filesystem = "xfs" }, func(m *Metadata) { m.Label = "not a label" }} {
		value := base
		mutate(&value)
		if err := ValidateMetadata(value); err == nil {
			t.Fatalf("accepted incompatible metadata %+v", value)
		}
	}
}
