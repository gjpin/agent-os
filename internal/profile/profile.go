// Package profile owns the host-side identity and metadata for the persistent
// per-VM agent profile disk. The disk contents are deliberately opaque here:
// credentials and agent state live inside the guest filesystem, never in this
// metadata file.
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gjpin/agent-os/internal/model"
)

const (
	SchemaVersion = 1
	Filesystem    = "ext4"
	MetadataName  = "metadata.json"
	ProfileMount  = "/var/lib/agent-os/profile"
)

// Metadata contains only the information needed to identify and safely reuse
// a profile disk. It must never grow a credential or guest-state field.
type Metadata struct {
	SchemaVersion int    `json:"schema_version"`
	Provider      string `json:"provider"`
	DiskID        string `json:"disk_id"`
	SizeGiB       int    `json:"size_gib"`
	Filesystem    string `json:"filesystem"`
	Label         string `json:"label"`
}

type Store struct {
	Root string
}

func NewStore(root string) Store { return Store{Root: root} }

func (s Store) Dir(name string) (string, error) {
	if !model.VMNameIsValid(name) || strings.ContainsAny(name, `/\\`) || name == "." || name == ".." {
		return "", fmt.Errorf("invalid VM name %q", name)
	}
	if strings.TrimSpace(s.Root) == "" {
		return "", errors.New("state directory is empty")
	}
	return filepath.Join(s.Root, "v1", "profiles", name), nil
}

func (s Store) MetadataPath(name string) (string, error) {
	dir, err := s.Dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, MetadataName), nil
}

func (s Store) Load(name string) (Metadata, error) {
	path, err := s.MetadataPath(name)
	if err != nil {
		return Metadata{}, err
	}
	dirInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return Metadata{}, err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return Metadata{}, errors.New("profile metadata directory is not a private directory")
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return Metadata{}, err
	}
	if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
		return Metadata{}, fmt.Errorf("profile metadata is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, err
	}
	var value Metadata
	if err := json.Unmarshal(data, &value); err != nil {
		return Metadata{}, fmt.Errorf("decode profile metadata %q: %w", path, err)
	}
	if err := ValidateMetadata(value); err != nil {
		return Metadata{}, fmt.Errorf("invalid profile metadata %q: %w", path, err)
	}
	return value, nil
}

func ValidateMetadata(value Metadata) error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported profile schema version %d", value.SchemaVersion)
	}
	if value.Provider != "lima" {
		return fmt.Errorf("unsupported profile provider %q", value.Provider)
	}
	if !validDiskID(value.DiskID) {
		return fmt.Errorf("invalid profile disk identity %q", value.DiskID)
	}
	if value.SizeGiB < 1 {
		return errors.New("profile disk size must be positive")
	}
	if value.Filesystem != Filesystem {
		return fmt.Errorf("unsupported profile filesystem %q", value.Filesystem)
	}
	if value.Label == "" || !validDiskLabel(value.Label) {
		return fmt.Errorf("invalid profile filesystem label %q", value.Label)
	}
	return nil
}

// DiskID returns a deterministic, length-safe identifier. Keep the readable
// VM-name prefix for operator diagnostics and use a full hash suffix to avoid
// collisions after truncation or punctuation normalization.
func DiskID(name string) string {
	clean := strings.ToLower(name)
	clean = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(clean, "-")
	clean = strings.Trim(clean, "-")
	if clean == "" {
		clean = "vm"
	}
	digest := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(digest[:])[:16]
	const maxLength = 48
	prefixLength := maxLength - len("agent-os-profile--") - len(suffix)
	if prefixLength < 1 {
		prefixLength = 1
	}
	if len(clean) > prefixLength {
		clean = clean[:prefixLength]
	}
	return "agent-os-profile-" + clean + "-" + suffix
}

func DiskLabel(diskID string) string {
	prefix := "agent-os-"
	// ext4 filesystem labels are limited to 16 bytes. Keep an agent-os marker
	// and the deterministic hash suffix; the full collision-resistant disk ID
	// remains in metadata and in the Lima disk identity.
	suffix := diskID
	if len(suffix) > 7 {
		suffix = suffix[len(suffix)-7:]
	}
	return prefix + suffix
}

func legacyDiskLabel(diskID string) string {
	suffix := diskID
	if len(suffix) > 11 {
		suffix = suffix[len(suffix)-11:]
	}
	return "lima-" + suffix
}

// DiskLabelIsCompatible accepts the current deterministic label and the
// previous Lima-prefixed form so existing Lima profiles remain reusable.
func DiskLabelIsCompatible(diskID, label string) bool {
	return label == DiskLabel(diskID) || label == legacyDiskLabel(diskID)
}

func validDiskID(value string) bool {
	return strings.HasPrefix(value, "agent-os-profile-") && len(value) <= 64 && regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(value)
}

func validDiskLabel(value string) bool {
	return len(value) <= 16 && regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(value)
}

func (s Store) Save(name string, value Metadata) error {
	if err := ValidateMetadata(value); err != nil {
		return err
	}
	path, err := s.MetadataPath(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return errors.New("profile path is not a private directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".metadata-*")
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
	dir, err := s.Dir(name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Clean(dir)); err != nil {
		return err
	}
	return nil
}
